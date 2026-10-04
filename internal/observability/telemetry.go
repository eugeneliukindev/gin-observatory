package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	otelpyroscope "github.com/grafana/otel-profiling-go"
	"github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/contrib/instrumentation/host"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

const (
	exportEvery        = 15 * time.Second
	exportTimeout      = 10 * time.Second
	profileUploadEvery = 10 * time.Second
)

// Requests served and the process serving them; whatever else is instrumented records into nothing.
var shippedMetrics = []string{"http.server.", "process.", "go."}

// What the profiler samples: the CPU, the heap, the goroutines and the time spent waiting on locks.
var profileTypes = []pyroscope.ProfileType{
	pyroscope.ProfileCPU,
	pyroscope.ProfileAllocObjects,
	pyroscope.ProfileAllocSpace,
	pyroscope.ProfileInuseObjects,
	pyroscope.ProfileInuseSpace,
	pyroscope.ProfileGoroutines,
	pyroscope.ProfileMutexCount,
	pyroscope.ProfileMutexDuration,
	pyroscope.ProfileBlockCount,
	pyroscope.ProfileBlockDuration,
}

// ServiceIdentity is who sends the telemetry: the same on traces, metrics and profiles of a process.
type ServiceIdentity struct {
	Name        string
	Environment string
}

// ConfigureTracing makes the process's tracer provider, shipping spans in batches over OTLP.
func ConfigureTracing(ctx context.Context, service ServiceIdentity, otlpEndpoint string) (*sdktrace.TracerProvider, error) {
	res, err := newResource(ctx, service)
	if err != nil {
		return nil, err
	}
	// An http:// endpoint means no TLS.
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpointURL(otlpEndpoint),
		otlptracegrpc.WithTimeout(exportTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	slog.InfoContext(ctx, "exporting traces over OTLP", "otlp_endpoint", otlpEndpoint)
	return provider, nil
}

// ConfigureMetrics makes the process's meter provider and measures the process; only served
// requests and the process ship.
func ConfigureMetrics(ctx context.Context, service ServiceIdentity, otlpEndpoint string) (*sdkmetric.MeterProvider, error) {
	res, err := newResource(ctx, service)
	if err != nil {
		return nil, err
	}
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpointURL(otlpEndpoint),
		otlpmetricgrpc.WithTimeout(exportTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("metric exporter: %w", err)
	}
	// One view decides for every instrument: a stream as it is for the names kept, a drop for the
	// rest. In this SDK a drop matched by one view silences the instrument, whatever else matches.
	shipOnlyKept := func(instrument sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		for _, kept := range shippedMetrics {
			if strings.HasPrefix(instrument.Name, kept) {
				return sdkmetric.Stream{Name: instrument.Name, Description: instrument.Description, Unit: instrument.Unit}, true
			}
		}
		return sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}}, true
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(exportEvery),
			// go.schedule.duration: how long a runnable goroutine waits for a thread.
			sdkmetric.WithProducer(otelruntime.NewProducer()),
		)),
		sdkmetric.WithView(shipOnlyKept),
	)
	otel.SetMeterProvider(provider)
	// The Go runtime — memory, goroutines, GOMAXPROCS — and the process's CPU time.
	if err := otelruntime.Start(otelruntime.WithMeterProvider(provider)); err != nil {
		return nil, fmt.Errorf("runtime metrics: %w", err)
	}
	if err := host.Start(host.WithMeterProvider(provider)); err != nil {
		return nil, fmt.Errorf("process metrics: %w", err)
	}
	slog.InfoContext(ctx, "exporting metrics over OTLP", "otlp_endpoint", otlpEndpoint)
	return provider, nil
}

// ConfigureProfiling starts profiling, its samples labelled with the spans of tracerProvider.
//
// Every span started after this call labels the samples taken on its goroutine, so a span links
// to the profile of exactly its own work.
func ConfigureProfiling(ctx context.Context, service ServiceIdentity, tracerProvider *sdktrace.TracerProvider, pyroscopeURL string) (*pyroscope.Profiler, error) {
	// Mutex and block profiles are off unless the runtime is told to sample them.
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(int(time.Millisecond))
	profiler, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: service.Name,
		ServerAddress:   pyroscopeURL,
		UploadRate:      profileUploadEvery,
		// The profile ingest protocol drops label names with dots.
		Tags:         map[string]string{strings.ReplaceAll(string(semconv.DeploymentEnvironmentNameKey), ".", "_"): service.Environment},
		ProfileTypes: profileTypes,
	})
	if err != nil {
		return nil, fmt.Errorf("profiling: %w", err)
	}
	otel.SetTracerProvider(otelpyroscope.NewTracerProvider(tracerProvider))
	slog.InfoContext(ctx, "sending profiles to Pyroscope", "pyroscope_url", pyroscopeURL)
	return profiler, nil
}

func newResource(ctx context.Context, service ServiceIdentity) (*resource.Resource, error) {
	// The container in Compose, the pod in Kubernetes: what an instance is, on either platform.
	hostName, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("host name: %w", err)
	}
	res, err := resource.New(ctx,
		// OTEL_RESOURCE_ATTRIBUTES: in Kubernetes it carries the pod and the node.
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(service.Name),
			// One per process: the series of two processes must not merge.
			semconv.ServiceInstanceID(hostName+"-"+strconv.Itoa(os.Getpid())),
			semconv.DeploymentEnvironmentNameKey.String(service.Environment),
			semconv.HostName(hostName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}
	return res, nil
}
