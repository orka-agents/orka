/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

// Substrate-backed RuntimePools (execution-workspace Phase 2) host the pool's
// single runtime instance inside a gVisor-isolated Substrate Actor reached
// through the provider router, instead of a controller-owned Deployment or an
// Agent Sandbox claim. The runtime container is always controller-rendered:
// the operator-owned base ActorTemplate contributes only infrastructure
// (worker pool placement, runsc build, snapshot location), and a derived,
// controller-owned ActorTemplate carries the immutable runtime image, the
// fence environment, and no credential material — the supervisor boots into
// an awaiting phase and the controller seeds credentials post-boot through
// the nonce-bound, controller-signed bootstrap endpoint. Prompt-level operations use the
// unchanged orka.harness.v2 protocol against the ActiveInstance; only
// workload materialization and endpoint dialing differ.
//
// Checkpointing a live supervisor is prohibited: gVisor suspension captures
// process memory — including live pool and provider-proxy credentials — into
// provider snapshot storage. Because the provider deletes only suspended
// actors, teardown is staged: destroy the workload's memory by deleting its
// single-workload worker Pod, settle the memoryless actor (a suspension with
// nothing left to checkpoint), then delete it. The exact instance is recycled
// whenever the provider reports a suspension or a snapshot for a booted
// actor. Actor suspend/resume and snapshot restore remain fail-closed until
// the provider offers credential-safe sessions.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace"
)

const (
	runtimePoolSubstrateTemplateSuffix         = "actor-template"
	runtimePoolSubstrateActorSuffix            = "actor"
	runtimePoolSubstrateDenyEgressSuffix       = "substrate-deny-egress"
	runtimePoolSubstrateDNSEgressSuffix        = "substrate-dns-egress"
	runtimePoolSubstrateProviderEgressSuffix   = "substrate-provider-egress"
	runtimePoolSubstrateProviderIngressSuffix  = "substrate-provider-ingress"
	runtimePoolSubstrateControllerEgressSuffix = "substrate-controller-egress"
	substrateObjectSpecField                   = "spec"
	substrateObjectLabelsField                 = "labels"
	substrateActorTemplateAPIVersion           = "v1alpha1"

	// substrateActorBootedAnnotation records the exact actor ID whose workload
	// this pool booted from scratch. It makes boot idempotent across controller
	// restarts and lets suspension detection ignore the provider's initial
	// created-but-never-resumed state.
	substrateActorBootedAnnotation = "orka.ai/substrate-actor-booted"
	// substrateActorRecyclingAnnotation records an in-progress staged actor
	// teardown so every reconcile resumes it before any other decision.
	substrateActorRecyclingAnnotation = "orka.ai/substrate-actor-recycling"
	// substrateActorCredentialSeededAnnotation records the exact actor lifetime
	// whose bootstrap payload was accepted or authenticated by this controller.
	substrateActorCredentialSeededAnnotation = "orka.ai/substrate-actor-credential-seeded"
	// substrateActorTemplateFenceAnnotation records the exact Kubernetes
	// ActorTemplate UID/resourceVersion that was validated before actor
	// creation. Any template update or delete/recreate changes this fence and
	// forces the actor to be recycled before credential bootstrap.
	substrateActorTemplateFenceAnnotation = "orka.ai/substrate-actor-template-fence"
	// substrateActorWorkerPlacementAnnotation freezes the exact WorkerPool
	// namespace/name admitted before actor creation. Teardown must not trust a
	// later read of the mutable derived ActorTemplate when selecting the worker
	// Pod whose memory it destroys.
	substrateActorWorkerPlacementAnnotation = "orka.ai/substrate-actor-worker-placement"
	// substrateActorWorkerPodFenceAnnotation records the exact provider worker
	// Pod name and UID verified before a booted actor is admitted. If the Actor
	// record disappears, teardown uses this controller-owned fence to destroy or
	// independently prove absence of the workload that may still hold secrets.
	substrateActorWorkerPodFenceAnnotation = "orka.ai/substrate-actor-worker-pod-fence"
	// substrateActorReplacementWorkerPodFenceAnnotation records a newly
	// validated provider worker Pod while the prior exact fence is retired.
	// Teardown promotes it only after proving the old workload absent.
	substrateActorReplacementWorkerPodFenceAnnotation = "orka.ai/substrate-actor-replacement-worker-pod-fence"
	// substrateActorWorkloadAbsentAnnotation records the exact actor whose
	// frozen worker Pod was observed absent before SettleActor. Providers may
	// clear placement when an actor becomes suspended or crashed, so teardown
	// must persist this proof before invoking the provider transition.
	substrateActorWorkloadAbsentAnnotation = "orka.ai/substrate-actor-workload-absent"
	// substrateNetworkPolicyNamespacesAnnotation records every provider worker
	// namespace where this pool materialized egress policies. Cross-namespace
	// policies cannot carry a RuntimePool owner reference, and finalization must
	// remain namespace-scoped under the operator-provided RoleBindings even if
	// the derived ActorTemplate is later removed.
	substrateNetworkPolicyNamespacesAnnotation = "orka.ai/substrate-network-policy-namespaces"
	// substrateActorSuspendedAnnotation records the exact actor ID whose
	// data-only suspension this controller requested. It is written before the
	// provider call so a restart resumes the same consensual suspension, and it
	// distinguishes a requested checkpoint from a provider-initiated suspension,
	// which stays fail-closed.
	substrateActorSuspendedAnnotation = "orka.ai/substrate-actor-suspended"
	// substrateActorResumingAnnotation records the exact actor ID whose cold
	// resume consumed the suspension consent but has not passed the
	// authenticated exact-instance Serving admission yet. While it stands the
	// actor's DurableDir is the only copy of the preserved session data, so
	// any recycle or loss of the actor is terminal
	// (runtimePoolWorkspaceResumeLostAnnotation) instead of a silent
	// reprovision. It retires once the pool durably reaches Serving.
	substrateActorResumingAnnotation = "orka.ai/substrate-actor-resuming"

	// substrateActorListenPort is the conventional actor service port the
	// provider router forwards to.
	substrateActorListenPort int32 = 80
	// substrateWorkerPoolLabel marks provider worker Pods; the staged actor
	// teardown refuses to delete any Pod that does not carry it.
	substrateWorkerPoolLabel = "ate.dev/worker-pool"
	// substrateRuntimeEntrypoint is the shared entrypoint of every immutable
	// ACP runtime image; the provider does not read image config, so the
	// rendered container must state it explicitly.
	substrateRuntimeEntrypoint = "/usr/local/bin/orka-acp-runtime"
	// substrateDurableWorkspaceVolume is the controller-owned DurableDir volume
	// rendered into data-only-suspendable derived templates. Base templates
	// must not define a volume with this reserved name.
	substrateDurableWorkspaceVolume = "orka-workspace"
	// substrateDurableWorkspaceMountPath is where the durable workspace volume
	// mounts inside the runtime container. Only repository/workspace data lives
	// under it; the supervisor session tree, home, temporary files, and every
	// credential stay on ephemeral storage.
	substrateDurableWorkspaceMountPath = "/durable/orka-workspace"
	// substrateSnapshotScopeData is the provider's DurableDir-only snapshot
	// scope; it never captures process memory.
	substrateSnapshotScopeData = "Data"
	// substrateSnapshotResumeColdBoot restores a data snapshot into a freshly
	// booted workload.
	substrateSnapshotResumeColdBoot = "ColdBoot"
)

type substrateRuntimePoolWorkerPlacementRecord struct {
	Namespace  string `json:"namespace"`
	WorkerPool string `json:"workerPool"`
}

type substrateRuntimePoolWorkerPodFenceRecord struct {
	ActorID   string    `json:"actorID"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

// +kubebuilder:rbac:groups=ate.dev,resources=actortemplates,verbs=get;list;watch

var substrateActorTemplateGVK = schema.GroupVersionKind{Group: "ate.dev", Version: substrateActorTemplateAPIVersion, Kind: "ActorTemplate"}

func runtimePoolSubstrateTemplateName(base string) string {
	return runtimePoolChildName(base, runtimePoolSubstrateTemplateSuffix)
}

func runtimePoolSubstrateActorID(base string) string {
	return runtimePoolChildName(base, runtimePoolSubstrateActorSuffix)
}

func substrateActorInstanceUID(actorID string) string {
	sum := sha256.Sum256([]byte("orka-substrate-runtime-instance\x00" + strings.TrimSpace(actorID)))
	return "workspace:" + hex.EncodeToString(sum[:])
}

func substrateActorRouteHost(actorID, dnsSuffix string) string {
	return actorID + "." + strings.Trim(strings.TrimSpace(dnsSuffix), ".")
}

func substrateActorMatchesRuntimeTemplate(
	actor *workspace.SubstrateRuntimeActor,
	actorID, templateNamespace, templateName string,
) bool {
	return actor != nil &&
		strings.TrimSpace(actor.ActorID) == actorID &&
		strings.TrimSpace(actor.TemplateNamespace) == templateNamespace &&
		strings.TrimSpace(actor.TemplateName) == templateName
}

func substrateRuntimeTemplateOwnedByPool(
	template, desired *unstructured.Unstructured,
) bool {
	if template == nil || desired == nil ||
		template.GetNamespace() != desired.GetNamespace() || template.GetName() != desired.GetName() {
		return false
	}
	observedLabels := template.GetLabels()
	desiredLabels := desired.GetLabels()
	for _, key := range []string{
		runtimePoolManagedByLabel,
		runtimePoolKeyLabel,
		runtimePoolNameLabel,
		runtimePoolNamespaceLabel,
		runtimePoolUIDLabel,
	} {
		if desiredLabels[key] == "" || observedLabels[key] != desiredLabels[key] {
			return false
		}
	}
	return true
}

func substrateRuntimeTemplateFence(template *unstructured.Unstructured) (string, error) {
	if template == nil {
		return "", fmt.Errorf("RuntimePool substrate ActorTemplate is required for revision fencing")
	}
	uid := strings.TrimSpace(string(template.GetUID()))
	resourceVersion := strings.TrimSpace(template.GetResourceVersion())
	if uid == "" || resourceVersion == "" {
		return "", fmt.Errorf("RuntimePool substrate ActorTemplate is missing its Kubernetes UID/resourceVersion fence")
	}
	return uid + "/" + resourceVersion, nil
}

func runtimePoolIsSubstrateBacked(pool *corev1alpha1.RuntimePool) bool {
	return pool != nil && pool.Spec.ExecutionWorkspace != nil &&
		pool.Spec.ExecutionWorkspace.Provider == corev1alpha1.WorkspaceProviderSubstrate
}

type substrateRuntimeActorControlWithTimeout struct {
	delegate workspace.SubstrateRuntimeActorControl
	timeout  time.Duration
}

func (c *substrateRuntimeActorControlWithTimeout) GetActor(
	ctx context.Context,
	actorID string,
) (*workspace.SubstrateRuntimeActor, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.delegate.GetActor(ctx, actorID)
}

func (c *substrateRuntimeActorControlWithTimeout) CreateActor(
	ctx context.Context,
	actorID, templateNamespace, templateName string,
) (*workspace.SubstrateRuntimeActor, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.delegate.CreateActor(ctx, actorID, templateNamespace, templateName)
}

func (c *substrateRuntimeActorControlWithTimeout) ResumeActor(
	ctx context.Context,
	actorID string,
	boot bool,
) (*workspace.SubstrateRuntimeActor, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.delegate.ResumeActor(ctx, actorID, boot)
}

func (c *substrateRuntimeActorControlWithTimeout) SettleActor(
	ctx context.Context,
	actorID string,
) (*workspace.SubstrateRuntimeActor, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.delegate.SettleActor(ctx, actorID)
}

func (c *substrateRuntimeActorControlWithTimeout) SuspendActorForDataCheckpoint(
	ctx context.Context,
	actorID string,
) (*workspace.SubstrateRuntimeActor, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.delegate.SuspendActorForDataCheckpoint(ctx, actorID)
}

func (c *substrateRuntimeActorControlWithTimeout) DeleteActor(ctx context.Context, actorID string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.delegate.DeleteActor(ctx, actorID)
}

func (c *substrateRuntimeActorControlWithTimeout) Close() error {
	return c.delegate.Close()
}

// substrateActorControlForCleanup remains available after the provider flag is
// disabled so existing Actors can drain and cannot strand RuntimePool
// finalizers. The enable gate controls new workload reconciliation, not
// mandatory provider cleanup.
func (r *RuntimePoolReconciler) substrateActorControlForCleanup() (workspace.SubstrateRuntimeActorControl, error) {
	cfg := r.SubstrateConfig.WithDefaults()
	if cfg.ClaimTimeout <= 0 {
		return nil, fmt.Errorf("substrate claim timeout must be greater than zero")
	}
	factory := r.SubstrateActorControlFactory
	if factory == nil {
		factory = defaultSubstrateRuntimeActorControlFactory
	}
	control, err := factory(cfg)
	if err != nil {
		return nil, err
	}
	return &substrateRuntimeActorControlWithTimeout{delegate: control, timeout: cfg.ClaimTimeout}, nil
}

func defaultSubstrateRuntimeActorControlFactory(cfg SubstrateConfig) (workspace.SubstrateRuntimeActorControl, error) {
	return workspace.NewSubstrateRuntimeActorControl(workspace.SubstrateConfig{
		APIEndpoint:           cfg.APIEndpoint,
		APICAFile:             cfg.APICAFile,
		APIInsecureSkipVerify: cfg.APIInsecureSkipVerify,
	})
}

// reconcileSubstrateBackedRuntimePool converges a Substrate-backed pool. It
// mirrors the Deployment and Agent Sandbox paths exactly, replacing only
// workload materialization and endpoint dialing.
//
//nolint:gocyclo // Workload materialization decisions stay auditable together, mirroring the other backends.
func (r *RuntimePoolReconciler) reconcileSubstrateBackedRuntimePool(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) (ctrl.Result, error) {
	substrateSpec := pool.Spec.ExecutionWorkspace.Substrate
	templateNamespace := substrateSpec.BaseTemplateNamespace
	// The template and the actor never carry credentials: pool Secrets stay in
	// the controller-owned runtime namespace and are seeded into the booted
	// supervisor through the nonce-bound, controller-signed credential bootstrap.
	if err := r.ensureRuntimePoolNamespace(ctx, cfg); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	authSecret, providerSecret, err := r.ensureRuntimePoolSecrets(ctx, pool, cfg)
	if err != nil {
		if errors.Is(err, errWorkspaceRuntimePoolAuthBindingLost) {
			return r.reconcileSubstrateRuntimePoolMissingAuthSecret(ctx, pool, cfg)
		}
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	actorID := runtimePoolSubstrateActorID(cfg.baseName)
	routeHost := substrateActorRouteHost(actorID, r.SubstrateConfig.ActorDNSSuffix)
	derivedTemplate, err := r.getSubstrateActorTemplate(ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	expectedTemplate := &unstructured.Unstructured{}
	expectedTemplate.SetNamespace(templateNamespace)
	expectedTemplate.SetName(runtimePoolSubstrateTemplateName(cfg.baseName))
	expectedTemplate.SetLabels(cloneStringMap(cfg.labels))
	templateOwned := derivedTemplate == nil || substrateRuntimeTemplateOwnedByPool(derivedTemplate, expectedTemplate)
	templateRevision := ""
	templateFence := ""
	templateIntegrityErr := error(nil)
	if derivedTemplate != nil && templateOwned {
		templateRevision, templateIntegrityErr = substrateRuntimeTemplateIntegrity(derivedTemplate)
		if templateIntegrityErr == nil {
			templateFence, err = substrateRuntimeTemplateFence(derivedTemplate)
			if err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
	}
	control, err := r.substrateActorControlForCleanup()
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	defer control.Close() //nolint:errcheck // best-effort connection teardown
	actor, err := control.GetActor(ctx, actorID)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if pool.Spec.DesiredReplicas != 0 && actor == nil &&
		(substrateActorConsensuallySuspended(pool, actorID) ||
			pool.Annotations[substrateActorResumingAnnotation] == actorID) {
		// The checkpointed actor vanished after resume demand was registered
		// (or mid-resume, after consent was consumed but before admission):
		// the preserved DurableDir state is unrecoverable, and creating a
		// replacement would silently boot from a re-materialized baseline.
		// Record the terminal loss so the workspace adapter fails the
		// workspace closed, and retire the stale consent.
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, runtimePoolWorkspaceResumeLostAnnotation,
			"checkpointed actor "+actorID+" vanished before cold resume completed"); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorSuspendedAnnotation, ""); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorResumingAnnotation, ""); err != nil {
			return ctrl.Result{}, err
		}
		status := r.baseRuntimePoolStatus(pool, 0)
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "the checkpointed provider actor is gone; the durable workspace data is unrecoverable and cold resume fails closed"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	if pool.Annotations[substrateActorResumingAnnotation] == actorID &&
		pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing {
		// The resumed actor passed the authenticated exact-instance Serving
		// fence and that admission is durably persisted; only now does the
		// resume-in-progress proof retire. Any earlier crash, bootstrap
		// conflict, or template-fence recycle stays terminal.
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorResumingAnnotation, ""); err != nil {
			return ctrl.Result{}, err
		}
	}
	if strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceResumeLostAnnotation]) != "" && pool.Spec.DesiredReplicas != 0 {
		// A recorded terminal resume loss is never reprovisioned over; the
		// pool stays Degraded until the workspace is deleted explicitly.
		status := r.baseRuntimePoolStatus(pool, 0)
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "durable workspace data was lost during a cold resume; the workspace fails closed and is never reprovisioned"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	if pool.Spec.DesiredReplicas != 0 &&
		(actor == nil || substrateActorAwaitingDataResume(pool, actor, actorID)) &&
		strings.TrimSpace(pool.Annotations[substrateActorRecyclingAnnotation]) == "" &&
		!substrateActorWorkloadProofRequired(pool, actorID) {
		rotating, rotateErr := r.rotateConsumedWorkspaceRuntimePoolAuthSecret(ctx, pool, cfg, authSecret)
		if rotateErr != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, rotateErr)
		}
		if rotating {
			status := r.baseRuntimePoolStatus(pool, 0)
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "rotating one-time credential bootstrap material before creating a replacement actor"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
	}
	bootstrapNonce := strings.TrimSpace(string(authSecret.Data[runtimePoolBootstrapNonceKey]))
	if bootstrapNonce == "" {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("RuntimePool auth Secret is missing the credential bootstrap nonce"))
	}
	bootstrapPublicKey, err := harnessv2.CredentialBootstrapPublicKey(authSecret.Data[runtimePoolBootstrapSigningSeedKey])
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("derive RuntimePool credential bootstrap public key: %w", err))
	}
	var desired substrateRuntimeTemplateRender
	desiredLoaded := false
	loadDesired := func() error {
		if desiredLoaded {
			return nil
		}
		baseTemplate, getErr := r.getSubstrateActorTemplate(ctx, templateNamespace, substrateSpec.BaseTemplateName)
		if getErr != nil {
			return getErr
		}
		if baseTemplate == nil {
			return fmt.Errorf(
				"substrate infrastructure ActorTemplate %s/%s is not found",
				templateNamespace, substrateSpec.BaseTemplateName,
			)
		}
		rendered, renderErr := r.renderSubstrateRuntimeTemplate(
			pool, cfg, baseTemplate, templateNamespace, actorID, bootstrapNonce, bootstrapPublicKey,
		)
		if renderErr != nil {
			return renderErr
		}
		desired = rendered
		desiredLoaded = true
		return nil
	}
	if actor != nil && derivedTemplate == nil && pool.Spec.DesiredReplicas != 0 {
		// A pre-existing or partially reconciled actor still needs a frozen,
		// controller-owned placement record before credential-safe teardown.
		// Materializing the already-rendered desired template does not admit or
		// seed the actor; it only prevents cleanup from depending on the mutable
		// infrastructure base template.
		if err := loadDesired(); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if err := r.createSubstrateActorTemplate(ctx, pool, desired.object); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		derivedTemplate, err = r.getSubstrateActorTemplate(ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName))
		if err != nil || derivedTemplate == nil {
			if err == nil {
				err = fmt.Errorf("created RuntimePool substrate actor template is not readable yet")
			}
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		templateOwned = substrateRuntimeTemplateOwnedByPool(derivedTemplate, expectedTemplate)
		if templateOwned {
			templateRevision, templateIntegrityErr = substrateRuntimeTemplateIntegrity(derivedTemplate)
			if templateIntegrityErr == nil {
				templateFence, err = substrateRuntimeTemplateFence(derivedTemplate)
				if err != nil {
					return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
				}
			}
		}
		if templateOwned && templateIntegrityErr == nil && templateRevision == desired.revision &&
			strings.TrimSpace(pool.Annotations[substrateActorWorkerPlacementAnnotation]) == "" {
			// A deterministic actor may predate the controller-owned template.
			// Freeze the exact rendered placement as the cleanup allowlist only
			// after the newly observed template matches that render byte-for-byte.
			if err := r.recordSubstrateRuntimePoolWorkerPlacement(ctx, pool, desired.object); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
	}
	if actor != nil && strings.TrimSpace(pool.Annotations[substrateActorWorkerPlacementAnnotation]) == "" &&
		templateOwned && templateIntegrityErr == nil && templateFence != "" {
		var placementTemplate *unstructured.Unstructured
		storedFence := strings.TrimSpace(pool.Annotations[substrateActorTemplateFenceAnnotation])
		switch storedFence {
		case templateFence:
			// The live template is still the exact object fenced at actor creation.
			placementTemplate = derivedTemplate
		case "":
			// Upgrade pools created before template fencing by comparing the live
			// template with an independently rendered desired template. Never use
			// an unfenced mutable template directly as the teardown allowlist.
			if err := loadDesired(); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
			if templateRevision == desired.revision {
				placementTemplate = desired.object
			}
		}
		if placementTemplate != nil {
			if err := r.recordSubstrateRuntimePoolWorkerPlacement(ctx, pool, placementTemplate); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
	}
	if actor != nil && strings.TrimSpace(pool.Annotations[substrateActorWorkerPlacementAnnotation]) == "" &&
		strings.TrimSpace(pool.Annotations[substrateActorTemplateFenceAnnotation]) == "" {
		// An unfenced actor whose deployed template no longer matches the
		// independently rendered desired template has no provable teardown
		// placement. Close admission without attempting an unsafe Pod deletion.
		if desiredLoaded && templateRevision != desired.revision {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("cannot prove the worker placement of an unfenced RuntimePool substrate actor"))
		}
	}

	replicas := int32(0)
	if actor != nil {
		replicas = 1
	}
	status := r.baseRuntimePoolStatus(pool, replicas)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionPodSecurityReady, metav1.ConditionUnknown, "ProviderIsolationPending", "provider-owned gVisor isolation is awaiting controller-enforced egress confinement")
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionQuotaReady, metav1.ConditionTrue, "ResourcesAdmitted", "provider worker capacity admitted the runtime workload")

	if strings.TrimSpace(pool.Annotations[substrateActorRecyclingAnnotation]) != "" ||
		(actor == nil && substrateActorWorkloadProofRequired(pool, actorID)) {
		// A staged teardown is in progress; resume it before any other
		// decision so a half-recycled or provider-orphaned workload can never
		// be admitted, seeded, or replaced before its exact Pod is absent.
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "recycling the exact provider actor without checkpointing supervisor memory"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if pool.Spec.DesiredReplicas == 0 && actor == nil {
		if substrateActorConsensuallySuspended(pool, actorID) {
			// The consensually suspended actor no longer exists, so no
			// checkpoint can be resumed. Clearing the stale consent keeps the
			// workspace adapter from reporting a Suspended workspace whose
			// data is gone; the pool then settles Stopped without consent and
			// the adapter fails the suspension closed instead of silently
			// re-materializing empty data on the next continuation.
			if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorSuspendedAnnotation, ""); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Once the exact actor workload is independently proven absent, mutable
		// derived-template ownership cannot block the Stopped barrier required
		// before deletion finalization checks and removes provider resources.
		return r.reconcileSubstrateRuntimePoolScaleDown(ctx, pool, cfg, control, derivedTemplate, nil, actorID, routeHost, status)
	}
	if pool.Spec.DesiredReplicas == 0 && derivedTemplate == nil {
		return r.reconcileSubstrateRuntimePoolScaleDownWithoutTemplate(ctx, pool, control, actorID, status)
	}
	if !templateOwned {
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "controller-derived substrate ActorTemplate does not carry the exact RuntimePool ownership identity"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	booted := pool.Annotations[substrateActorBootedAnnotation] == actorID
	if actor != nil && !substrateActorMatchesRuntimeTemplate(
		actor,
		actorID,
		templateNamespace,
		runtimePoolSubstrateTemplateName(cfg.baseName),
	) {
		// Actor IDs are deterministic, so an existing actor is not proof of
		// ownership. A template mismatch proves this actor was not created from
		// the controller-owned workload, so never resume, recycle, probe, or
		// credential-seed it.
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "provider actor does not use the controller-derived runtime template; refusing to modify the foreign actor"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	if actor != nil && templateIntegrityErr != nil {
		// A same-name template with valid ownership labels is still not trusted
		// when its declared revision does not match its observed contents. Never
		// resume, probe, or credential-seed an actor booted from that workload.
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "controller-derived substrate ActorTemplate contents do not match their declared revision; recycling the exact actor before credential bootstrap"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if actor != nil && pool.Spec.DesiredReplicas != 0 &&
		substrateActorAwaitingDataResume(pool, actor, actorID) &&
		templateOwned && derivedTemplate != nil {
		// A consensual cold resume rebuilds the workload from the current
		// derived template, so the rotated one-time bootstrap material must be
		// rendered into it before ResumeActor — replacing the template in
		// place instead of recycling the actor, whose data snapshot is the
		// whole point of the suspension. The actor is suspended: no live
		// credentialed process can observe the template transition.
		if err := loadDesired(); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if templateIntegrityErr != nil || templateRevision != desired.revision {
			// The in-place refresh is licensed only for rotated one-time
			// bootstrap material. Any other change — placement, runsc
			// configuration, volumes, image — means the data checkpoint was
			// taken under a different infrastructure contract, and resuming
			// the snapshotted actor under it would bypass the recycle path
			// that every other template change takes; recycle fail-closed.
			bootstrapOnly := false
			if templateIntegrityErr == nil {
				deployedNeutral, deployedErr := substrateRuntimeTemplateBootstrapNeutralRevision(derivedTemplate)
				desiredNeutral, desiredErr := substrateRuntimeTemplateBootstrapNeutralRevision(desired.object)
				bootstrapOnly = deployedErr == nil && desiredErr == nil && deployedNeutral == desiredNeutral
			}
			if !bootstrapOnly {
				// Recycling destroys the data checkpoint: record the terminal
				// loss FIRST so the workspace adapter fails the linked
				// workspace closed instead of publishing Ready over a fresh
				// actor with a re-materialized baseline.
				if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, runtimePoolWorkspaceResumeLostAnnotation,
					"suspended actor recycled for a non-bootstrap template change; its data checkpoint is destroyed"); err != nil {
					return ctrl.Result{}, err
				}
				return r.recycleSubstrateActorForInstanceMismatch(
					ctx, pool, control, actorID, status,
					"derived runtime template changed beyond bootstrap material while the actor was suspended; recycling the exact actor because its data checkpoint no longer matches the infrastructure contract",
				)
			}
			if err := r.updateSubstrateActorTemplate(ctx, derivedTemplate, desired.object); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "refreshing the derived runtime template with rotated bootstrap material before cold resume"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		if strings.TrimSpace(pool.Annotations[substrateActorTemplateFenceAnnotation]) != templateFence {
			if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorTemplateFenceAnnotation, templateFence); err != nil {
				return ctrl.Result{}, err
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "re-fencing the refreshed derived runtime template before cold resume"
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
	}
	if actor != nil && strings.TrimSpace(pool.Annotations[substrateActorTemplateFenceAnnotation]) != templateFence {
		return r.recycleSubstrateActorForInstanceMismatch(
			ctx, pool, control, actorID, status,
			"controller-derived substrate ActorTemplate changed after validation; recycling the exact actor before credential bootstrap",
		)
	}
	if actor != nil && booted && substrateActorConsensuallySuspended(pool, actorID) {
		// A restart persisted the suspension consent but not the separate
		// boot-identity discard. Finish that transition idempotently before
		// the foreign-suspension guard runs: the recorded checkpoint is ours,
		// and a stale boot record must never classify it as provider-initiated
		// and recycle the actor's valid data snapshot, nor block the
		// awaiting-data-resume predicate that requires the boot record gone.
		for _, annotation := range []string{substrateActorBootedAnnotation, substrateActorCredentialSeededAnnotation} {
			if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, annotation, ""); err != nil {
				return ctrl.Result{}, err
			}
		}
		booted = false
	}
	if actor != nil && booted && (actor.SuspendedOrSuspending() ||
		(!substrateRuntimePoolSuspendCapable(pool) && actor.SnapshotObserved) ||
		(pool.Spec.DesiredReplicas != 0 && actor.Crashed())) {
		// A booted supervisor holds live pool and provider-proxy credentials in
		// process memory; a provider-side crash, suspension, or snapshot cannot
		// be reused safely. Recycle the exact instance and rotate its one-time
		// bootstrap material before replacement. Data-only-suspendable pools
		// expect snapshot records after a requested suspension (which always
		// clears the boot record first), so a snapshot alone is not evidence of
		// a provider-initiated suspension there — but a booted actor observed
		// suspending without a recorded request still is.
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "provider suspended or snapshotted the runtime actor; suspension is prohibited for ACP RuntimeSessions and the exact instance is being recycled"
		if actor.Crashed() {
			status.Message = "provider crashed the runtime actor; the exact instance is being recycled before credential rotation"
		}
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if pool.Spec.DesiredReplicas == 0 && substrateWorkspaceSuspendRequested(pool) {
		// Every fail-closed pre-check above already ran: the actor exists, is
		// template-matched, and the deployed derived template passed ownership,
		// integrity, and fence validation.
		return r.reconcileSubstrateRuntimePoolSuspend(ctx, pool, cfg, control, derivedTemplate, actor, actorID, routeHost, status)
	}
	if pool.Spec.DesiredReplicas == 0 {
		return r.reconcileSubstrateRuntimePoolScaleDown(ctx, pool, cfg, control, derivedTemplate, actor, actorID, routeHost, status)
	}
	if !r.SubstrateEnabled {
		if actor == nil {
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		}
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "substrate provider is disabled; existing RuntimePool admission remains closed until it scales to zero"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if err := loadDesired(); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	rolloutPending := derivedTemplate != nil && (templateIntegrityErr != nil || templateRevision != desired.revision)
	if rolloutPending {
		return r.reconcileSubstrateRuntimePoolRollout(ctx, pool, cfg, control, derivedTemplate, actor, actorID, routeHost, desired, status)
	}

	if derivedTemplate == nil {
		if err := r.createSubstrateActorTemplate(ctx, pool, desired.object); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if derivedTemplate, err = r.getSubstrateActorTemplate(ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName)); err != nil || derivedTemplate == nil {
			if err == nil {
				err = fmt.Errorf("created RuntimePool substrate actor template is not readable yet")
			}
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if !substrateRuntimeTemplateOwnedByPool(derivedTemplate, desired.object) {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("created RuntimePool substrate ActorTemplate does not carry the exact RuntimePool ownership identity"))
		}
		createdRevision, integrityErr := substrateRuntimeTemplateIntegrity(derivedTemplate)
		if integrityErr != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, integrityErr)
		}
		if createdRevision != desired.revision {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("created RuntimePool substrate ActorTemplate does not match the desired runtime revision"))
		}
		templateFence, err = substrateRuntimeTemplateFence(derivedTemplate)
		if err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
	}
	networkPoliciesChanged, err := r.ensureSubstrateRuntimePoolNetworkPolicies(ctx, pool, cfg, derivedTemplate)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionPodSecurityReady, metav1.ConditionTrue, "ProviderIsolated", "provider-owned gVisor isolation and controller-enforced default-deny egress host the runtime workload")
	if networkPoliciesChanged && actor != nil {
		// A successful API write is not proof that the CNI dataplane has
		// observed the new policy yet. Existing Actors must cross a reconcile
		// boundary before any credential bootstrap or authenticated probe.
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "waiting for provider WorkerPool egress confinement to become observable before credential bootstrap"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if err := r.pruneStaleSubstrateRuntimePoolSecrets(ctx, pool, cfg, authSecret.Name, providerSecret.Name); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}

	if actor == nil {
		if pool.Annotations[substrateActorTemplateFenceAnnotation] != templateFence {
			if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorTemplateFenceAnnotation, templateFence); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.recordSubstrateRuntimePoolWorkerPlacement(ctx, pool, derivedTemplate); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if err := r.verifySubstrateRuntimeTemplateFence(
			ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName), templateFence,
		); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		createdActor, err := control.CreateActor(ctx, actorID, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName))
		if err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if !substrateActorMatchesRuntimeTemplate(
			createdActor,
			actorID,
			templateNamespace,
			runtimePoolSubstrateTemplateName(cfg.baseName),
		) {
			return r.finishRuntimePoolResourceFailure(
				ctx,
				pool,
				cfg,
				fmt.Errorf("provider returned an actor that does not use the controller-derived runtime template; refusing to resume the foreign actor"),
			)
		}
		if err := r.verifySubstrateRuntimeTemplateFence(
			ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName), templateFence,
		); err != nil {
			return r.recycleSubstrateActorForInstanceMismatch(
				ctx, pool, control, actorID, status,
				"controller-derived substrate ActorTemplate changed during actor creation; recycling the exact actor before credential bootstrap",
			)
		}
		// Boot from scratch exactly once per actor lifetime: a supervisor
		// lifetime is exactly one boot, so the fence boot ID is never resumed
		// from a snapshot.
		resumedActor, err := control.ResumeActor(ctx, actorID, true)
		if err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if err := r.verifySubstrateRuntimeTemplateFence(
			ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName), templateFence,
		); err != nil {
			return r.recycleSubstrateActorForInstanceMismatch(
				ctx, pool, control, actorID, status,
				"controller-derived substrate ActorTemplate changed while the actor booted; recycling the exact actor before credential bootstrap",
			)
		}
		if resumedActor != nil && resumedActor.Running() {
			if err := r.verifySubstrateActorWorkerPlacement(ctx, pool, resumedActor); err != nil {
				if errors.Is(err, errSubstrateWorkerPodFenceConflict) {
					return r.recycleSubstrateActorForInstanceMismatch(
						ctx, pool, control, actorID, status,
						"provider actor physical worker changed; recycling it before credential bootstrap rotation",
					)
				}
				r.applyProviderRuntimePoolColdStartStatus(pool, &status, sanitizeRuntimePoolMessage("provider actor placement is not ready: "+err.Error()))
				return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
			}
			if err := r.setSubstrateActorBootedAnnotation(ctx, pool, actorID); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, "provider actor is booting the runtime workload")
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !booted {
		bootCandidate := actor
		if !actor.Running() {
			bootFromScratch := true
			if substrateActorConsensuallySuspended(pool, actorID) {
				// Cold resume from the data-only checkpoint: the DurableDir
				// workspace is restored while the supervisor process still
				// boots from scratch under the template's fromData: ColdBoot
				// policy, with a fresh boot identity and a repeated signed
				// credential bootstrap.
				if err := verifySubstrateDeployedDataSnapshotPolicy(derivedTemplate); err != nil {
					return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
				}
				bootFromScratch = false
			}
			bootCandidate, err = control.ResumeActor(ctx, actorID, bootFromScratch)
			if err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
		if bootCandidate == nil || !bootCandidate.Running() {
			r.applyProviderRuntimePoolColdStartStatus(pool, &status, "waiting for the provider actor workload to run")
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		if err := r.verifySubstrateActorWorkerPlacement(ctx, pool, bootCandidate); err != nil {
			if errors.Is(err, errSubstrateWorkerPodFenceConflict) {
				return r.recycleSubstrateActorForInstanceMismatch(
					ctx, pool, control, actorID, status,
					"provider actor physical worker changed; recycling it before credential bootstrap rotation",
				)
			}
			r.applyProviderRuntimePoolColdStartStatus(pool, &status, sanitizeRuntimePoolMessage("provider actor placement is not ready: "+err.Error()))
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		if err := r.setSubstrateActorBootedAnnotation(ctx, pool, actorID); err != nil {
			return ctrl.Result{}, err
		}
		// A consensual suspension is consumed by exactly one resume: with the
		// fresh boot recorded, the checkpoint record retires so a later
		// replacement actor can never be resumed from stale data. The
		// resume-in-progress proof takes its place until the authenticated
		// Serving admission succeeds, keeping any interim recycle terminal.
		if substrateActorConsensuallySuspended(pool, actorID) {
			if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorResumingAnnotation, actorID); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorSuspendedAnnotation, ""); err != nil {
			return ctrl.Result{}, err
		}
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, "provider actor boot is being recorded before exact-instance admission")
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !actor.Running() {
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, "waiting for the provider actor workload to run")
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if err := r.verifySubstrateActorWorkerPlacement(ctx, pool, actor); err != nil {
		if errors.Is(err, errSubstrateWorkerPodFenceConflict) {
			return r.recycleSubstrateActorForInstanceMismatch(
				ctx, pool, control, actorID, status,
				"provider actor physical worker changed; recycling it before credential bootstrap rotation",
			)
		}
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, sanitizeRuntimePoolMessage("provider actor placement is not ready: "+err.Error()))
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if err := r.verifySubstrateRuntimeTemplateFence(
		ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName), templateFence,
	); err != nil {
		return r.recycleSubstrateActorForInstanceMismatch(
			ctx, pool, control, actorID, status,
			"controller-derived substrate ActorTemplate changed while the actor booted; recycling the exact actor before credential bootstrap",
		)
	}

	// Seed the booted supervisor's credentials before the authenticated
	// probe. The PUT is idempotent for identical payloads; a payload conflict
	// means another party seeded this workload first, so the exact instance is
	// recycled instead of trusted.
	syntheticPod := substrateSyntheticInstancePod(pool, cfg, actor, actorID, routeHost)
	workerPodFence, err := substrateRuntimePoolWorkerPodFenceFromAnnotation(pool)
	if err != nil || workerPodFence == nil {
		if err == nil {
			err = fmt.Errorf("provider actor exact worker Pod fence is not recorded")
		}
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if err := r.bindWorkspaceRuntimePoolBootstrapInstance(ctx, pool, authSecret, workerPodFence.UID); err != nil {
		if errors.Is(err, errRuntimePoolBootstrapInstanceConflict) {
			return r.recycleSubstrateActorForInstanceMismatch(
				ctx, pool, control, actorID, status,
				"provider actor physical worker changed; recycling it before credential bootstrap rotation",
			)
		}
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	bootstrapAlreadyComplete, err := r.seedSubstrateSupervisorCredentials(ctx, routeHost, bootstrapNonce, authSecret, providerSecret)
	if err != nil {
		if errors.Is(err, errSubstrateCredentialConflict) {
			if recycleErr := r.recycleSubstrateActor(ctx, pool, control, actorID); recycleErr != nil {
				return ctrl.Result{}, recycleErr
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "provider actor was credential-seeded by another party; recycling the exact instance"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		r.applyProviderRuntimePoolColdStartStatus(
			pool,
			&status,
			sanitizeRuntimePoolMessage("credential bootstrap is not complete: "+err.Error()),
		)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if err := r.verifySubstrateRuntimeTemplateFence(
		ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName), templateFence,
	); err != nil {
		return r.recycleSubstrateActorForInstanceMismatch(
			ctx, pool, control, actorID, status,
			"controller-derived substrate ActorTemplate changed during credential bootstrap; recycling the exact actor",
		)
	}
	if bootstrapAlreadyComplete && pool.Annotations[substrateActorCredentialSeededAnnotation] != actorID {
		probe, probeErr := r.supervisorClientForPool(pool).Probe(
			ctx,
			runtimePoolInstanceEndpoint(pool, syntheticPod),
			string(authSecret.Data[runtimePoolControllerTokenKey]),
			authSecret.Data[runtimePoolCapabilitySecretKey],
		)
		if probeErr == nil {
			_, probeErr = validateRuntimePoolProbeForRollout(pool, cfg, syntheticPod, probe, r.now())
		}
		if probeErr != nil {
			r.applyProviderRuntimePoolColdStartStatus(
				pool,
				&status,
				"provider actor credential bootstrap route is unavailable or completion is not yet authenticated; retrying",
			)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
	}
	if pool.Annotations[substrateActorCredentialSeededAnnotation] != actorID {
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorCredentialSeededAnnotation, actorID); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileRuntimePoolServing(ctx, pool, cfg, []corev1.Pod{*syntheticPod}, []corev1.Pod{*syntheticPod}, authSecret, status)
}

// reconcileSubstrateRuntimePoolMissingAuthSecret completes the existing
// exact-workload-proving actor recycle before unpublishing a lost Secret
// binding. Normal reconciliation then creates fresh credentials and applies
// the consumed-bootstrap rotation barrier before creating another Actor.
func (r *RuntimePoolReconciler) reconcileSubstrateRuntimePoolMissingAuthSecret(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) (ctrl.Result, error) {
	control, err := r.substrateActorControlForCleanup()
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	defer control.Close() //nolint:errcheck // best-effort connection teardown

	actorID := runtimePoolSubstrateActorID(cfg.baseName)
	actor, err := control.GetActor(ctx, actorID)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	replicas := int32(0)
	if actor != nil {
		replicas = 1
	}
	if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
		return ctrl.Result{}, err
	}

	status := r.baseRuntimePoolStatus(pool, replicas)
	status.ActiveInstance = nil
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, "bound runtime credentials are unavailable")
	if strings.TrimSpace(pool.Annotations[substrateActorRecyclingAnnotation]) != "" {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.Message = "bound runtime credentials disappeared; recycling the exact provider actor before credential rotation"
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if err := r.patchRuntimePoolAnnotation(
		ctx,
		pool,
		runtimePoolPrivateAuthSecretBindingAnnotation(cfg.controllerEpoch),
		"",
	); err != nil {
		return ctrl.Result{}, err
	}
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
	if pool.Spec.DesiredReplicas == 0 {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	}
	status.CurrentReplicas = 0
	status.Message = "provider actor absence is proven; rotating credentials before any replacement"
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// reconcileSubstrateRuntimePoolScaleDownWithoutTemplate stops an actor whose
// controller-derived template was removed out of band. Authenticated drain can
// no longer reconstruct the deployed generation, so cleanup falls back to the
// persisted actor placement and exact worker-Pod fences after controller-owned
// work is quiescent. teardownSubstrateActor remains fail-closed if those fences
// are missing, invalid, or no longer identify the actor's workload.
func (r *RuntimePoolReconciler) reconcileSubstrateRuntimePoolScaleDownWithoutTemplate(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	control workspace.SubstrateRuntimeActorControl,
	actorID string,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, "runtime pool is scaling down")
	if !runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity) ||
		!runtimePoolRolloutControllerWorkIsQuiescent(pool.Status.Capacity) {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.Message = "waiting for controller reservations or finalization work before stopping a provider actor with a missing runtime template"
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
		return ctrl.Result{}, err
	}
	status.ActiveInstance = nil
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "runtime template is unavailable; stopping the exact provider actor from persisted workload fences"
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// errSubstrateCredentialConflict marks a seeding payload conflict: the
// supervisor was already seeded with different credentials.
var (
	errSubstrateCredentialConflict        = errors.New("supervisor credentials were seeded by another party")
	errSubstrateCredentialAlreadyComplete = errors.New("supervisor credential bootstrap is already complete")
	errSubstrateWorkerPodFenceConflict    = errors.New("provider actor worker Pod does not match the recorded exact Pod fence")
)

// seedSubstrateSupervisorCredentials performs the one-time, idempotent
// credential bootstrap PUT against the exact actor route host.
func (r *RuntimePoolReconciler) seedSubstrateSupervisorCredentials(
	ctx context.Context,
	routeHost, nonce string,
	authSecret, providerSecret *corev1.Secret,
) (bool, error) {
	request := harnessv2.CredentialBootstrapRequest{
		ControllerToken:  strings.TrimSpace(string(authSecret.Data[runtimePoolControllerTokenKey])),
		CapabilitySecret: strings.TrimSpace(string(authSecret.Data[runtimePoolCapabilitySecretKey])),
		ProviderToken:    strings.TrimSpace(string(providerSecret.Data[runtimePoolProviderTokenKey])),
	}
	if err := request.Validate(); err != nil {
		return false, fmt.Errorf("pool credentials are incomplete: %w", err)
	}
	if r.SubstrateCredentialSeeder != nil {
		err := r.SubstrateCredentialSeeder(ctx, routeHost, nonce, authSecret.Data[runtimePoolBootstrapSigningSeedKey], request)
		if errors.Is(err, errSubstrateCredentialAlreadyComplete) {
			return true, nil
		}
		return false, err
	}
	httpClient, err := r.substrateSupervisorHTTPClient()
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	seedCtx, cancel := context.WithTimeout(ctx, runtimePoolProbeTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		seedCtx, http.MethodPut,
		urlSchemeHTTP+"://"+routeHost+harnessv2.CredentialBootstrapPath,
		bytes.NewReader(payload),
	)
	if err != nil {
		return false, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(harnessv2.CredentialBootstrapNonceHeader, nonce)
	signature, err := harnessv2.SignCredentialBootstrap(authSecret.Data[runtimePoolBootstrapSigningSeedKey], nonce, payload)
	if err != nil {
		return false, fmt.Errorf("sign credential bootstrap request: %w", err)
	}
	httpRequest.Header.Set(harnessv2.CredentialBootstrapSignatureHeader, signature)
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return false, err
	}
	defer response.Body.Close() //nolint:errcheck // response body is unused
	switch response.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return false, nil
	case http.StatusConflict:
		return false, errSubstrateCredentialConflict
	case http.StatusNotFound:
		// A logical-router 404 is ambiguous: the actor route may not be published
		// yet, or the supervisor may have completed bootstrap and replaced the
		// phase server. An authenticated probe may prove the latter; otherwise
		// reconciliation remains closed and retries the bootstrap route.
		return true, nil
	default:
		return false, fmt.Errorf("credential bootstrap returned status %d", response.StatusCode)
	}
}

func (r *RuntimePoolReconciler) recycleSubstrateActorForInstanceMismatch(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	control workspace.SubstrateRuntimeActorControl,
	actorID string,
	status corev1alpha1.RuntimePoolStatus,
	message string,
) (ctrl.Result, error) {
	if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
		return ctrl.Result{}, err
	}
	status.ActiveInstance = nil
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = message
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// substrateSyntheticInstancePod adapts the provider actor into the shared
// exact-instance selection shape. Its profile and provider-token-generation
// annotations mirror the pool intent rather than independently observed Pod
// state; the authoritative verification for Substrate is the derived-template
// revision rollout plus the authenticated probe fence.
func substrateSyntheticInstancePod(
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	actor *workspace.SubstrateRuntimeActor,
	actorID, routeHost string,
) *corev1.Pod {
	name := actor.PodName
	if name == "" {
		name = actorID
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// The synthetic Pod namespace is the controller-owned RuntimePool
			// namespace containing the epoch-scoped auth Secrets. Provider worker
			// placement stays internal to Substrate actor control and teardown.
			Namespace: cfg.namespace,
			Name:      name,
			UID:       types.UID(substrateActorInstanceUID(actorID)),
			Annotations: map[string]string{
				runtimePoolProfileAnnotation:                 pool.Spec.Runtime.Profile.Digest,
				runtimePoolProviderTokenGenerationAnnotation: cfg.providerProxy.tokenGeneration,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: routeHost},
	}
}

// reconcileSubstrateRuntimePoolRollout mirrors the Recreate rollout barriers:
// authenticated drain of the exact old instance validated against the deployed
// derived-template identity, persisted quiescence, actor deletion, and only
// then the new immutable template.
//
//nolint:gocyclo // The rollout barriers intentionally mirror the audited Deployment path.
func (r *RuntimePoolReconciler) reconcileSubstrateRuntimePoolRollout(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	control workspace.SubstrateRuntimeActorControl,
	derivedTemplate *unstructured.Unstructured,
	actor *workspace.SubstrateRuntimeActor,
	actorID, routeHost string,
	desired substrateRuntimeTemplateRender,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	status.Message = "runtime template changed; admission is closed before provider actor replacement"
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)

	if actor == nil {
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		oldNamespace, _, err := substrateRuntimePoolWorkerPlacementFromTemplate(derivedTemplate)
		if err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		newNamespace, _, err := substrateRuntimePoolWorkerPlacementFromTemplate(desired.object)
		if err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if oldNamespace != newNamespace {
			remaining, deleteErr := r.deleteSubstrateRuntimePoolNetworkPoliciesForTemplate(ctx, pool, cfg, derivedTemplate)
			if deleteErr != nil {
				return ctrl.Result{}, deleteErr
			}
			if remaining {
				status.ActiveInstance = nil
				status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
				status.Message = "old provider worker egress policies are terminating before the runtime template changes"
				return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
			}
		}
		if err := r.updateSubstrateActorTemplate(ctx, derivedTemplate, desired.object); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		status.ActiveInstance = nil
		if pool.Spec.DesiredReplicas == 0 {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
			status.Message = "new runtime template is staged with no provider actor demand"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "RolloutConverged", status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		status.Message = "old provider actor terminated; starting the new immutable runtime template"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStarting, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if pool.Status.ActiveInstance == nil {
		if !runtimePoolRolloutControllerWorkIsQuiescent(status.Capacity) {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
			status.Message = "waiting for controller reservations or finalization work before replacing an unadmitted provider actor"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonDraining, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "replacing an unadmitted provider actor before applying the new template"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !actor.Running() {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "cannot authenticate the previous active runtime instance before actor replacement"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	validationPool, validationConfig, deployedAuthSecret, err := r.substrateRuntimePoolDeployedValidationTarget(
		ctx, pool, cfg, derivedTemplate,
	)
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	deployedSyntheticPod := substrateSyntheticInstancePod(validationPool, validationConfig, actor, actorID, routeHost)
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(pool, deployedSyntheticPod), string(deployedAuthSecret.Data[runtimePoolControllerTokenKey]), deployedAuthSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated rollout status probe failed: %w", err))
	}
	active, err := validateRuntimePoolProbeForRollout(validationPool, validationConfig, deployedSyntheticPod, probe, r.now())
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	if runtimePoolSupervisorRestartedInPlace(pool.Status.ActiveInstance, active) {
		return r.reconcileRuntimePoolInPlaceSupervisorRestart(ctx, pool, nil, deployedSyntheticPod, active, status)
	}
	if !runtimePoolRolloutActiveInstanceMatches(pool.Status.ActiveInstance, active) {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated runtime identity changed before rollout drain"))
	}
	status.ActiveInstance = active
	applyRuntimePoolProbeCapacity(&status, cfg, probe)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionTrue, "PodScheduled", "selected provider actor remains placed during rollout drain")

	if !probe.Status.Drain.Requested {
		reason := "runtime_pool_rollout_" + runtimePoolShortRevision(desired.revision)
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(pool, deployedSyntheticPod),
			string(deployedAuthSecret.Data[runtimePoolControllerTokenKey]),
			deployedAuthSecret.Data[runtimePoolCapabilitySecretKey],
			probe.Status,
			reason,
		); err != nil {
			return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated rollout drain request failed: %w", err))
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.Message = runtimePoolMessageRolloutDrainRequested
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonDraining, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if !runtimePoolRolloutProbeIsQuiescent(status.Capacity, probe.Status) {
		if r.runtimePoolRolloutTimedOut(pool) {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "timed out waiting for authenticated rollout drain barriers; preserving the old provider actor"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, runtimePoolRolloutReasonTimedOut, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.Message = runtimePoolMessageRolloutSettling
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonDraining, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if !runtimePoolRolloutQuiescencePersisted(pool) {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleQuiescent
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageRolloutQuiescent
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonQuiescent, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
		return ctrl.Result{}, err
	}
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent old provider actor is stopping before the template changes"
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// reconcileSubstrateRuntimePoolScaleDown mirrors the scale-to-zero barriers:
// authenticated drain, observed quiescence, a persisted Quiescent barrier,
// then direct actor deletion. Suspension is never used as a stop mechanism.
//
//nolint:gocyclo // The scale-down barriers intentionally mirror the audited Deployment path.
func (r *RuntimePoolReconciler) reconcileSubstrateRuntimePoolScaleDown(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	control workspace.SubstrateRuntimeActorControl,
	derivedTemplate *unstructured.Unstructured,
	actor *workspace.SubstrateRuntimeActor,
	actorID, routeHost string,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, "runtime pool is scaling down")

	if actor == nil {
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
		status.ActiveInstance = nil
		status.Message = runtimePoolMessageStopped
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionUnknown, "ScaledToZero", status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "ScaledToZero", status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if suspendPending, err := r.linkedWorkspaceSuspendIntentPending(ctx, pool); err != nil {
		return ctrl.Result{}, err
	} else if suspendPending {
		// The linked workspace was patched to Suspended but the adapter has
		// not recorded the pool's suspension intent yet (a restart or the
		// idle reaper's scale-to-zero can land first). Ordinary teardown here
		// would delete the actor and destroy the data the class froze a
		// Suspend action for; wait for the durable intent instead.
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.Message = "linked workspace requests suspension; waiting for the durable pool suspension intent before any teardown"
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if pool.Status.ActiveInstance == nil {
		if !runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity) {
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
			status.Message = "waiting for controller reservations or finalization work before stopping an unadmitted provider actor"
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "stopping a provider actor that never became active"
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !actor.Running() {
		status.ActiveInstance = nil
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		if !runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity) {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.Message = runtimePoolMessageDrainUnauthenticated
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.Message = runtimePoolMessageDrainUnauthenticated
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	validationPool, validationConfig, authSecret, err := r.substrateRuntimePoolDeployedValidationTarget(
		ctx, pool, cfg, derivedTemplate,
	)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	syntheticPod := substrateSyntheticInstancePod(validationPool, validationConfig, actor, actorID, routeHost)
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(validationPool, syntheticPod), string(authSecret.Data[runtimePoolControllerTokenKey]), authSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage("authenticated drain status probe failed: " + err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	active, err := validateRuntimePoolProbe(validationPool, validationConfig, syntheticPod, probe, r.now())
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage(err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	status.ActiveInstance = active
	applyRuntimePoolProbeCapacity(&status, cfg, probe)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionTrue, "PodScheduled", "selected provider actor is placed")
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "ExactInstanceReady", "selected provider actor and supervisor profile are ready")

	if !probe.Status.Drain.Requested {
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(validationPool, syntheticPod),
			string(authSecret.Data[runtimePoolControllerTokenKey]),
			authSecret.Data[runtimePoolCapabilitySecretKey],
			probe.Status,
			"runtime_pool_scale_to_zero",
		); err != nil {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = sanitizeRuntimePoolMessage("authenticated drain request failed: " + err.Error())
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainRequested
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if !runtimePoolProbeIsQuiescent(pool.Status.Capacity, probe.Status) {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainSettling
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleQuiescent
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainQuiescent
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
		return ctrl.Result{}, err
	}
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent provider actor is stopping"
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// reconcileSubstrateRuntimePoolSuspend drives a requested data-only cold
// suspension: the same authenticated drain barriers as scale-down, then a
// consensual provider checkpoint of only the DurableDir workspace volume.
// Consent is persisted before the provider call so a controller restart
// resumes the same suspension, and the boot record is cleared first so the
// provider-initiated-suspension guard never mistakes this checkpoint for a
// foreign one. Process memory is never captured: the deployed template's
// exact data-only snapshot policy is re-proven at this boundary.
//
//nolint:gocyclo // The suspension state machine keeps every barrier and fail-closed branch auditable together.
func (r *RuntimePoolReconciler) reconcileSubstrateRuntimePoolSuspend(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	control workspace.SubstrateRuntimeActorControl,
	derivedTemplate *unstructured.Unstructured,
	actor *workspace.SubstrateRuntimeActor,
	actorID, routeHost string,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, "runtime pool is suspending its data-only workspace")

	if substrateActorConsensuallySuspended(pool, actorID) {
		switch {
		case actor.Suspended():
			// Finalize: the workload Pod is gone and only DurableDir data was
			// checkpointed. Clear the exact-instance fences so cold resume
			// re-proves fresh placement and bootstrap material.
			for _, annotation := range []string{
				substrateActorWorkerPodFenceAnnotation,
				substrateActorReplacementWorkerPodFenceAnnotation,
				substrateActorWorkloadAbsentAnnotation,
			} {
				if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, annotation, ""); err != nil {
					return ctrl.Result{}, err
				}
			}
			status.ActiveInstance = nil
			// The worker workload is gone: only the suspended actor object
			// remains, so the pool reports a completed scale-to-zero.
			status.CurrentReplicas = 0
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "provider actor is suspended with a data-only workspace checkpoint; cold resume restores the logical session with a fresh boot"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "WorkspaceSuspended", status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		case actor.Suspending():
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "waiting for the provider data-only checkpoint to settle"
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		default:
			if !actor.Running() {
				// The actor crashed (or otherwise left the running state)
				// before the checkpoint settled: no valid suspension can be
				// replayed against it. Clear the stale consent and let the
				// plain scale-down machine settle fail-closed; the workspace
				// adapter then reports the failed suspension instead of the
				// provider rejecting the same replay forever.
				if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorSuspendedAnnotation, ""); err != nil {
					return ctrl.Result{}, err
				}
				return r.reconcileSubstrateRuntimePoolScaleDown(ctx, pool, cfg, control, derivedTemplate, actor, actorID, routeHost, status)
			}
			// A restart raced the provider call; the policy was proven before
			// consent was recorded, so repeat the idempotent suspension.
			if err := verifySubstrateDeployedDataSnapshotPolicy(derivedTemplate); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
			if _, err := control.SuspendActorForDataCheckpoint(ctx, actorID); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "requested the provider data-only checkpoint"
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
	}

	if !actor.Running() || pool.Status.ActiveInstance == nil {
		// Suspension preserves an admitted, quiescent instance. Anything else
		// has nothing coherent to checkpoint; the plain scale-down machine
		// settles it fail-closed (the workspace adapter then reports the
		// failed suspension instead of a suspended workspace).
		return r.reconcileSubstrateRuntimePoolScaleDown(ctx, pool, cfg, control, derivedTemplate, actor, actorID, routeHost, status)
	}

	validationPool, validationConfig, authSecret, err := r.substrateRuntimePoolDeployedValidationTarget(
		ctx, pool, cfg, derivedTemplate,
	)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	syntheticPod := substrateSyntheticInstancePod(validationPool, validationConfig, actor, actorID, routeHost)
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(validationPool, syntheticPod), string(authSecret.Data[runtimePoolControllerTokenKey]), authSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage("authenticated pre-suspension probe failed: " + err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	active, err := validateRuntimePoolProbe(validationPool, validationConfig, syntheticPod, probe, r.now())
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage(err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	status.ActiveInstance = active
	applyRuntimePoolProbeCapacity(&status, cfg, probe)

	if !probe.Status.Drain.Requested {
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(validationPool, syntheticPod),
			string(authSecret.Data[runtimePoolControllerTokenKey]),
			authSecret.Data[runtimePoolCapabilitySecretKey],
			probe.Status,
			"runtime_pool_workspace_suspend",
		); err != nil {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = sanitizeRuntimePoolMessage("authenticated pre-suspension drain request failed: " + err.Error())
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainRequested
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !runtimePoolProbeIsQuiescent(pool.Status.Capacity, probe.Status) {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainSettling
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent {
		// The persisted Quiescent barrier proves prompt and workspace-writer
		// settlement across a reconcile boundary before any provider mutation.
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleQuiescent
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainQuiescent
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if err := verifySubstrateDeployedDataSnapshotPolicy(derivedTemplate); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	// Persist consent, then discard the boot identity: a supervisor lifetime is
	// exactly one boot, and the provider-initiated-suspension guard must never
	// interpret this recorded checkpoint as a foreign one.
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorSuspendedAnnotation, actorID); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorBootedAnnotation, ""); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorCredentialSeededAnnotation, ""); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := control.SuspendActorForDataCheckpoint(ctx, actorID); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	status.ActiveInstance = nil
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent provider actor is checkpointing its data-only workspace"
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// recycleSubstrateActor advances the credential-safe staged teardown of the
// exact actor and, once it is fully gone, clears the boot and recycling
// records so the replacement boots from scratch. The teardown may span
// several reconciles; the recycling annotation guarantees every reconcile
// resumes it first.
func (r *RuntimePoolReconciler) recycleSubstrateActor(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	control workspace.SubstrateRuntimeActorControl,
	actorID string,
) error {
	if pool.Annotations[substrateActorResumingAnnotation] == actorID &&
		strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceResumeLostAnnotation]) == "" {
		// The actor being destroyed holds the only copy of a cold-resumed
		// workspace that never completed admission; record the terminal loss
		// BEFORE teardown so the pool can never provision a fresh actor over
		// the lost session data.
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, runtimePoolWorkspaceResumeLostAnnotation,
			"cold-resumed actor "+actorID+" was recycled before its resume completed admission"); err != nil {
			return err
		}
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorRecyclingAnnotation, actorID); err != nil {
		return err
	}
	gone, err := r.teardownSubstrateActor(ctx, pool, control, actorID)
	if err != nil {
		return err
	}
	if !gone {
		return nil
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorBootedAnnotation, ""); err != nil {
		return err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorCredentialSeededAnnotation, ""); err != nil {
		return err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorTemplateFenceAnnotation, ""); err != nil {
		return err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkerPlacementAnnotation, ""); err != nil {
		return err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkerPodFenceAnnotation, ""); err != nil {
		return err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorReplacementWorkerPodFenceAnnotation, ""); err != nil {
		return err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkloadAbsentAnnotation, ""); err != nil {
		return err
	}
	// A recreated deterministic-name actor must never resume from a
	// predecessor's data checkpoint; the consent and resume-in-progress
	// records die with the actor (the terminal loss, when one was recorded,
	// stays).
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorSuspendedAnnotation, ""); err != nil {
		return err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorResumingAnnotation, ""); err != nil {
		return err
	}
	return r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorRecyclingAnnotation, "")
}

// teardownSubstrateActor advances one stage of the credential-safe actor
// teardown and reports whether the actor is fully gone. A live workload is
// never checkpointed: its memory is destroyed first by deleting the
// single-workload provider worker Pod; once the workload is provably absent
// the actor is settled into the provider's deletable suspended state — with
// nothing left to checkpoint — and then deleted. Callers requeue while the
// teardown is in progress.
//
//nolint:gocyclo // Credential-safe teardown keeps each fail-closed workload and Actor transition auditable in one state machine.
func (r *RuntimePoolReconciler) teardownSubstrateActor(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	control workspace.SubstrateRuntimeActorControl,
	actorID string,
) (bool, error) {
	actor, err := control.GetActor(ctx, actorID)
	if err != nil {
		return false, err
	}
	if actor == nil {
		if !substrateActorWorkloadProofRequired(pool, actorID) ||
			pool.Annotations[substrateActorWorkloadAbsentAnnotation] == actorID {
			return true, nil
		}
		workloadGone, err := r.destroySubstrateActorWorkload(ctx, pool, actorID, nil)
		if err != nil {
			return false, err
		}
		if !workloadGone {
			return false, nil
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkloadAbsentAnnotation, actorID); err != nil {
			return false, err
		}
		// Cross a reconcile boundary after persisting the independent absence
		// proof before callers remove isolation, credentials, or finalizers.
		return false, nil
	}
	workspaceSpec := pool.Spec.ExecutionWorkspace
	if workspaceSpec == nil || workspaceSpec.Substrate == nil || !substrateActorMatchesRuntimeTemplate(
		actor,
		actorID,
		workspaceSpec.Substrate.BaseTemplateNamespace,
		runtimePoolSubstrateTemplateName(runtimePoolResourceName(pool.Namespace, pool.Name)),
	) {
		return false, fmt.Errorf("refusing to recycle foreign RuntimePool substrate actor %q", actorID)
	}
	if actor.Suspended() && strings.TrimSpace(actor.PodNamespace) == "" && strings.TrimSpace(actor.PodName) == "" &&
		pool.Annotations[substrateActorBootedAnnotation] != actorID &&
		pool.Annotations[substrateActorCredentialSeededAnnotation] != actorID {
		// CreateActor returns a suspended object before its first ResumeActor.
		// With no boot record, credentials, or worker placement, no workload
		// memory exists to destroy before deleting that never-booted Actor.
		if err := control.DeleteActor(ctx, actorID); err != nil {
			return false, fmt.Errorf("delete never-booted RuntimePool substrate actor: %w", err)
		}
		return true, nil
	}
	if pool.Annotations[substrateActorWorkloadAbsentAnnotation] != actorID {
		workloadGone, err := r.destroySubstrateActorWorkload(ctx, pool, actorID, actor)
		if err != nil {
			return false, err
		}
		if !workloadGone {
			return false, nil
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkloadAbsentAnnotation, actorID); err != nil {
			return false, err
		}
		// Persist the absence proof, then re-read the Actor below and prove its
		// then-current placement absent immediately before any settle operation.
		// This also protects a later reconcile that resumes from the marker.
	}

	actor, err = control.GetActor(ctx, actorID)
	if err != nil {
		return false, err
	}
	if actor == nil {
		return true, nil
	}
	if !substrateActorMatchesRuntimeTemplate(
		actor,
		actorID,
		workspaceSpec.Substrate.BaseTemplateNamespace,
		runtimePoolSubstrateTemplateName(runtimePoolResourceName(pool.Namespace, pool.Name)),
	) {
		return false, fmt.Errorf("refusing to recycle foreign RuntimePool substrate actor %q", actorID)
	}
	if (actor.Suspended() || actor.Crashed()) &&
		strings.TrimSpace(actor.PodNamespace) == "" && strings.TrimSpace(actor.PodName) == "" {
		// The prior exact workload absence proof remains sufficient once the
		// provider reports a deletable terminal state with no current placement.
		// Any reported placement still goes through the fresh proof below.
		if err := control.DeleteActor(ctx, actorID); err != nil {
			return false, fmt.Errorf("delete RuntimePool substrate actor: %w", err)
		}
		return true, nil
	}
	workloadGone, err := r.destroySubstrateActorWorkload(ctx, pool, actorID, actor)
	if err != nil {
		return false, err
	}
	if !workloadGone {
		return false, nil
	}
	if actor.Suspended() || actor.Crashed() {
		if err := control.DeleteActor(ctx, actorID); err != nil {
			return false, fmt.Errorf("delete RuntimePool substrate actor: %w", err)
		}
		return true, nil
	}
	if actor.Suspending() {
		// A provider-initiated suspension may already be in flight, but teardown
		// must still destroy and verify absence of the live credentialed workload
		// before waiting for the actor to become deletable.
		return false, nil
	}
	if _, err := control.SettleActor(ctx, actorID); err != nil {
		return false, fmt.Errorf("settle RuntimePool substrate actor: %w", err)
	}
	return false, nil
}

func (r *RuntimePoolReconciler) verifySubstrateActorWorkerPlacement(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	actor *workspace.SubstrateRuntimeActor,
) error {
	workerNamespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromAnnotation(pool)
	if err != nil {
		return err
	}
	if actor == nil {
		return fmt.Errorf("provider actor is required for worker placement verification")
	}
	namespace := strings.TrimSpace(actor.PodNamespace)
	name := strings.TrimSpace(actor.PodName)
	if namespace == "" || name == "" {
		return fmt.Errorf("provider actor worker placement is incomplete")
	}
	if namespace != workerNamespace {
		return fmt.Errorf(
			"provider actor worker namespace %s does not match infrastructure WorkerPool namespace %s",
			namespace, workerNamespace,
		)
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		return fmt.Errorf("read provider actor worker Pod %s/%s: %w", namespace, name, err)
	}
	if strings.TrimSpace(pod.Labels[substrateWorkerPoolLabel]) != workerPool {
		return fmt.Errorf(
			"provider actor worker Pod %s/%s %s label does not match infrastructure WorkerPool %s",
			namespace, name, substrateWorkerPoolLabel, workerPool,
		)
	}
	if pod.DeletionTimestamp != nil {
		return fmt.Errorf("provider actor worker Pod %s/%s is terminating", namespace, name)
	}
	return r.recordSubstrateRuntimePoolWorkerPodFence(ctx, pool, actor.ActorID, pod)
}

// destroySubstrateActorWorkload destroys the memory of a live actor workload
// by deleting its assigned provider worker Pod (provider workers host exactly
// one workload; the pool Deployment replaces the Pod fresh) and reports
// whether the workload is provably gone.
//
//nolint:gocyclo // Exact worker-fence replacement and deletion branches stay together at one fail-closed boundary.
func (r *RuntimePoolReconciler) destroySubstrateActorWorkload(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	actorID string,
	actor *workspace.SubstrateRuntimeActor,
) (bool, error) {
	fence, err := substrateRuntimePoolWorkerPodFenceFromAnnotation(pool)
	if err != nil {
		return false, err
	}
	if fence == nil {
		if actor == nil {
			return false, fmt.Errorf("refusing to complete RuntimePool substrate actor teardown: exact provider worker Pod fence is not recorded")
		}
		if err := r.verifySubstrateActorWorkerPlacement(ctx, pool, actor); err != nil {
			return false, err
		}
		// Persist the exact Pod identity before any destructive action so a
		// subsequent loss of the Actor record cannot erase the cleanup target.
		return false, nil
	}
	if fence.ActorID != actorID {
		return false, fmt.Errorf(
			"refusing to complete RuntimePool substrate actor teardown: recorded worker Pod fence belongs to actor %q, not %q",
			fence.ActorID, actorID,
		)
	}
	placementChanged := actor != nil &&
		(strings.TrimSpace(actor.PodNamespace) != fence.Namespace || strings.TrimSpace(actor.PodName) != fence.Name)
	replacementFence, err := substrateRuntimePoolReplacementWorkerPodFenceFromAnnotation(pool)
	if err != nil {
		return false, err
	}
	replacementMatchesActor := actor != nil && replacementFence != nil &&
		replacementFence.ActorID == actorID &&
		replacementFence.Namespace == strings.TrimSpace(actor.PodNamespace) &&
		replacementFence.Name == strings.TrimSpace(actor.PodName)
	stageReplacementFence := func() (bool, error) {
		if actor == nil {
			return false, fmt.Errorf("refusing to settle RuntimePool substrate actor: provider worker Pod changed without a current actor placement")
		}
		if err := r.verifySubstrateActorWorkerPlacement(ctx, pool, actor); err != nil && !errors.Is(err, errSubstrateWorkerPodFenceConflict) {
			return false, err
		}
		// recordSubstrateRuntimePoolWorkerPodFence persists the validated
		// replacement separately and deliberately returns a fence conflict.
		// Cross a reconcile boundary before promoting or deleting it.
		return false, nil
	}
	if placementChanged && !replacementMatchesActor {
		return stageReplacementFence()
	}
	promoteReplacementFence := func() (bool, error) {
		if !replacementMatchesActor {
			return false, fmt.Errorf("refusing to settle RuntimePool substrate actor: provider worker Pod changed without a validated replacement fence")
		}
		value, err := json.Marshal(replacementFence)
		if err != nil {
			return false, fmt.Errorf("encode replacement RuntimePool Substrate worker Pod fence: %w", err)
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkerPodFenceAnnotation, string(value)); err != nil {
			return false, err
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorReplacementWorkerPodFenceAnnotation, ""); err != nil {
			return false, err
		}
		// Cross a reconcile boundary before deleting the promoted exact Pod.
		return false, nil
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	pod := &corev1.Pod{}
	err = reader.Get(ctx, types.NamespacedName{Namespace: fence.Namespace, Name: fence.Name}, pod)
	if apierrors.IsNotFound(err) {
		if placementChanged {
			return promoteReplacementFence()
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if pod.UID != fence.UID {
		if actor != nil {
			if !replacementMatchesActor || replacementFence.UID != pod.UID {
				return stageReplacementFence()
			}
			return promoteReplacementFence()
		}
		// With no Actor record, a same-name replacement is unrelated to the
		// fenced workload and must not be deleted.
		return true, nil
	}
	if pod.DeletionTimestamp != nil {
		return false, nil
	}
	if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete provider worker Pod hosting the recycled actor: %w", err)
	}
	return false, nil
}

func substrateActorWorkloadProofRequired(pool *corev1alpha1.RuntimePool, actorID string) bool {
	if pool == nil {
		return false
	}
	return pool.Annotations[substrateActorBootedAnnotation] == actorID ||
		pool.Annotations[substrateActorCredentialSeededAnnotation] == actorID ||
		strings.TrimSpace(pool.Annotations[substrateActorWorkerPodFenceAnnotation]) != "" ||
		strings.TrimSpace(pool.Annotations[substrateActorReplacementWorkerPodFenceAnnotation]) != ""
}

func substrateRuntimePoolWorkerPodFenceFromAnnotation(
	pool *corev1alpha1.RuntimePool,
) (*substrateRuntimePoolWorkerPodFenceRecord, error) {
	return substrateRuntimePoolWorkerPodFenceFromAnnotationKey(pool, substrateActorWorkerPodFenceAnnotation)
}

func substrateRuntimePoolReplacementWorkerPodFenceFromAnnotation(
	pool *corev1alpha1.RuntimePool,
) (*substrateRuntimePoolWorkerPodFenceRecord, error) {
	return substrateRuntimePoolWorkerPodFenceFromAnnotationKey(pool, substrateActorReplacementWorkerPodFenceAnnotation)
}

func substrateRuntimePoolWorkerPodFenceFromAnnotationKey(
	pool *corev1alpha1.RuntimePool,
	key string,
) (*substrateRuntimePoolWorkerPodFenceRecord, error) {
	if pool == nil {
		return nil, fmt.Errorf("RuntimePool is required for Substrate actor teardown")
	}
	raw := strings.TrimSpace(pool.Annotations[key])
	if raw == "" {
		return nil, nil
	}
	var fence substrateRuntimePoolWorkerPodFenceRecord
	if err := json.Unmarshal([]byte(raw), &fence); err != nil {
		return nil, fmt.Errorf("RuntimePool recorded an invalid Substrate worker Pod fence")
	}
	fence.ActorID = strings.TrimSpace(fence.ActorID)
	fence.Namespace = strings.TrimSpace(fence.Namespace)
	fence.Name = strings.TrimSpace(fence.Name)
	if fence.ActorID == "" {
		return nil, fmt.Errorf("RuntimePool recorded a Substrate worker Pod fence without an actor ID")
	}
	if errs := k8svalidation.IsDNS1123Label(fence.Namespace); len(errs) != 0 {
		return nil, fmt.Errorf("RuntimePool recorded an invalid Substrate worker Pod namespace")
	}
	if errs := k8svalidation.IsDNS1123Subdomain(fence.Name); len(errs) != 0 {
		return nil, fmt.Errorf("RuntimePool recorded an invalid Substrate worker Pod name")
	}
	if fence.UID == "" {
		return nil, fmt.Errorf("RuntimePool recorded a Substrate worker Pod fence without a UID")
	}
	return &fence, nil
}

func (r *RuntimePoolReconciler) recordSubstrateRuntimePoolWorkerPodFence(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	actorID string,
	pod *corev1.Pod,
) error {
	if pod == nil || strings.TrimSpace(pod.Namespace) == "" || strings.TrimSpace(pod.Name) == "" || pod.UID == "" {
		return fmt.Errorf("provider actor worker Pod identity is incomplete")
	}
	desired := substrateRuntimePoolWorkerPodFenceRecord{
		ActorID: strings.TrimSpace(actorID), Namespace: strings.TrimSpace(pod.Namespace), Name: strings.TrimSpace(pod.Name), UID: pod.UID,
	}
	if desired.ActorID == "" {
		return fmt.Errorf("provider actor ID is required for the worker Pod fence")
	}
	existing, err := substrateRuntimePoolWorkerPodFenceFromAnnotation(pool)
	if err != nil {
		return err
	}
	value, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("encode RuntimePool Substrate worker Pod fence: %w", err)
	}
	if existing != nil {
		if *existing == desired {
			return nil
		}
		replacement, err := substrateRuntimePoolReplacementWorkerPodFenceFromAnnotation(pool)
		if err != nil {
			return err
		}
		if replacement != nil && *replacement != desired {
			return fmt.Errorf("provider actor worker Pod changed again before the prior replacement was fenced")
		}
		if replacement == nil {
			if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorReplacementWorkerPodFenceAnnotation, string(value)); err != nil {
				return err
			}
		}
		return errSubstrateWorkerPodFenceConflict
	}
	return r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkerPodFenceAnnotation, string(value))
}

func substrateRuntimePoolWorkerPlacementFromTemplate(template *unstructured.Unstructured) (string, string, error) {
	if template == nil {
		return "", "", fmt.Errorf("RuntimePool substrate ActorTemplate is required for WorkerPool placement")
	}
	workerPool, found, err := unstructured.NestedString(template.Object, "spec", "workerPoolRef", "name")
	if err != nil || !found || strings.TrimSpace(workerPool) == "" {
		return "", "", fmt.Errorf("RuntimePool derived substrate ActorTemplate has no workerPoolRef.name")
	}
	workerNamespace, _, err := unstructured.NestedString(template.Object, "spec", "workerPoolRef", "namespace")
	if err != nil {
		return "", "", fmt.Errorf("RuntimePool derived substrate ActorTemplate has an invalid workerPoolRef.namespace")
	}
	workerNamespace = strings.TrimSpace(workerNamespace)
	if workerNamespace == "" {
		workerNamespace = template.GetNamespace()
	}
	if workerNamespace == "" {
		return "", "", fmt.Errorf("RuntimePool derived substrate ActorTemplate has no WorkerPool namespace")
	}
	return workerNamespace, strings.TrimSpace(workerPool), nil
}

func substrateRuntimePoolWorkerPlacementFromAnnotation(pool *corev1alpha1.RuntimePool) (string, string, error) {
	if pool == nil {
		return "", "", fmt.Errorf("RuntimePool is required for Substrate actor teardown")
	}
	raw := strings.TrimSpace(pool.Annotations[substrateActorWorkerPlacementAnnotation])
	if raw == "" {
		return "", "", fmt.Errorf("RuntimePool admitted Substrate worker placement is not recorded")
	}
	var placement substrateRuntimePoolWorkerPlacementRecord
	if err := json.Unmarshal([]byte(raw), &placement); err != nil {
		return "", "", fmt.Errorf("RuntimePool recorded an invalid Substrate worker placement")
	}
	placement.Namespace = strings.TrimSpace(placement.Namespace)
	placement.WorkerPool = strings.TrimSpace(placement.WorkerPool)
	if errs := k8svalidation.IsDNS1123Label(placement.Namespace); len(errs) != 0 {
		return "", "", fmt.Errorf("RuntimePool recorded an invalid Substrate worker namespace")
	}
	if errs := k8svalidation.IsDNS1123Subdomain(placement.WorkerPool); len(errs) != 0 {
		return "", "", fmt.Errorf("RuntimePool recorded an invalid Substrate WorkerPool name")
	}
	return placement.Namespace, placement.WorkerPool, nil
}

func (r *RuntimePoolReconciler) recordSubstrateRuntimePoolWorkerPlacement(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	template *unstructured.Unstructured,
) error {
	workerNamespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromTemplate(template)
	if err != nil {
		return err
	}
	value, err := json.Marshal(substrateRuntimePoolWorkerPlacementRecord{
		Namespace: workerNamespace, WorkerPool: workerPool,
	})
	if err != nil {
		return fmt.Errorf("encode RuntimePool Substrate worker placement: %w", err)
	}
	return r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorWorkerPlacementAnnotation, string(value))
}

func substrateRuntimePoolNetworkPolicyNamespaces(pool *corev1alpha1.RuntimePool) ([]string, error) {
	if pool == nil {
		return nil, fmt.Errorf("RuntimePool is required for Substrate NetworkPolicy cleanup")
	}
	raw := strings.TrimSpace(pool.Annotations[substrateNetworkPolicyNamespacesAnnotation])
	if raw == "" {
		return nil, nil
	}
	seen := map[string]struct{}{}
	namespaces := make([]string, 0, strings.Count(raw, ",")+1)
	for item := range strings.SplitSeq(raw, ",") {
		namespace := strings.TrimSpace(item)
		if errs := k8svalidation.IsDNS1123Label(namespace); len(errs) != 0 {
			return nil, fmt.Errorf("RuntimePool recorded an invalid Substrate NetworkPolicy namespace")
		}
		if _, duplicate := seen[namespace]; duplicate {
			continue
		}
		seen[namespace] = struct{}{}
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	return namespaces, nil
}

func (r *RuntimePoolReconciler) recordSubstrateRuntimePoolNetworkPolicyNamespace(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	namespace string,
) error {
	namespace = strings.TrimSpace(namespace)
	if errs := k8svalidation.IsDNS1123Label(namespace); len(errs) != 0 {
		return fmt.Errorf("substrate NetworkPolicy namespace is invalid")
	}
	namespaces, err := substrateRuntimePoolNetworkPolicyNamespaces(pool)
	if err != nil {
		return err
	}
	if slices.Contains(namespaces, namespace) {
		return nil
	}
	namespaces = append(namespaces, namespace)
	sort.Strings(namespaces)
	return r.setSubstrateRuntimePoolAnnotation(
		ctx, pool, substrateNetworkPolicyNamespacesAnnotation, strings.Join(namespaces, ","),
	)
}

func (r *RuntimePoolReconciler) ensureSubstrateRuntimePoolNetworkPolicies(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	template *unstructured.Unstructured,
) (bool, error) {
	workerNamespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromTemplate(template)
	if err != nil {
		return false, err
	}
	// Record every namespace before creating cross-namespace children so a
	// controller crash or later template deletion cannot make cleanup depend on
	// a cluster-wide NetworkPolicy list.
	for _, namespace := range []string{workerNamespace, cfg.providerProxy.namespace} {
		if err := r.recordSubstrateRuntimePoolNetworkPolicyNamespace(ctx, pool, namespace); err != nil {
			return false, err
		}
	}
	changed := false
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	for _, desired := range r.substrateRuntimePoolNetworkPolicies(cfg, workerNamespace, workerPool) {
		current := &networkingv1.NetworkPolicy{}
		key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
		if err := reader.Get(ctx, key, current); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("read Substrate RuntimePool NetworkPolicy %q: %w", desired.Name, err)
			}
			policy := desired.DeepCopy()
			policy.Labels = mergeStringMap(policy.Labels, cfg.labels)
			if err := r.setRuntimePoolControllerReference(pool, policy); err != nil {
				return false, err
			}
			if err := r.Create(ctx, policy); err != nil {
				return false, fmt.Errorf("create Substrate RuntimePool NetworkPolicy %q: %w", desired.Name, err)
			}
			changed = true
			continue
		}
		if !substrateRuntimePoolNetworkPolicyOwnedByPool(current, pool, cfg) {
			return false, fmt.Errorf("same-name Substrate RuntimePool NetworkPolicy %q does not carry the exact RuntimePool ownership identity", desired.Name)
		}
		if apiequality.Semantic.DeepEqual(current.Spec, desired.Spec) {
			continue
		}
		base := current.DeepCopy()
		current.Labels = mergeStringMap(current.Labels, cfg.labels)
		current.Spec = desired.Spec
		if err := r.Patch(ctx, current, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("update Substrate RuntimePool NetworkPolicy %q: %w", desired.Name, err)
		}
		changed = true
	}
	return changed, nil
}

func (r *RuntimePoolReconciler) substrateRuntimePoolNetworkPolicies(
	cfg runtimePoolConfig,
	workerNamespace, workerPool string,
) []networkingv1.NetworkPolicy {
	selector := metav1.LabelSelector{MatchLabels: map[string]string{
		substrateWorkerPoolLabel: workerPool,
	}}
	controllerNamespace := controllerNamespaceForRuntimePool(r.ControllerNamespace)
	return []networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: runtimePoolChildName(cfg.baseName, runtimePoolSubstrateDenyEgressSuffix), Namespace: workerNamespace},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: runtimePoolChildName(cfg.baseName, runtimePoolSubstrateDNSEgressSuffix), Namespace: workerNamespace},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: metav1.NamespaceSystem}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: new(corev1.ProtocolUDP), Port: new(intstr.FromInt32(53))},
						{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(53))},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: runtimePoolChildName(cfg.baseName, runtimePoolSubstrateProviderEgressSuffix), Namespace: workerNamespace},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: cfg.providerProxy.namespace}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: cloneStringMap(cfg.providerProxy.podLabels)},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(cfg.providerProxy.port))}},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: runtimePoolChildName(cfg.baseName, runtimePoolSubstrateProviderIngressSuffix), Namespace: cfg.providerProxy.namespace},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: cloneStringMap(cfg.providerProxy.podLabels)},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: workerNamespace}},
						PodSelector:       &selector,
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(cfg.providerProxy.port))}},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: runtimePoolChildName(cfg.baseName, runtimePoolSubstrateControllerEgressSuffix), Namespace: workerNamespace},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: controllerNamespace}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{runtimePoolNetworkRoleLabel: AgentSandboxNamespaceStrategyController}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(r.ControllerAPIPort))}},
				}},
			},
		},
	}
}

func substrateRuntimePoolNetworkPolicyOwnedByPool(
	policy *networkingv1.NetworkPolicy,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) bool {
	if policy == nil || pool == nil {
		return false
	}
	for key, value := range cfg.labels {
		if value == "" || policy.Labels[key] != value {
			return false
		}
	}
	return policy.Namespace != pool.Namespace || metav1.IsControlledBy(policy, pool)
}

func (r *RuntimePoolReconciler) deleteSubstrateRuntimePoolNetworkPolicies(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	template *unstructured.Unstructured,
) (bool, error) {
	namespaces, err := substrateRuntimePoolNetworkPolicyNamespaces(pool)
	if err != nil {
		return false, err
	}
	if template != nil {
		workerNamespace, _, placementErr := substrateRuntimePoolWorkerPlacementFromTemplate(template)
		if placementErr != nil {
			return false, placementErr
		}
		found := slices.Contains(namespaces, workerNamespace)
		if !found {
			namespaces = append(namespaces, workerNamespace)
			sort.Strings(namespaces)
		}
	}
	if len(namespaces) == 0 {
		return false, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	expectedNames := []string{
		runtimePoolChildName(cfg.baseName, runtimePoolSubstrateDenyEgressSuffix),
		runtimePoolChildName(cfg.baseName, runtimePoolSubstrateDNSEgressSuffix),
		runtimePoolChildName(cfg.baseName, runtimePoolSubstrateProviderEgressSuffix),
		runtimePoolChildName(cfg.baseName, runtimePoolSubstrateProviderIngressSuffix),
		runtimePoolChildName(cfg.baseName, runtimePoolSubstrateControllerEgressSuffix),
	}
	remaining := false
	for _, namespace := range namespaces {
		for _, name := range expectedNames {
			current := &networkingv1.NetworkPolicy{}
			key := types.NamespacedName{Namespace: namespace, Name: name}
			if err := reader.Get(ctx, key, current); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return false, fmt.Errorf("read Substrate RuntimePool NetworkPolicy %q for cleanup: %w", name, err)
			}
			if !substrateRuntimePoolNetworkPolicyOwnedByPool(current, pool, cfg) {
				return false, fmt.Errorf("refusing to delete foreign Substrate RuntimePool NetworkPolicy %q", name)
			}
			if !current.DeletionTimestamp.IsZero() {
				remaining = true
				continue
			}
			if err := r.Delete(ctx, current, deleteCurrentObjectPreconditions(current)...); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete Substrate RuntimePool NetworkPolicy %q: %w", name, err)
			}
			remaining = true
		}
	}
	return remaining, nil
}

// deleteSubstrateRuntimePoolNetworkPoliciesForTemplate removes only the
// policies placed by one deployed template. Rollout uses this narrower path
// when the WorkerPool namespace changes so newly created replacement policies
// in the destination namespace are preserved.
func (r *RuntimePoolReconciler) deleteSubstrateRuntimePoolNetworkPoliciesForTemplate(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	template *unstructured.Unstructured,
) (bool, error) {
	workerNamespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromTemplate(template)
	if err != nil {
		return false, err
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	remaining := false
	for _, desired := range r.substrateRuntimePoolNetworkPolicies(cfg, workerNamespace, workerPool) {
		current := &networkingv1.NetworkPolicy{}
		key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
		if err := reader.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("read Substrate RuntimePool NetworkPolicy %q for cleanup: %w", desired.Name, err)
		}
		if !substrateRuntimePoolNetworkPolicyOwnedByPool(current, pool, cfg) {
			return false, fmt.Errorf("refusing to delete foreign Substrate RuntimePool NetworkPolicy %q", desired.Name)
		}
		if current.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, current, deleteCurrentObjectPreconditions(current)...); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete Substrate RuntimePool NetworkPolicy %q: %w", desired.Name, err)
			}
		}
		remaining = true
	}
	return remaining, nil
}

func (r *RuntimePoolReconciler) setSubstrateActorBootedAnnotation(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	actorID string,
) error {
	return r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorBootedAnnotation, actorID)
}

func (r *RuntimePoolReconciler) setSubstrateRuntimePoolAnnotation(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	key, value string,
) error {
	current := pool.Annotations[key]
	if current == value || (value == "" && current == "") {
		return nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	if value == "" {
		delete(pool.Annotations, key)
	} else {
		pool.Annotations[key] = value
	}
	if err := r.Patch(ctx, pool, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record RuntimePool substrate annotation: %w", err)
	}
	return nil
}

func (r *RuntimePoolReconciler) verifySubstrateRuntimeTemplateFence(
	ctx context.Context,
	namespace, name, expected string,
) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return fmt.Errorf("RuntimePool substrate ActorTemplate fence is not recorded")
	}
	template, err := r.getSubstrateActorTemplate(ctx, namespace, name)
	if err != nil {
		return err
	}
	if template == nil {
		return fmt.Errorf("RuntimePool substrate ActorTemplate disappeared after validation")
	}
	observed, err := substrateRuntimeTemplateFence(template)
	if err != nil {
		return err
	}
	if observed != expected {
		return fmt.Errorf("RuntimePool substrate ActorTemplate UID/resourceVersion changed after validation")
	}
	return nil
}

func (r *RuntimePoolReconciler) getSubstrateActorTemplate(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, template); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if apimeta.IsNoMatchError(err) || k8sRuntimeIsMissingKindError(err) {
			return nil, fmt.Errorf("read substrate ActorTemplate: the Substrate provider CRDs are not installed; Substrate-backed RuntimePools require an externally operated Substrate installation")
		}
		return nil, fmt.Errorf("read substrate ActorTemplate: %w", err)
	}
	return template, nil
}

func (r *RuntimePoolReconciler) getSubstrateActorTemplateForCleanup(
	ctx context.Context,
	namespace, name string,
) (*unstructured.Unstructured, error) {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, template); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) || k8sRuntimeIsMissingKindError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read substrate ActorTemplate for cleanup: %w", err)
	}
	return template, nil
}

func (r *RuntimePoolReconciler) createSubstrateActorTemplate(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	desired *unstructured.Unstructured,
) error {
	template := desired.DeepCopy()
	if err := r.setRuntimePoolControllerReference(pool, template); err != nil {
		return err
	}
	if err := r.Create(ctx, template); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create RuntimePool substrate actor template: %w", err)
	}
	return nil
}

func (r *RuntimePoolReconciler) updateSubstrateActorTemplate(
	ctx context.Context,
	template *unstructured.Unstructured,
	desired *unstructured.Unstructured,
) error {
	if template == nil {
		return fmt.Errorf("RuntimePool substrate actor template is required for a template update")
	}
	base := template.DeepCopy()
	template.Object["spec"] = desired.Object["spec"]
	template.SetLabels(desired.GetLabels())
	template.SetAnnotations(desired.GetAnnotations())
	if err := r.Patch(ctx, template, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("update RuntimePool substrate actor template: %w", err)
	}
	return nil
}

type substrateRuntimeTemplateRender struct {
	object   *unstructured.Unstructured
	revision string
}

// renderSubstrateRuntimeTemplate derives the controller-owned runtime
// ActorTemplate: operator infrastructure fields are copied verbatim from the
// base template, and the single runtime container is the exact canonical
// supervisor container with fence identity literals and read-once bootstrap
// secret references replacing Kubernetes-only mounts and field refs.
func (r *RuntimePoolReconciler) renderSubstrateRuntimeTemplate(
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	baseTemplate *unstructured.Unstructured,
	templateNamespace, actorID, bootstrapNonce, bootstrapPublicKey string,
) (substrateRuntimeTemplateRender, error) {
	baseSpec, found, err := unstructured.NestedMap(baseTemplate.Object, "spec")
	if err != nil || !found {
		return substrateRuntimeTemplateRender{}, fmt.Errorf("substrate infrastructure ActorTemplate %s/%s has no readable spec", templateNamespace, baseTemplate.GetName())
	}
	infrastructure := k8sruntime.DeepCopyJSON(baseSpec)
	delete(infrastructure, "containers")
	// snapshotsConfig is copied verbatim for non-suspendable pools: the
	// provider requires it and builds a per-template "golden snapshot" by
	// booting one instance and checkpointing it. That checkpoint is safe only
	// because the rendered container carries no credentials at all — the
	// supervisor boots into the awaiting-bootstrap phase and receives
	// credentials from the controller after the real actor is booted, so a
	// golden snapshot captures a waiting, credential-free process plus the
	// public per-pool nonce and verification key.
	dataSuspend := substrateRuntimePoolSuspendCapable(pool)
	if dataSuspend {
		// A data-only-suspendable pool never relies on provider snapshot
		// defaults: onPause and onCommit checkpoint only DurableDir volumes and
		// a data snapshot resumes through a cold process boot, so no checkpoint
		// can ever capture supervisor memory or credentials.
		if err := validateSubstrateDurableVolumeCollision(infrastructure); err != nil {
			return substrateRuntimeTemplateRender{}, err
		}
		volumes, _, _ := unstructured.NestedSlice(infrastructure, "volumes")
		volumes = append(volumes, map[string]any{
			"name":       substrateDurableWorkspaceVolume,
			"durableDir": map[string]any{},
		})
		infrastructure["volumes"] = volumes
		// Override only the policy keys: the operator's base snapshotsConfig
		// carries required provider fields such as the storage location, and
		// dropping them would leave the provider unable to build or persist
		// the data checkpoint.
		snapshots := map[string]any{}
		if base, ok := infrastructure["snapshotsConfig"].(map[string]any); ok {
			maps.Copy(snapshots, base)
		}
		snapshots["onPause"] = substrateSnapshotScopeData
		snapshots["onCommit"] = substrateSnapshotScopeData
		snapshots["onResume"] = map[string]any{"fromData": substrateSnapshotResumeColdBoot}
		infrastructure["snapshotsConfig"] = snapshots
	}

	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	// Secret names are irrelevant to the rendered container (credentials are
	// bootstrap-seeded, never template-referenced); the canonical template only
	// contributes the immutable image, fence identity, and non-secret env.
	canonical := r.runtimePoolPodTemplate(pool, cfg, selector, "unused-auth", "unused-provider")
	container := substrateRuntimeContainer(canonical.Spec.Containers[0], templateNamespace, actorID, bootstrapNonce, bootstrapPublicKey)
	if dataSuspend {
		container.VolumeMounts = []corev1.VolumeMount{{
			Name: substrateDurableWorkspaceVolume, MountPath: substrateDurableWorkspaceMountPath,
		}}
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "ORKA_ACP_DURABLE_WORKSPACE_DIR", Value: substrateDurableWorkspaceMountPath,
		})
	}

	containerMap, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&container)
	if err != nil {
		return substrateRuntimeTemplateRender{}, fmt.Errorf("render substrate runtime container: %w", err)
	}
	if resources, ok := containerMap["resources"].(map[string]any); ok && len(resources) == 0 {
		delete(containerMap, "resources")
	}
	spec := infrastructure
	spec["containers"] = []any{containerMap}

	labels := mergeStringMap(cloneStringMap(cfg.labels), map[string]string{
		"orka.ai/execution-workspace": scheduledRunLabelValue,
		"orka.ai/workspace-provider":  string(corev1alpha1.WorkspaceProviderSubstrate),
	})
	annotations := map[string]string{
		runtimePoolProfileAnnotation:                 pool.Spec.Runtime.Profile.Digest,
		runtimePoolProviderTokenGenerationAnnotation: cfg.providerProxy.tokenGeneration,
		runtimePoolPIDsAnnotation:                    "4096",
	}

	object := &unstructured.Unstructured{Object: map[string]any{substrateObjectSpecField: spec}}
	object.SetGroupVersionKind(substrateActorTemplateGVK)
	object.SetName(runtimePoolSubstrateTemplateName(cfg.baseName))
	object.SetNamespace(templateNamespace)
	object.SetLabels(labels)
	object.SetAnnotations(annotations)
	revision, err := substrateRuntimeTemplateObjectRevision(object)
	if err != nil {
		return substrateRuntimeTemplateRender{}, err
	}
	annotations[runtimePoolTemplateRevisionAnnotation] = revision
	object.SetAnnotations(annotations)
	return substrateRuntimeTemplateRender{object: object, revision: revision}, nil
}

// substrateRuntimeContainer adapts the canonical supervisor container to
// provider hosting: Kubernetes downward-API field refs become exact literals,
// credential file mounts disappear entirely (the supervisor boots in the
// awaiting-bootstrap phase with public nonce and signature verification key), and Pod-only
// surfaces (pull policy, mounts, probes, security context) are dropped because the
// provider's gVisor sandbox owns them.
func substrateRuntimeContainer(
	canonical corev1.Container,
	templateNamespace, actorID, bootstrapNonce, bootstrapPublicKey string,
) corev1.Container {
	container := *canonical.DeepCopy()
	container.ImagePullPolicy = ""
	container.VolumeMounts = nil
	container.SecurityContext = nil
	container.StartupProbe = nil
	container.ReadinessProbe = nil
	container.LivenessProbe = nil
	container.Lifecycle = nil
	container.Resources = corev1.ResourceRequirements{}
	// Unlike kubelet, the provider builds the OCI runtime spec strictly from
	// the template and never reads the image config, so the immutable runtime
	// entrypoint must be stated explicitly — otherwise `runsc create` receives
	// an empty argv and rejects the workload.
	container.Command = []string{substrateRuntimeEntrypoint}
	// The provider router forwards actor traffic on the conventional actor
	// port 80 (every proven actor workload listens there); the controller-side
	// route transport dials the router, so the logical URL port never matters.
	container.Ports = []corev1.ContainerPort{{ContainerPort: substrateActorListenPort, Protocol: corev1.ProtocolTCP}}

	env := make([]corev1.EnvVar, 0, len(container.Env)+4)
	for _, item := range container.Env {
		switch item.Name {
		case "ORKA_ACP_LISTEN_ADDRESS":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: fmt.Sprintf(":%d", substrateActorListenPort)})
		case "ORKA_ACP_POD_UID":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: substrateActorInstanceUID(actorID)})
		case "ORKA_ACP_POD_NAME":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: actorID})
		case "ORKA_ACP_POD_NAMESPACE":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: templateNamespace})
		case runtimePoolControllerTokenFileEnv, runtimePoolCapabilitySecretFileEnv, runtimePoolProviderTokenFileEnv:
			// Provider workspaces have no Secret mounts; the read-once
			// bootstrap variables below replace the file paths.
		default:
			env = append(env, item)
		}
	}
	env = append(env, corev1.EnvVar{Name: "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE", Value: bootstrapNonce})
	env = append(env, corev1.EnvVar{Name: harnessv2.CredentialBootstrapPublicKeyEnv, Value: bootstrapPublicKey})
	container.Env = env
	return container
}

// substrateFullMemoryRestoreGateOpen reports whether credential-safe
// full-memory restore is available. It is hard-false by design: a Substrate
// Full snapshot captures supervisor process memory — live pool, capability,
// provider-proxy, model, repository, and child-process credentials plus
// in-flight prompt and publication state — and restoring one is prohibited
// until every prerequisite in ADR 0030 holds: no long-lived shared credential
// in supervisor memory, restore credentials bound to the exact Session UID,
// generation, Actor lifetime, snapshot generation, boot ID, controller epoch,
// and expiry, pre-suspend credential generations revoked before a restored
// process can reach any endpoint, sealed-boot restored supervisors that
// discard snapshotted credentials, one-live-writer restore fencing across
// clones and duplicate restores, and passing live adversarial coverage.
// Flipping this function is not enough to enable the mode: the enforcement
// points below and the admission-layer enum validation each reject non-Data
// policies independently, so the gate cannot open by accident.
func substrateFullMemoryRestoreGateOpen() bool { return false }

// linkedWorkspaceSuspendIntentPending reports a suspend-capable pool whose
// linked ExecutionWorkspace has DesiredState Suspended while the pool's own
// durable suspension intent is not recorded yet: the crash window between the
// workspace patch and the adapter's pool annotation must never fall through
// to ordinary teardown.
func (r *RuntimePoolReconciler) linkedWorkspaceSuspendIntentPending(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
) (bool, error) {
	if !substrateRuntimePoolSuspendCapable(pool) || substrateWorkspaceSuspendRequested(pool) {
		return false, nil
	}
	name := strings.TrimSpace(pool.Labels[acpExecutionWorkspaceLinkLabel])
	if name == "" {
		return false, nil
	}
	linked := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: name}, linked); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return linked.DeletionTimestamp.IsZero() &&
		linked.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended, nil
}

// substrateRuntimePoolSuspendCapable reports whether the pool's immutable
// binding permits data-only cold suspension.
func substrateRuntimePoolSuspendCapable(pool *corev1alpha1.RuntimePool) bool {
	// The provider gate keeps a stale or tampered pool carrying a foreign
	// backend block from being classified as substrate-suspendable.
	return pool != nil && pool.Spec.ExecutionWorkspace != nil &&
		pool.Spec.ExecutionWorkspace.Provider == corev1alpha1.WorkspaceProviderSubstrate &&
		pool.Spec.ExecutionWorkspace.Substrate != nil &&
		pool.Spec.ExecutionWorkspace.Substrate.SuspendMode == string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly)
}

// substrateWorkspaceSuspendRequested reports the workspace adapter's
// suspension intent. It is honored only on suspend-capable pools.
func substrateWorkspaceSuspendRequested(pool *corev1alpha1.RuntimePool) bool {
	return runtimePoolWorkspaceSuspendIntentSet(pool) && substrateRuntimePoolSuspendCapable(pool)
}

// substrateActorConsensuallySuspended reports whether this controller
// requested the actor's current suspension, distinguishing it from a
// provider-initiated suspension that stays fail-closed.
func substrateActorConsensuallySuspended(pool *corev1alpha1.RuntimePool, actorID string) bool {
	return pool != nil && substrateRuntimePoolSuspendCapable(pool) &&
		strings.TrimSpace(pool.Annotations[substrateActorSuspendedAnnotation]) == actorID
}

// substrateActorAwaitingDataResume reports a consensually suspended actor
// whose cold resume has not booted yet, so consumed one-time bootstrap
// material rotates before the fresh boot exactly as it does for a replacement
// actor.
func substrateActorAwaitingDataResume(pool *corev1alpha1.RuntimePool, actor *workspace.SubstrateRuntimeActor, actorID string) bool {
	return actor != nil && !actor.Running() &&
		substrateActorConsensuallySuspended(pool, actorID) &&
		strings.TrimSpace(pool.Annotations[substrateActorBootedAnnotation]) == ""
}

// validateSubstrateDurableVolumeCollision rejects an infrastructure template
// that already defines the controller-reserved durable workspace volume.
func validateSubstrateDurableVolumeCollision(spec map[string]any) error {
	volumes, found, err := unstructured.NestedSlice(spec, "volumes")
	if err != nil || !found {
		return nil //nolint:nilerr // An unreadable volume list renders no collision; the provider validates shape.
	}
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(volume, "name"); name == substrateDurableWorkspaceVolume {
			return fmt.Errorf(
				"substrate infrastructure ActorTemplate defines the reserved volume %q; the controller owns the durable workspace volume",
				substrateDurableWorkspaceVolume,
			)
		}
	}
	return nil
}

// verifySubstrateDeployedDataSnapshotPolicy re-parses the deployed derived
// template and requires the exact data-only snapshot policy before a live
// actor may be suspended. The template fence already pins the object; this is
// a second, content-level proof at the suspension boundary.
func verifySubstrateDeployedDataSnapshotPolicy(template *unstructured.Unstructured) error {
	if template == nil {
		return fmt.Errorf("deployed RuntimePool substrate actor template is required before suspension")
	}
	onPause, _, _ := unstructured.NestedString(template.Object, "spec", "snapshotsConfig", "onPause")
	onCommit, _, _ := unstructured.NestedString(template.Object, "spec", "snapshotsConfig", "onCommit")
	fromData, _, _ := unstructured.NestedString(template.Object, "spec", "snapshotsConfig", "onResume", "fromData")
	if onPause != substrateSnapshotScopeData || onCommit != substrateSnapshotScopeData || fromData != substrateSnapshotResumeColdBoot {
		if substrateFullMemoryRestoreGateOpen() {
			// Unreachable until ADR 0030's prerequisites are implemented; the
			// gate exists so the future mode has exactly one opening point.
			return fmt.Errorf("full-memory snapshot policies are not yet reviewed for this template")
		}
		return fmt.Errorf(
			"deployed RuntimePool substrate actor template does not render the exact data-only snapshot policy; full-memory restore is gated until its credential-safety prerequisites are met (ADR 0030), so no live actor is suspended or resumed under it",
		)
	}
	return nil
}

func substrateRuntimeTemplateObjectRevision(template *unstructured.Unstructured) (string, error) {
	if template == nil {
		return "", fmt.Errorf("RuntimePool substrate actor template is required")
	}
	spec, found, err := unstructured.NestedMap(template.Object, substrateObjectSpecField)
	if err != nil || !found {
		return "", fmt.Errorf("RuntimePool substrate actor template has no readable spec")
	}
	annotations := cloneStringMap(template.GetAnnotations())
	delete(annotations, runtimePoolTemplateRevisionAnnotation)
	return runtimePoolJSONRevision(map[string]any{
		substrateObjectLabelsField: cloneStringMap(template.GetLabels()),
		"annotations":              annotations,
		substrateObjectSpecField:   spec,
	})
}

// substrateRuntimeTemplateBootstrapNeutralRevision computes the template
// revision with every bootstrap-scoped value blanked: the one-time credential
// bootstrap nonce and verification key inside the rendered container, the
// pool fence generation the cold boot must re-adopt, and the provider-proxy
// token generation annotation whose token is likewise bootstrap-seeded. Two renders with equal neutral revisions differ only in
// rotated bootstrap material, which licenses the suspended-actor in-place
// template refresh.
func substrateRuntimeTemplateBootstrapNeutralRevision(template *unstructured.Unstructured) (string, error) {
	if template == nil {
		return "", fmt.Errorf("RuntimePool substrate actor template is required")
	}
	neutral := template.DeepCopy()
	containers, found, err := unstructured.NestedSlice(neutral.Object, substrateObjectSpecField, "containers")
	if err != nil {
		return "", fmt.Errorf("RuntimePool substrate actor template containers are unreadable: %w", err)
	}
	if found {
		for _, rawContainer := range containers {
			container, ok := rawContainer.(map[string]any)
			if !ok {
				continue
			}
			envs, ok := container["env"].([]any)
			if !ok {
				continue
			}
			for _, rawEnv := range envs {
				env, ok := rawEnv.(map[string]any)
				if !ok {
					continue
				}
				name, _ := env["name"].(string)
				switch name {
				case "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE", harnessv2.CredentialBootstrapPublicKeyEnv,
					"ORKA_ACP_RUNTIME_POOL_GENERATION", "ORKA_ACP_CONTROLLER_EPOCH",
					"ORKA_ACP_PROVIDER_TOKEN_GENERATION":
					// Every cold-boot fence input is bootstrap-scoped: the
					// pool fence generation advances with the suspend/resume
					// intents themselves, the controller epoch advances on
					// restart, and the provider token generation rotates with
					// its Secret — the resumed boot must adopt all of them,
					// so none may push a suspended checkpoint through the
					// non-bootstrap recycle path.
					env["value"] = ""
				}
			}
		}
		if err := unstructured.SetNestedSlice(neutral.Object, containers, substrateObjectSpecField, "containers"); err != nil {
			return "", fmt.Errorf("RuntimePool substrate actor template containers are unwritable: %w", err)
		}
	}
	annotations := neutral.GetAnnotations()
	delete(annotations, runtimePoolProviderTokenGenerationAnnotation)
	neutral.SetAnnotations(annotations)
	return substrateRuntimeTemplateObjectRevision(neutral)
}

func substrateRuntimeTemplateIntegrity(template *unstructured.Unstructured) (string, error) {
	if template == nil {
		return "", fmt.Errorf("deployed RuntimePool substrate actor template is missing")
	}
	declared := strings.TrimSpace(template.GetAnnotations()[runtimePoolTemplateRevisionAnnotation])
	if declared == "" {
		return "", fmt.Errorf("deployed RuntimePool substrate actor template has no declared revision")
	}
	observed, err := substrateRuntimeTemplateObjectRevision(template)
	if err != nil {
		return "", err
	}
	if observed != declared {
		return "", fmt.Errorf(
			"deployed RuntimePool substrate actor template revision does not match observed contents (declared %s, observed %s)",
			runtimePoolShortRevision(declared), runtimePoolShortRevision(observed),
		)
	}
	return declared, nil
}

// substrateTemplatePodTemplateSpec reconstructs the logical Pod template shape
// from a deployed derived ActorTemplate so rollout drains validate the old
// instance against the exact identity it was booted with.
func substrateTemplatePodTemplateSpec(template *unstructured.Unstructured) (corev1.PodTemplateSpec, error) {
	containersRaw, found, err := unstructured.NestedSlice(template.Object, "spec", "containers")
	if err != nil || !found || len(containersRaw) != 1 {
		return corev1.PodTemplateSpec{}, fmt.Errorf("deployed RuntimePool substrate actor template is invalid")
	}
	containerMap, ok := containersRaw[0].(map[string]any)
	if !ok {
		return corev1.PodTemplateSpec{}, fmt.Errorf("deployed RuntimePool substrate actor template is invalid")
	}
	container := corev1.Container{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(containerMap, &container); err != nil {
		return corev1.PodTemplateSpec{}, fmt.Errorf("deployed RuntimePool substrate actor template is invalid: %w", err)
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      cloneStringMap(template.GetLabels()),
			Annotations: cloneStringMap(template.GetAnnotations()),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{container}},
	}, nil
}

// substrateRuntimePoolDeployedValidationTarget reconstructs the exact
// identity and epoch-scoped controller credentials of an already materialized
// Actor. The mutable infrastructure template is not part of runtime identity,
// so recovery and authenticated teardown rely only on the frozen derived
// template.
func (r *RuntimePoolReconciler) substrateRuntimePoolDeployedValidationTarget(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	derivedTemplate *unstructured.Unstructured,
) (*corev1alpha1.RuntimePool, runtimePoolConfig, *corev1.Secret, error) {
	deployedTemplate, err := substrateTemplatePodTemplateSpec(derivedTemplate)
	if err != nil {
		return nil, runtimePoolConfig{}, nil, err
	}
	validationPool, validationConfig, err := runtimePoolPodTemplateValidationTarget(pool, deployedTemplate)
	if err != nil {
		return nil, runtimePoolConfig{}, nil, err
	}
	// The rendered template records immutable runtime identity, while the
	// controller-owned namespace remains the stable location of pool Secrets.
	validationConfig.namespace = cfg.namespace
	deployedAuthSecret, err := r.substrateTemplateAuthSecret(ctx, pool, cfg, deployedTemplate)
	if err != nil {
		return nil, runtimePoolConfig{}, nil, err
	}
	return validationPool, validationConfig, deployedAuthSecret, nil
}

// substrateTemplateAuthSecret resolves the exact private controller auth Secret
// the deployed instance was seeded with. Substrate templates record only the
// controller epoch; the RuntimePool binds that epoch to an immutable Secret UID.
func (r *RuntimePoolReconciler) substrateTemplateAuthSecret(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	deployed corev1.PodTemplateSpec,
) (*corev1.Secret, error) {
	rawEpoch := ""
	if len(deployed.Spec.Containers) == 1 {
		rawEpoch = strings.TrimSpace(runtimePoolLiteralEnvironment(deployed.Spec.Containers[0].Env)["ORKA_ACP_CONTROLLER_EPOCH"])
	}
	if rawEpoch == "" {
		return nil, fmt.Errorf("deployed RuntimePool auth Secret reference is missing")
	}
	epoch, err := strconv.ParseInt(rawEpoch, 10, 64)
	if err != nil || epoch <= 0 {
		return nil, fmt.Errorf("deployed RuntimePool controller epoch is invalid")
	}
	return r.boundPrivateWorkspaceRuntimePoolAuthSecret(ctx, pool, cfg, epoch)
}

// pruneStaleSubstrateRuntimePoolSecrets removes epoch-scoped credential
// Secrets in the runtime namespace that are no longer current. The provider
// template namespace remains credential-free.
func (r *RuntimePoolReconciler) pruneStaleSubstrateRuntimePoolSecrets(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	currentNames ...string,
) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	keep := make(map[string]struct{}, len(currentNames))
	for _, name := range currentNames {
		addRuntimeSecretName(keep, name)
	}
	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(cfg.namespace), client.MatchingLabels{
		runtimePoolManagedByLabel: runtimePoolManagedByLabelValue,
		runtimePoolKeyLabel:       cfg.labels[runtimePoolKeyLabel],
		runtimePoolUIDLabel:       string(pool.UID),
	}); err != nil {
		return fmt.Errorf("list managed RuntimePool Secrets for stale credential cleanup: %w", err)
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if _, current := keep[secret.Name]; current || !runtimePoolManagedCredentialSecret(secret, cfg) {
			continue
		}
		if err := r.deleteRuntimePoolManagedSecret(ctx, secret); err != nil {
			return err
		}
	}
	return nil
}

// deleteSubstrateRuntimePoolChildren removes the provider actor and the
// derived template during pool finalization; pool Secrets live in the runtime
// namespace and are swept by the generic child cleanup. Actor deletion is
// mandatory: an unreachable Substrate control plane blocks finalization
// rather than leaking a credentialed runtime workload.
func (r *RuntimePoolReconciler) deleteSubstrateRuntimePoolChildren(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) (bool, error) {
	substrateSpec := pool.Spec.ExecutionWorkspace.Substrate
	if substrateSpec == nil {
		return false, nil
	}
	templateNamespace := substrateSpec.BaseTemplateNamespace
	actorID := runtimePoolSubstrateActorID(cfg.baseName)
	control, err := r.substrateActorControlForCleanup()
	if err != nil {
		return false, err
	}
	defer control.Close() //nolint:errcheck // best-effort connection teardown
	gone, err := r.teardownSubstrateActor(ctx, pool, control, actorID)
	if err != nil {
		return false, err
	}
	if !gone {
		// The staged teardown still needs the derived template: the provider's
		// settle workflow resolves it while transitioning the memoryless actor
		// into the deletable suspended state. Delete it only after the actor
		// is gone.
		return true, nil
	}
	template, err := r.getSubstrateActorTemplateForCleanup(ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return false, err
	}
	if template != nil {
		expected := &unstructured.Unstructured{}
		expected.SetNamespace(templateNamespace)
		expected.SetName(runtimePoolSubstrateTemplateName(cfg.baseName))
		expected.SetLabels(map[string]string{
			runtimePoolManagedByLabel: runtimePoolManagedByLabelValue,
			runtimePoolKeyLabel:       runtimePoolKey(pool.Namespace, pool.Name),
			runtimePoolNameLabel:      pool.Name,
			runtimePoolNamespaceLabel: pool.Namespace,
			runtimePoolUIDLabel:       string(pool.UID),
		})
		if !substrateRuntimeTemplateOwnedByPool(template, expected) {
			return false, fmt.Errorf("same-name RuntimePool substrate ActorTemplate does not carry the exact RuntimePool ownership identity")
		}
	}
	policiesRemaining, policyErr := r.deleteSubstrateRuntimePoolNetworkPolicies(ctx, pool, cfg, template)
	if policyErr != nil {
		return false, policyErr
	}
	if policiesRemaining {
		return true, nil
	}
	if template == nil {
		return false, nil
	}
	if err := r.Delete(ctx, template, deleteCurrentObjectPreconditions(template)...); err == nil {
		// Deletion is asynchronous. Keep the finalizer until an uncached
		// follow-up observes NotFound so a terminating template cannot outlive
		// the RuntimePool ownership record.
		return true, nil
	} else if !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) && !k8sRuntimeIsMissingKindError(err) {
		return false, fmt.Errorf("delete RuntimePool substrate actor template: %w", err)
	}

	// Pool Secrets live in the runtime namespace and are swept by the generic
	// pool child cleanup; nothing secret ever exists in the template namespace.
	return false, nil
}
