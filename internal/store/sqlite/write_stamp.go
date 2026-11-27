package sqlite

import (
	"context"
	"fmt"
	"math"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type writeStamp struct {
	epoch    int64
	sequence int64
	stamped  bool
}

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

func (w writeStamp) replaceCondition(table string) string {
	if !w.stamped {
		return ""
	}
	return fmt.Sprintf(" WHERE (%[1]s.write_epoch, %[1]s.write_seq) < (excluded.write_epoch, excluded.write_seq)", table)
}
