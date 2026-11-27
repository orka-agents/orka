package usage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventObservationRefusesMalformedUsageInAnyCase(t *testing.T) {
	attributed := store.UsageAttribution{TaskUID: "task-uid"}
	for _, content := range []string{`{"usage":"not an object"}`, `{"USAGE":"not an object"}`, `{"Usage":7}`} {
		event := store.ExecutionEvent{
			Namespace: "tenant", StreamType: events.ExecutionEventStreamTypeTask, StreamID: "task", TaskName: "task",
			Type: events.ExecutionEventTypeModelUsageUpdated, Content: []byte(content), CreatedAt: time.Now().UTC(),
		}
		_, err := ExecutionEventObservation(event, attributed)
		require.ErrorIs(t, err, store.ErrValidation, content)

		observation, err := ExecutionEventObservation(event, store.UsageAttribution{})
		require.NoError(t, err, content)
		require.Nil(t, observation, content)
	}
}
