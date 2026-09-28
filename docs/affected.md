# `--affected`: run only what your changes affect

In a monorepo, most changes touch a small part of the code, yet CI often builds
and tests everything. `--affected` asks git what changed and runs only the tasks
those changes affect.

```shell
# In a pull request: what do my changes affect, compared with main?
taskgraph --affected --since origin/main

# Run the test tasks that are affected, skip the rest
taskgraph --affected --since origin/main web:test api:test docs:test
```

```text
task: "docs:test" is not affected by the changes - skipped
task: [shared:build] go build ./...
task: [web:build] go build ./cmd/web
...
```

## What counts as changed

- **Always:** uncommitted changes, staged or not, and untracked files that
  git doesn't ignore.
- **With `--since <ref>`:** also everything committed since the merge base of
  `<ref>` and `HEAD`, i.e. what a pull request from your branch would contain.
  `<ref>` can be a branch, tag or commit, such as `origin/main`, `main` or
  `v1.2.0`.

Deleted and renamed files count too. taskgraph's own state folder (`.task/`) is
ignored.

## What counts as affected

A task is affected when any of these is true:

1. **A changed file matches its `sources:`.** The same globs and `exclude:`
   rules as the up-to-date check are used.
2. **The Taskfile that defines it changed.**
3. **It runs an affected task,** through `deps:` or a `task:` call. This is the
   same graph that [`--graph`](graph-and-explain.md) prints.

So if `libs/shared` changes, every app that depends on `shared:build` is
affected, and so is `test`, which depends on the apps' tests:

```text
$ taskgraph --affected
test          via web:test, api:test
web:test      via web:build
web:build     via shared:build
shared:build  libs/shared/lib.go
api:test      via api:build
api:build     apps/api/src/main.go; via shared:build
```

A task without `sources:` is only affected through its Taskfile or the tasks it
runs. Give the tasks that should take part `sources:`. They also make those
tasks skip work when nothing changed.

## Two modes

- **With task names:** runs only the ones that are affected. The others are
  reported as skipped. If none are affected, it says so and exits with 0.
- **With no task names:** lists every affected task (except internal ones)
  and why, without running anything. Add `--json` for machine-readable output:

```shell
taskgraph --affected --since origin/main --json
```

```json
[
  { "task": "web:build", "files": ["apps/web/src/main.go"], "via": ["shared:build"] },
  { "task": "shared:build", "taskfileChanged": true }
]
```

Each entry has:

- `task`: the task name.
- `files`: changed files matching the task's sources, relative to the root
  Taskfile.
- `taskfileChanged`: set when the task's Taskfile changed.
- `via`: the affected tasks it runs.

## In CI

GitHub Actions, testing only what a pull request affects:

```yaml
- uses: actions/checkout@v6
  with:
    fetch-depth: 0 # --since needs the history back to the merge base

- run: taskgraph --affected --since origin/${{ github.base_ref }} test
```

Or build a job matrix from the affected tasks:

```yaml
- id: affected
  run: echo "tasks=$(taskgraph --affected --since origin/main --json | jq -c '[.[].task | select(endswith(":test"))]')" >> "$GITHUB_OUTPUT"
```

## Notes

- A shallow clone needs enough history to find the merge base. Use
  `fetch-depth: 0`, or fetch the base branch.
- For deleted files, patterns that use brace expansion (`{a,b}`) are not
  matched.
- Tasks are compiled as for a real run to read their `sources:`, so dynamic
  (`sh:`) variables are evaluated.
- `--since` only applies with `--affected`.
