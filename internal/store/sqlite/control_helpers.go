package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const (
	defaultOutboxClaimLimit = 50
	maxOutboxClaimLimit     = 500
)

type controlQueryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func normalizeControllerEpochName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = store.DefaultControllerEpochName
	}
	if err := store.ValidateControlIdentifier("controller epoch name", name); err != nil {
		return "", err
	}
	if name != store.DefaultControllerEpochName {
		return "", store.ValidationErrorf("first-release control store supports only controller epoch %q", store.DefaultControllerEpochName)
	}
	return name, nil
}

func normalizeEpochFence(fence store.ControllerEpochFence) (store.ControllerEpochFence, error) {
	name, err := normalizeControllerEpochName(fence.Name)
	if err != nil {
		return store.ControllerEpochFence{}, err
	}
	fence.Name = name
	fence.HolderID = strings.TrimSpace(fence.HolderID)
	if err := store.ValidateControlIdentifier("controller epoch holder ID", fence.HolderID); err != nil {
		return store.ControllerEpochFence{}, err
	}
	if fence.Epoch < 1 {
		return store.ControllerEpochFence{}, store.ValidationErrorf("controller epoch must be at least 1")
	}
	return fence, nil
}

func requireControllerEpoch(ctx context.Context, q controlQueryRower, fence store.ControllerEpochFence) error {
	fence, err := normalizeEpochFence(fence)
	if err != nil {
		return err
	}
	var epoch int64
	var holder string
	err = q.QueryRowContext(ctx,
		`SELECT epoch, holder_id FROM controller_epochs WHERE name = ?`,
		fence.Name,
	).Scan(&epoch, &holder)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: controller epoch %q does not exist", store.ErrConflict, fence.Name)
	}
	if err != nil {
		return err
	}
	if epoch != fence.Epoch || holder != fence.HolderID {
		return fmt.Errorf("%w: controller epoch fence %s/%d/%s does not match current %d/%s", store.ErrConflict, fence.Name, fence.Epoch, fence.HolderID, epoch, holder)
	}
	return nil
}

func normalizeControlTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func normalizeOptionalControlTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func marshalOptionalControlJSON(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if reflected.IsNil() {
			return nil, nil
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func unmarshalOptionalControlJSON(data []byte, value any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, value)
}

func controlJSONPresent(data []byte) bool {
	trimmed := strings.TrimSpace(string(data))
	return trimmed != "" && trimmed != "null"
}

func controlConflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrConflict, fmt.Sprintf(format, args...))
}

func rowsAffectedExactlyOne(result sql.Result, what string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return controlConflict("stale %s mutation", what)
	}
	return nil
}

func canonicalBytesDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func nullTimeValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

var _ store.DurableControlStore = (*Store)(nil)
