# Task graph, `--explain` and cycle detection

## `--graph`: see what runs what

`taskgraph --graph` prints the tasks and the tasks they run, instead of running
anything. With task names it shows only what those tasks reach; with none it
shows the whole Taskfile.

```shell
taskgraph --graph            # every task, as a tree
taskgraph --graph test       # only what `test` runs
```

```text
default
└── test
    ├── build
    │   ├── gen
    │   └── lint [internal]
    ├── report [call]
    └── deploy:staging [call]
broken
└── does-not-exist [not found]
ping
└── pong
    └── ping [call]  ⟲ cycle

Cycles:
  ping -> pong -> ping
```

Plain children are `deps:`, which run in parallel before the task.
Children marked `[call]` are tasks called from `cmds:` with `task:`, which run
in order as part of the task. Tasks that don't exist are marked
`[not found]`, and cycles are listed at the end.

### Formats

Choose one with `--graph-format`:

| Format    | Use it for                                                                  |
| --------- | --------------------------------------------------------------------------- |
| `tree`    | Reading in a terminal (the default).                                        |
| `mermaid` | Pasting into a Markdown file, PR or wiki. GitHub renders it as a diagram.  |
| `dot`     | Graphviz: `taskgraph --graph --graph-format dot \| dot -Tsvg > graph.svg`. |
| `json`    | Scripts, CI checks and coding agents.                                       |

In `dot` and `mermaid` output, `[call]` edges are dashed, internal tasks have a
dashed border, and missing tasks and cycle edges are red.

The JSON output looks like this:

```json
{
  "nodes": [
    { "name": "build", "desc": "Build the app", "sources": ["src/**/*.go"], "generates": ["bin/app"] },
    { "name": "does-not-exist", "missing": true }
  ],
  "edges": [{ "from": "build", "to": "gen", "kind": "dep" }],
  "cycles": [["ping", "pong", "ping"]]
}
```

Building the graph never runs commands, so task names that depend on dynamic
(`sh:`) variables are left unresolved.

## `--explain`: why will this run?

`taskgraph --explain` works out whether each task would run, and why, without
running any task commands or updating any stored state.

```shell
taskgraph --explain build
```

```text
build: will run
│   - 3 source files changed since the last run
│       modified src/a.txt
│       removed  src/b.txt
│       added    src/c.txt
├── gen: up to date
│       - all status checks passed
└── lint: will run
        - it has no sources or status checks, so it always runs
```

Each task gets one of four verdicts:

| Verdict      | Meaning                                                                            |
| ------------ | ---------------------------------------------------------------------------------- |
| `will run`   | Its commands would run: sources changed, a status check failed, it was forced, etc. |
| `up to date` | Its sources and status checks say there is nothing to do.                          |
| `skipped`    | Its `if:` condition is false, or it isn't meant for this OS or architecture.       |
| `will fail`  | A precondition fails or a required variable is missing.                            |

It explains:

- **Checksum sources:** which files were **added**, **modified** or
  **removed** since the last successful run.
- **Timestamp sources:** which files are **newer** than the last run.
- **Status checks:** which `status:` commands failed.
- **Generated files:** which `generates:` patterns match no file.
- **Everything else:** `--force`, `if:` conditions, preconditions,
  platforms and missing required variables.

Deps are always explained, because they run before a task's own up-to-date
check. Tasks called from `cmds:` are only listed when the task itself will run.

For scripts and agents, add `--json`:

```shell
taskgraph --explain --json build
```

```json
[
  {
    "task": "build",
    "verdict": "will-run",
    "reasons": ["3 source files changed since the last run"],
    "fingerprint": {
      "upToDate": false,
      "method": "checksum",
      "sourceFiles": 3,
      "changes": [{ "path": "src/a.txt", "change": "modified" }]
    },
    "children": [{ "task": "gen", "kind": "dep", "verdict": "up-to-date", "reasons": ["all status checks passed"] }]
  }
]
```

`--explain` does run the checks a real run would: dynamic variables, `if:`
conditions, preconditions and `status:` commands. Those are expected to have no
side effects.

### How changed files are tracked

For the `checksum` method, taskgraph stores a per-file record next to the
existing checksum, in `.task/manifest/<task>.json`. The combined checksum is
computed exactly as Task computes it, so existing `.task/checksum` files stay
valid. After switching from Task, the first `--explain` of a changed task may
say that sources changed without listing them. From the next run on, files
are listed.

## Cycle detection

A task that ends up calling itself with the same variables can never finish.
Task only caught this after 1,000 calls, and printed a message 1,000 levels
deep. It hung forever when watching, or when the tasks used `run: once`.
taskgraph stops at the first repeat and names the cycle:

```text
$ taskgraph a
task: Cycle detected in task calls: a -> b -> c -> a
```

The exit code is `208`. A task may still call itself when the variables change
on each call, e.g. to count down.
