/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	forkcontext "github.com/sozercan/orka/internal/fork"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tracing"
)

type ForkTaskRequest struct {
	AfterSeq    *int64                        `json:"afterSeq,omitempty"`
	NewTaskName string                        `json:"newTaskName,omitempty"`
	AgentRef    *corev1alpha1.AgentReference  `json:"agentRef,omitempty"`
	Prompt      string                        `json:"prompt,omitempty"`
	Workspace   *corev1alpha1.WorkspaceConfig `json:"workspace,omitempty"`
}

type ForkTaskResponse struct {
	Namespace      string              `json:"namespace"`
	SourceTaskName string              `json:"sourceTaskName"`
	NewTaskName    string              `json:"newTaskName"`
	AfterSeq       int64               `json:"afterSeq"`
	ForkContext    forkcontext.Context `json:"forkContext"`
}

type ForkSessionRequest struct {
	AfterSeq       *int64                       `json:"afterSeq,omitempty"`
	NewSessionName string                       `json:"newSessionName,omitempty"`
	SeedTaskName   string                       `json:"seedTaskName,omitempty"`
	AgentRef       *corev1alpha1.AgentReference `json:"agentRef,omitempty"`
	Prompt         string                       `json:"prompt,omitempty"`
}

type ForkSessionResponse struct {
	Namespace         string              `json:"namespace"`
	SourceSessionName string              `json:"sourceSessionName"`
	NewSessionName    string              `json:"newSessionName"`
	SeedTaskName      string              `json:"seedTaskName,omitempty"`
	AfterSeq          int64               `json:"afterSeq"`
	ForkContext       forkcontext.Context `json:"forkContext"`
}

// ForkTask handles POST /api/v1/tasks/{id}/fork.
func (h *Handlers) ForkTask(c fiber.Ctx) error {
	sourceName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	access := h.taskAccess()
	if err := access.authorizeReadable(c, "forkTask", namespace, sourceName); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}

	var req ForkTaskRequest
	if len(c.Body()) > 0 {
		if err := c.Bind().JSON(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}

	source, err := access.loadAuthorized(c, "forkTask", namespace, sourceName)
	if err != nil {
		return err
	}

	latest, err := h.executionEventStore.GetLatestExecutionEventSeq(
		c.Context(),
		namespace,
		events.ExecutionEventStreamTypeTask,
		sourceName,
	)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get latest execution event sequence: %v", err))
	}
	afterSeq := latest
	if req.AfterSeq != nil {
		afterSeq = *req.AfterSeq
	}
	if afterSeq < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be non-negative")
	}
	if afterSeq > latest {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be 0, latest, or an existing event sequence")
	}
	validAfterSeq, err := taskEventSeqExists(c.Context(), h.executionEventStore, namespace, sourceName, afterSeq, latest)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to validate execution event sequence: %v", err))
	}
	if !validAfterSeq {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be 0, latest, or an existing event sequence")
	}
	eventsBefore, err := listTaskEventsThrough(c.Context(), h.executionEventStore, namespace, sourceName, afterSeq)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}

	forkCtx := forkcontext.BuildContext(namespace, sourceName, afterSeq, eventsBefore, forkcontext.DefaultMaxEvents)
	idempotencyKey := strings.TrimSpace(c.Get("Idempotency-Key"))
	newName := strings.TrimSpace(req.NewTaskName)
	// Idempotent recovery is opt-in via an explicit Idempotency-Key. We do NOT
	// infer idempotency from (source, afterSeq, requester) alone, because that
	// tuple omits the divergent overrides (prompt/agentRef/workspace) — forking
	// one checkpoint into several distinct branches is the primary use case, so
	// the default must mint a unique name. With a key, the caller is explicitly
	// asserting "same logical fork", so retries collapse onto one object.
	idempotent := newName == "" && idempotencyKey != ""
	if newName == "" {
		if idempotent {
			newName = deterministicForkTaskName(sourceName, afterSeq, forkRequesterIdentity(c), idempotencyKey)
		} else {
			newName = generatedForkTaskName(sourceName)
		}
	}

	spec := *source.Spec.DeepCopy()
	applyForkRequestOverrides(&spec, req)
	if err := applyForkContextToSpec(&spec, forkCtx); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode fork context: %v", err))
	}

	forked := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: namespace,
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(sourceName),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName:                sourceName,
				labels.AnnotationForkSourceTask:                sourceName,
				labels.AnnotationForkSourceSeq:                 strconv.FormatInt(afterSeq, 10),
				labels.AnnotationForkContextTruncated:          strconv.FormatBool(forkCtx.Truncated),
				labels.AnnotationDisableCoordinationToolInject: strconv.FormatBool(true),
			},
		},
		Spec: spec,
	}
	stampTaskRequesterFromUserInfo(forked, GetUserInfo(c))
	tracing.StampTaskTraceContext(c.Context(), forked)
	if err := h.authorizeContextTokenTaskCreate(c, createTaskRequestFromTask(forked), namespace); err != nil {
		return err
	}
	if err := h.authorizeTaskCreate(c.Context(), c, forked); err != nil {
		return err
	}

	sourceSessionName, forkedSessionName, err := h.resolveForkSessionNames(c.Context(), namespace, source, forked)
	if err != nil {
		return err
	}

	// Reject oversize objects BEFORE any write. The apiserver/etcd object-size
	// limit is enforced only on the real persisted write (a DryRunAll create does
	// not catch it), so guard with an explicit serialized-size budget here and
	// surface 413 rather than a generic 500 with an orphaned request event.
	if size := forkedTaskSerializedSize(forked); size > maxForkedTaskSerializedBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge,
			fmt.Sprintf("forked task is too large (%d bytes > %d limit); fork from an earlier checkpoint or shorten the prompt", size, maxForkedTaskSerializedBytes))
	}

	// Create the Task FIRST (it is the authoritative object). Only after a
	// confirmed-successful create do we append fork timeline events, so a failed
	// create can never orphan a TaskForkRequested/Created event (invariant 1 & 4).
	if err := h.client.Create(c.Context(), forked); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			// Idempotent recovery only when the caller opted in with an
			// Idempotency-Key: the existing object is the same logical fork (same
			// source + checkpoint), so return it instead of creating a duplicate.
			// Without a key (default unique auto-name, or an explicit user-supplied
			// name), a collision is a genuine conflict — we never silently alias a
			// divergent fork onto a pre-existing Task whose spec differs.
			if idempotent {
				if existing, ok := h.matchingExistingFork(c.Context(), namespace, newName, sourceName, afterSeq); ok {
					return c.Status(fiber.StatusOK).JSON(ForkTaskResponse{Namespace: namespace, SourceTaskName: sourceName, NewTaskName: existing.Name, AfterSeq: afterSeq, ForkContext: forkCtx})
				}
			}
			return fiber.NewError(fiber.StatusConflict, "forked task already exists")
		case apierrors.IsRequestEntityTooLargeError(err):
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "forked task is too large; fork from an earlier checkpoint or shorten the prompt")
		default:
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create forked task: %v", err))
		}
	}

	// Timeline events are best-effort AFTER the authoritative create. A failure
	// here must not fail the fork (the Task already exists and is self-describing
	// via its parent labels/annotations); log and continue (invariant 1).
	h.appendForkTimelineEvents(c.Context(), namespace, sourceName, newName, afterSeq, sourceSessionName, forkedSessionName)

	return c.Status(fiber.StatusCreated).JSON(ForkTaskResponse{Namespace: namespace, SourceTaskName: sourceName, NewTaskName: newName, AfterSeq: afterSeq, ForkContext: forkCtx})
}

// maxForkedTaskSerializedBytes bounds the serialized forked Task well under the
// kube-apiserver/etcd ~1.5 MiB object-size limit, leaving headroom for
// server-side managed fields and status.
const maxForkedTaskSerializedBytes = 1024 * 1024

func forkedTaskSerializedSize(task *corev1alpha1.Task) int {
	if task == nil {
		return 0
	}
	data, err := json.Marshal(task)
	if err != nil {
		// Treat an unmarshalable object as oversize (fail-closed).
		return maxForkedTaskSerializedBytes + 1
	}
	return len(data)
}

// forkRequesterIdentity returns a stable identifier for the caller used to scope
// deterministic fork names, so different users forking the same checkpoint do not
// collide. Falls back to empty when unauthenticated.
func forkRequesterIdentity(c fiber.Ctx) string {
	if info := GetUserInfo(c); info != nil {
		if u := strings.TrimSpace(info.Username); u != "" {
			return u
		}
	}
	return ""
}

const (
	sessionForkProvenanceRole   = "system"
	sessionForkProvenanceName   = "orka.sessionForkProvenance"
	sessionForkProvenancePrefix = "Fork context through session execution event checkpoint:"
)

// deterministicForkTaskName derives a stable fork name so retries of the same
// logical fork (identified by an explicit Idempotency-Key) resolve to the same
// object. Used only on the opt-in idempotent path.
func (h *Handlers) recoverSessionForkResponse(c fiber.Ctx, namespace, sourceName, newSessionName, idempotencyKey, expectedRequestDigest string, req ForkSessionRequest) (*ForkSessionResponse, error) {
	ctx := c.Context()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		existing, err := h.sessionStore.GetSession(ctx, namespace, newSessionName)
		if err != nil {
			lastErr = err
		} else if recoveredAfterSeq, recoveredSeedTaskName, recoveredRequestDigest, _, recoveredCtx, ok := sessionForkProvenance(existing, namespace, sourceName); ok {
			if expectedRequestDigest != "" && recoveredRequestDigest != expectedRequestDigest {
				return nil, fiber.NewError(fiber.StatusConflict, "idempotency key was already used for a different session fork request")
			}
			trustedSeedTaskName := expectedSessionForkSeedTaskName(sourceName, recoveredAfterSeq, idempotencyKey, req)
			if trustedSeedTaskName == "" {
				if recoveredSeedTaskName != "" {
					return nil, fiber.NewError(fiber.StatusConflict, "idempotent fork seed task provenance does not match request")
				}
				return &ForkSessionResponse{Namespace: namespace, SourceSessionName: sourceName, NewSessionName: newSessionName, AfterSeq: recoveredAfterSeq, ForkContext: recoveredCtx}, nil
			}
			if recoveredSeedTaskName != "" && recoveredSeedTaskName != trustedSeedTaskName {
				return nil, fiber.NewError(fiber.StatusConflict, "idempotent fork seed task provenance does not match request")
			}
			recoveredSeedTaskName = trustedSeedTaskName
			if !h.existingSessionForkSeedTaskMatches(ctx, namespace, sourceName, newSessionName, recoveredAfterSeq, recoveredRequestDigest, recoveredSeedTaskName, nil) {
				probe := &corev1alpha1.Task{}
				probeErr := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: recoveredSeedTaskName}, probe)
				if probeErr == nil {
					return nil, fiber.NewError(fiber.StatusConflict, "idempotent fork seed task already exists with different provenance")
				}
				lastErr = probeErr
			} else {
				return &ForkSessionResponse{Namespace: namespace, SourceSessionName: sourceName, NewSessionName: newSessionName, AfterSeq: recoveredAfterSeq, SeedTaskName: recoveredSeedTaskName, ForkContext: recoveredCtx}, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if lastErr != nil && !errors.Is(lastErr, store.ErrNotFound) && !apierrors.IsNotFound(lastErr) {
				return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to recover idempotent fork session: %v", lastErr))
			}
			return nil, fiber.NewError(fiber.StatusConflict, "idempotent fork session already exists without matching provenance")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *Handlers) findSessionForkNameByIdempotencyKey(ctx context.Context, namespace, sourceName, idempotencyKey string) (string, error) {
	expectedDigest := sessionForkIdempotencyKeyDigest(idempotencyKey)
	if h == nil || h.sessionStore == nil || expectedDigest == "" {
		return "", nil
	}
	sessions, err := h.sessionStore.ListSessions(ctx, namespace)
	if err != nil {
		return "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list fork sessions: %v", err))
	}
	for _, session := range sessions {
		record, err := h.sessionStore.GetSession(ctx, namespace, session.Name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to inspect fork session: %v", err))
		}
		_, _, _, keyDigest, _, ok := sessionForkProvenance(record, namespace, sourceName)
		if ok && keyDigest == expectedDigest {
			return session.Name, nil
		}
	}
	return "", nil
}

func expectedSessionForkSeedTaskName(sourceName string, afterSeq int64, idempotencyKey string, req ForkSessionRequest) string {
	if seedTaskName := strings.TrimSpace(req.SeedTaskName); seedTaskName != "" {
		return seedTaskName
	}
	if !shouldCreateSessionForkSeedTask(req) {
		return ""
	}
	return deterministicSessionForkSeedTaskName(sourceName, afterSeq, "", strings.TrimSpace(idempotencyKey)+":seed")
}

func sessionForkProvenance(session *store.SessionRecord, namespace, sourceName string) (int64, string, string, string, forkcontext.Context, bool) {
	if session == nil {
		return 0, "", "", "", forkcontext.Context{}, false
	}
	for _, message := range session.Messages {
		if message.Role != sessionForkProvenanceRole || message.Name != sessionForkProvenanceName {
			continue
		}
		_, raw, ok := strings.Cut(message.Content, sessionForkProvenancePrefix+"\n")
		if !ok {
			continue
		}
		var payload struct {
			SourceSessionName    string                     `json:"sourceSessionName"`
			AfterSeq             int64                      `json:"afterSeq"`
			SeedTaskName         string                     `json:"seedTaskName"`
			RequestDigest        string                     `json:"requestDigest"`
			IdempotencyKeyDigest string                     `json:"idempotencyKeyDigest"`
			Truncated            bool                       `json:"truncated"`
			Events               []forkcontext.EventSummary `json:"events"`
		}
		if json.Unmarshal([]byte(raw), &payload) != nil || payload.SourceSessionName != sourceName {
			continue
		}
		return payload.AfterSeq, strings.TrimSpace(payload.SeedTaskName), strings.TrimSpace(payload.RequestDigest), strings.TrimSpace(payload.IdempotencyKeyDigest), forkcontext.Context{
			SourceNamespace: namespace,
			SourceSession:   sourceName,
			AfterSeq:        payload.AfterSeq,
			Events:          payload.Events,
			Truncated:       payload.Truncated,
		}, true
	}
	return 0, "", "", "", forkcontext.Context{}, false
}

func sessionForkIdempotencyKeyDigest(idempotencyKey string) string {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(idempotencyKey))
	return hex.EncodeToString(sum[:])
}

func sessionForkRequestDigest(req ForkSessionRequest, afterSeq int64) string {
	body := map[string]any{
		"prompt":         req.Prompt,
		"seedTaskName":   strings.TrimSpace(req.SeedTaskName),
		"newSessionName": strings.TrimSpace(req.NewSessionName),
		"agentRef":       req.AgentRef,
	}
	if req.AfterSeq != nil {
		body["afterSeq"] = afterSeq
	}
	data, _ := json.Marshal(body)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func deterministicForkSessionName(sourceName string, afterSeq int64, requester, idempotencyKey string) string {
	seed := idempotencyKey
	if seed == "" {
		seed = fmt.Sprintf("%s\x00%d\x00%s", sourceName, afterSeq, requester)
	}
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%s-fork-%s", forkcontext.SanitizeTaskNamePrefix(sourceName), hex.EncodeToString(sum[:8]))
}

func deterministicForkTaskName(sourceName string, afterSeq int64, requester, idempotencyKey string) string {
	seed := idempotencyKey
	if seed == "" {
		seed = fmt.Sprintf("%s\x00%d\x00%s", sourceName, afterSeq, requester)
	}
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%s-fork-%s", forkcontext.SanitizeTaskNamePrefix(sourceName), hex.EncodeToString(sum[:4]))
}

func deterministicSessionForkSeedTaskName(newSessionName string, afterSeq int64, requester, idempotencyKey string) string {
	seed := idempotencyKey
	if seed == "" {
		seed = fmt.Sprintf("%s\x00%d\x00%s", newSessionName, afterSeq, requester)
	}
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%s-fork-%s", forkcontext.SanitizeTaskNamePrefix(newSessionName), hex.EncodeToString(sum[:8]))
}

// generatedForkTaskName mints a unique fork name (random suffix). This is the
// default for auto-named forks so that forking one checkpoint into several
// distinct branches always creates distinct Tasks; it never aliases divergent
// forks. Retry-safety for the same logical fork is opt-in via an Idempotency-Key
// (see deterministicForkTaskName); the create-before-events ordering already
// removes the transient-failure retry that previously caused duplicates.
func generatedForkTaskName(sourceName string) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Sprintf("%s-fork-%d", forkcontext.SanitizeTaskNamePrefix(sourceName), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-fork-%s", forkcontext.SanitizeTaskNamePrefix(sourceName), hex.EncodeToString(suffix[:]))
}

// matchingExistingFork returns the existing Task at newName if it is the same
// logical fork (same source task and checkpoint seq), enabling idempotent
// recovery on retry.
func (h *Handlers) matchingExistingFork(ctx context.Context, namespace, newName, sourceName string, afterSeq int64) (*corev1alpha1.Task, bool) {
	existing := &corev1alpha1.Task{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: newName}, existing); err != nil {
		return nil, false
	}
	if existing.Annotations[labels.AnnotationForkSourceTask] != sourceName {
		return nil, false
	}
	if existing.Annotations[labels.AnnotationForkSourceSeq] != strconv.FormatInt(afterSeq, 10) {
		return nil, false
	}
	return existing, true
}

// appendForkTimelineEvents records the fork request + created events on the
// source and forked streams. It is best-effort: the authoritative Task already
// exists, so an append failure is logged but does not fail the request.
func (h *Handlers) appendForkTimelineEvents(ctx context.Context, namespace, sourceName, newName string, afterSeq int64, sourceSessionName, forkedSessionName string) {
	requestContent, err := marshalForkEventContent(sourceName, newName, afterSeq, "request")
	if err != nil {
		log.Info("fork: failed to encode request event content", "error", err, "task", sourceName)
		return
	}
	if _, err := h.executionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   namespace,
		StreamType:  events.ExecutionEventStreamTypeTask,
		StreamID:    sourceName,
		TaskName:    sourceName,
		SessionName: sourceSessionName,
		Type:        events.ExecutionEventTypeTaskForkRequested,
		Severity:    events.ExecutionEventSeverityInfo,
		Summary:     "task fork requested",
		Content:     requestContent,
	}); err != nil {
		log.Info("fork: failed to append request event", "error", err, "task", sourceName)
	}

	createdContent, err := marshalForkEventContent(sourceName, newName, afterSeq, "created")
	if err != nil {
		log.Info("fork: failed to encode created event content", "error", err, "task", newName)
		return
	}
	for _, event := range []struct {
		streamID    string
		sessionName string
	}{
		{streamID: sourceName, sessionName: sourceSessionName},
		{streamID: newName, sessionName: forkedSessionName},
	} {
		if _, err := h.executionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace:   namespace,
			StreamType:  events.ExecutionEventStreamTypeTask,
			StreamID:    event.streamID,
			TaskName:    event.streamID,
			SessionName: event.sessionName,
			Type:        events.ExecutionEventTypeTaskForkCreated,
			Severity:    events.ExecutionEventSeverityInfo,
			Summary:     "task fork created",
			Content:     createdContent,
		}); err != nil {
			log.Info("fork: failed to append created event", "error", err, "stream", event.streamID)
		}
	}
}

func applyForkRequestOverrides(spec *corev1alpha1.TaskSpec, req ForkTaskRequest) {
	if spec == nil {
		return
	}
	spec.RequestedBy = nil
	spec.Transaction = nil
	if req.AgentRef != nil {
		spec.AgentRef = req.AgentRef
	}
	if prompt := strings.TrimSpace(req.Prompt); prompt != "" {
		spec.Prompt = prompt
		if spec.AI != nil {
			spec.AI.Prompt = prompt
		}
	}
	if req.Workspace != nil {
		spec.Workspace = req.Workspace
	}
	clearForkSchedule(spec)
}

func clearForkSchedule(spec *corev1alpha1.TaskSpec) {
	if spec == nil {
		return
	}
	spec.Schedule = ""
	spec.TimeZone = nil
	spec.ConcurrencyPolicy = ""
	spec.StartingDeadlineSeconds = nil
	spec.SuccessfulRunsHistoryLimit = nil
	spec.FailedRunsHistoryLimit = nil
	spec.Suspend = nil
}

func applyForkContextToSpec(spec *corev1alpha1.TaskSpec, forkCtx forkcontext.Context) error {
	if spec == nil || len(forkCtx.Events) == 0 {
		return nil
	}
	encoded, err := json.Marshal(forkCtx)
	if err != nil {
		return err
	}
	contextBlock := "Fork context through execution event checkpoint:\n" + string(encoded)
	if spec.AI != nil {
		spec.AI.Prompt = joinForkContextPrompt(contextBlock, spec.AI.Prompt)
		if spec.Type == corev1alpha1.TaskTypeAI && strings.TrimSpace(spec.Prompt) == "" {
			return nil
		}
	}
	if spec.Type == corev1alpha1.TaskTypeAgent || strings.TrimSpace(spec.Prompt) != "" {
		spec.Prompt = joinForkContextPrompt(contextBlock, spec.Prompt)
	}
	return nil
}

func joinForkContextPrompt(contextBlock, prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return contextBlock
	}
	return contextBlock + "\n\nUser continuation prompt:\n" + prompt
}

func (h *Handlers) resolveForkSessionNames(
	ctx context.Context,
	namespace string,
	source *corev1alpha1.Task,
	forked *corev1alpha1.Task,
) (string, string, error) {
	sourceSessionName, err := h.existingSessionNameForTask(ctx, namespace, source)
	if err != nil {
		return "", "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get source session: %v", err))
	}
	if sessionNameForTask(source) != "" && sourceSessionName == "" {
		forked.Spec.SessionRef = nil
	}
	forkedSessionName, err := h.existingSessionNameForTask(ctx, namespace, forked)
	if err != nil {
		return "", "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get forked session: %v", err))
	}
	return sourceSessionName, forkedSessionName, nil
}

func marshalForkEventContent(sourceName, newName string, afterSeq int64, eventKind string) (json.RawMessage, error) {
	content, err := json.Marshal(map[string]any{"sourceTaskName": sourceName, "newTaskName": newName, "afterSeq": afterSeq})
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode fork %s event: %v", eventKind, err))
	}
	return content, nil
}

// ForkSession handles POST /api/v1/sessions/{id}/fork.
func (h *Handlers) ForkSession(c fiber.Ctx) error {
	sourceName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "forkSession", h.contextTokenAuthorization.SessionReadScopes); err != nil {
		return err
	}
	if err := h.ensureSessionReadable(c, namespace, sourceName); err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "forkSessionWrite", h.contextTokenAuthorization.SessionWriteScopes); err != nil {
		return err
	}
	if h.executionEventStore == nil || h.sessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session fork storage not enabled")
	}

	sourceSessionType, err := h.getSourceSessionType(c.Context(), namespace, sourceName)
	if err != nil {
		return err
	}

	var req ForkSessionRequest
	if len(c.Body()) > 0 {
		if err := c.Bind().JSON(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}

	_, latestSeq, err := h.executionEventStore.ListSessionExecutionEvents(c.Context(), store.SessionExecutionEventFilter{
		Namespace:   namespace,
		SessionName: sourceName,
		Limit:       1,
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get latest session event sequence: %v", err))
	}
	afterSeq := latestSeq
	if req.AfterSeq != nil {
		afterSeq = *req.AfterSeq
	}
	if afterSeq < 0 || afterSeq > latestSeq {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be 0, latest, or an existing session event sequence")
	}
	validAfterSeq, err := sessionEventSeqExists(c.Context(), h.executionEventStore, namespace, sourceName, afterSeq, latestSeq)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to validate session event sequence: %v", err))
	}
	if !validAfterSeq {
		return fiber.NewError(fiber.StatusBadRequest, "afterSeq must be 0, latest, or an existing session event sequence")
	}
	sessionEvents, err := listSessionEventsThrough(c.Context(), h.executionEventStore, namespace, sourceName, afterSeq)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list session execution events: %v", err))
	}
	forkCtx := forkcontext.BuildSessionContext(namespace, sourceName, afterSeq, sessionEvents, forkcontext.DefaultMaxEvents)

	newSessionName, seedTaskName, recovered, err := h.prepareSessionForkNames(c, namespace, sourceName, afterSeq, req)
	if err != nil {
		return err
	}
	if recovered != nil {
		return c.Status(fiber.StatusOK).JSON(recovered)
	}
	seedTask, err := h.prepareSessionForkSeedTask(c, namespace, sourceName, newSessionName, seedTaskName, afterSeq, req, forkCtx)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	if err := h.sessionStore.CreateSession(c.Context(), &store.SessionRecord{
		Namespace:   namespace,
		Name:        newSessionName,
		SessionType: sourceSessionType,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		if isIdempotentSessionFork(c, req) {
			if recovered, recoverErr := h.recoverSessionForkResponse(c, namespace, sourceName, newSessionName, strings.TrimSpace(c.Get("Idempotency-Key")), sessionForkRequestDigest(req, afterSeq), req); recoverErr == nil {
				return c.Status(fiber.StatusOK).JSON(recovered)
			} else {
				return recoverErr
			}
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create forked session: %v", err))
	}
	if err := h.appendSessionForkProvenanceOrCleanup(c.Context(), namespace, newSessionName, sourceName, seedTaskName, sessionForkRequestDigest(req, afterSeq), strings.TrimSpace(c.Get("Idempotency-Key")), afterSeq, forkCtx); err != nil {
		return err
	}
	if recovered, err := h.createSessionForkSeedTask(c, namespace, sourceName, newSessionName, afterSeq, req, seedTask); recovered != nil || err != nil {
		if recovered != nil {
			return c.Status(fiber.StatusOK).JSON(recovered)
		}
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(ForkSessionResponse{
		Namespace:         namespace,
		SourceSessionName: sourceName,
		NewSessionName:    newSessionName,
		SeedTaskName:      seedTaskName,
		AfterSeq:          afterSeq,
		ForkContext:       forkCtx,
	})
}

func (h *Handlers) prepareSessionForkSeedTask(
	c fiber.Ctx,
	namespace string,
	sourceName string,
	newSessionName string,
	seedTaskName string,
	afterSeq int64,
	req ForkSessionRequest,
	forkCtx forkcontext.Context,
) (*corev1alpha1.Task, error) {
	if !shouldCreateSessionForkSeedTask(req) {
		return nil, nil
	}
	seedTaskType, err := h.sessionForkSeedTaskType(c.Context(), namespace, req.AgentRef)
	if err != nil {
		return nil, err
	}
	seedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      seedTaskName,
			Namespace: namespace,
			Annotations: map[string]string{
				labels.AnnotationForkSourceSession:        sourceName,
				labels.AnnotationForkSourceSessionSeq:     strconv.FormatInt(afterSeq, 10),
				labels.AnnotationForkContextTruncated:     strconv.FormatBool(forkCtx.Truncated),
				labels.AnnotationSessionForkRequestDigest: sessionForkRequestDigest(req, afterSeq),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:       seedTaskType,
			Prompt:     sessionForkPrompt(forkCtx, req.Prompt),
			AgentRef:   req.AgentRef,
			SessionRef: &corev1alpha1.SessionReference{Name: newSessionName, Append: true},
		},
	}
	stampTaskRequesterFromUserInfo(seedTask, GetUserInfo(c))
	if err := h.authorizeContextTokenTaskCreate(c, createTaskRequestFromTask(seedTask), namespace); err != nil {
		return nil, err
	}
	if err := h.authorizeTaskCreate(c.Context(), c, seedTask); err != nil {
		return nil, err
	}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: seedTaskName, Namespace: namespace}, &corev1alpha1.Task{}); err == nil {
		if isIdempotentSessionFork(c, req) && h.existingSessionForkSeedTaskMatches(c.Context(), namespace, sourceName, newSessionName, afterSeq, sessionForkRequestDigest(req, afterSeq), seedTaskName, seedTask) {
			return nil, nil
		}
		return nil, fiber.NewError(fiber.StatusConflict, "seed task already exists")
	} else if !apierrors.IsNotFound(err) {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to check seed task: %v", err))
	}
	return seedTask, nil
}

func (h *Handlers) appendSessionForkProvenanceOrCleanup(
	ctx context.Context,
	namespace string,
	newSessionName string,
	sourceName string,
	seedTaskName string,
	requestDigest string,
	idempotencyKey string,
	afterSeq int64,
	forkCtx forkcontext.Context,
) error {
	if err := appendSessionForkProvenance(ctx, h.sessionStore, namespace, newSessionName, sourceName, seedTaskName, requestDigest, idempotencyKey, afterSeq, forkCtx); err != nil {
		_ = h.sessionStore.DeleteSession(context.Background(), namespace, newSessionName)
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to write fork provenance: %v", err))
	}
	return nil
}

func (h *Handlers) createSessionForkSeedTask(
	c fiber.Ctx,
	namespace string,
	sourceName string,
	newSessionName string,
	afterSeq int64,
	req ForkSessionRequest,
	seedTask *corev1alpha1.Task,
) (*ForkSessionResponse, error) {
	if seedTask == nil {
		return nil, nil
	}
	if err := h.client.Create(c.Context(), seedTask); err == nil {
		return nil, nil
	} else {
		if isIdempotentSessionFork(c, req) && apierrors.IsAlreadyExists(err) && h.existingSessionForkSeedTaskMatches(c.Context(), namespace, sourceName, newSessionName, afterSeq, sessionForkRequestDigest(req, afterSeq), seedTask.Name, seedTask) {
			if recovered, recoverErr := h.recoverSessionForkResponse(c, namespace, sourceName, newSessionName, strings.TrimSpace(c.Get("Idempotency-Key")), sessionForkRequestDigest(req, afterSeq), req); recoverErr == nil {
				return recovered, nil
			}
		}
		_ = h.sessionStore.DeleteSession(context.Background(), namespace, newSessionName)
		if apierrors.IsAlreadyExists(err) {
			return nil, fiber.NewError(fiber.StatusConflict, "seed task already exists")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create seed task: %v", err))
	}
}

func (h *Handlers) existingSessionForkSeedTaskMatches(ctx context.Context, namespace, sourceName, newSessionName string, afterSeq int64, requestDigest, seedTaskName string, expected *corev1alpha1.Task) bool {
	if h == nil || h.client == nil || strings.TrimSpace(seedTaskName) == "" {
		return false
	}
	existing := &corev1alpha1.Task{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: seedTaskName}, existing); err != nil {
		return false
	}
	matches := existing.Spec.SessionRef != nil &&
		existing.Spec.SessionRef.Name == newSessionName &&
		existing.Annotations[labels.AnnotationForkSourceSession] == sourceName &&
		existing.Annotations[labels.AnnotationForkSourceSessionSeq] == strconv.FormatInt(afterSeq, 10) &&
		existing.Annotations[labels.AnnotationSessionForkRequestDigest] == requestDigest
	if !matches || expected == nil {
		return matches
	}
	return existing.Spec.Type == expected.Spec.Type &&
		existing.Spec.Prompt == expected.Spec.Prompt &&
		reflect.DeepEqual(existing.Spec.AgentRef, expected.Spec.AgentRef) &&
		reflect.DeepEqual(existing.Spec.SessionRef, expected.Spec.SessionRef)
}

func (h *Handlers) getSourceSessionType(ctx context.Context, namespace, sourceName string) (string, error) {
	sourceSession, err := h.sessionStore.GetSession(ctx, namespace, sourceName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fiber.NewError(fiber.StatusNotFound, "source session not found")
		}
		return "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get source session: %v", err))
	}
	sessionType := strings.TrimSpace(sourceSession.SessionType)
	if sessionType == "" {
		sessionType = "task"
	}
	return sessionType, nil
}

func taskEventSeqExists(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	taskName string,
	seq,
	latestSeq int64,
) (bool, error) {
	return newTaskTimelineReader(eventStore, namespace, taskName).seqExists(ctx, seq, latestSeq)
}

func listTaskEventsThrough(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	taskName string,
	throughSeq int64,
) ([]store.ExecutionEvent, error) {
	return newTaskTimelineReader(eventStore, namespace, taskName).listRecentThrough(ctx, throughSeq, forkcontext.DefaultMaxEvents+1)
}
func sessionEventSeqExists(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	sessionName string,
	seq,
	latestSeq int64,
) (bool, error) {
	if seq == 0 || seq == latestSeq {
		return true, nil
	}
	if seq < 0 || seq > latestSeq {
		return false, nil
	}
	listed, _, err := eventStore.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
		Namespace:   namespace,
		SessionName: sessionName,
		AfterSeq:    seq - 1,
		Limit:       1,
	})
	if err != nil {
		return false, err
	}
	return len(listed) == 1 && listed[0].SessionSeq == seq, nil
}

func listSessionEventsThrough(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	sessionName string,
	throughSeq int64,
) ([]store.SessionExecutionEvent, error) {
	if throughSeq == 0 {
		return nil, nil
	}
	readLimit := forkcontext.DefaultMaxEvents + 1
	windowEnd := throughSeq
	out := make([]store.SessionExecutionEvent, 0, readLimit)
	for windowEnd > 0 && len(out) < readLimit {
		after := max(windowEnd-int64(store.MaxExecutionEventLimit), 0)
		batch, _, err := eventStore.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
			Namespace:   namespace,
			SessionName: sessionName,
			AfterSeq:    after,
			Limit:       store.MaxExecutionEventLimit,
		})
		if err != nil {
			return nil, err
		}
		windowEvents := make([]store.SessionExecutionEvent, 0, len(batch))
		for _, event := range batch {
			if event.SessionSeq > windowEnd || event.SessionSeq > throughSeq {
				break
			}
			windowEvents = append(windowEvents, event)
		}
		if len(windowEvents) > 0 {
			combined := make([]store.SessionExecutionEvent, 0, len(windowEvents)+len(out))
			combined = append(combined, windowEvents...)
			combined = append(combined, out...)
			if len(combined) > readLimit {
				combined = combined[len(combined)-readLimit:]
			}
			out = combined
		}
		if after == 0 {
			break
		}
		windowEnd = after
	}
	return out, nil
}

func appendSessionForkProvenance(
	ctx context.Context,
	sessionStore store.SessionStore,
	namespace,
	newSessionName,
	sourceSessionName string,
	seedTaskName string,
	requestDigest string,
	idempotencyKey string,
	afterSeq int64,
	forkCtx forkcontext.Context,
) error {
	content, err := json.Marshal(map[string]any{
		"sourceSessionName":    sourceSessionName,
		"afterSeq":             afterSeq,
		"seedTaskName":         seedTaskName,
		"requestDigest":        requestDigest,
		"idempotencyKeyDigest": sessionForkIdempotencyKeyDigest(idempotencyKey),
		"truncated":            forkCtx.Truncated,
		"events":               forkCtx.Events,
		"note":                 "logical session fork only; workspace snapshot fork is not included",
	})
	if err != nil {
		return err
	}
	return sessionStore.AppendMessages(ctx, namespace, newSessionName, []store.SessionMessage{{
		Role:      sessionForkProvenanceRole,
		Name:      sessionForkProvenanceName,
		Content:   sessionForkProvenancePrefix + "\n" + string(content),
		Timestamp: time.Now().UTC(),
	}})
}

func sessionForkPrompt(forkCtx forkcontext.Context, prompt string) string {
	encoded, err := json.Marshal(forkCtx)
	contextBlock := "Fork context through session execution event checkpoint:"
	if err == nil {
		contextBlock += "\n" + string(encoded)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return contextBlock
	}
	return contextBlock + "\n\nUser continuation prompt:\n" + prompt
}

func shouldCreateSessionForkSeedTask(req ForkSessionRequest) bool {
	return strings.TrimSpace(req.SeedTaskName) != "" || strings.TrimSpace(req.Prompt) != "" || req.AgentRef != nil
}

func isIdempotentSessionFork(c fiber.Ctx, _ ForkSessionRequest) bool {
	return strings.TrimSpace(c.Get("Idempotency-Key")) != ""
}

func (h *Handlers) prepareSessionForkNames(
	c fiber.Ctx,
	namespace string,
	sourceName string,
	afterSeq int64,
	req ForkSessionRequest,
) (newSessionName string, seedTaskName string, recovered *ForkSessionResponse, err error) {
	idempotencyKey := strings.TrimSpace(c.Get("Idempotency-Key"))
	expectedDigest := sessionForkRequestDigest(req, afterSeq)
	newSessionName = strings.TrimSpace(req.NewSessionName)
	idempotent := idempotencyKey != ""
	if newSessionName == "" {
		if idempotent {
			newSessionName = deterministicForkSessionName(sourceName, afterSeq, forkRequesterIdentity(c), idempotencyKey)
		} else {
			newSessionName = generatedForkSessionName(sourceName)
		}
	}
	seedTaskName = strings.TrimSpace(req.SeedTaskName)
	if shouldCreateSessionForkSeedTask(req) && seedTaskName == "" {
		if idempotent {
			seedTaskName = deterministicSessionForkSeedTaskName(sourceName, afterSeq, forkRequesterIdentity(c), idempotencyKey+":seed")
		} else {
			seedTaskName = generatedForkTaskName(newSessionName)
		}
	}
	if idempotent {
		matchedName, matchErr := h.findSessionForkNameByIdempotencyKey(c.Context(), namespace, sourceName, idempotencyKey)
		if matchErr != nil {
			return "", "", nil, matchErr
		}
		if matchedName != "" && matchedName != newSessionName {
			recovered, recoverErr := h.recoverSessionForkResponse(c, namespace, sourceName, matchedName, idempotencyKey, expectedDigest, req)
			if recoverErr != nil {
				return "", "", nil, recoverErr
			}
			return matchedName, recovered.SeedTaskName, recovered, nil
		}
	}
	if _, err := h.sessionStore.GetSession(c.Context(), namespace, newSessionName); err == nil {
		if idempotent {
			recovered, recoverErr := h.recoverSessionForkResponse(c, namespace, sourceName, newSessionName, idempotencyKey, expectedDigest, req)
			if recoverErr != nil {
				return "", "", nil, recoverErr
			}
			return newSessionName, recovered.SeedTaskName, recovered, nil
		}
		return "", "", nil, fiber.NewError(fiber.StatusConflict, "forked session already exists")
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", "", nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to check forked session: %v", err))
	}
	return newSessionName, seedTaskName, nil, nil
}

func (h *Handlers) sessionForkSeedTaskType(ctx context.Context, namespace string, agentRef *corev1alpha1.AgentReference) (corev1alpha1.TaskType, error) {
	if h == nil || h.client == nil || agentRef == nil || strings.TrimSpace(agentRef.Name) == "" {
		return corev1alpha1.TaskTypeAI, nil
	}
	agentNamespace := strings.TrimSpace(agentRef.Namespace)
	if agentNamespace != "" && agentNamespace != namespace {
		return "", fiber.NewError(fiber.StatusForbidden, "agentRef namespace must match fork namespace")
	}
	if agentNamespace == "" {
		agentNamespace = namespace
	}
	agent := &corev1alpha1.Agent{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: agentNamespace, Name: strings.TrimSpace(agentRef.Name)}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return corev1alpha1.TaskTypeAI, nil
		}
		return "", fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent for session fork seed task: %v", err))
	}
	if agent.Spec.Runtime != nil {
		return corev1alpha1.TaskTypeAgent, nil
	}
	return corev1alpha1.TaskTypeAI, nil
}

func generatedForkSessionName(sourceName string) string {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Sprintf("%s-fork-%d", forkcontext.SanitizeTaskNamePrefix(sourceName), time.Now().Unix())
	}
	return fmt.Sprintf("%s-fork-%s", forkcontext.SanitizeTaskNamePrefix(sourceName), hex.EncodeToString(suffix[:]))
}
