package task_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	task "github.com/XXXDelirious/taskgraph"
	"github.com/XXXDelirious/taskgraph/internal/tracing"
)

func TestTracingRecordsRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data, err := os.ReadFile("testdata/tracing/Taskfile.yml")
	require.NoError(t, err)
	writeFile(t, dir, "Taskfile.yml", string(data))

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := tracing.New(true, provider.Tracer("test"))

	e := newExplainExecutor(t, dir, task.WithTracer(tracer))
	require.NoError(t, e.Run(t.Context(), &task.Call{Task: "default"}))
	require.Error(t, e.Run(t.Context(), &task.Call{Task: "broken"}))

	status := map[string]string{}
	var cmds []string
	for _, s := range tracer.Spans() {
		switch s.Kind {
		case tracing.KindTask:
			status[s.Name] = s.Status
		case tracing.KindCmd:
			cmds = append(cmds, s.Name)
			assert.NotEmpty(t, s.Attrs["task.name"])
		}
	}
	assert.Equal(t, map[string]string{
		"default": tracing.StatusRan,
		"a":       tracing.StatusRan,
		"skipped": tracing.StatusSkipped,
		"fresh":   tracing.StatusUpToDate,
		"broken":  tracing.StatusFailed,
	}, status)
	assert.ElementsMatch(t, []string{"echo a", `echo "$TRACEPARENT" > traceparent.txt`, "exit 1"}, cmds)

	// Commands get a TRACEPARENT from the same trace, pointing at their span.
	tp, err := os.ReadFile(filepath.Join(dir, "traceparent.txt"))
	require.NoError(t, err)
	parts := strings.Split(strings.TrimSpace(string(tp)), "-")
	require.Len(t, parts, 4, "got %q", tp)

	var found bool
	for _, s := range recorder.Ended() {
		if s.Name() == `cmd echo "$TRACEPARENT" > traceparent.txt` {
			found = true
			assert.Equal(t, parts[1], s.SpanContext().TraceID().String())
			assert.Equal(t, parts[2], s.SpanContext().SpanID().String())
		}
	}
	assert.True(t, found)
}

func TestTracingDisabledByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data, err := os.ReadFile("testdata/tracing/Taskfile.yml")
	require.NoError(t, err)
	writeFile(t, dir, "Taskfile.yml", string(data))

	e := newExplainExecutor(t, dir)
	require.NoError(t, e.Run(t.Context(), &task.Call{Task: "default"}))
	tp, err := os.ReadFile(filepath.Join(dir, "traceparent.txt"))
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(os.Getenv("TRACEPARENT")), strings.TrimSpace(string(tp)), "without tracing, TRACEPARENT is passed through untouched")
}
