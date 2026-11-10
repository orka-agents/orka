package applicationexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (b *Backend) requestInput(request contract.ExecutionRequest) (contract.ExecutionRequest, string, error) {
	if b == nil || !b.config.Enabled {
		return contract.ExecutionRequest{}, "", contract.ErrDisabled
	}
	if err := contract.RequireEvidenceScope(request.EvidenceScope, b.EvidenceScope()); err != nil {
		return contract.ExecutionRequest{}, "", err
	}
	if request.EvidenceScope != contract.ApplicationRuntime || request.Namespace != b.config.Namespace ||
		!validUID(request.ID) || !validUID(request.ParentTaskUID) ||
		request.Deadline == nil || request.Deadline.IsZero() ||
		len(validation.IsDNS1123Subdomain(request.ParentTaskName)) != 0 || request.ParentTaskName == "" ||
		b.admit(request) != nil {
		return contract.ExecutionRequest{}, "", contract.ErrInvalid
	}
	input := b.preparationInput(request, "", nil)
	if pv.ValidatePreparationInput(input) != nil {
		return contract.ExecutionRequest{}, "", contract.ErrInvalid
	}
	content, err := marshalInput(request)
	if err != nil || len(content) > 256<<10 {
		return contract.ExecutionRequest{}, "", contract.ErrInvalid
	}
	var frozen contract.ExecutionRequest
	if json.Unmarshal(content, &frozen) != nil {
		return contract.ExecutionRequest{}, "", contract.ErrInvalid
	}
	return frozen, executionDigest(b.ConfigurationDigest(), frozen), nil
}

func executionDigest(configuration string, request contract.ExecutionRequest) string {
	content, _ := json.Marshal(struct {
		Configuration string
		Request       contract.ExecutionRequest
	}{configuration, request})
	return pv.Digest(content)
}

func (b *Backend) RequestDigest(request contract.ExecutionRequest) (string, error) {
	_, digest, err := b.requestInput(request)
	return digest, err
}

func (b *Backend) preparationInput(request contract.ExecutionRequest, taskUID string, r *run) pv.ApplicationPreparationInput {
	reportDigest, _ := pv.StableReportDigest(request.Problem, request.Checks.Scope)
	dependencies := map[string]string{
		contract.EvidenceScopeKey:          string(contract.ApplicationRuntime),
		pv.ApplicationPolicyKey:            pv.ApplicationPolicyVersion,
		pv.ApplicationObserverImageKey:     b.config.WorkerImage,
		pv.ApplicationObserverImageIDKey:   b.config.WorkerImageID,
		pv.PreparationPolicyKey:            pv.PreparationPolicy,
		pv.PreparationConfigurationKey:     b.ConfigurationDigest(),
		"orka.application.target-cluster":  b.config.TargetClusterUID,
		"orka.application.control-cluster": b.config.ControlClusterUID,
	}
	if b.config.RuntimeQualification != nil {
		digest, _ := pv.ApplicationRuntimeQualificationDigest(*b.config.RuntimeQualification)
		dependencies[pv.ApplicationRuntimeQualificationKey] = digest
	}
	if r != nil {
		dependencies["orka.application.node"] = r.state.Target.NodeName
		dependencies["orka.application.node-uid"] = string(r.state.Target.NodeUID)
		dependencies["orka.application.ledger-uid"] = string(r.object.UID)
		dependencies["orka.application.storage-class-uid"] = string(r.state.Target.StorageClassUID)
		dependencies["orka.application.authorization"] = r.state.AuthorizationDigest
		dependencies["orka.application.resource-policy"] = policyDigest(request.ApplicationPolicy)
		if len(r.state.ClusterReads) != 0 {
			dependencies["orka.application.cluster-read-set"] = clusterReadDigest(r)
		}
		dependencies["orka.application.deadline"] = r.state.Deadline.Format(time.RFC3339Nano)
		for _, side := range pv.ActionSides(request.Action) {
			if namespace := r.effect(resourceKey(side, "namespace")); namespace != nil {
				dependencies["orka.application.namespace."+side] = namespace.Name
				dependencies["orka.application.namespace-uid."+side] = string(namespace.UID)
			}
			dependencies["orka.application.boundary."+side] = r.state.Boundary[side]
			dependencies["orka.application.source-boundary."+side] = r.state.Boundary[side+"/source"]
			if identities := r.state.SubjectTLS[side]; len(identities) != 0 {
				content, _ := json.Marshal(identities)
				dependencies["orka.application.subject-tls."+side] = pv.Digest(content)
			}
			if request.ApplicationPolicy != nil {
				if namespace := r.effect(resourceKey(protectedSide(side), "namespace")); namespace != nil {
					dependencies["orka.application.observer-namespace."+side] = namespace.Name
					dependencies["orka.application.observer-namespace-uid."+side] = string(namespace.UID)
				}
			}
		}
	}
	return pv.ApplicationPreparationInput{Version: pv.PreparationProtocolVersion, ID: request.ID, Namespace: request.Namespace,
		ParentTaskName: request.ParentTaskName, ParentTaskUID: request.ParentTaskUID, TaskUID: taskUID, Patch: []byte(request.Patch),
		Manifest: pv.Manifest{Version: pv.SchemaVersion, Action: request.Action, ReportDigest: reportDigest,
			Problem: request.Problem, Scope: request.Checks.Scope, Gaps: request.Checks.Gaps,
			Checks: request.Checks.Checks, Application: request.Checks.Application, ApplicationPolicy: request.ApplicationPolicy,
			DeclaredChanges: request.DeclaredChanges,
			Sources:         pv.Sources{Repository: request.Source.Repository, Original: pv.SourceIdentity{Commit: request.Source.Commit, Tree: request.Source.Tree}},
			Environment: pv.Environment{Image: b.config.ToolchainImage, ImageID: b.config.ToolchainImageID,
				Platform: b.config.Platform, Profile: pv.ApplicationRuntimeProfile, Dependencies: dependencies}}}
}

func (b *Backend) createRun(ctx context.Context, request contract.ExecutionRequest, digest string, qualification bool) (*run, error) {
	existing, err := b.load(ctx, runName(request))
	if err == nil {
		if existing.state.InputDigest != digest || existing.state.Configuration != b.ConfigurationDigest() ||
			existing.state.Qualification != qualification || !reflect.DeepEqual(existing.state.Request, request) {
			return nil, contract.ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, contract.ErrNotFound) {
		return nil, err
	}
	identity, err := b.verifyTarget(ctx, nil)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	deadline := now.Add(b.config.RunTimeout)
	if !qualification {
		if request.Deadline == nil || !request.Deadline.After(now) {
			return nil, safeError(contract.ErrUnavailable, context.DeadlineExceeded)
		}
		if request.Deadline.Before(deadline) {
			deadline = *request.Deadline
		}
	}
	r := &run{object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: runName(request), Namespace: request.Namespace,
		Labels: map[string]string{stateLabel: "v1"}, Finalizers: []string{cleanupFinalizer}}},
		state: state{Version: stateVersion, Configuration: b.ConfigurationDigest(), InputDigest: digest,
			Request: request, Qualification: qualification, ControlUID: b.config.ControlClusterUID, TargetUID: b.config.TargetClusterUID,
			Target: identity, CreatedAt: now, Deadline: deadline, Task: taskReceipt{Name: runName(request)},
			ArmTasks: make(map[string]taskReceipt), Pods: make(map[string]podReceipt),
			Artifacts: make(map[string]pv.ApplicationArtifact), Boundary: make(map[string]string),
			BoundaryReports: make(map[string]pv.ApplicationBoundaryReport), Credentials: make(map[string]credentialReceipt),
			Actions: make(map[string]actionReceipt)}}
	if err := b.parent(ctx, r); err != nil {
		return nil, err
	}
	content, err := json.Marshal(r.state)
	if err != nil || len(content) > maxStateBytes {
		return nil, contract.ErrInvalid
	}
	r.object.Data = map[string]string{stateKey: string(content)}
	if err := b.control.Create(ctx, r.object); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if r.object.UID == "" {
		return nil, contract.ErrConflict
	}
	return r, nil
}

func (b *Backend) Start(ctx context.Context, request contract.ExecutionRequest, accepted func(contract.ExecutionReceipt) error) (*pv.Record, error) {
	frozen, digest, err := b.requestInput(request)
	if err != nil {
		return nil, noEffectsError{cause: err}
	}
	if accepted == nil {
		return nil, noEffectsError{cause: contract.ErrInvalid}
	}
	if !frozen.Deadline.After(time.Now()) {
		return nil, noEffectsError{cause: safeError(contract.ErrUnavailable, context.DeadlineExceeded)}
	}
	if err := b.lockEffects(ctx); err != nil {
		return nil, noEffectsError{cause: err}
	}
	r, err := b.createRun(ctx, frozen, digest, false)
	if err == nil && (r.state.Cancelled || r.state.Failed != "") {
		err = contract.ErrConflict
	}
	if err == nil && !r.state.Finished {
		err = b.ensureTask(ctx, r, "")
	}
	<-b.effectGate
	if err != nil {
		return nil, err
	}
	receipt := r.receipt()
	if receipt.TaskUID == "" {
		return nil, ErrUncertain
	}
	if err := accepted(receipt); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	return b.await(ctx, frozen, receipt)
}

func (b *Backend) Recover(ctx context.Context, request contract.ExecutionRequest) (contract.ExecutionRecovery, error) {
	frozen, digest, err := b.requestInput(request)
	if err != nil {
		return contract.ExecutionRecovery{}, err
	}
	r, err := b.load(ctx, runName(frozen))
	if err != nil {
		return contract.ExecutionRecovery{}, err
	}
	if r.state.Qualification || r.state.Configuration != b.ConfigurationDigest() || r.state.InputDigest != digest ||
		r.state.ControlUID != b.config.ControlClusterUID || r.state.TargetUID != b.config.TargetClusterUID ||
		!reflect.DeepEqual(r.state.Request, frozen) {
		return contract.ExecutionRecovery{}, contract.ErrConflict
	}
	if r.state.Task.UID == "" {
		if r.state.Task.Attempted && !r.state.Task.Rejected {
			return contract.ExecutionRecovery{}, ErrUncertain
		}
		if err := b.recoverRootReservation(ctx, r); err != nil {
			return contract.ExecutionRecovery{}, err
		}
		return contract.ExecutionRecovery{Settled: true}, nil
	}
	receipt := r.receipt()
	if _, err := b.boundRun(ctx, receipt); err != nil {
		return contract.ExecutionRecovery{}, err
	}
	return contract.ExecutionRecovery{Receipt: &receipt}, nil
}

func (b *Backend) Resume(ctx context.Context, request contract.ExecutionRequest, receipt contract.ExecutionReceipt) (*pv.Record, error) {
	frozen, digest, err := b.requestInput(request)
	if err != nil {
		return nil, err
	}
	if receipt.InputDigest != digest || receipt.Namespace != frozen.Namespace || receipt.TaskName != runName(frozen) || receipt.TaskUID == "" {
		return nil, contract.ErrConflict
	}
	return b.await(ctx, frozen, receipt)
}

func (b *Backend) boundRun(ctx context.Context, receipt contract.ExecutionReceipt) (*run, error) {
	if receipt.Namespace != b.config.Namespace || !validUID(receipt.TaskUID) ||
		!digestPattern.MatchString(receipt.InputDigest) || !strings.HasPrefix(receipt.TaskName, "app-exec-") {
		return nil, contract.ErrConflict
	}
	r, err := b.load(ctx, receipt.TaskName)
	if err != nil {
		return nil, err
	}
	if r.receipt() != receipt || r.state.TargetUID != b.config.TargetClusterUID || r.state.ControlUID != b.config.ControlClusterUID {
		return nil, contract.ErrConflict
	}
	return r, nil
}

func (b *Backend) await(ctx context.Context, request contract.ExecutionRequest, receipt contract.ExecutionReceipt) (*pv.Record, error) {
	for {
		if err := b.lockEffects(ctx); err != nil {
			return nil, err
		}
		record, done, err := b.advanceRequest(ctx, request, receipt)
		<-b.effectGate
		if err != nil || done {
			return record, err
		}
		if err := b.pause(ctx); err != nil {
			return nil, err
		}
	}
}

func (b *Backend) advanceRequest(ctx context.Context, request contract.ExecutionRequest, receipt contract.ExecutionReceipt) (*pv.Record, bool, error) {
	r, err := b.boundRun(ctx, receipt)
	if err != nil {
		return nil, false, err
	}
	if !reflect.DeepEqual(r.state.Request, request) || r.state.Configuration != b.ConfigurationDigest() {
		return nil, false, contract.ErrConflict
	}
	if r.state.Cancelled || r.state.Failed != "" {
		return nil, false, contract.ErrConflict
	}
	if err := b.parent(ctx, r); err != nil {
		return nil, false, err
	}
	if err := b.checkTask(ctx, r, false); err != nil {
		if anyTaskRejected(r) {
			return nil, false, b.fail(ctx, r, taskCreateRejected, err)
		}
		return nil, false, err
	}
	if r.state.Finished {
		if !r.state.Settled {
			if done, err := b.cleanup(ctx, r); err != nil || !done {
				return nil, false, err
			}
		}
		record, err := b.finalRecord(ctx, r)
		return record, err == nil, err
	}
	if !time.Now().Before(r.state.Deadline) {
		return nil, false, b.fail(ctx, r, "deadline-exceeded", context.DeadlineExceeded)
	}
	if err := b.authorizeEffect(ctx, r); err != nil {
		return nil, false, b.fail(ctx, r, "authority-changed", err)
	}
	done, err := b.advanceApplication(ctx, r)
	if err != nil {
		if ctx.Err() != nil || pvkube.IsTransientError(err) {
			return nil, false, err
		}
		return nil, false, b.fail(ctx, r, "application-execution-failed", err)
	}
	if !done {
		return nil, false, nil
	}
	r.state.Finished = true
	if err := b.save(ctx, r); err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

func (b *Backend) fail(ctx context.Context, r *run, reason string, cause error) error {
	if r.state.Failed == "" {
		r.state.Failed = reason
		if err := b.save(ctx, r); err != nil {
			return errors.Join(cause, err)
		}
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.config.CleanupTimeout)
	defer cancel()
	_, err := b.cleanup(cleanupCtx, r)
	return errors.Join(cause, err)
}

// CheckReadiness executes actual bounded probes through an Orka Task. Merely
// finding a NetworkPolicy, a namespace, a CNI DaemonSet, or a ready Node never
// advertises application-runtime. Each later application run repeats the proof.
func (b *Backend) CheckReadiness(ctx context.Context) error {
	if b == nil || !b.config.Enabled {
		return contract.ErrDisabled
	}
	b.qualified.Store(false)
	if err := b.store.InitializeKubernetesValidationStore(ctx); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if err := b.recoverRetained(ctx); err != nil {
		return err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return contract.ErrUnavailable
	}
	request := contract.ExecutionRequest{ID: "qualification-" + hex.EncodeToString(nonce), Namespace: b.config.Namespace,
		ParentTaskName: "qualification", ParentTaskUID: b.config.ControlClusterUID, Action: pv.ValidateReport,
		EvidenceScope: contract.ApplicationRuntime}
	if err := b.lockEffects(ctx); err != nil {
		return err
	}
	r, err := b.createRun(ctx, request, executionDigest(b.ConfigurationDigest(), request), true)
	if err == nil {
		err = b.ensureTask(ctx, r, "")
	}
	<-b.effectGate
	if err != nil {
		return err
	}
	operation, cancel := context.WithDeadline(ctx, r.state.Deadline)
	defer cancel()
	for {
		if err := b.lockEffects(operation); err != nil {
			return b.cancelQualification(ctx, r, err)
		}
		current, err := b.boundRun(operation, r.receipt())
		done := false
		if err == nil {
			r = current
			done, err = b.advanceQualification(operation, r)
		}
		<-b.effectGate
		if err != nil {
			return b.cancelQualification(ctx, r, err)
		}
		if done {
			b.qualified.Store(true)
			return nil
		}
		if err := b.pause(operation); err != nil {
			return b.cancelQualification(ctx, r, err)
		}
	}
}

func (b *Backend) cancelQualification(ctx context.Context, r *run, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.config.CleanupTimeout)
	defer cancel()
	err := b.Cancel(ctx, r.receipt())
	return errors.Join(safeError(ErrIsolation, cause), err)
}
