package v1

import (
	"context"
	"time"
)

type WriteWatermark struct {
	DatasetID  string
	Value      int64
	AdvancedAt time.Time
}

type WriteWatermarkStore interface {
	AdvanceWriteWatermark(ctx context.Context) (WriteWatermark, error)
	GetWriteWatermark(ctx context.Context) (WriteWatermark, error)
}
