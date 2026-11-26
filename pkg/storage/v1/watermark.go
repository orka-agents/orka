package v1

import (
	"context"
	"time"
)

// WriteWatermark is the write watermark one dataset holds: a counter that
// only fenced watermark writes increase.
type WriteWatermark struct {
	// DatasetID is the dataset that holds the watermark.
	DatasetID string
	// Value is how many watermark advances the dataset has committed. It is
	// zero for a dataset that was never advanced, and it never decreases.
	Value int64
	// AdvancedAt is the provider clock when Value last increased, or zero when
	// it never has.
	AdvancedAt time.Time
}

// WriteWatermarkStore keeps a dataset's write watermark, which lets Orka
// detect a dataset that lost acknowledged writes.
//
// Orka advances the watermark of each dataset it is bound to with a fenced
// write, about once a minute, and once the write returns nil it records the
// value in Kubernetes. A dataset that reports a lower watermark than the
// value Kubernetes recorded has lost writes it acknowledged, or was restored
// from an older copy that kept its dataset identity. Orka then stops work
// and waits for an operator instead of resuming, so a matching dataset
// identity alone never lets restored work continue.
//
// Both providers implement it, because each dataset holds its own
// watermark.
type WriteWatermarkStore interface {
	// AdvanceWriteWatermark increases the dataset's watermark by exactly one
	// and returns the watermark it now holds. It is a write: it carries a
	// [WriteEnvelope], commits only under the current fence, and a replay of
	// the same operation returns the original watermark without advancing it
	// again.
	AdvanceWriteWatermark(ctx context.Context) (WriteWatermark, error)
	// GetWriteWatermark returns the watermark the dataset holds, with Value
	// zero when it was never advanced.
	GetWriteWatermark(ctx context.Context) (WriteWatermark, error)
}
