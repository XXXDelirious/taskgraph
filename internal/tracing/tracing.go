// Package tracing records what taskgraph does while it runs tasks, as spans.
// Spans can be kept in memory to write a profile, exported to OpenTelemetry,
// or both. A nil *Tracer records nothing, so callers never need to check
// whether tracing is enabled.
package tracing

import (
	"context"
	"maps"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Kind is what a span measures.
type Kind string

const (
	KindRun   Kind = "run"   // the whole taskgraph invocation
	KindTask  Kind = "task"  // one run of a task, including its deps
	KindDeps  Kind = "deps"  // waiting for a task's deps
	KindCheck Kind = "check" // preconditions and up-to-date check
	KindCmd   Kind = "cmd"   // one command
	KindWait  Kind = "wait"  // waiting for a concurrency slot or another run
)

// Span statuses, set on task spans.
const (
	StatusRan      = "ran"
	StatusUpToDate = "up-to-date"
	StatusSkipped  = "skipped"
	StatusFailed   = "failed"
)

// Span is one timed operation.
type Span struct {
	ID     int
	Parent *Span
	Kind   Kind
	Name   string
	Start  time.Time
	End    time.Time
	Attrs  map[string]string
	Status string
	Err    string

	tracer *Tracer
	otel   trace.Span
}

// Tracer creates spans.
type Tracer struct {
	mu     sync.Mutex
	spans  []*Span
	record bool
	otel   trace.Tracer
	nextID int
}

// New returns a tracer. With record set, it keeps every span for [Tracer.Spans].
// With otelTracer set, it also creates OpenTelemetry spans.
func New(record bool, otelTracer trace.Tracer) *Tracer {
	if !record && otelTracer == nil {
		return nil
	}
	return &Tracer{record: record, otel: otelTracer}
}

type spanKey struct{}

// FromContext returns the span stored in ctx, or nil.
func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

// Start starts a span as a child of the span in ctx, and returns a context
// that carries the new span.
func (t *Tracer) Start(ctx context.Context, kind Kind, name string, attrs ...string) (context.Context, *Span) {
	if t == nil {
		return ctx, nil
	}
	s := &Span{
		Kind:   kind,
		Name:   name,
		Start:  time.Now(),
		Parent: FromContext(ctx),
		Attrs:  map[string]string{},
		tracer: t,
	}
	for i := 0; i+1 < len(attrs); i += 2 {
		s.Attrs[attrs[i]] = attrs[i+1]
	}

	t.mu.Lock()
	t.nextID++
	s.ID = t.nextID
	if t.record {
		t.spans = append(t.spans, s)
	}
	t.mu.Unlock()

	if t.otel != nil {
		kv := []attribute.KeyValue{attribute.String("taskgraph.kind", string(kind))}
		for k, v := range s.Attrs {
			kv = append(kv, attribute.String(k, v))
		}
		ctx, s.otel = t.otel.Start(ctx, spanName(kind, name), trace.WithAttributes(kv...), trace.WithTimestamp(s.Start))
	}
	return context.WithValue(ctx, spanKey{}, s), s
}

func spanName(kind Kind, name string) string {
	switch kind {
	case KindTask:
		return "task " + name
	case KindCmd:
		return "cmd " + name
	default:
		return name
	}
}

// SetAttr sets an attribute on the span.
func (s *Span) SetAttr(key, value string) {
	if s == nil {
		return
	}
	s.tracer.mu.Lock()
	s.Attrs[key] = value
	s.tracer.mu.Unlock()
	if s.otel != nil {
		s.otel.SetAttributes(attribute.String(key, value))
	}
}

// SetStatus sets the outcome of a task span, e.g. [StatusUpToDate].
func (s *Span) SetStatus(status string) {
	if s == nil {
		return
	}
	s.tracer.mu.Lock()
	s.Status = status
	s.tracer.mu.Unlock()
	if s.otel != nil {
		s.otel.SetAttributes(attribute.String("taskgraph.status", status))
	}
}

// Finish ends the span. A non-nil err marks it as failed.
func (s *Span) Finish(err error) {
	if s == nil {
		return
	}
	end := time.Now()
	s.tracer.mu.Lock()
	s.End = end
	if err != nil {
		s.Err = err.Error()
		if s.Status == "" || s.Status == StatusRan {
			s.Status = StatusFailed
		}
	} else if s.Status == "" && s.Kind == KindTask {
		s.Status = StatusRan
	}
	status := s.Status
	s.tracer.mu.Unlock()

	if s.otel != nil {
		if err != nil {
			s.otel.RecordError(err)
			s.otel.SetStatus(codes.Error, err.Error())
		}
		if status != "" {
			s.otel.SetAttributes(attribute.String("taskgraph.status", status))
		}
		s.otel.End(trace.WithTimestamp(end))
	}
}

// Spans returns a copy of the recorded spans, in start order. Spans that have
// not finished are given the current time as their end.
func (t *Tracer) Spans() []*Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	spans := make([]*Span, len(t.spans))
	for i, s := range t.spans {
		c := *s
		c.Attrs = maps.Clone(s.Attrs)
		if c.End.IsZero() {
			c.End = now
		}
		spans[i] = &c
	}
	// Point parents at the copies.
	byID := make(map[int]*Span, len(spans))
	for _, s := range spans {
		byID[s.ID] = s
	}
	for _, s := range spans {
		if s.Parent != nil {
			s.Parent = byID[s.Parent.ID]
		}
	}
	return spans
}

// TraceParent returns the W3C traceparent header value for the OpenTelemetry
// span in ctx, or "" if there is none. Commands get it in their environment
// as TRACEPARENT, so tools that support it can join the trace.
func TraceParent(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// ContextWithTraceParent returns ctx with the remote parent described by a
// W3C traceparent value, such as the TRACEPARENT environment variable. It
// returns ctx unchanged if the value is empty or invalid.
func ContextWithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{"traceparent": traceparent}
	return propagation.TraceContext{}.Extract(ctx, carrier)
}
