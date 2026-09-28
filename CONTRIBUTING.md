# Contributing to taskgraph

Thanks for taking the time to contribute.

## Setup

You need Go (see `go.mod` for the minimum version). [mise](https://mise.jdx.dev)
users can run `mise install` to get Go and the dev tools listed in `mise.toml`.

## Common commands

```shell
go run ./cmd/taskgraph --list-all   # see the project's own tasks
go run ./cmd/taskgraph test         # run the test suite (uses gotestsum)
go test ./...                       # run the test suite without gotestsum
go test -race ./...                 # run it with the race detector
go run ./cmd/taskgraph lint         # run golangci-lint
```

Tests that exercise watch mode and signal handling are behind build tags:

```shell
go run ./cmd/taskgraph test:all
```

## Golden files

Many tests compare output against golden files in `testdata/**/testdata`. After
an intended output change, regenerate them with:

```shell
go run ./cmd/taskgraph generate:fixtures
```

Review the diff of the regenerated files before committing.

## Pull requests

- Keep each pull request focused on one change.
- Add or update tests.
- Add a line to the `Unreleased` section of `CHANGELOG.md`.
- Tests must pass with and without `-race`.

## Tests that share output buffers

When a test captures executor output, use `SyncBuffer` (in `task_test.go`)
rather than `bytes.Buffer`. Tasks run their dependencies in parallel, and a
plain `bytes.Buffer` is not safe for concurrent writes.
