package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestNilTracerIsNoop(t *testing.T) {
	t.Parallel()

	var tr *Tracer
	ctx, span := tr.Start(t.Context(), KindTask, "build")
	assert.Nil(t, span)
	span.SetAttr("k", "v")
	span.SetStatus(StatusRan)
	span.Finish(errors.New("boom"))
	assert.Nil(t, FromContext(ctx))
	assert.Nil(t, tr.Spans())
	assert.Nil(t, New(false, nil), "a tracer that neither records nor exports is nil")
}

func TestSpansNestAndRecordStatus(t *testing.T) {
	t.Parallel()

	tr := New(true, nil)
	ctx, run := tr.Start(t.Context(), KindRun, "taskgraph build")
	ctx, task := tr.Start(ctx, KindTask, "build", "task.name", "build")
	_, cmd := tr.Start(ctx, KindCmd, "go build")
	cmd.Finish(nil)
	task.Finish(nil)

	_, skipped := tr.Start(FromContextCtx(t, run), KindTask, "lint")
	skipped.SetStatus(StatusUpToDate)
	skipped.Finish(nil)

	_, failed := tr.Start(FromContextCtx(t, run), KindTask, "test")
	failed.Finish(errors.New("exit status 1"))
	run.Finish(nil)

	spans := tr.Spans()
	require.Len(t, spans, 5)
	byName := map[string]*Span{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	assert.Equal(t, "build", byName["go build"].Parent.Name)
	assert.Equal(t, "taskgraph build", byName["build"].Parent.Name)
	assert.Equal(t, "build", byName["build"].Attrs["task.name"])
	assert.Equal(t, StatusRan, byName["build"].Status)
	assert.Equal(t, StatusUpToDate, byName["lint"].Status)
	assert.Equal(t, StatusFailed, byName["test"].Status)
	assert.Equal(t, "exit status 1", byName["test"].Err)
}

// FromContextCtx returns a context that carries span, for starting siblings.
func FromContextCtx(t *testing.T, span *Span) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), spanKey{}, span)
}

// span builds a finished span for profile tests. Times are in milliseconds.
func span(id int, parent *Span, kind Kind, name string, start, end int) *Span {
	origin := time.Unix(0, 0)
	return &Span{
		ID: id, Parent: parent, Kind: kind, Name: name,
		Start: origin.Add(time.Duration(start) * time.Millisecond),
		End:   origin.Add(time.Duration(end) * time.Millisecond),
		Attrs: map[string]string{}, Status: StatusRan,
	}
}

// profileSpans is a run of `ci`, which depends on `lint` and `test` in
// parallel; `test` depends on `build`, which depends on `gen` and `assets`.
func profileSpans() []*Span {
	run := span(1, nil, KindRun, "taskgraph ci", 0, 1100)
	ci := span(2, run, KindTask, "ci", 0, 1100)
	lint := span(3, ci, KindTask, "lint", 0, 200)
	test := span(4, ci, KindTask, "test", 0, 1000)
	build := span(5, test, KindTask, "build", 0, 700)
	gen := span(6, build, KindTask, "gen", 0, 100)
	assets := span(7, build, KindTask, "assets", 0, 250)
	buildCmd := span(8, build, KindCmd, "go build", 300, 700)
	testCmd := span(9, test, KindCmd, "go test", 700, 1000)
	return []*Span{run, ci, lint, test, build, gen, assets, buildCmd, testCmd}
}

func TestSummarize(t *testing.T) {
	t.Parallel()

	sum := Summarize(profileSpans(), 3)
	assert.Equal(t, 1100*time.Millisecond, sum.Total)

	var path []string
	for _, step := range sum.CriticalPath {
		path = append(path, step.Task)
	}
	assert.Equal(t, []string{"ci", "test", "build", "assets"}, path)

	require.Len(t, sum.Slowest, 3)
	assert.Equal(t, "build", sum.Slowest[0].Task)
	assert.Equal(t, 450*time.Millisecond, sum.Slowest[0].Self, "build minus its parallel deps (0-250ms)")
	assert.Equal(t, "test", sum.Slowest[1].Task)
	assert.Equal(t, 300*time.Millisecond, sum.Slowest[1].Self)

	var b strings.Builder
	require.NoError(t, sum.WriteSummary(&b))
	assert.Contains(t, b.String(), "Total: 1.10s")
	assert.Contains(t, b.String(), "└─ assets  250ms")
}

func TestAssignLanesNestsSpans(t *testing.T) {
	t.Parallel()

	spans := profileSpans()
	lanes := assignLanes(spans)
	for _, a := range spans {
		for _, b := range spans {
			if a.ID >= b.ID || lanes[a.ID] != lanes[b.ID] {
				continue
			}
			overlap := a.Start.Before(b.End) && b.Start.Before(a.End)
			nested := (isAncestor(a, b) && !b.End.After(a.End)) || (isAncestor(b, a) && !a.End.After(b.End))
			assert.False(t, overlap && !nested, "%s and %s overlap on lane %d without nesting", a.Name, b.Name, lanes[a.ID])
		}
	}
	assert.Equal(t, lanes[4], lanes[5], "test and build are sequential in one chain, so they share a lane")
	assert.NotEqual(t, lanes[3], lanes[4], "lint runs in parallel with test")
}

func TestWriteChromeTrace(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	require.NoError(t, WriteChromeTrace(&b, profileSpans()))
	var trace struct {
		TraceEvents []struct {
			Name string            `json:"name"`
			Ph   string            `json:"ph"`
			Ts   int64             `json:"ts"`
			Dur  int64             `json:"dur"`
			Tid  int               `json:"tid"`
			Args map[string]string `json:"args"`
		} `json:"traceEvents"`
	}
	require.NoError(t, json.Unmarshal(b.Bytes(), &trace))
	var complete int
	for _, e := range trace.TraceEvents {
		if e.Ph == "X" {
			complete++
			if e.Name == "build" {
				assert.Equal(t, int64(700_000), e.Dur)
				assert.Equal(t, StatusRan, e.Args["status"])
			}
		}
	}
	assert.Equal(t, 9, complete)
}

func TestTraceParentRoundTrip(t *testing.T) {
	t.Parallel()

	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := ContextWithTraceParent(t.Context(), parent)
	assert.Equal(t, parent, TraceParent(ctx))
	assert.Empty(t, TraceParent(t.Context()))
	assert.Empty(t, TraceParent(ContextWithTraceParent(t.Context(), "not-a-traceparent")))
}

func TestOTelEnabled(t *testing.T) { //nolint:paralleltest // uses t.Setenv
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	assert.False(t, OTelEnabled())

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	assert.True(t, OTelEnabled())

	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	assert.False(t, OTelEnabled())

	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	assert.False(t, OTelEnabled())
}

// TestOTelExport sends spans to a fake OTLP/HTTP collector and checks what
// arrives.
func TestOTelExport(t *testing.T) { //nolint:paralleltest // uses t.Setenv
	var (
		mu       sync.Mutex
		received []*coltracepb.ExportTraceServiceRequest
	)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if assert.NoError(t, err) && assert.Equal(t, "/v1/traces", r.URL.Path) {
			req := &coltracepb.ExportTraceServiceRequest{}
			if assert.NoError(t, proto.Unmarshal(body, req)) {
				mu.Lock()
				received = append(received, req)
				mu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=test")

	otelTracer, shutdown, err := SetupOTel(t.Context(), "1.2.3")
	require.NoError(t, err)
	tr := New(false, otelTracer)

	ctx, task := tr.Start(t.Context(), KindTask, "build", "task.name", "build")
	cmdCtx, cmd := tr.Start(ctx, KindCmd, "go build")
	assert.Regexp(t, regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`), TraceParent(cmdCtx))
	cmd.Finish(errors.New("exit status 2"))
	task.Finish(errors.New("exit status 2"))
	require.NoError(t, shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	var names []string
	var service, env string
	spanIDs := map[string][]byte{}
	parents := map[string][]byte{}
	for _, req := range received {
		for _, rs := range req.ResourceSpans {
			for _, attr := range rs.Resource.Attributes {
				switch attr.Key {
				case "service.name":
					service = attr.Value.GetStringValue()
				case "deployment.environment":
					env = attr.Value.GetStringValue()
				}
			}
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					names = append(names, s.Name)
					spanIDs[s.Name] = s.SpanId
					parents[s.Name] = s.ParentSpanId
				}
			}
		}
	}
	slices.Sort(names)
	assert.Equal(t, []string{"cmd go build", "task build"}, names)
	assert.Equal(t, "taskgraph", service)
	assert.Equal(t, "test", env)
	assert.Equal(t, spanIDs["task build"], parents["cmd go build"])
}
