// Package middleware gives every request a key, turns what a handler could not handle into a
// response and writes a line for every response.
package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"go-observatory/internal/observability"
)

// RequestIDHeader is the header a request's key arrives in, if the caller has one, and is
// returned in.
const RequestIDHeader = "X-Request-ID"

// Access gives the request a key, returns it in the response and writes a line for every
// response. A request that failed — an error handed to c.Error, a panic — gets its error in the
// line and an exception event on its span. Paths in excludedPaths — liveness probes — get neither
// a key nor a line.
func Access(excludedPaths ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if slices.Contains(excludedPaths, c.Request.URL.Path) {
			c.Next()
			return
		}

		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		started := time.Now()
		c.Request = c.Request.WithContext(observability.WithRequestID(c.Request.Context(), id))
		// The request's key on its span, so a trace is found by the key a log line or a caller has.
		trace.SpanFromContext(c.Request.Context()).SetAttributes(
			attribute.StringSlice("http.response.header.x_request_id", []string{id}),
		)
		c.Header(RequestIDHeader, id)

		c.Next()

		ctx := c.Request.Context()
		failure := c.Errors.Last()
		if failure == nil {
			write(ctx, c, "HTTP request handled", started, nil)
			return
		}
		// otelgin has the span's status and error.type; the event adds what failed and where. Not
		// span.RecordError: it names a wrapped error by its wrapper, *fmt.wrapError.
		exception := []attribute.KeyValue{
			semconv.ExceptionType(errorType(failure.Err)),
			semconv.ExceptionMessage(failure.Error()),
		}
		if stack := stackOf(failure.Err); stack != "" {
			exception = append(exception, semconv.ExceptionStacktrace(stack))
		}
		trace.SpanFromContext(ctx).AddEvent(semconv.ExceptionEventName, trace.WithAttributes(exception...))
		write(ctx, c, "HTTP request failed", started, failure.Err)
	}
}

// errorType names the error as error.type on the metrics does: its type through the fmt.Errorf
// wrappers, *strconv.NumError for a failed Atoi however deeply wrapped.
func errorType(err error) string {
	return semconv.ErrorType(err).Value.AsString()
}

// stackOf is the stack a panic happened on; an error returned has none.
func stackOf(err error) string {
	if panicked, ok := errors.AsType[*PanicError](err); ok {
		return panicked.Stack
	}
	return ""
}

func write(ctx context.Context, c *gin.Context, message string, started time.Time, failure error) {
	attrs := []slog.Attr{
		slog.String("method", c.Request.Method),
		slog.String("path", c.Request.URL.Path),
		// The template the metrics carry as http_route; none outside the API: a 404, the static files.
		slog.String("route", c.FullPath()),
		slog.String("query", c.Request.URL.RawQuery),
		slog.Int("status", c.Writer.Status()),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	if failure != nil {
		// Fields rather than text: lines are filtered by the kind of failure.
		attrs = append(attrs,
			slog.String("error_type", errorType(failure)),
			slog.String("error_message", failure.Error()),
		)
		if stack := stackOf(failure); stack != "" {
			attrs = append(attrs, slog.String("error_stack", stack))
		}
	}
	slog.LogAttrs(ctx, severity(c.Writer.Status()), message, attrs...)
}

func severity(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
