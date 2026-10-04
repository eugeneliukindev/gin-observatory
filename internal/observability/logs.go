// Package observability is how the process speaks about itself: JSON log lines with the keys of
// their request, traces and metrics over OTLP, and CPU profiles labelled with spans.
package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// LevelCritical is above ERROR: the process cannot go on as it is.
const LevelCritical = slog.Level(12)

// The logger of our own code; `caller` tells where a line is from.
const ownLogger = "observatory"

// The names a line carries for its level: the same as the Python service's, so one Loki label
// serves both.
var levelNames = map[slog.Level]string{
	slog.LevelDebug: "DEBUG",
	slog.LevelInfo:  "INFO",
	slog.LevelWarn:  "WARNING",
	slog.LevelError: "ERROR",
	LevelCritical:   "CRITICAL",
}

// ConfigureLogging makes this process write JSON lines to stdout, ours from `level`.
func ConfigureLogging(level slog.Level) {
	slog.SetDefault(NewLogger(os.Stdout, level, ownLogger))
}

// NewLogger returns a logger named `name` that writes JSON lines to w from `level` up.
func NewLogger(w io.Writer, level slog.Leveler, name string) *slog.Logger {
	json := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: renameBuiltIn})
	return slog.New(contextualHandler{json}).With("logger", name)
}

// renameBuiltIn gives the built-in keys the names Alloy and the dashboard read.
func renameBuiltIn(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		// UTC to the millisecond, as the Python service writes it.
		return slog.String("ts", a.Value.Time().UTC().Format("2006-01-02T15:04:05.000-07:00"))
	case slog.LevelKey:
		level, _ := a.Value.Any().(slog.Level)
		name, ok := levelNames[level]
		if !ok {
			name = level.String()
		}
		return slog.String("lvl", name)
	}
	return a
}

// contextualHandler stamps every record with where it was written, the request key and the current
// trace and span — empty outside a request or a trace, so every line has the same fields.
type contextualHandler struct{ slog.Handler }

func (h contextualHandler) Handle(ctx context.Context, record slog.Record) error {
	traceID, spanID := "", ""
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		traceID, spanID = span.TraceID().String(), span.SpanID().String()
	}
	// A new record, so the keys come first and the line's own fields after them.
	stamped := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	stamped.AddAttrs(
		slog.String("caller", caller(record.PC)),
		slog.String("request_id", RequestID(ctx)),
		slog.String("trace_id", traceID),
		slog.String("span_id", spanID),
	)
	record.Attrs(func(a slog.Attr) bool {
		stamped.AddAttrs(a)
		return true
	})
	return h.Handler.Handle(ctx, stamped)
}

func (h contextualHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextualHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextualHandler) WithGroup(name string) slog.Handler {
	return contextualHandler{h.Handler.WithGroup(name)}
}

// caller is `package:function:line`, as the Python service's `module:function:line`.
func caller(pc uintptr) string {
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	// go-observatory/internal/middleware.write → middleware, write.
	qualified := frame.Function[strings.LastIndex(frame.Function, "/")+1:]
	pkg, function, _ := strings.Cut(qualified, ".")
	return pkg + ":" + function + ":" + strconv.Itoa(frame.Line)
}
