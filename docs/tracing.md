# Profiling and OpenTelemetry tracing

taskgraph can record what it does during a run: every task, the time spent
waiting for dependencies, up-to-date checks, and every command. You can look at
this as a profile on your machine, or send it to an OpenTelemetry backend from
CI.

## `--profile`: where did the time go?

```shell
taskgraph --profile trace.json ci
```

After the run, taskgraph writes the trace and prints a summary:

```text
task: Profile written to trace.json (open it in https://ui.perfetto.dev)

Total: 1.09s

Critical path:
  ci  1.09s
     └─ test  980ms
        └─ build  670ms
           └─ assets  261ms

Slowest tasks (own time, excluding tasks they run):
  build      408ms
  test       310ms
  assets     261ms
  lint       211ms
  gen        111ms
```

- **Critical path:** the chain of tasks that set the total time. At each
  step it follows the task that finished last. Speeding up anything off this
  path won't make the run faster.
- **Slowest tasks:** ranked by their own time: their commands and checks,
  minus the time covered by the tasks they run. Tasks that were up to date or
  skipped are marked.

`trace.json` uses the Chrome trace format. Open it in
[ui.perfetto.dev](https://ui.perfetto.dev), `chrome://tracing` or
[speedscope](https://www.speedscope.app) to see a timeline. Tasks that run in
parallel appear on separate lanes:

```text
lane 1  ci ─────────────────────────────────────────────────────────
        test ─────────────────────────────────────────────┐
        build ────────────────────────────┐  go test ─────┘
        gen ──┐   go build ───────────────┘
lane 2  lint ────────┐
lane 3  assets ──────────────┐
```

The profile is written even when a task fails.

## OpenTelemetry

When the standard OpenTelemetry environment variables point to an OTLP
endpoint, taskgraph exports its spans there. You don't need a flag or any
config file:

```shell
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
taskgraph ci
```

The standard variables are supported:

| Variable                                                                     | Effect                                             |
| ---------------------------------------------------------------------------- | -------------------------------------------------- |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`          | Where to send spans. Tracing is on when either is set. |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL`          | `http/protobuf` (default) or `grpc`.               |
| `OTEL_EXPORTER_OTLP_HEADERS`                                                 | e.g. an API key for your backend.                  |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`                              | Override `service.name` (default `taskgraph`) and add attributes. |
| `OTEL_SDK_DISABLED=true`, `OTEL_TRACES_EXPORTER=none`                        | Turn it off.                                       |

The spans are:

| Span                                 | Attributes                                            |
| ------------------------------------ | ----------------------------------------------------- |
| `taskgraph <tasks>`                  | The root span for the whole invocation.               |
| `task <name>`                        | `task.name`, `taskgraph.status` (`ran`, `up-to-date`, `skipped`, `failed`) |
| `deps`                               | Time spent waiting for a task's dependencies.         |
| `up-to-date check`                   | Preconditions plus the sources/status check, and `up_to_date`. |
| `cmd <command>`                      | `task.name`, `cmd`                                    |
| `waiting for ...`                    | Waiting for a `--concurrency` slot, or for a `run: once` task started elsewhere. |

Failed tasks and commands record the error and set the span status to
`Error`.

### Joining a larger trace

- **Incoming:** if the `TRACEPARENT` environment variable holds a W3C
  trace context, taskgraph's root span becomes its child. So a CI system or
  wrapper script that starts a trace sees taskgraph inside it.
- **Outgoing:** every command a task runs gets `TRACEPARENT` set to its own
  `cmd` span. Tools that read it (including a nested `taskgraph`, test
  runners and build tools that support OpenTelemetry) add their spans under
  the command that ran them.

### Example: a local collector with Jaeger

```shell
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/jaeger:latest
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
taskgraph ci
# open http://localhost:16686 and search for the "taskgraph" service
```

## Notes

- Tracing is off in watch mode (`--watch`), whose runs never end.
- `--profile` cannot be combined with `--dry`, `--watch`, `--graph`,
  `--explain`, `--mcp`, `--status` or `--summary`.
- With neither `--profile` nor an OTLP endpoint, no spans are created at all.
