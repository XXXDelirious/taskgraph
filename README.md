# taskgraph

**A Task-compatible task runner built for AI coding agents and large monorepos.**

taskgraph runs the same `Taskfile.yml` files as [Task](https://taskfile.dev),
and adds features aimed at how software is built today: coding agents that need
safe, well-described tools, and monorepos where CI time matters.

> taskgraph is a fork of [go-task/task](https://github.com/go-task/task) v3.51.1.
> All credit for the original task runner goes to Andrey Nering and the Task
> contributors. See [Acknowledgements](#acknowledgements).

## What taskgraph adds

| Feature | What it does |
| --- | --- |
| [MCP server](docs/mcp.md) | `taskgraph --mcp` serves your Taskfile to coding agents (Cursor, GitHub Copilot and other MCP clients) over the Model Context Protocol. Each documented task becomes a tool, with typed inputs from `requires:`. |
| [`--graph`](docs/graph-and-explain.md#--graph-see-what-runs-what) | Prints the task graph as a tree, Mermaid, Graphviz DOT or JSON, and flags missing tasks and cycles. |
| [`--explain`](docs/graph-and-explain.md#--explain-why-will-this-run) | Says whether each task would run and why, down to which source files were added, modified or removed. Add `--json` for machine-readable output. |
| [`--affected`](docs/affected.md) | Runs only the tasks affected by what changed in git (`--since origin/main` for pull requests), following sources, Taskfile edits and the dependency graph. With no task names, lists them, optionally as JSON for CI. |
| [Cycle detection](docs/graph-and-explain.md#cycle-detection) | Stops at the first cyclic call and names it (`a -> b -> a`), instead of failing after 1,000 calls or hanging. |

## Install

With Go 1.25 or newer:

```shell
go install github.com/XXXDelirious/taskgraph/cmd/taskgraph@latest
```

Or download a binary from the
[releases page](https://github.com/XXXDelirious/taskgraph/releases), or use the
install script:

```shell
sh -c "$(curl --location https://raw.githubusercontent.com/XXXDelirious/taskgraph/main/install.sh)" -- -d -b ~/.local/bin
```

Check the install:

```shell
taskgraph --version
# taskgraph 0.1.0 (compatible with Task 3.51.1)
```

## Quick start

```yaml
# Taskfile.yml
version: '3'

tasks:
  build:
    desc: Build the app
    sources: ['**/*.go']
    generates: ['bin/app']
    cmds:
      - go build -o bin/app .

  test:
    desc: Run the tests
    deps: [build]
    cmds:
      - go test ./...
```

```shell
taskgraph test        # runs build (if anything changed), then test
taskgraph --list      # lists tasks that have a description
```

Everything in the [Task documentation](https://taskfile.dev/docs/guide)
applies: variables, includes, `sources`/`generates`, preconditions, watch mode,
and so on.

## Compatibility with Task

- **Taskfiles:** the same file names (`Taskfile.yml`, `taskfile.yaml`, ...), the
  same schema (`version: '3'`), and the same features.
- **Configuration:** `.taskrc.yml`, the `TASK_*` environment variables and the
  `.task/` state folder are unchanged, so both tools can be used in the same
  project.
- **Versions:** taskgraph has its own version number. Taskfile schema checks and
  the `{{.TASK_VERSION}}` variable use the compatible Task version (3.51.1); the
  taskgraph version is available as `{{.TASKGRAPH_VERSION}}`.
- **Binary name:** the command is `taskgraph`. To keep typing `task`, add
  `alias task=taskgraph` to your shell profile.

## Shell completion

```shell
# bash
eval "$(taskgraph --completion bash)"
# zsh
eval "$(taskgraph --completion zsh)"
# fish
taskgraph --completion fish | source
# PowerShell
Invoke-Expression (&taskgraph --completion powershell | Out-String)
```

## Using it as a Go library

```go
import (
	task "github.com/XXXDelirious/taskgraph"
)

e := task.NewExecutor(task.WithDir("."))
if err := e.Setup(); err != nil {
	return err
}
return e.Run(ctx, &task.Call{Task: "build"})
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). In short:

```shell
go run ./cmd/taskgraph test   # run the test suite
go test -race ./...           # run it with the race detector
```

## Acknowledgements

taskgraph is built on [Task](https://github.com/go-task/task), created by
[Andrey Nering](https://github.com/andreynering) and maintained by the Task
team and community. The original copyright notice is kept in
[LICENSE](LICENSE), and Task's full history is kept in
[CHANGELOG.md](CHANGELOG.md).

## License

[MIT](LICENSE)
