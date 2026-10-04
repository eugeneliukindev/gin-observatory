// Package app builds the application: telemetry, clients, the database and the routes.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"go-observatory/internal/cache"
	"go-observatory/internal/config"
	"go-observatory/internal/database"
	"go-observatory/internal/dependencies"
	"go-observatory/internal/middleware"
	"go-observatory/internal/observability"
	"go-observatory/internal/router"
	"go-observatory/web"
)

const (
	placeholderURL     = "https://jsonplaceholder.typicode.com"
	placeholderTimeout = 5 * time.Second
	healthPath         = "/health"
)

// App is the application and what it holds open.
type App struct {
	// Handler serves the page and the API.
	Handler http.Handler
	closers []func(context.Context) error
}

// New sets the process up — telemetry, clients, the database — and returns the application.
func New(ctx context.Context, settings config.Settings) (*App, error) {
	app := &App{}
	service := observability.ServiceIdentity{Name: settings.Obs.ServiceName, Environment: string(settings.Obs.Environment)}
	tracerProvider, err := observability.ConfigureTracing(ctx, service, settings.Obs.OTLP.Endpoint)
	if err != nil {
		return nil, err
	}
	app.closers = append(app.closers, tracerProvider.Shutdown)
	meterProvider, err := observability.ConfigureMetrics(ctx, service, settings.Obs.OTLP.Endpoint)
	if err != nil {
		return nil, errors.Join(err, app.Close(ctx))
	}
	app.closers = append(app.closers, meterProvider.Shutdown)
	profiler, err := observability.ConfigureProfiling(ctx, service, tracerProvider, settings.Obs.Pyroscope.URL)
	if err != nil {
		return nil, errors.Join(err, app.Close(ctx))
	}
	app.closers = append(app.closers, func(context.Context) error { return profiler.Stop() })

	base, err := url.Parse(placeholderURL)
	if err != nil {
		return nil, errors.Join(err, app.Close(ctx))
	}
	client := &http.Client{
		Timeout:   placeholderTimeout,
		Transport: baseURL{base: base, next: otelhttp.NewTransport(http.DefaultTransport)},
	}

	db, err := database.Open(settings.DB.Path)
	if err != nil {
		return nil, errors.Join(err, app.Close(ctx))
	}
	slog.InfoContext(ctx, "SQLite database opened", "database_path", settings.DB.Path)
	app.closers = append(app.closers, func(ctx context.Context) error {
		err := db.Close()
		slog.InfoContext(ctx, "SQLite database closed")
		return err
	})

	page, err := web.Page()
	if err != nil {
		return nil, errors.Join(err, app.Close(ctx))
	}
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.HandleMethodNotAllowed = true
	engine.SetHTMLTemplate(page)
	engine.Use(
		// The probe says nothing.
		otelgin.Middleware(service.Name, otelgin.WithFilter(func(r *http.Request) bool { return r.URL.Path != healthPath })),
		//nolint:contextcheck // gin.Context passes for a context; the request's carries the span
		middleware.Access(healthPath),
		// Inside the line, so the line has the status they answer with.
		middleware.Recovery(),
		middleware.Errors(),
		dependencies.Provide(&dependencies.Dependencies{
			HTTP:     client,
			Cache:    cache.New[dependencies.Post](),
			Database: db,
			Settings: settings,
		}),
	)
	engine.StaticFS("/static", http.FS(web.Static()))
	router.Register(engine)
	app.Handler = engine
	return app, nil
}

// Close closes what the application holds, the last opened first: the database before telemetry,
// so its last spans and lines still leave.
func (a *App) Close(ctx context.Context) error {
	var errs []error
	for _, closeOne := range slices.Backward(a.closers) {
		errs = append(errs, closeOne(ctx))
	}
	a.closers = nil
	return errors.Join(errs...)
}

// baseURL sends a request for a path to the source: a handler names the path only.
type baseURL struct {
	base *url.URL
	next http.RoundTripper
}

func (b baseURL) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL = b.base.ResolveReference(r.URL)
	r.Host = r.URL.Host
	return b.next.RoundTrip(r)
}
