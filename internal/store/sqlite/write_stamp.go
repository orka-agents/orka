package sqlite

import (
	"context"
	"fmt"
	"math"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// writeStamp is the order stamp of a whole-object replacement: the epoch and
// sequence of its write envelope. Results and artifacts store the stamp of
// the write that stored them; rows written before schema version 4 have the
// stamp (0, 0). A write without an envelope, which only package tests make,
// is unstamped and replaces unconditionally.
type writeStamp struct {
	epoch    int64
	sequence int64
	stamped  bool
}

// writeStampFrom returns the order stamp of the write ctx carries.
func writeStampFrom(ctx context.Context) (writeStamp, error) {
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		return writeStamp{}, nil
	}
	if envelope.Sequence > math.MaxInt64 {
		return writeStamp{}, store.ValidationErrorf("write envelope sequence %d exceeds %d", envelope.Sequence, int64(math.MaxInt64))
	}
	return writeStamp{epoch: envelope.Fence.Epoch, sequence: int64(envelope.Sequence), stamped: true}, nil
}

// replaceCondition is the ON CONFLICT DO UPDATE condition that applies a
// replacement of a row in table only when the write's stamp is greater than
// the stored one. An unstamped write always applies.
func (w writeStamp) replaceCondition(table string) string {
	if !w.stamped {
		return ""
	}
	return fmt.Sprintf(" WHERE (%[1]s.write_epoch, %[1]s.write_seq) < (excluded.write_epoch, excluded.write_seq)", table)
}
