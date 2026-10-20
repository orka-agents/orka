package storageclient_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// recordingStore is the in-process SQLite provider with a recorder in front
// of every write that carries content Orka redacts, sanitizes or derives. It
// records each text value it receives before passing the call on.
type recordingStore struct {
	*sqlite.Store

	mu    sync.Mutex
	calls map[string]int
	texts []recordedText
}

type recordedText struct {
	method string
	value  string
}

func newRecordingStore(t *testing.T) *recordingStore {
	t.Helper()
	return &recordingStore{Store: openSQLite(t), calls: map[string]int{}}
}

func (r *recordingStore) record(method string, values ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[method]++
	for _, value := range values {
		collectTexts(reflect.ValueOf(value), map[uintptr]bool{}, func(text string) {
			r.texts = append(r.texts, recordedText{method: method, value: text})
		})
	}
}

// collectTexts visits every string and byte slice reachable from value,
// including fields hidden from JSON.
func collectTexts(value reflect.Value, seen map[uintptr]bool, visit func(string)) {
	switch value.Kind() {
	case reflect.String:
		visit(value.String())
	case reflect.Pointer:
		if value.IsNil() || seen[value.Pointer()] {
			return
		}
		seen[value.Pointer()] = true
		collectTexts(value.Elem(), seen, visit)
	case reflect.Interface:
		if !value.IsNil() {
			collectTexts(value.Elem(), seen, visit)
		}
	case reflect.Struct:
		for _, field := range value.Fields() {
			collectTexts(field, seen, visit)
		}
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
			visit(string(value.Bytes()))
			return
		}
		for i := range value.Len() {
			collectTexts(value.Index(i), seen, visit)
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			collectTexts(key, seen, visit)
			collectTexts(value.MapIndex(key), seen, visit)
		}
	default:
	}
}

func (r *recordingStore) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	r.record("AppendExecutionEvent", event)
	return r.Store.AppendExecutionEvent(ctx, event)
}

func (r *recordingStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	r.record("AppendExecutionEventIfAbsent", event, key)
	return r.Store.AppendExecutionEventIfAbsent(ctx, event, key)
}

func (r *recordingStore) AppendExecutionEventWithPlanIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string, plan *store.PlanState) (*store.ExecutionEvent, bool, error) {
	r.record("AppendExecutionEventWithPlanIfAbsent", event, key, plan)
	return r.Store.AppendExecutionEventWithPlanIfAbsent(ctx, event, key, plan)
}

func (r *recordingStore) CreateMemory(ctx context.Context, memory *store.Memory) error {
	r.record("CreateMemory", memory)
	return r.Store.CreateMemory(ctx, memory)
}

func (r *recordingStore) UpdateMemory(ctx context.Context, memory *store.Memory) error {
	r.record("UpdateMemory", memory)
	return r.Store.UpdateMemory(ctx, memory)
}

func (r *recordingStore) CreateMemoryProposal(ctx context.Context, proposal *store.MemoryProposal) error {
	r.record("CreateMemoryProposal", proposal)
	return r.Store.CreateMemoryProposal(ctx, proposal)
}

func (r *recordingStore) ReviewMemoryProposal(ctx context.Context, review store.MemoryProposalReview) error {
	r.record("ReviewMemoryProposal", review)
	return r.Store.ReviewMemoryProposal(ctx, review)
}

func (r *recordingStore) ApplyMemoryProposal(ctx context.Context, apply store.MemoryProposalApply) (*store.Memory, error) {
	r.record("ApplyMemoryProposal", apply)
	return r.Store.ApplyMemoryProposal(ctx, apply)
}

func (r *recordingStore) SaveThreatModel(ctx context.Context, model *store.ThreatModel) error {
	r.record("SaveThreatModel", model)
	return r.Store.SaveThreatModel(ctx, model)
}

func (r *recordingStore) SaveThreatModelIfVersion(ctx context.Context, model *store.ThreatModel, expectedVersion int64) error {
	r.record("SaveThreatModelIfVersion", model, expectedVersion)
	return r.Store.SaveThreatModelIfVersion(ctx, model, expectedVersion)
}

// ApplyScanTaskIngestion hands the callback a recording transaction store,
// standing in for the provider's transaction-scoped store.
func (r *recordingStore) ApplyScanTaskIngestion(
	_ context.Context,
	ingestion *store.ScanTaskIngestion,
	_ func(*store.ScanRun) error,
	apply func(store.SecurityStore, *store.ScanRun) error,
) (bool, error) {
	r.record("ApplyScanTaskIngestion", ingestion)
	return true, apply(recordingSecurityStore{SecurityStore: r.Store, recorder: r}, &store.ScanRun{})
}

type recordingSecurityStore struct {
	store.SecurityStore
	recorder *recordingStore
}

func (s recordingSecurityStore) SaveThreatModel(ctx context.Context, model *store.ThreatModel) error {
	s.recorder.record("ingestion SaveThreatModel", model)
	return s.SecurityStore.SaveThreatModel(ctx, model)
}

func (s recordingSecurityStore) SaveThreatModelIfVersion(ctx context.Context, model *store.ThreatModel, expectedVersion int64) error {
	s.recorder.record("ingestion SaveThreatModelIfVersion", model, expectedVersion)
	return s.SecurityStore.SaveThreatModelIfVersion(ctx, model, expectedVersion)
}

func (r *recordingStore) RecordUsage(ctx context.Context, observation store.UsageObservation) error {
	r.record("RecordUsage", observation)
	return r.Store.RecordUsage(ctx, observation)
}

func (r *recordingStore) CommitSessionTurnFinalization(ctx context.Context, request store.CommitSessionTurnFinalizationRequest) (*store.SessionTurn, error) {
	r.record("CommitSessionTurnFinalization", request)
	return r.Store.CommitSessionTurnFinalization(ctx, request)
}

func (r *recordingStore) requireCalled(t *testing.T, methods ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, method := range methods {
		require.Positive(t, r.calls[method], "the provider never received %s, so the test proves nothing about it", method)
	}
}

func (r *recordingStore) requireNoSecret(t *testing.T, secrets map[string]string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.texts)
	for _, text := range r.texts {
		for name, secret := range secrets {
			require.NotContains(t, text.value, secret, "%s received the %s secret", text.method, name)
		}
	}
}

func (r *recordingStore) requireReceived(t *testing.T, method, want string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, text := range r.texts {
		if text.method == method && strings.Contains(text.value, want) {
			return
		}
	}
	t.Fatalf("%s never received %q", method, want)
}

func protectedSecrets() map[string]string {
	return map[string]string{
		"OpenAI key":   "sk-" + strings.Repeat("q", 28),
		"GitHub token": "ghp_" + strings.Repeat("r", 36),
		"bearer value": "bearer-value-for-redaction",
	}
}

// TestProviderReceivesOnlyRedactedContent drives every write outside the
// gateway family whose content Orka redacts, sanitizes or derives through the
// client and proves that the provider receives none of the secrets the caller
// supplied. TestProviderReceivesOnlySanitizedGatewayText covers gateway writes.
func TestProviderReceivesOnlyRedactedContent(t *testing.T) {
	secrets := protectedSecrets()
	openAI, github, bearer := secrets["OpenAI key"], secrets["GitHub token"], secrets["bearer value"]
	recorder := newRecordingStore(t)
	client, err := storageclient.NewEmbedded(recorder)
	require.NoError(t, err)
	ctx := t.Context()

	content, err := json.Marshal(map[string]any{"apiKey": openAI, "note": "token " + github, "safe": "kept"})
	require.NoError(t, err)
	event := func(streamID string) *store.ExecutionEvent {
		return &store.ExecutionEvent{
			Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask, StreamID: streamID, TaskName: streamID,
			Type: events.ExecutionEventTypeModelMessage, Summary: "Authorization: Bearer " + bearer,
			Content: content, ContentText: "api_key=" + openAI,
		}
	}
	_, err = client.AppendExecutionEvent(ctx, event("plain"))
	require.NoError(t, err)
	_, _, err = client.AppendExecutionEventIfAbsent(ctx, event("deduplicated"), "key")
	require.NoError(t, err)
	_, _, err = client.AppendExecutionEventWithPlanIfAbsent(ctx, event("planned"), "key",
		&store.PlanState{Namespace: "tenant", TaskName: "planned"})
	require.NoError(t, err)

	// Usage derived from a worker report and from the controller journal
	// carries provider and model names; both reach the provider redacted.
	workerUsage, err := json.Marshal(map[string]any{"usage": map[string]any{
		"id": "call-1", "counterID": "call-1", "provider": openAI, "model": github,
		"inputTokens": 10, "outputTokens": 2, "status": store.UsageStatusCompleted,
	}})
	require.NoError(t, err)
	_, err = client.AppendExecutionEvent(store.WithUsageAttribution(ctx, store.UsageAttribution{TaskUID: "task-uid"}),
		&store.ExecutionEvent{Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "worker",
			TaskName: "worker", Type: events.ExecutionEventTypeModelMessage, Content: workerUsage})
	require.NoError(t, err)
	harnessUsage, err := json.Marshal(map[string]any{
		"harnessV2":   map[string]any{"taskUID": "task-uid", "promptID": "prompt", "taskAttempt": 1, "sequence": 1},
		"inputTokens": 5, "outputTokens": 1, "usageReported": true, "provider": openAI,
	})
	require.NoError(t, err)
	_, _, err = client.AppendExecutionEventIfAbsent(store.WithUsageAttribution(ctx, store.UsageAttribution{Model: "model " + github}),
		&store.ExecutionEvent{Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "journal",
			TaskName: "journal", Type: events.ExecutionEventTypeModelUsageUpdated, Content: harnessUsage}, "usage-1")
	require.NoError(t, err)
	require.NoError(t, client.RecordUsage(ctx, store.UsageObservation{
		ID: "direct", Namespace: "tenant", TaskUID: "task-uid", CounterID: "direct", Scope: store.UsageScopeCall,
		Source: store.UsageSourceProvider, Provider: openAI, Model: github, Status: store.UsageStatusCompleted,
		ObservedAt: time.Now().UTC(),
	}))

	memory := &store.Memory{Namespace: "tenant", Content: "the deploy token is " + github}
	require.NoError(t, client.CreateMemory(ctx, memory))
	memory.Content = "rotate api_key=" + openAI
	require.NoError(t, client.UpdateMemory(ctx, memory))
	proposal := &store.MemoryProposal{
		Namespace: "tenant", Type: "memory", Title: "token is " + github, Description: "api_key=" + openAI,
		Content: "Authorization: Bearer " + bearer, Patch: "secret=" + openAI,
	}
	require.NoError(t, client.CreateMemoryProposal(ctx, proposal))
	require.NoError(t, client.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: "tenant", ID: proposal.ID, Status: "accepted", Reviewer: "token is " + github, ReviewNote: "api_key=" + openAI,
	}))
	_, err = client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: "tenant", ID: proposal.ID, AppliedBy: "token is " + github})
	require.NoError(t, err)

	threatModel := func(content string) *store.ThreatModel {
		return &store.ThreatModel{Namespace: "tenant", RepositoryScan: "repo", Source: "edited", Content: content}
	}
	require.NoError(t, client.SaveThreatModel(ctx, threatModel("api_key="+openAI)))
	require.NoError(t, client.SaveThreatModelIfVersion(ctx, threatModel("token is "+github), 1))
	_, err = client.ApplyScanTaskIngestion(ctx, &store.ScanTaskIngestion{}, nil, func(tx store.SecurityStore, _ *store.ScanRun) error {
		if err := tx.SaveThreatModel(ctx, threatModel("Authorization: Bearer "+bearer)); err != nil {
			return err
		}
		return tx.SaveThreatModelIfVersion(ctx, threatModel("api_key="+openAI), 3)
	})
	require.NoError(t, err)

	recorder.requireCalled(t,
		"AppendExecutionEvent", "AppendExecutionEventIfAbsent", "AppendExecutionEventWithPlanIfAbsent", "RecordUsage",
		"CreateMemory", "UpdateMemory", "CreateMemoryProposal", "ReviewMemoryProposal", "ApplyMemoryProposal",
		"SaveThreatModel", "SaveThreatModelIfVersion", "ingestion SaveThreatModel", "ingestion SaveThreatModelIfVersion",
	)
	recorder.requireNoSecret(t, secrets)
	// The provider received the content around the secrets, so the check
	// above is not passing on empty records.
	recorder.requireReceived(t, "AppendExecutionEvent", "kept")
	recorder.requireReceived(t, "AppendExecutionEvent", "task-uid/call-1")
	recorder.requireReceived(t, "AppendExecutionEventIfAbsent", "acp/task-uid/prompt/1/")
	recorder.requireReceived(t, "CreateMemory", "the deploy token is")
	recorder.requireReceived(t, "ingestion SaveThreatModel", "Authorization:")
}

func TestClientDerivesWorkerUsageOnlyForTheAttributedTask(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := t.Context()
	content, err := json.Marshal(map[string]any{"usage": map[string]any{
		"id": "call", "counterID": "counter", "inputTokens": 7, "outputTokens": 3, "status": store.UsageStatusCompleted,
	}})
	require.NoError(t, err)
	report := func(ctx context.Context, streamID string) {
		t.Helper()
		_, err := client.AppendExecutionEvent(ctx, &store.ExecutionEvent{Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask,
			StreamID: streamID, TaskName: streamID, Type: events.ExecutionEventTypeModelMessage, Content: content})
		require.NoError(t, err)
	}
	// Without controller attribution a "usage" field is ordinary content.
	report(ctx, "unattributed")
	data, err := client.LoadUsage(ctx, store.UsageFilter{Namespaces: []string{"tenant"}, AsOf: time.Now().UTC()})
	require.NoError(t, err)
	require.Empty(t, data.Observations)

	report(store.WithUsageAttribution(ctx, store.UsageAttribution{TaskUID: "task-uid"}), "attributed")
	data, err = client.LoadUsage(ctx, store.UsageFilter{Namespaces: []string{"tenant"}, AsOf: time.Now().UTC()})
	require.NoError(t, err)
	require.Len(t, data.Observations, 1)
	observation := data.Observations[0]
	require.Equal(t, "task-uid/call", observation.ID)
	require.Equal(t, "task-uid/counter", observation.CounterID)
	require.Equal(t, "task-uid", observation.TaskUID)
	require.Equal(t, store.UsageScopeCall, observation.Scope)
	require.Equal(t, store.UsageSourceProvider, observation.Source)

	input, output, err := client.TaskTokenUsage(ctx, "tenant", "task-uid")
	require.NoError(t, err)
	require.Equal(t, [2]int64{7, 3}, [2]int64{input, output})

	// An invalid worker report fails the append rather than being dropped,
	// and counts as an append failure.
	failures := func() float64 {
		return metrics.CounterVecValue(metrics.ExecutionEventAppendFailuresTotal,
			store.ExecutionEventStreamTypeTask, events.ExecutionEventTypeModelMessage)
	}
	before := failures()
	invalid, err := json.Marshal(map[string]any{"usage": map[string]any{"id": "bad", "counterID": "c", "status": "unknown"}})
	require.NoError(t, err)
	_, err = client.AppendExecutionEvent(store.WithUsageAttribution(ctx, store.UsageAttribution{TaskUID: "task-uid"}),
		&store.ExecutionEvent{Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "invalid",
			TaskName: "invalid", Type: events.ExecutionEventTypeModelMessage, Content: invalid})
	require.ErrorIs(t, err, store.ErrValidation)
	require.InDelta(t, before+1, failures(), 0)
}

// TestClientCountsSanitizationOncePerNewEvent checks that the client records
// the redaction and truncation metrics once for each newly appended event and
// not for a deduplicated replay.
func TestClientCountsSanitizationOncePerNewEvent(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := t.Context()
	streamType, eventType := store.ExecutionEventStreamTypeTask, events.ExecutionEventTypeToolCallCompleted
	redactions := func() float64 {
		return metrics.CounterVecValue(metrics.ExecutionEventRedactionsTotal, streamType, eventType)
	}
	truncations := func() float64 {
		return metrics.CounterVecValue(metrics.ExecutionEventTruncationsTotal, streamType, eventType)
	}
	redactedBefore, truncatedBefore := redactions(), truncations()
	event := func(streamID string) *store.ExecutionEvent {
		return &store.ExecutionEvent{Namespace: "tenant", StreamType: streamType, StreamID: streamID, TaskName: streamID,
			Type: eventType, Summary: "api_key=" + protectedSecrets()["OpenAI key"],
			ContentText: strings.Repeat("x", events.MaxExecutionEventContentTextChars+1)}
	}
	_, err := client.AppendExecutionEvent(ctx, event("plain"))
	require.NoError(t, err)
	_, appended, err := client.AppendExecutionEventIfAbsent(ctx, event("deduplicated"), "key")
	require.NoError(t, err)
	require.True(t, appended)
	_, appended, err = client.AppendExecutionEventIfAbsent(ctx, event("deduplicated"), "key")
	require.NoError(t, err)
	require.False(t, appended)
	_, appended, err = client.AppendExecutionEventWithPlanIfAbsent(ctx, event("planned"), "key",
		&store.PlanState{Namespace: "tenant", TaskName: "planned"})
	require.NoError(t, err)
	require.True(t, appended)
	_, appended, err = client.AppendExecutionEventWithPlanIfAbsent(ctx, event("planned"), "key",
		&store.PlanState{Namespace: "tenant", TaskName: "planned"})
	require.NoError(t, err)
	require.False(t, appended)
	require.InDelta(t, redactedBefore+3, redactions(), 0, "one redaction per new event")
	require.InDelta(t, truncatedBefore+3, truncations(), 0, "one truncation per new event")
}

func TestClientTaskTokenUsageAttributesOnlyTheTaskShare(t *testing.T) {
	ctx := t.Context()
	client := newEmbeddedClient(t)
	at := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	record := func(task, id, counter, scope string, input, output int64, offset time.Duration) {
		t.Helper()
		require.NoError(t, client.RecordUsage(ctx, store.UsageObservation{Namespace: "team", TaskUID: task, TaskName: task,
			ID: id, CounterID: counter, Scope: scope, Source: store.UsageSourceAgent,
			InputTokens: &input, OutputTokens: &output, Status: "running", ObservedAt: at.Add(offset)}))
	}
	// A runtime session counter is cumulative across turns: the earlier turn
	// left it at 100/10, so task-b consumed only the increase to 160/16.
	record("task-a", "a-session", "acp-session/s/1", store.UsageScopeSession, 100, 10, 0)
	record("task-b", "b-session", "acp-session/s/1", store.UsageScopeSession, 160, 16, 2*time.Second)
	// A per-call counter counts its final value once, not every snapshot.
	record("task-b", "b-call-running", "call-b", store.UsageScopeCall, 3, 0, time.Second)
	record("task-b", "b-call-final", "call-b", store.UsageScopeCall, 7, 2, 3*time.Second)
	record("task-c", "c-call", "call-c", store.UsageScopeCall, 9, 9, time.Second)

	for _, tt := range []struct {
		namespace, task string
		input, output   int64
	}{
		{namespace: "team", task: "task-b", input: 67, output: 8},
		{namespace: "team", task: "task-c", input: 9, output: 9},
		{namespace: "team", task: "unreported"},
		{namespace: "other-team", task: "task-b"},
	} {
		input, output, err := client.TaskTokenUsage(ctx, tt.namespace, tt.task)
		require.NoError(t, err)
		require.Equal(t, [2]int64{tt.input, tt.output}, [2]int64{input, output}, "%s/%s", tt.namespace, tt.task)
	}
}
