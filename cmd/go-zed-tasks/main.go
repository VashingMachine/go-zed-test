package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	env "github.com/caarlos0/env/v11"
)

const (
	envPrefix = "ZED_GO_TASKS_"
)

type Config struct {
	TasksPath            string   `env:"TASKS_PATH" envDefault:".zed/tasks.json"`
	DebugPath            string   `env:"DEBUG_PATH" envDefault:".zed/debug.json"`
	LabelPrefix          string   `env:"LABEL_PREFIX" envDefault:"go:"`
	DebugLabelPrefix     string   `env:"DEBUG_LABEL_PREFIX" envDefault:"go:debug:"`
	GoBinary             string   `env:"GO_BINARY" envDefault:"go"`
	TestNameRegex        string   `env:"TEST_NAME_REGEX" envDefault:"^Test"`
	GoListRegex          string   `env:"GO_LIST_REGEX" envDefault:"^Test"`
	AdditionalGoTestArgs []string `env:"ADDITIONAL_GO_TEST_ARGS" envDefault:"" envSeparator:","`
	UseNewTerminal       bool     `env:"USE_NEW_TERMINAL" envDefault:"false"`
	AllowConcurrentRuns  bool     `env:"ALLOW_CONCURRENT_RUNS" envDefault:"false"`
	Reveal               string   `env:"REVEAL" envDefault:"always"`
	Hide                 string   `env:"HIDE" envDefault:"never"`
	PruneGenerated       bool     `env:"PRUNE_GENERATED" envDefault:"true"`
	GeneratedEnvKey      string   `env:"GENERATED_ENV_KEY" envDefault:"ZED_GO_TEST_TASK_GENERATED"`
	GeneratedEnvValue    string   `env:"GENERATED_ENV_VALUE" envDefault:"1"`
	SubtestTimeout       string   `env:"SUBTEST_DISCOVERY_TIMEOUT" envDefault:"30s"`
}

type mergeStats struct {
	Added   int
	Updated int
	Removed int
}

type goTestJSONEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
}

type commonOptions struct {
	rootPath     string
	tasksPathArg string
	debugPathArg string
	dryRun       bool
}

type generateOptions struct {
	commonOptions
	goFilePath       string
	goTestArgs       stringSliceFlag
	subtestTimeout   string
	discoverSubtests bool
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		exitf("%v", err)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return runGenerate(args, generateTargetTasks)
	}

	switch args[0] {
	case "generate":
		return runGenerate(args[1:], generateTargetTasks)
	case "generate-debug":
		return runGenerate(args[1:], generateTargetDebug)
	case "debug":
		return runGenerate(args[1:], generateTargetDebug)
	case "clear":
		return runClear(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		// Backward compatibility: allow direct flags without explicit subcommand.
		return runGenerate(args, generateTargetTasks)
	}
}

type generateTarget string

const (
	generateTargetTasks generateTarget = "tasks"
	generateTargetDebug generateTarget = "debug"
)

func runGenerate(args []string, target generateTarget) error {
	var opts generateOptions
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.goFilePath, "file", "", "Path to the Go file currently open in Zed (required).")
	fs.StringVar(&opts.rootPath, "root", "", "Workspace root. If empty, auto-detected from go.mod/.git.")
	fs.StringVar(&opts.tasksPathArg, "tasks", "", "Override tasks JSON path.")
	fs.StringVar(&opts.debugPathArg, "debug", "", "Override debug JSON path.")
	fs.Var(&opts.goTestArgs, "go-test-arg", "Extra go test argument (repeatable). Example: -go-test-arg=-v -go-test-arg=-count=1")
	fs.StringVar(&opts.subtestTimeout, "subtest-timeout", "", "Timeout for discover-subtests test execution (e.g. 30s, 2m).")
	fs.BoolVar(&opts.discoverSubtests, "discover-subtests", false, "Run tests with go test -json and include discovered subtests.")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "Print resulting tasks JSON instead of writing it.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if opts.goFilePath == "" {
		return fmt.Errorf("missing required flag: -file")
	}

	absFilePath, err := filepath.Abs(opts.goFilePath)
	if err != nil {
		return fmt.Errorf("resolve file path: %w", err)
	}

	info, err := os.Stat(absFilePath)
	if err != nil {
		return fmt.Errorf("stat file %q: %w", absFilePath, err)
	}

	if info.IsDir() {
		return fmt.Errorf("file path points to a directory: %q", absFilePath)
	}

	if filepath.Ext(absFilePath) != ".go" {
		return fmt.Errorf("file must have .go extension: %q", absFilePath)
	}

	if opts.rootPath == "" {
		opts.rootPath = detectWorkspaceRoot(filepath.Dir(absFilePath))
	}

	absRootPath, err := filepath.Abs(opts.rootPath)
	if err != nil {
		return fmt.Errorf("resolve root path: %w", err)
	}

	cfg, err := loadConfig(opts.commonOptions)
	if err != nil {
		return err
	}

	allExtraGoTestArgs := make([]string, 0, len(cfg.AdditionalGoTestArgs)+len(opts.goTestArgs)+len(fs.Args()))
	allExtraGoTestArgs = append(allExtraGoTestArgs, cfg.AdditionalGoTestArgs...)
	allExtraGoTestArgs = append(allExtraGoTestArgs, opts.goTestArgs...)
	// Support passing args after `--`, e.g. -- -v -count=1.
	allExtraGoTestArgs = append(allExtraGoTestArgs, fs.Args()...)

	testNamePattern, err := regexp.Compile(cfg.TestNameRegex)
	if err != nil {
		return fmt.Errorf("invalid test_name_regex %q: %w", cfg.TestNameRegex, err)
	}

	testsInFile, err := findTestsInFile(absFilePath, testNamePattern)
	if err != nil {
		return fmt.Errorf("find tests in file: %w", err)
	}

	packageDir := filepath.Dir(absFilePath)
	testsListedByGo, err := listTestsWithGo(cfg.GoBinary, packageDir, cfg.GoListRegex)
	if err != nil {
		return fmt.Errorf("list tests with go: %w", err)
	}

	runnableTests := intersectTests(testsInFile, testsListedByGo)
	sort.Strings(runnableTests)

	pkgArg, err := packageArg(absRootPath, packageDir)
	if err != nil {
		return fmt.Errorf("build package argument: %w", err)
	}

	relFilePath := absFilePath
	if rel, relErr := filepath.Rel(absRootPath, absFilePath); relErr == nil {
		relFilePath = filepath.ToSlash(rel)
	}

	selectedTests := append([]string(nil), runnableTests...)
	discoveredTests := []string{}
	discoveredNewCount := 0
	subtestDiscoveryTimeout := time.Duration(0)
	if opts.discoverSubtests {
		subtestDiscoveryTimeout, err = resolveSubtestTimeout(cfg.SubtestTimeout, opts.subtestTimeout)
		if err != nil {
			return err
		}

		discoveredTests, err = discoverSubtestsWithGo(
			cfg.GoBinary,
			packageDir,
			runnableTests,
			subtestDiscoveryTimeout,
			allExtraGoTestArgs,
		)
		if err != nil {
			return fmt.Errorf("discover subtests: %w", err)
		}

		selectedTests = mergeUniqueTests(runnableTests, discoveredTests)
		sort.Strings(selectedTests)
		discoveredNewCount = countUniqueNotInBase(runnableTests, discoveredTests)
	}

	if target == generateTargetTasks {
		generatedTasks := makeGeneratedTasks(selectedTests, pkgArg, relFilePath, cfg, allExtraGoTestArgs)

		tasksAbsPath := resolvePath(absRootPath, cfg.TasksPath)
		mergedTasks, stats, err := mergeTasks(tasksAbsPath, generatedTasks, cfg)
		if err != nil {
			return fmt.Errorf("merge tasks: %w", err)
		}

		output, err := marshalTasks(mergedTasks)
		if err != nil {
			return err
		}

		if opts.dryRun {
			_, _ = os.Stdout.Write(output)
			return nil
		}

		if err := writeTasks(tasksAbsPath, output); err != nil {
			return fmt.Errorf("write tasks file: %w", err)
		}

		fmt.Printf("Updated %s\n", tasksAbsPath)
		fmt.Printf("Discovered in file: %d, runnable with go test -list: %d\n", len(testsInFile), len(runnableTests))
		if opts.discoverSubtests {
			fmt.Printf("Discovered by runtime execution: %d (new: %d, timeout %s)\n", len(discoveredTests), discoveredNewCount, subtestDiscoveryTimeout)
		}
		fmt.Printf("Tasks added: %d, updated: %d, removed: %d\n", stats.Added, stats.Updated, stats.Removed)
		for _, testName := range selectedTests {
			fmt.Printf("Generated task: %s%s\n", cfg.LabelPrefix, testName)
		}
		return nil
	}

	if target == generateTargetDebug {
		generatedDebugConfigs := makeGeneratedDebugConfigs(selectedTests, pkgArg, relFilePath, cfg, allExtraGoTestArgs)

		debugAbsPath := resolvePath(absRootPath, cfg.DebugPath)
		mergedDebug, stats, err := mergeTasks(debugAbsPath, generatedDebugConfigs, cfg)
		if err != nil {
			return fmt.Errorf("merge debug configs: %w", err)
		}

		output, err := marshalTasks(mergedDebug)
		if err != nil {
			return err
		}

		if opts.dryRun {
			_, _ = os.Stdout.Write(output)
			return nil
		}

		if err := writeTasks(debugAbsPath, output); err != nil {
			return fmt.Errorf("write debug file: %w", err)
		}

		fmt.Printf("Updated %s\n", debugAbsPath)
		fmt.Printf("Discovered in file: %d, runnable with go test -list: %d\n", len(testsInFile), len(runnableTests))
		if opts.discoverSubtests {
			fmt.Printf("Discovered by runtime execution: %d (new: %d, timeout %s)\n", len(discoveredTests), discoveredNewCount, subtestDiscoveryTimeout)
		}
		fmt.Printf("Debug configs added: %d, updated: %d, removed: %d\n", stats.Added, stats.Updated, stats.Removed)
		for _, testName := range selectedTests {
			fmt.Printf("Generated debug config: %s%s\n", cfg.DebugLabelPrefix, testName)
		}
		return nil
	}

	return fmt.Errorf("unsupported generate target %q", target)
}

func runClear(args []string) error {
	var opts commonOptions
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.rootPath, "root", "", "Workspace root. If empty, auto-detected from go.mod/.git.")
	fs.StringVar(&opts.tasksPathArg, "tasks", "", "Override tasks JSON path.")
	fs.StringVar(&opts.debugPathArg, "debug", "", "Override debug JSON path.")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "Print resulting tasks JSON instead of writing it.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if opts.rootPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		opts.rootPath = detectWorkspaceRoot(cwd)
	}

	absRootPath, err := filepath.Abs(opts.rootPath)
	if err != nil {
		return fmt.Errorf("resolve root path: %w", err)
	}

	cfg, err := loadConfig(opts)
	if err != nil {
		return err
	}

	tasksAbsPath := resolvePath(absRootPath, cfg.TasksPath)
	existing, err := readTasks(tasksAbsPath)
	if err != nil {
		return fmt.Errorf("read tasks %q: %w", tasksAbsPath, err)
	}

	filtered := make([]map[string]any, 0, len(existing))
	removed := 0
	for _, task := range existing {
		if isGenerated(task, cfg) {
			removed++
			continue
		}
		filtered = append(filtered, task)
	}

	output, err := marshalTasks(filtered)
	if err != nil {
		return err
	}

	if opts.dryRun {
		_, _ = os.Stdout.Write(output)
		return nil
	}

	if err := writeTasks(tasksAbsPath, output); err != nil {
		return fmt.Errorf("write tasks file: %w", err)
	}

	fmt.Printf("Updated %s\n", tasksAbsPath)
	fmt.Printf("Removed generated tasks: %d\n", removed)
	return nil
}

func loadConfig(opts commonOptions) (Config, error) {
	cfg, err := env.ParseAsWithOptions[Config](env.Options{
		Prefix: envPrefix,
	})
	if err != nil {
		return Config{}, fmt.Errorf("load config from env: %w", err)
	}

	if opts.tasksPathArg != "" {
		cfg.TasksPath = opts.tasksPathArg
	}
	if opts.debugPathArg != "" {
		cfg.DebugPath = opts.debugPathArg
	}
	return cfg, nil
}

func printUsage() {
	fmt.Println(`Usage:
  go-zed-tasks generate -file <path/to/file_test.go> [flags]
  go-zed-tasks generate-debug -file <path/to/file_test.go> [flags]
  go-zed-tasks clear [flags]

Commands:
  generate        Scan file tests and write/update one Zed task per test.
  generate-debug  Scan file tests and write/update one Zed debug config per test.
  debug           Alias for generate-debug.
  clear           Remove all previously auto-generated tasks.

Flags (both commands):
  -root      Workspace root (auto-detected if omitted)
  -tasks     Override tasks file path
  -debug     Override debug file path
  -dry-run   Print resulting JSON instead of writing

Generate-only:
  -file      Go file to scan (required)
  -go-test-arg  Extra go test argument (repeatable), also supports args after --.
  -discover-subtests Run tests with go test -json and include discovered subtests.
  -subtest-timeout Timeout for subtest discovery execution (default from env, 30s).

Configuration:
  Uses environment variables with prefix ZED_GO_TASKS_.
  Example: ZED_GO_TASKS_LABEL_PREFIX=unit:

Backward compatibility:
  go-zed-tasks -file <path> behaves the same as "generate".`)
}

func findTestsInFile(path string, namePattern *regexp.Regexp) ([]string, error) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var names []string
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		name := fn.Name.Name
		if !namePattern.MatchString(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

func listTestsWithGo(goBinary, packageDir, listRegex string) (map[string]struct{}, error) {
	cmd := exec.Command(goBinary, "test", "-list", listRegex, ".")
	cmd.Dir = packageDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go test -list failed in %s: %w\n%s", packageDir, err, strings.TrimSpace(string(out)))
	}

	names := make(map[string]struct{})
	identPattern := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" ||
			strings.HasPrefix(line, "ok ") ||
			strings.HasPrefix(line, "? ") ||
			strings.HasPrefix(line, "PASS") ||
			strings.HasPrefix(line, "FAIL") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		name := fields[0]
		if !identPattern.MatchString(name) {
			continue
		}
		names[name] = struct{}{}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return names, nil
}

func intersectTests(fileTests []string, listed map[string]struct{}) []string {
	result := make([]string, 0, len(fileTests))
	for _, name := range fileTests {
		if _, ok := listed[name]; ok {
			result = append(result, name)
		}
	}
	return result
}

func packageArg(root, packageDir string) (string, error) {
	rel, err := filepath.Rel(root, packageDir)
	if err != nil {
		return "", err
	}

	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ".", nil
	}

	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("package directory %q is outside root %q", packageDir, root)
	}

	return "./" + rel, nil
}

func makeGeneratedTasks(testNames []string, pkgArg, relFilePath string, cfg Config, extraGoTestArgs []string) []map[string]any {
	tasks := make([]map[string]any, 0, len(testNames))
	for _, testName := range testNames {
		args := make([]string, 0, 5+len(extraGoTestArgs))
		args = append(args, "test")
		args = append(args, extraGoTestArgs...)
		args = append(args, pkgArg, "-run", runPatternForTestName(testName))

		task := map[string]any{
			"label":                 cfg.LabelPrefix + testName,
			"command":               cfg.GoBinary,
			"args":                  args,
			"use_new_terminal":      cfg.UseNewTerminal,
			"allow_concurrent_runs": cfg.AllowConcurrentRuns,
			"reveal":                cfg.Reveal,
			"hide":                  cfg.Hide,
			"env": map[string]any{
				cfg.GeneratedEnvKey: cfg.GeneratedEnvValue,
				"ZED_GO_TEST_NAME":  testName,
				"ZED_GO_TEST_FILE":  relFilePath,
			},
		}
		tasks = append(tasks, task)
	}
	return tasks
}

func makeGeneratedDebugConfigs(testNames []string, pkgArg, relFilePath string, cfg Config, extraGoTestArgs []string) []map[string]any {
	configs := make([]map[string]any, 0, len(testNames))
	for _, testName := range testNames {
		taskArgs := make([]string, 0, len(extraGoTestArgs)+2)
		taskArgs = append(taskArgs, normalizeGoTestArgsForDelve(extraGoTestArgs)...)
		taskArgs = append(taskArgs, "-test.run", runPatternForTestName(testName))

		config := map[string]any{
			"label":   cfg.DebugLabelPrefix + testName,
			"adapter": "Delve",
			"request": "launch",
			"mode":    "test",
			"program": pkgArg,
			"args":    taskArgs,
			"env": map[string]any{
				cfg.GeneratedEnvKey: cfg.GeneratedEnvValue,
				"ZED_GO_TEST_NAME":  testName,
				"ZED_GO_TEST_FILE":  relFilePath,
			},
		}
		configs = append(configs, config)
	}
	return configs
}

func normalizeGoTestArgsForDelve(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "-v":
			out = append(out, "-test.v")
		case arg == "-count":
			// Bare -count is not useful without a value for debug configs.
			continue
		case strings.HasPrefix(arg, "-count="):
			out = append(out, "-test."+strings.TrimPrefix(arg, "-"))
		default:
			out = append(out, arg)
		}
	}
	return out
}

func resolveSubtestTimeout(fromEnv, fromFlag string) (time.Duration, error) {
	value := strings.TrimSpace(fromEnv)
	if strings.TrimSpace(fromFlag) != "" {
		value = strings.TrimSpace(fromFlag)
	}
	if value == "" {
		value = "30s"
	}

	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid subtest discovery timeout %q: %w", value, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("subtest discovery timeout must be > 0, got %q", value)
	}
	return timeout, nil
}

func discoverSubtestsWithGo(
	goBinary string,
	packageDir string,
	topLevelTests []string,
	timeout time.Duration,
	extraGoTestArgs []string,
) ([]string, error) {
	if len(topLevelTests) == 0 {
		return []string{}, nil
	}

	args := []string{"test", "-json", "-count=1", "-timeout", timeout.String()}
	args = append(args, sanitizeDiscoveryGoTestArgs(extraGoTestArgs)...)
	args = append(args, "-run", buildTopLevelRunPattern(topLevelTests), ".")

	cmd := exec.Command(goBinary, args...)
	cmd.Dir = packageDir
	out, err := cmd.CombinedOutput()

	discovered, parseErr := parseRunEventsFromGoTestJSON(out)
	if parseErr != nil {
		return nil, parseErr
	}

	// Discovery can still be useful even if tests failed; only fail hard when nothing was discovered.
	if err != nil && len(discovered) == 0 {
		return nil, fmt.Errorf("go test discovery failed in %s: %w\n%s", packageDir, err, strings.TrimSpace(string(out)))
	}

	return discovered, nil
}

func sanitizeDiscoveryGoTestArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "-json", arg == "-run", strings.HasPrefix(arg, "-run="),
			arg == "-list", strings.HasPrefix(arg, "-list="),
			arg == "-timeout", strings.HasPrefix(arg, "-timeout="),
			arg == "-count", strings.HasPrefix(arg, "-count="):
			continue
		default:
			out = append(out, arg)
		}
	}
	return out
}

func parseRunEventsFromGoTestJSON(output []byte) ([]string, error) {
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var ev goTestJSONEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// Ignore non-JSON lines and keep scanning.
			continue
		}
		if ev.Action == "run" && ev.Test != "" {
			seen[ev.Test] = struct{}{}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	tests := make([]string, 0, len(seen))
	for name := range seen {
		tests = append(tests, name)
	}
	sort.Strings(tests)
	return tests, nil
}

func mergeUniqueTests(base []string, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))

	for _, testName := range base {
		if _, ok := seen[testName]; ok {
			continue
		}
		seen[testName] = struct{}{}
		merged = append(merged, testName)
	}
	for _, testName := range extra {
		if _, ok := seen[testName]; ok {
			continue
		}
		seen[testName] = struct{}{}
		merged = append(merged, testName)
	}

	return merged
}

func countUniqueNotInBase(base []string, candidates []string) int {
	baseSet := make(map[string]struct{}, len(base))
	for _, name := range base {
		baseSet[name] = struct{}{}
	}

	seen := make(map[string]struct{}, len(candidates))
	count := 0
	for _, name := range candidates {
		if _, done := seen[name]; done {
			continue
		}
		seen[name] = struct{}{}
		if _, exists := baseSet[name]; !exists {
			count++
		}
	}
	return count
}

func buildTopLevelRunPattern(testNames []string) string {
	if len(testNames) == 1 {
		return "^" + regexp.QuoteMeta(testNames[0]) + "$"
	}

	parts := make([]string, 0, len(testNames))
	for _, name := range testNames {
		parts = append(parts, regexp.QuoteMeta(name))
	}
	sort.Strings(parts)
	return "^(" + strings.Join(parts, "|") + ")$"
}

func runPatternForTestName(testName string) string {
	if testName == "" {
		return "^$"
	}

	segments := strings.Split(testName, "/")
	for i, segment := range segments {
		segments[i] = "^" + regexp.QuoteMeta(segment) + "$"
	}
	return strings.Join(segments, "/")
}

func mergeTasks(tasksPath string, generated []map[string]any, cfg Config) ([]map[string]any, mergeStats, error) {
	existing, err := readTasks(tasksPath)
	if err != nil {
		return nil, mergeStats{}, err
	}

	filtered := make([]map[string]any, 0, len(existing))
	removed := 0
	for _, task := range existing {
		if cfg.PruneGenerated && isGenerated(task, cfg) {
			removed++
			continue
		}
		filtered = append(filtered, task)
	}

	labelIndex := make(map[string]int, len(filtered))
	for i, task := range filtered {
		if label, ok := task["label"].(string); ok {
			labelIndex[label] = i
		}
	}

	added := 0
	updated := 0
	for _, task := range generated {
		label, _ := task["label"].(string)
		if idx, ok := labelIndex[label]; ok {
			filtered[idx] = task
			updated++
			continue
		}
		filtered = append(filtered, task)
		labelIndex[label] = len(filtered) - 1
		added++
	}

	return filtered, mergeStats{Added: added, Updated: updated, Removed: removed}, nil
}

func marshalTasks(tasks []map[string]any) ([]byte, error) {
	output, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("serialize tasks JSON: %w", err)
	}
	return append(output, '\n'), nil
}

func writeTasks(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create tasks directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	return nil
}

func readTasks(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}

	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return []map[string]any{}, nil
	}

	normalized, err := normalizeRelaxedJSON(data)
	if err != nil {
		return nil, err
	}

	var tasks []map[string]any
	if err := json.Unmarshal(normalized, &tasks); err != nil {
		return nil, err
	}

	return tasks, nil
}

func normalizeRelaxedJSON(data []byte) ([]byte, error) {
	withoutComments, err := stripJSONComments(data)
	if err != nil {
		return nil, err
	}
	return stripTrailingCommas(withoutComments), nil
}

func stripJSONComments(data []byte) ([]byte, error) {
	var out []byte
	out = make([]byte, 0, len(data))

	inString := false
	inLineComment := false
	inBlockComment := false
	escape := false

	for i := 0; i < len(data); i++ {
		ch := data[i]

		if inLineComment {
			if ch == '\n' {
				inLineComment = false
				out = append(out, ch)
			}
			continue
		}

		if inBlockComment {
			if ch == '*' && i+1 < len(data) && data[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}

		if inString {
			out = append(out, ch)
			if escape {
				escape = false
				continue
			}
			if ch == '\\' {
				escape = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}

		if ch == '/' && i+1 < len(data) {
			next := data[i+1]
			if next == '/' {
				inLineComment = true
				i++
				continue
			}
			if next == '*' {
				inBlockComment = true
				i++
				continue
			}
		}

		out = append(out, ch)
	}

	if inBlockComment {
		return nil, fmt.Errorf("unterminated block comment in tasks file")
	}
	if inString {
		return nil, fmt.Errorf("unterminated string in tasks file")
	}
	return out, nil
}

func stripTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escape := false

	for i := 0; i < len(data); i++ {
		ch := data[i]

		if inString {
			out = append(out, ch)
			if escape {
				escape = false
				continue
			}
			if ch == '\\' {
				escape = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}

		if ch == ',' {
			j := i + 1
			for j < len(data) && isJSONWhitespace(data[j]) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}

		out = append(out, ch)
	}

	return out
}

func isJSONWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

func isGenerated(task map[string]any, cfg Config) bool {
	envAny, ok := task["env"]
	if !ok {
		return false
	}

	env, ok := envAny.(map[string]any)
	if !ok {
		return false
	}

	valAny, ok := env[cfg.GeneratedEnvKey]
	if !ok {
		return false
	}

	val, ok := valAny.(string)
	if !ok {
		return false
	}

	return val == cfg.GeneratedEnvValue
}

func detectWorkspaceRoot(start string) string {
	for current := start; ; {
		if fileExists(filepath.Join(current, "go.mod")) || pathExists(filepath.Join(current, ".git")) {
			return current
		}

		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	cwd, err := os.Getwd()
	if err != nil {
		return start
	}
	return cwd
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func exitf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
