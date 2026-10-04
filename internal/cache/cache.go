// Package cache keeps payloads in memory, each until its own deadline.
package cache

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("go-observatory/internal/cache")

type entry[Payload any] struct {
	deadline time.Time
	payload  Payload
}

// MemoryCache is payloads by key, each until its own deadline, with an internal span per call.
type MemoryCache[Payload any] struct {
	mu      sync.Mutex
	entries map[string]entry[Payload]
}

// New starts empty.
func New[Payload any]() *MemoryCache[Payload] {
	return &MemoryCache[Payload]{entries: map[string]entry[Payload]{}}
}

// Get returns the payload under key, or false if it is absent or expired.
func (c *MemoryCache[Payload]) Get(ctx context.Context, key string) (Payload, bool) {
	_, span := tracer.Start(ctx, "cache get", trace.WithAttributes(attribute.String("cache.key", key)))
	defer span.End()

	c.mu.Lock()
	defer c.mu.Unlock()
	// An absent key reads as one long expired.
	found, ok := c.entries[key]
	hit := ok && time.Now().Before(found.deadline)
	span.SetAttributes(attribute.Bool("cache.hit", hit))
	return found.payload, hit
}

// Set keeps payload under key for ttl.
func (c *MemoryCache[Payload]) Set(ctx context.Context, key string, payload Payload, ttl time.Duration) {
	_, span := tracer.Start(ctx, "cache set", trace.WithAttributes(attribute.String("cache.key", key)))
	defer span.End()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry[Payload]{deadline: time.Now().Add(ttl), payload: payload}
}
