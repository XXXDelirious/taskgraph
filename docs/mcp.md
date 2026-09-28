# MCP server: your Taskfile as tools for coding agents

`taskgraph --mcp` serves the tasks in your Taskfile as tools over the
[Model Context Protocol](https://modelcontextprotocol.io) (MCP). Coding agents
such as Cursor, VS Code / GitHub Copilot and any other MCP client can then run
your project's real build, test and lint steps, with their dependencies,
variables and up-to-date checks, instead of guessing shell commands.

```text
agent ──MCP (stdio)──▶ taskgraph --mcp ──▶ Taskfile.yml
         tools/list      one tool per task, plus explain + graph
         tools/call      runs the task, returns its output and exit code
```

## Set it up

The server speaks MCP over stdin and stdout, and uses the Taskfile in the
directory it starts in (or the one given with `-d` / `-t`). MCP clients are
set up with a small JSON file that names the command to start.

**Project config:** for clients that read a project-level `.mcp.json`, commit
this at the root of the project so everyone gets it:

```json
{
  "mcpServers": {
    "taskgraph": { "command": "taskgraph", "args": ["--mcp"] }
  }
}
```

**Cursor:** the same JSON, in `.cursor/mcp.json`.

**VS Code (GitHub Copilot):** in `.vscode/mcp.json`:

```json
{
  "servers": {
    "taskgraph": { "type": "stdio", "command": "taskgraph", "args": ["--mcp"] }
  }
}
```

**Desktop apps and other clients that don't start in your project:** pass the
directory with `-d`:

```json
{
  "mcpServers": {
    "my-project": { "command": "taskgraph", "args": ["--mcp", "-d", "/path/to/project"] }
  }
}
```

## Which tasks become tools

By default, the tools are the same tasks as `taskgraph --list` shows: every task
that has a `desc` and is not `internal`. Your task descriptions become the tool
descriptions the agent reads, so write them for a reader who has never seen the
project.

To choose exactly which tasks are exposed, list them:

```shell
taskgraph --mcp build test lint
```

To hide one task, or to expose a task without a description, use the `mcp` key
(upstream Task ignores it, so the Taskfile stays compatible):

```yaml
tasks:
  deploy:prod:
    desc: Deploy to production
    mcp: false # never offered to agents

  lint:
    desc: Lint the code
    mcp:
      read_only: true # hints for the client
      idempotent: true

  db:reset:
    desc: Drop and recreate the dev database
    mcp:
      destructive: true
```

`read_only`, `destructive` and `idempotent` become MCP
[tool annotations](https://modelcontextprotocol.io/specification/2025-06-18/server/tools#tool-annotations).
Clients use them, for example, to decide which tools need the user's approval.

## What each tool looks like

A task becomes a tool named after the task. Characters that tool names can't
contain, such as the `:` in `docs:build`, become `_` (`docs_build`).

```yaml
build:
  desc: Build the app
  deps: [gen]
  sources: ['src/**/*.go']
  requires:
    vars:
      - name: TARGET
        enum: [linux, darwin]
  cmds:
    - go build -o bin/app ./cmd/app
```

becomes:

```json
{
  "name": "build",
  "description": "Build the app\n\nFirst runs its dependencies: gen.\n\nDoes nothing if it is already up to date, unless force is set.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "TARGET": { "type": "string", "enum": ["linux", "darwin"], "description": "Required variable TARGET." },
      "vars": { "type": "object", "additionalProperties": { "type": "string" } },
      "force": { "type": "boolean" }
    },
    "required": ["TARGET"],
    "additionalProperties": false
  }
}
```

The inputs are:

- **Required variables** (`requires.vars`) become required inputs. An `enum`
  becomes a list of allowed values.
- **`vars`** sets any other variable, like `NAME=value` on the command line.
- **`cli_args`** is offered when the task uses `{{.CLI_ARGS}}`, and fills it in.
- **`force`** runs the task even if it is up to date.

The result contains the task's output and exit code. Failures are reported as
tool errors, so the agent sees what went wrong:

```text
Task "test" failed (exit code 1): task: Failed to run task "test": exit status 1

task: [test] go test ./...
--- FAIL: TestParse (0.00s)
...
```

The same data is available as structured content:
`{ "task", "success", "exitCode", "durationMs", "output", "truncated", "error" }`.
Only the last 64 KiB of output is returned, because the end of a build or test
log is usually what matters.

## Built-in tools

| Tool                | What it does                                                                                                   |
| ------------------- | -------------------------------------------------------------------------------------------------------------- |
| `taskgraph_explain` | [`--explain`](graph-and-explain.md#--explain-why-will-this-run) for agents: would these tasks run, and why? Read-only. |
| `taskgraph_graph`   | [`--graph`](graph-and-explain.md#--graph-see-what-runs-what) for agents: which tasks does a task run? Read-only.        |

Both accept `tasks` (a list of task names) and optional `vars`.

## Safety

- Only the tasks you describe (or list) are exposed, and `internal` tasks never
  are.
- Tasks run exactly as they do from the command line, with the same
  preconditions, required variables and `enum` checks.
- Tasks with a `prompt:` are cancelled rather than confirmed, since an agent
  cannot answer the prompt. The tool description tells the agent to ask you to
  run them.
- Tasks cannot read from stdin, which belongs to the MCP connection.
- Mark risky tasks `mcp: { destructive: true }` so clients ask before running
  them, or hide them with `mcp: false`.

Each call re-reads the Taskfile, so edits to existing tasks apply to the next
call. Restart the server to pick up new or renamed tasks.

## Use it from Go

```go
import (
	task "github.com/XXXDelirious/taskgraph"
	"github.com/XXXDelirious/taskgraph/mcpserver"
)

s, err := mcpserver.New(mcpserver.Options{
	ExecutorOptions: []task.ExecutorOption{task.WithDir("path/to/project")},
})
if err != nil {
	return err
}
return s.Run(ctx) // stdio; or use s.MCPServer() with another transport
```
