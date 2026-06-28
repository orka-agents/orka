/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
)

// MapCarrier adapts a string map to OTel's TextMapCarrier.
type MapCarrier map[string]string

func (c MapCarrier) Get(key string) string {
	if c == nil {
		return ""
	}
	if value, ok := c[key]; ok {
		return value
	}
	lower := strings.ToLower(key)
	for k, value := range c {
		if strings.ToLower(k) == lower {
			return value
		}
	}
	return ""
}

func (c MapCarrier) Set(key, value string) {
	if c != nil {
		c[key] = value
	}
}

func (c MapCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}

func InjectContext(ctx context.Context) MapCarrier {
	carrier := MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier
}

func ExtractContext(ctx context.Context, carrier MapCarrier) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
