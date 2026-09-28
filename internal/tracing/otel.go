package tracing

import (
	"context"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/XXXDelirious/taskgraph/errors"
)

// OTelEnabled reports whether the standard OpenTelemetry environment
// variables ask for traces to be exported: an OTLP endpoint is set, and
// neither OTEL_SDK_DISABLED nor OTEL_TRACES_EXPORTER=none turns it off.
func OTelEnabled() bool {
	if disabled, _ := strconv.ParseBool(os.Getenv("OTEL_SDK_DISABLED")); disabled {
		return false
	}
	if exporter := os.Getenv("OTEL_TRACES_EXPORTER"); exporter != "" && !strings.Contains(exporter, "otlp") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// SetupOTel creates an OpenTelemetry tracer that exports spans over OTLP,
// configured by the standard OTEL_* environment variables (endpoint,
// headers, protocol, service name, resource attributes, ...). The protocol
// is http/protobuf unless OTEL_EXPORTER_OTLP_(TRACES_)PROTOCOL is "grpc".
//
// Call shutdown before exiting to flush the spans.
func SetupOTel(ctx context.Context, version string) (tracer trace.Tracer, shutdown func(context.Context) error, err error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	var exporter *otlptrace.Exporter
	if protocol == "grpc" {
		exporter, err = otlptracegrpc.New(ctx)
	} else {
		exporter, err = otlptracehttp.New(ctx)
	}
	if err != nil {
		return nil, nil, err
	}

	res, err := resource.Merge(
		resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName("taskgraph"),
			semconv.ServiceVersion(version),
		),
		// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES take precedence.
		resource.Environment(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	return provider.Tracer("github.com/XXXDelirious/taskgraph"), provider.Shutdown, nil
}
