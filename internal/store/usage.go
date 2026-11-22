package store

import "context"

type UsageAttribution struct {
	TaskUID string
	Model   string
}

type usageAttributionKey struct{}

func WithUsageAttribution(ctx context.Context, attribution UsageAttribution) context.Context {
	return context.WithValue(ctx, usageAttributionKey{}, attribution)
}

func UsageAttributionFrom(ctx context.Context) UsageAttribution {
	attribution, _ := ctx.Value(usageAttributionKey{}).(UsageAttribution)
	return attribution
}
