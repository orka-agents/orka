package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	retainedPoolCancelOperation   = "cancel_prompt"
	retainedPoolValidateOperation = "create_workspace_delta"
	retainedPoolFinalizeOperation = "finalize_runtime_session_publication"
)

// runtimePoolRetainedCleanupClient grants no new execution authority. A new
// controller owner may finish an exact, still-authenticated old native boot's
// cleanup, even when that boot must first validate an abandoned terminal turn.
// Every mutation revalidates the current leader, frozen Task, exact Pod/pool,
// immutable auth material, and authenticated old runtime fence. The ordinary
// runtimePoolClient remains current-epoch-only for admission.
//
//nolint:gocyclo // Revalidate the full frozen Task, pool, Pod, Session and auth identity together before cleanup.
func (d *ACPDispatcher) runtimePoolRetainedCleanupClient(
	ctx context.Context, task *corev1alpha1.Task, taskUID types.UID,
	pool *corev1alpha1.RuntimePool, owner store.ControllerEpochFence, deleteOnly bool,
) (*harnessv2.Client, harnessv2.Fence, error) {
	fail := func(err error) (*harnessv2.Client, harnessv2.Fence, error) { return nil, harnessv2.Fence{}, err }
	if task == nil || task.Status.Execution == nil || pool == nil || pool.Status.ActiveInstance == nil || d.APIReader == nil || d.Store == nil || pool.Spec.ExecutionWorkspace != nil {
		return fail(errors.New("retained native runtime cleanup authority is incomplete"))
	}
	binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	e := task.Status.Execution.DeepCopy()
	active := pool.Status.ActiveInstance.DeepCopy()
	if binding == nil || binding.Backend != corev1alpha1.AgentExecutionBackendRuntimePool || binding.Task.UID != taskUID ||
		string(pool.UID) != e.RuntimePoolUID || pool.Name != e.RuntimePoolName || active.RuntimeInstanceID != e.RuntimeInstanceID || active.BootID != e.RuntimeSessionSupervisorBootID ||
		active.ProfileDigest != binding.RuntimeProfileDigest || pool.Spec.Runtime.Profile.Digest != binding.RuntimeProfileDigest ||
		active.ControllerEpoch < 1 || active.ControllerEpoch > owner.Epoch || active.PodNamespace == "" || active.PodUID == "" || active.PodName == "" {
		return fail(fmt.Errorf("%w: retained pool does not match frozen cleanup identity", store.ErrConflict))
	}
	digest, err := canonicalAgentExecutionBindingDigest(*binding)
	if err != nil || digest != binding.BindingDigest {
		return fail(fmt.Errorf("%w: retained runtime binding integrity failed", store.ErrConflict))
	}
	secret, err := d.runtimeAuthSecret(ctx, pool)
	if err != nil {
		return fail(err)
	}
	if secret.UID == "" || secret.ResourceVersion == "" || !secret.DeletionTimestamp.IsZero() {
		return fail(errors.New("retained runtime auth identity is incomplete"))
	}
	expectedPool := pool.DeepCopy()
	expectedTask := task.DeepCopy()
	var runtimeClient *harnessv2.Client
	expected := harnessv2.Fence{RuntimeInstanceID: harnessv2.RuntimeInstanceID(active.RuntimeInstanceID), SupervisorBootID: harnessv2.SupervisorBootID(active.BootID), ControllerEpoch: uint64(active.ControllerEpoch), RuntimePoolUID: harnessv2.RuntimePoolUID(pool.UID), RuntimeProfileDigest: harnessv2.ProfileDigest(binding.RuntimeProfileDigest), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion, RuntimeSessionUID: harnessv2.RuntimeSessionUID(e.RuntimeSessionUID), RuntimeSessionGeneration: uint64(e.RuntimeSessionGeneration)}
	verifyObjects := func(checkCtx context.Context) error {
		if err := requireAgentRuntimeRecoveryFence(checkCtx, d.Store, owner); err != nil {
			return err
		}
		current := &corev1alpha1.RuntimePool{}
		if err := d.APIReader.Get(checkCtx, client.ObjectKeyFromObject(expectedPool), current); err != nil {
			return err
		}
		a := current.Status.ActiveInstance
		if current.UID != expectedPool.UID || current.Generation != expectedPool.Generation || current.Spec.ExecutionWorkspace != nil || a == nil ||
			a.PodUID != active.PodUID || a.PodName != active.PodName || a.PodNamespace != active.PodNamespace || a.PodAddress != active.PodAddress ||
			a.RuntimeInstanceID != active.RuntimeInstanceID || a.BootID != active.BootID || a.ControllerEpoch != active.ControllerEpoch || a.ProfileDigest != active.ProfileDigest ||
			current.Spec.Runtime.Profile.Digest != expectedPool.Spec.Runtime.Profile.Digest {
			return fmt.Errorf("%w: retained runtime pool authority changed", store.ErrConflict)
		}
		latest := &corev1alpha1.Task{}
		if err := d.APIReader.Get(checkCtx, client.ObjectKeyFromObject(expectedTask), latest); err != nil {
			return err
		}
		currentBinding := executionBinding(latest, corev1alpha1.AgentRuntimeContractHarnessV2)
		if latest.UID != expectedTask.UID || acpTaskControlUID(latest) != taskUID || currentBinding == nil || currentBinding.BindingDigest != binding.BindingDigest || latest.Status.Execution == nil ||
			latest.Status.Execution.Attempt != e.Attempt || latest.Status.Execution.PromptID != e.PromptID || latest.Status.Execution.RequestDigest != e.RequestDigest ||
			sessionRuntimeCleanupIdentityForExecution(latest.Status.Execution) != sessionRuntimeCleanupIdentityForExecution(e) {
			return fmt.Errorf("%w: retained runtime Task authority changed", store.ErrConflict)
		}
		if !deleteOnly && (latest.Status.Execution.State != e.State || latest.Status.Delivery == nil || latest.Status.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested) {
			return fmt.Errorf("%w: retained validation Task outcome changed", store.ErrConflict)
		}
		currentDigest, digestErr := canonicalAgentExecutionBindingDigest(*currentBinding)
		if digestErr != nil || currentDigest != binding.BindingDigest {
			return fmt.Errorf("%w: retained Task binding integrity changed", store.ErrConflict)
		}
		if expectedTask.Spec.SessionRef != nil {
			session, err := d.Store.GetSessionControl(checkCtx, expectedTask.Namespace, expectedTask.Spec.SessionRef.Name)
			if err != nil {
				return err
			}
			if session.SessionUID != e.RuntimeSessionUID || (session.Lease != nil && (deleteOnly || session.Lease.TaskUID != string(taskUID) || session.Lease.Attempt != int64(e.Attempt) || session.Lease.PromptID != e.PromptID)) {
				return fmt.Errorf("%w: retained runtime Session is owned by another turn", store.ErrConflict)
			}
		}
		pod := &corev1.Pod{}
		if err := d.APIReader.Get(checkCtx, client.ObjectKey{Namespace: active.PodNamespace, Name: active.PodName}, pod); err != nil {
			return err
		}
		if string(pod.UID) != active.PodUID || pod.Labels[runtimePoolUIDLabel] != string(pool.UID) || pod.Labels[runtimePoolNameLabel] != pool.Name || pod.Labels[runtimePoolNamespaceLabel] != pool.Namespace {
			return fmt.Errorf("%w: retained runtime Pod identity changed", store.ErrConflict)
		}
		if !runtimePoolCleanupPodAddressMatches(active.PodAddress, pod.Status.PodIP) {
			return fmt.Errorf("%w: retained runtime address is not the exact Pod address", store.ErrConflict)
		}
		auth, err := d.runtimeAuthSecret(checkCtx, current)
		if err != nil {
			return err
		}
		if auth.UID != secret.UID || auth.ResourceVersion != secret.ResourceVersion || !auth.DeletionTimestamp.IsZero() ||
			!bytes.Equal(auth.Data[runtimePoolControllerTokenKey], secret.Data[runtimePoolControllerTokenKey]) || !bytes.Equal(auth.Data[runtimePoolCapabilitySecretKey], secret.Data[runtimePoolCapabilitySecretKey]) {
			return fmt.Errorf("%w: retained runtime authentication changed", store.ErrConflict)
		}
		return nil
	}
	if err := verifyObjects(ctx); err != nil {
		return fail(err)
	}
	endpoint, err := url.Parse(exactPodEndpoint(active.PodAddress))
	if err != nil {
		return fail(err)
	}
	standard, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fail(errors.New("retained runtime cleanup requires a standard direct HTTP transport"))
	}
	base := standard.Clone()
	base.Proxy = nil
	finalization, err := abandonedValidationFinalization(task, taskUID, harnessv2.WorkspaceDeltaID("delta-"+e.PromptID))
	if err != nil {
		return fail(err)
	}
	transport := &retainedPoolCleanupTransport{
		base: base, endpoint: endpoint, control: d.Store, owner: owner, fence: &expected,
		taskUID: taskUID, taskAttempt: uint32(e.Attempt), promptID: harnessv2.PromptID(e.PromptID), deleteOnly: deleteOnly, finalization: finalization,
		verify: func(checkCtx context.Context) error {
			if err := verifyObjects(checkCtx); err != nil {
				return err
			}
			status, err := runtimeClient.Status(checkCtx)
			if err != nil {
				return err
			}
			return validateSessionRuntimeCleanupStatus(expected, status)
		},
	}
	runtimeClient, err = harnessv2.NewClient(endpoint.String(),
		harnessv2.WithHTTPClient(&http.Client{Transport: transport}),
		harnessv2.WithControllerBearerToken(strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKey]))),
		harnessv2.WithOperationCapabilitySecret(secret.Data[runtimePoolCapabilitySecretKey]),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{RuntimeProfileDigest: expected.RuntimeProfileDigest, RuntimeInstanceID: expected.RuntimeInstanceID}),
		harnessv2.WithBeforeMutation(func(_ context.Context, operation string) error {
			if !runtimePoolRetainedCleanupOperationAllowed(operation, deleteOnly) {
				return errors.New("retained runtime cleanup cannot perform admission or unrelated mutations")
			}
			return nil
		}),
	)
	if err != nil {
		return fail(err)
	}
	status, err := runtimeClient.Status(ctx)
	if err != nil {
		return fail(err)
	}
	// Retain the old supervisor's authenticated generation instead of using
	// a newer desired Deployment generation as execution authority.
	if status.Fence.RuntimePoolGeneration == 0 || status.Fence.RuntimePoolGeneration > uint64(pool.Generation) {
		return fail(errors.New("retained runtime pool generation is invalid"))
	}
	expected.RuntimePoolGeneration = status.Fence.RuntimePoolGeneration
	if err := validateSessionRuntimeCleanupStatus(expected, status); err != nil {
		return fail(err)
	}
	return runtimeClient, expected, nil
}

func runtimePoolRetainedCleanupOperationAllowed(operation string, deleteOnly bool) bool {
	if operation == "delete_runtime_session" {
		return true
	}
	if deleteOnly {
		return false
	}
	switch operation {
	case retainedPoolCancelOperation, retainedPoolValidateOperation, retainedPoolFinalizeOperation:
		return true
	default:
		return false
	}
}

func runtimePoolCleanupPodAddressMatches(address, podIP string) bool {
	endpoint, err := url.Parse(exactPodEndpoint(address))
	if err != nil {
		return false
	}
	ip := net.ParseIP(endpoint.Hostname())
	return ip != nil && ip.Equal(net.ParseIP(podIP))
}
