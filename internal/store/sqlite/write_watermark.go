package sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var _ storagev1.WriteWatermarkStore = (*Store)(nil)

// AdvanceWriteWatermark implements storagev1.WriteWatermarkStore. The
// watermark is a column of the dataset's identity row, so it moves with the
// dataset: a copy restored from an older file holds the older value.
func (s *Store) AdvanceWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	return writeOperation(ctx, s, "AdvanceWriteWatermark", nil, func(ctx context.Context) (storagev1.WriteWatermark, error) {
		tx, err := s.beginTx(ctx)
		if err != nil {
			return storagev1.WriteWatermark{}, err
		}
		defer func() { _ = tx.Rollback() }()
		result, err := tx.ExecContext(ctx,
			`UPDATE store_dataset SET write_watermark = write_watermark + 1, write_watermark_at_ns = ? WHERE singleton = 1`,
			s.now().UnixNano(),
		)
		if err != nil {
			return storagev1.WriteWatermark{}, fmt.Errorf("advance SQLite write watermark: %w", err)
		}
		if rows, err := result.RowsAffected(); err != nil {
			return storagev1.WriteWatermark{}, err
		} else if rows != 1 {
			return storagev1.WriteWatermark{}, errors.New("SQLite dataset identity must contain exactly one record")
		}
		watermark, err := readWriteWatermark(ctx, tx)
		if err != nil {
			return storagev1.WriteWatermark{}, err
		}
		if err := tx.Commit(); err != nil {
			return storagev1.WriteWatermark{}, err
		}
		return watermark, nil
	})
}

// GetWriteWatermark implements storagev1.WriteWatermarkStore.
func (s *Store) GetWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	var watermark storagev1.WriteWatermark
	err := s.readSnapshot(ctx, func(q taskDataExecutor) error {
		var err error
		watermark, err = readWriteWatermark(ctx, q)
		return err
	})
	if err != nil {
		return storagev1.WriteWatermark{}, err
	}
	return watermark, nil
}

func readWriteWatermark(ctx context.Context, q taskDataExecutor) (storagev1.WriteWatermark, error) {
	record, err := readDataset(q)
	if err != nil {
		return storagev1.WriteWatermark{}, err
	}
	var value, advancedAt int64
	if err := q.QueryRowContext(ctx,
		`SELECT write_watermark, write_watermark_at_ns FROM store_dataset WHERE singleton = 1`,
	).Scan(&value, &advancedAt); err != nil {
		return storagev1.WriteWatermark{}, fmt.Errorf("read SQLite write watermark: %w", err)
	}
	if value < 0 || advancedAt < 0 || (value == 0) != (advancedAt == 0) {
		return storagev1.WriteWatermark{}, fmt.Errorf("invalid SQLite write watermark %d advanced at %d", value, advancedAt)
	}
	watermark := storagev1.WriteWatermark{DatasetID: record.id, Value: value}
	if advancedAt != 0 {
		watermark.AdvancedAt = time.Unix(0, advancedAt).UTC()
	}
	return watermark, nil
}
