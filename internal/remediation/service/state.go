package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/source"
)

const (
	runLabel               = "remediation.orka.ai/run"
	stateLabel             = "remediation.orka.ai/state"
	activeLabel            = "remediation.orka.ai/active"
	roleLabel              = "remediation.orka.ai/role"
	operationLabel         = "remediation.orka.ai/operation"
	digestAnnotation       = "remediation.orka.ai/input-digest"
	stateVersion           = 1
	maxInputBytes          = 768 << 10
	maxStateBytes          = 128 << 10
	maxBlobBytes           = 4 << 20
	blobPartBytes          = 256 << 10
	stepSource             = "source"
	stepCompare            = "compare"
	parentRole             = "parent"
	reasonCancelled        = "cancelled"
	reasonDeadlineExceeded = "deadline-exceeded"
	reasonIdentityConflict = "identity-conflict"
	boolTrue               = "true"
	boolFalse              = "false"
)

type input struct {
	ID                    string                 `json:"id"`
	Mode                  contract.Mode          `json:"mode"`
	Report                []byte                 `json:"report"`
	Repository            string                 `json:"repository"`
	Ref                   string                 `json:"ref"`
	SubjectDigest         string                 `json:"subjectDigest"`
	RequiredEvidenceScope contract.EvidenceScope `json:"requiredEvidenceScope,omitempty"`
}

type authority struct {
	BackendDigest      string                 `json:"backendDigest"`
	EvidenceScope      contract.EvidenceScope `json:"evidenceScope,omitempty"`
	AIWorkerImage      string                 `json:"aiWorkerImage"`
	BoundedExecution   bool                   `json:"boundedExecution,omitempty"`
	AgentName          string                 `json:"agentName"`
	AgentUID           string                 `json:"agentUID"`
	AgentGeneration    int64                  `json:"agentGeneration"`
	AgentDigest        string                 `json:"agentDigest"`
	ProviderName       string                 `json:"providerName,omitempty"`
	ProviderUID        string                 `json:"providerUID,omitempty"`
	ProviderGeneration int64                  `json:"providerGeneration,omitempty"`
	ProviderDigest     string                 `json:"providerDigest,omitempty"`
}

type blobPart struct {
	Name   string `json:"name"`
	UID    string `json:"uid"`
	Digest string `json:"digest"`
}

type blob struct {
	Digest string     `json:"digest"`
	Bytes  int        `json:"bytes"`
	Parts  []blobPart `json:"parts"`
}

func (b blob) empty() bool { return b.Digest == "" }

type taskOperation struct {
	Name      string             `json:"name"`
	UID       string             `json:"uid,omitempty"`
	Attempted bool               `json:"attempted"`
	Spec      blob               `json:"spec"`
	Output    blob               `json:"output"`
	Job       *nativeJobIdentity `json:"job,omitempty"`
	ModelCall *modelCallClaim    `json:"modelCall,omitempty"`
}

type modelCallClaim struct {
	JobUID string `json:"jobUID"`
	PodUID string `json:"podUID"`
}

type executionOperation struct {
	Request          blob                       `json:"request"`
	InputDigest      string                     `json:"inputDigest,omitempty"`
	Attempted        bool                       `json:"attempted"`
	AdmissionAttempt uint64                     `json:"admissionAttempt,omitempty"`
	Rejected         bool                       `json:"rejected,omitempty"`
	Settled          bool                       `json:"settled,omitempty"`
	Receipt          *contract.ExecutionReceipt `json:"receipt,omitempty"`
	Record           blob                       `json:"record"`
}

type completion struct {
	Phase   string `json:"phase"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

type state struct {
	Version         int                            `json:"version"`
	ID              string                         `json:"id"`
	Namespace       string                         `json:"namespace"`
	InputDigest     string                         `json:"inputDigest"`
	Authority       authority                      `json:"authority"`
	CreatedAt       time.Time                      `json:"createdAt"`
	Deadline        time.Time                      `json:"deadline"`
	Status          contract.Status                `json:"status"`
	Step            string                         `json:"step"`
	CancelRequested bool                           `json:"cancelRequested"`
	StopReason      string                         `json:"stopReason,omitempty"`
	Settled         bool                           `json:"settled"`
	Parent          taskOperation                  `json:"parent"`
	ParentDeleted   bool                           `json:"parentDeleted,omitempty"`
	Final           *completion                    `json:"final,omitempty"`
	Result          blob                           `json:"result"`
	ResultPublished bool                           `json:"resultPublished,omitempty"`
	Source          *source.Target                 `json:"source,omitempty"`
	Inventory       blob                           `json:"inventory"`
	Packet          blob                           `json:"packet"`
	Checks          blob                           `json:"checks"`
	Candidate       blob                           `json:"candidate"`
	Changes         []pv.DeclaredChange            `json:"changes,omitempty"`
	Baseline        blob                           `json:"baseline"`
	Comparison      blob                           `json:"comparison"`
	ReviewRound     int                            `json:"reviewRound"`
	Agents          map[string]*taskOperation      `json:"agents"`
	Executions      map[string]*executionOperation `json:"executions"`
}

type run struct {
	object *corev1.ConfigMap
	input  input
	state  state
}

func stateName(id string) string {
	return "rem-" + strings.TrimPrefix(pv.Digest([]byte(id)), "sha256:")[:40]
}

func (s *Coordinator) load(ctx context.Context, id string) (*run, error) {
	identity, fenced := ctx.Value(runIdentityContextKey{}).(runIdentity)
	if fenced && (identity.namespace != s.config.Namespace || identity.id != id || identity.uid == "") {
		return nil, errRunIdentityChanged
	}
	object := new(corev1.ConfigMap)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: stateName(id)}, object); err != nil {
		if apierrors.IsNotFound(err) {
			if fenced {
				return nil, errRunIdentityChanged
			}
			return nil, contract.ErrNotFound
		}
		return nil, contract.ErrUnavailable
	}
	if err := checkRunIdentity(ctx, object.Namespace, id, object.UID); err != nil {
		return nil, err
	}
	return decodeRun(object, id, s.config.Namespace)
}

func decodeRun(object *corev1.ConfigMap, id, namespace string) (*run, error) {
	r := &run{object: object}
	data := []byte(object.Data["state"])
	inputData := []byte(object.Data["input"])
	if object.DeletionTimestamp != nil || object.UID == "" || object.Name != stateName(id) ||
		object.Namespace != namespace || object.Labels[stateLabel] != "v1" ||
		len(data) > maxStateBytes || len(inputData) > maxInputBytes ||
		json.Unmarshal(data, &r.state) != nil || json.Unmarshal(inputData, &r.input) != nil {
		return nil, contract.ErrConflict
	}
	st := &r.state
	if !validRunStateIdentity(r, id, namespace, inputData) ||
		st.ReviewRound < 0 || st.ReviewRound > 1 || !validStatus(st.Status) || !validOperations(st) ||
		st.Status.EvidenceScope != st.Authority.EvidenceScope ||
		((r.input.RequiredEvidenceScope != "" || st.Authority.EvidenceScope != "") &&
			contract.RequireEvidenceScope(r.input.RequiredEvidenceScope, st.Authority.EvidenceScope) != nil) ||
		(st.Status.PatchReady && st.Candidate.empty()) ||
		(st.Step != stepSource && !contract.TerminalPhase(st.Status.Phase) && st.Source == nil) {
		return nil, contract.ErrConflict
	}
	if st.Authority.BoundedExecution &&
		(st.CreatedAt.IsZero() || !st.Deadline.After(st.CreatedAt) ||
			st.Deadline.Sub(st.CreatedAt) > contract.MaxRunDuration || len(st.Agents) > contract.MaxModelCalls) {
		return nil, contract.ErrConflict
	}
	return r, nil
}

func validRunStateIdentity(r *run, id, namespace string, input []byte) bool {
	st := &r.state
	return st.Version == stateVersion && st.ID == id && r.input.ID == id && st.Namespace == namespace &&
		st.Status.ID == id && st.Status.Namespace == namespace && st.InputDigest == pv.Digest(input) &&
		st.Agents != nil && st.Executions != nil && !st.Deadline.IsZero()
}

func validOperations(st *state) bool {
	for _, op := range st.Agents {
		if op == nil || (op.ModelCall != nil &&
			(!st.Authority.BoundedExecution || !op.Attempted || op.UID == "" || op.ModelCall.JobUID == "" || op.ModelCall.PodUID == "")) {
			return false
		}
	}
	for _, op := range st.Executions {
		if op == nil || ((op.Rejected || op.Settled) && (!op.Attempted || op.Receipt != nil || !op.Record.empty())) ||
			(op.Rejected && op.Settled) {
			return false
		}
	}
	return true
}

func validStatus(status contract.Status) bool {
	if status.EvidenceScope != "" && !status.EvidenceScope.Valid() {
		return false
	}
	if status.Outcome == contract.OutcomeVerified &&
		(status.Phase != contract.PhaseSucceeded || status.Stage != contract.StageComplete || !status.PatchReady) {
		return false
	}
	phases := []string{contract.PhasePending, contract.PhaseRunning, contract.PhaseSucceeded,
		contract.PhaseFailed, contract.PhaseCancelled, contract.PhaseTimedOut, contract.PhaseNeedsInput}
	stages := []string{contract.StageReproduce, contract.StageGenerate, contract.StageVerify, contract.StageComplete}
	outcomes := []string{"", contract.OutcomeReproduced, contract.OutcomeNotReproduced, contract.OutcomeUnableToValidate,
		contract.OutcomeUnableToVerify, contract.OutcomeUnverified, contract.OutcomeVerified, contract.OutcomeCancelled, contract.OutcomeTimedOut}
	reasons := []string{"accepted", "source-unavailable", "source-too-large", "agent-configuration-changed",
		"agent-task-failed", "agent-output-invalid", "checks-invalid", "environment-unsupported",
		"original-not-reproduced", "original-evidence-invalid", "original-setup-failed", "report-reproduced",
		"candidate-invalid", "candidate-unverified", "candidate-not-fixed", "candidate-regression",
		"validation-rejected", "validation-invalid", "verified", reasonCancelled, reasonDeadlineExceeded,
		"execution-unavailable", reasonIdentityConflict, "state-conflict", contract.ReasonEvidenceScopeUnsupported,
		"model-call-budget-exhausted"}
	return slices.Contains(phases, status.Phase) && slices.Contains(stages, status.Stage) &&
		slices.Contains(outcomes, status.Outcome) && slices.Contains(reasons, status.Reason)
}

func (s *Coordinator) save(ctx context.Context, r *run) error {
	if err := checkRunIdentity(ctx, r.state.Namespace, r.state.ID, r.object.UID); err != nil {
		return err
	}
	object, err := stateObject(r)
	if err != nil {
		return err
	}
	if err := s.config.Client.Update(ctx, object); err != nil {
		if apierrors.IsConflict(err) {
			return contract.ErrConflict
		}
		return contract.ErrUnavailable
	}
	r.object = object
	return nil
}

func stateObject(r *run) (*corev1.ConfigMap, error) {
	data, err := json.Marshal(r.state)
	if err != nil || len(data) > maxStateBytes {
		return nil, contract.ErrConflict
	}
	object := r.object.DeepCopy()
	object.Data["state"] = string(data)
	if object.Labels == nil {
		object.Labels = make(map[string]string)
	}
	object.Labels[activeLabel] = activeStateValue(&r.state)
	return object, nil
}

func (s *Coordinator) update(ctx context.Context, id string, change func(*run) error) (*run, error) {
	for range 5 {
		// Every retry rechecks the driver's UID fence, not just the first read.
		r, err := s.load(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := change(r); err != nil {
			return nil, err
		}
		if err := s.save(ctx, r); err != nil {
			if errors.Is(err, contract.ErrConflict) {
				continue
			}
			return nil, err
		}
		return r, nil
	}
	return nil, contract.ErrConflict
}

func (s *Coordinator) putJSON(ctx context.Context, r *run, value any) (blob, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return blob{}, contract.ErrInvalid
	}
	return s.putBlob(ctx, r, data)
}

func (s *Coordinator) putBlob(ctx context.Context, r *run, data []byte) (blob, error) {
	if len(data) == 0 || len(data) > maxBlobBytes {
		return blob{}, contract.ErrInvalid
	}
	result := blob{Digest: pv.Digest(data), Bytes: len(data)}
	for start := 0; start < len(data); start += blobPartBytes {
		end := min(start+blobPartBytes, len(data))
		partData := data[start:end]
		partDigest := pv.Digest(partData)
		nameDigest := pv.Digest([]byte(string(r.object.UID) + ":" + partDigest))
		name := "rem-data-" + strings.TrimPrefix(nameDigest, "sha256:")[:40]
		immutable := true
		expected := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: s.config.Namespace,
				Labels: map[string]string{runLabel: r.object.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "ConfigMap", Name: r.object.Name, UID: r.object.UID,
				}},
			},
			Immutable:  &immutable,
			BinaryData: map[string][]byte{"content": bytes.Clone(partData)},
		}
		object := new(corev1.ConfigMap)
		err := s.config.Reader.Get(ctx, client.ObjectKeyFromObject(expected), object)
		if apierrors.IsNotFound(err) {
			if err := s.requireCurrentRun(ctx, r); err != nil {
				return blob{}, err
			}
			err = s.config.Client.Create(ctx, expected)
			// An uncertain artifact acknowledgement can be recovered by its
			// content, without authorizing any execution.
			if err == nil {
				object = expected
			} else {
				err = s.config.Reader.Get(ctx, client.ObjectKeyFromObject(expected), object)
			}
		}
		if err != nil {
			return blob{}, contract.ErrUnavailable
		}
		if object.UID == "" || object.DeletionTimestamp != nil ||
			object.Immutable == nil || !*object.Immutable || len(object.Data) != 0 ||
			len(object.BinaryData) != 1 || !bytes.Equal(object.BinaryData["content"], partData) ||
			!reflect.DeepEqual(object.OwnerReferences, expected.OwnerReferences) ||
			object.Labels[runLabel] != r.object.Name {
			return blob{}, contract.ErrConflict
		}
		result.Parts = append(result.Parts, blobPart{Name: name, UID: string(object.UID), Digest: partDigest})
	}
	return result, nil
}

func (s *Coordinator) readJSON(ctx context.Context, r *run, reference blob, value any) error {
	data, err := s.readBlob(ctx, r, reference)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, value) != nil {
		return contract.ErrConflict
	}
	return nil
}

func (s *Coordinator) readBlob(ctx context.Context, r *run, reference blob) ([]byte, error) {
	if reference.Bytes < 1 || reference.Bytes > maxBlobBytes || len(reference.Parts) == 0 ||
		len(reference.Parts) > maxBlobBytes/blobPartBytes {
		return nil, contract.ErrConflict
	}
	data := make([]byte, 0, reference.Bytes)
	for _, part := range reference.Parts {
		object := new(corev1.ConfigMap)
		if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: part.Name}, object); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, contract.ErrConflict
			}
			return nil, contract.ErrUnavailable
		}
		content := object.BinaryData["content"]
		if object.UID != types.UID(part.UID) || object.DeletionTimestamp != nil ||
			object.Immutable == nil || !*object.Immutable || len(content) > blobPartBytes ||
			object.Labels[runLabel] != r.object.Name || pv.Digest(content) != part.Digest ||
			len(object.OwnerReferences) != 1 || object.OwnerReferences[0].UID != r.object.UID ||
			len(data)+len(content) > reference.Bytes {
			return nil, contract.ErrConflict
		}
		data = append(data, content...)
	}
	if len(data) != reference.Bytes || pv.Digest(data) != reference.Digest {
		return nil, contract.ErrConflict
	}
	return data, nil
}

// EnsureNoActiveRuns rejects disablement while retained work still needs its
// coordinator. Terminal but unsettled cancellation also retains authority.
func EnsureNoActiveRuns(ctx context.Context, reader client.Reader, namespace string) error {
	if reader == nil || namespace == "" {
		return contract.ErrUnavailable
	}
	items, err := listPendingStates(ctx, reader, namespace)
	if err != nil {
		return err
	}
	for index := range items {
		object := &items[index]
		var st state
		if json.Unmarshal([]byte(object.Data["state"]), &st) != nil {
			return contract.ErrConflict
		}
		r, err := decodeRun(object, st.ID, namespace)
		if err != nil || !contract.TerminalPhase(r.state.Status.Phase) || !r.state.Settled {
			return contract.ErrConflict
		}
		if r.state.Parent.Attempted {
			parent := new(corev1alpha1.Task)
			err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: r.state.Parent.Name}, parent)
			if apierrors.IsNotFound(err) && (r.state.Parent.UID == "" || r.state.ParentDeleted) {
				continue
			}
			if err != nil || string(parent.UID) != r.state.Parent.UID ||
				(parent.Status.Phase != corev1alpha1.TaskPhaseSucceeded && parent.Status.Phase != corev1alpha1.TaskPhaseFailed &&
					parent.Status.Phase != corev1alpha1.TaskPhaseCancelled) {
				return contract.ErrConflict
			}
		}
	}
	return nil
}
