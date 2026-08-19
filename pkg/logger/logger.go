// Package logger provides the application's structured logger.
//
// docs/07 - System Architecture.md §48 requires structured logs and forbids
// logging passwords, tokens, manuscript content, or payment secrets. This
// package deliberately exposes no "log the whole request body" helper.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// contextKey is unexported so no other package can collide with our keys.
type contextKey struct{ name string }

var requestIDKey = &contextKey{"request_id"}

// New builds the process logger. Production emits JSON for log aggregation;
// development emits text for human readability.
func New(env, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if env == "production" || env == "staging" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithRequestID stores a request ID on the context so downstream layers
// (services, repositories, workers) can correlate their logs with the HTTP
// request that triggered them - docs/07 §49.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request ID carried by ctx, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// FromContext returns base annotated with the context's request ID when one is
// present, so callers can log without threading the ID through by hand.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := RequestIDFrom(ctx); id != "" {
		return base.With(slog.String("request_id", id))
	}
	return base
}
