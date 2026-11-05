package store

import "context"

// UsageAttribution is controller-owned accounting context for execution
// events appended through the storage client. Callers attach it only to the
// append call. The client derives a usage observation from the event and this
// context and masks the attribution before calling the provider.
type UsageAttribution struct {
	// TaskUID is the authenticated Task that reported worker model usage.
	// The client counts a worker "usage" report only when it is set.
	TaskUID string
	// Model is the model frozen in the controller's runtime profile. It
	// replaces the runtime's free-text model field on harness usage.
	Model string
}

type usageAttributionKey struct{}

// WithUsageAttribution returns a context whose execution event appends carry
// attribution. The storage client reads it when it derives usage.
func WithUsageAttribution(ctx context.Context, attribution UsageAttribution) context.Context {
	return context.WithValue(ctx, usageAttributionKey{}, attribution)
}

// UsageAttributionFrom returns the attribution set by WithUsageAttribution.
func UsageAttributionFrom(ctx context.Context) UsageAttribution {
	attribution, _ := ctx.Value(usageAttributionKey{}).(UsageAttribution)
	return attribution
}
