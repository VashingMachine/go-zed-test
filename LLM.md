# LLM Integration Guide: `go-zed-tasks`

This file is for AI assistants that need to configure or run this project for Zed/VS Code + Go test workflows.

## What this tool does

`go-zed-tasks` scans a selected Go test file and generates:
- Zed tasks in `.zed/tasks.json` (`generate`, default editor mode)
- Zed debug configs in `.zed/debug.json` (`generate-debug` / `debug`)
- VS Code tasks in `.vscode/tasks.json` (`generate -editor vscode`)
- VS Code debug configs in `.vscode/launch.json` (`debug -editor vscode`)

It can also discover runtime/dynamic subtests via `go test -json`.

## Most useful commands

Generate tasks for tests in current file:

```bash
go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} generate -file ${ZED_FILE}
```

Generate debug configs for tests in current file:

```bash
go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} debug -file ${ZED_FILE}
```

Generate VS Code tasks/debug configs for tests in current file:

```bash
go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} generate -editor vscode -file ${file}
go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} debug -editor vscode -file ${file}
```

Include dynamic subtests:

```bash
go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} generate -file ${ZED_FILE} -discover-subtests
go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} debug -file ${ZED_FILE} -discover-subtests
```

Pass custom `go test` args:

```bash
-go-test-arg='-v' -go-test-arg='-count=1'
```

## Recommended `tasks.json` snippet

```json
[
  {
    "label": "go-discover",
    "command": "go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} generate -file ${ZED_FILE} -discover-subtests -go-test-arg='-v' -go-test-arg='-count=1'",
    "env": { "ZED_GO_TASKS_PRUNE_GENERATED": "false" },
    "reveal": "always",
    "reveal_target": "dock",
    "hide": "never",
    "shell": "system"
  },
  {
    "label": "go-discover-debug",
    "command": "go run github.com/VashingMachine/go-zed-test/cmd/go-zed-tasks@${VERSION} debug -file ${ZED_FILE} -discover-subtests -go-test-arg='-v' -go-test-arg='-count=1'",
    "reveal": "always",
    "reveal_target": "dock",
    "hide": "never",
    "shell": "system"
  }
]
```

## Env configuration (prefix: `ZED_GO_TASKS_`)

Important keys:
- `TASKS_PATH` (default `.zed/tasks.json`, or `.vscode/tasks.json` when `-editor vscode` and not explicitly set)
- `DEBUG_PATH` (default `.zed/debug.json`, or `.vscode/launch.json` when `-editor vscode` and not explicitly set)
- `LABEL_PREFIX` (default `go:`)
- `DEBUG_LABEL_PREFIX` (default `go:debug:`)
- `ADDITIONAL_GO_TEST_ARGS` (comma-separated)
- `PRUNE_GENERATED` (default `true`)
- `GENERATED_ENV_KEY` / `GENERATED_ENV_VALUE`
- `SUBTEST_DISCOVERY_TIMEOUT` (default `30s`)

Example:

```bash
export ZED_GO_TASKS_SUBTEST_DISCOVERY_TIMEOUT=45s
export ZED_GO_TASKS_PRUNE_GENERATED=false
```

## Behavior notes for assistants

- `go test -list` does not include runtime-created subtests. Use `-discover-subtests` when subtests are expected.
- Runtime discovery logs include:
  - total runtime discovered tests
  - number of newly discovered tests beyond static list
- Relaxed JSON is supported when reading Zed and VS Code files (comments + trailing commas).
- Generated entries are marked via env (`GENERATED_ENV_KEY=GENERATED_ENV_VALUE`) and can be cleared safely with `clear`.
