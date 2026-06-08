/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/workspace"
)

const (
	// toolHealthCheckInterval is how often tools are re-checked.
	toolHealthCheckInterval = 5 * time.Minute

	// toolHealthCheckTimeout is the HTTP timeout for health checks.
	toolHealthCheckTimeout = 10 * time.Second

	substrateMCPToolActorFinalizer = "orka.ai/substrate-mcp-tool-actor-cleanup"
	substrateMCPToolActorIDAnno    = "orka.ai/substrate-mcp-tool-actor-id"
	substrateMCPToolBootedIDAnno   = "orka.ai/substrate-mcp-tool-booted-id"

	substrateMCPToolActorPoolNameAnno      = "orka.ai/substrate-mcp-tool-actor-pool-name"
	substrateMCPToolActorPoolNamespaceAnno = "orka.ai/substrate-mcp-tool-actor-pool-namespace"

	substrateMCPToolCleanupActorIDAnno        = "orka.ai/substrate-mcp-tool-cleanup-actor-id"
	substrateMCPToolCleanupPoolNameAnno       = "orka.ai/substrate-mcp-tool-cleanup-pool-name"
	substrateMCPToolCleanupPoolNamespaceAnno  = "orka.ai/substrate-mcp-tool-cleanup-pool-namespace"
	substrateMCPToolCleanupNonPooledValueAnno = "non-pooled"

	substrateMCPToolActorLeasePurpose = "substrate-mcp-tool-actor-lease"

	substratePoolActorLeaseToolNSAnno   = "orka.ai/substrate-pool-tool-namespace"
	substratePoolActorLeaseToolNameAnno = "orka.ai/substrate-pool-tool-name"
	substratePoolActorLeaseToolUIDAnno  = "orka.ai/substrate-pool-tool-uid"
)

// ToolReconciler reconciles a Tool object
type ToolReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// HTTPClient is the HTTP client used for health checks. If nil, a default is used.
	HTTPClient *http.Client

	// SkipSSRFValidation disables SSRF protection for testing. Do NOT set to true in production.
	SkipSSRFValidation bool

	// SubstrateEnabled enables durable MCP tool actors.
	SubstrateEnabled          bool
	SubstrateConfig           SubstrateConfig
	EnforceNamespaceIsolation bool

	// SubstrateExecutorFactory is injectable for tests.
	SubstrateExecutorFactory func(SubstrateConfig) (workspace.WorkspaceExecutor, error)
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=tools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools/finalizers,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile validates the Tool configuration, performs a health check on the HTTP endpoint,
// and updates the Tool's status accordingly.
func (r *ToolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Tool
	tool := &corev1alpha1.Tool{}
	if err := r.Get(ctx, req.NamespacedName, tool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Tool", "tool", tool.Name, "url", toolHTTPURL(tool))
	if !tool.DeletionTimestamp.IsZero() {
		return r.finalizeSubstrateMCPTool(ctx, tool)
	}
	if (tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil) &&
		controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
		return r.finalizeSubstrateMCPTool(ctx, tool)
	}

	// Validate the tool configuration
	if err := r.validateTool(ctx, tool); err != nil {
		logger.Error(err, "Tool validation failed")
		return r.updateStatus(ctx, tool, false, err.Error())
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
		return r.reconcileSubstrateMCPTool(ctx, tool)
	}

	// Perform health check on the HTTP endpoint
	if err := r.healthCheck(ctx, tool); err != nil {
		logger.Info("Tool health check failed", "tool", tool.Name, "error", err.Error())
		return r.updateStatus(ctx, tool, false, err.Error())
	}

	// Tool is valid and reachable
	return r.updateStatus(ctx, tool, true, "")
}

// validateTool validates the Tool spec.
func (r *ToolReconciler) validateTool(ctx context.Context, tool *corev1alpha1.Tool) error {
	// Validate description
	if tool.Spec.Description == "" {
		return fmt.Errorf("description is required")
	}
	if err := r.validateToolHTTPAuth(ctx, tool); err != nil {
		return err
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
		return r.validateSubstrateMCPTool(ctx, tool)
	}
	if tool.Spec.HTTP == nil {
		return fmt.Errorf("http is required unless mcp.substrateActor is set")
	}
	// Validate URL
	if tool.Spec.HTTP.URL == "" {
		return fmt.Errorf("http.url is required")
	}
	if _, err := url.ParseRequestURI(tool.Spec.HTTP.URL); err != nil {
		return fmt.Errorf("invalid http.url %q: %w", tool.Spec.HTTP.URL, err)
	}

	// Block private/internal network targets (unless in test mode)
	if !r.SkipSSRFValidation {
		parsedURL, _ := url.Parse(tool.Spec.HTTP.URL)
		host := parsedURL.Hostname()
		blockedHosts := []string{
			"169.254.169.254",
			"metadata.google.internal",
			"kubernetes.default",
			"kubernetes.default.svc",
		}
		if slices.Contains(blockedHosts, host) {
			return fmt.Errorf("tool URL host %q is not allowed", host)
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return fmt.Errorf("tool URL must not target private/loopback addresses")
			}
		}
		// Resolve DNS hostnames and block private/loopback IPs
		if net.ParseIP(host) == nil {
			ips, err := net.LookupHost(host)
			if err == nil {
				for _, ipStr := range ips {
					ip := net.ParseIP(ipStr)
					if ip == nil {
						continue
					}
					if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
						return fmt.Errorf("tool URL resolves to private/loopback IP %s", ipStr)
					}
				}
			}
		}
	}

	return nil
}

func (r *ToolReconciler) validateToolHTTPAuth(ctx context.Context, tool *corev1alpha1.Tool) error {
	if tool == nil || tool.Spec.HTTP == nil {
		return nil
	}
	if tool.Spec.HTTP.AuthSecretRef != nil {
		secret := &corev1.Secret{}
		key := client.ObjectKey{Name: tool.Spec.HTTP.AuthSecretRef.Name, Namespace: tool.Namespace}
		if err := r.Get(ctx, key, secret); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced auth secret %q not found", tool.Spec.HTTP.AuthSecretRef.Name)
			}
			return fmt.Errorf("failed to get auth secret %q: %w", tool.Spec.HTTP.AuthSecretRef.Name, err)
		}
		if _, ok := secret.Data[tool.Spec.HTTP.AuthSecretRef.Key]; !ok {
			return fmt.Errorf("key %q not found in auth secret %q", tool.Spec.HTTP.AuthSecretRef.Key, tool.Spec.HTTP.AuthSecretRef.Name)
		}
	}

	// Validate authInject + authBodyKey combination
	if tool.Spec.HTTP.AuthInject == "body" && tool.Spec.HTTP.AuthBodyKey == "" {
		return fmt.Errorf("authBodyKey is required when authInject is 'body'")
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil && tool.Spec.HTTP.AuthInject == "body" {
		return fmt.Errorf("MCP tools do not support authInject=body")
	}

	return nil
}

func (r *ToolReconciler) validateSubstrateMCPTool(ctx context.Context, tool *corev1alpha1.Tool) error {
	if !r.SubstrateEnabled {
		return fmt.Errorf("MCP substrateActor requires substrate to be enabled")
	}
	actor := tool.Spec.MCP.SubstrateActor
	if strings.TrimSpace(actor.TemplateRef.Name) == "" {
		return fmt.Errorf("mcp.substrateActor.templateRef.name is required")
	}
	if actor.PoolRef != nil && strings.TrimSpace(actor.PoolRef.Name) == "" {
		return fmt.Errorf("mcp.substrateActor.poolRef.name is required")
	}
	templateRequest := r.substrateMCPTemplateRequest(tool)
	if err := validateSubstrateActorTemplateResource(ctx, r.Client, templateRequest); err != nil {
		return err
	}
	if _, _, _, err := r.resolveSubstrateMCPActorPool(ctx, tool, templateRequest); err != nil {
		return err
	}
	return nil
}

func (r *ToolReconciler) substrateMCPTemplateRequest(tool *corev1alpha1.Tool) *ExecutionWorkspaceRequest {
	actorSpec := tool.Spec.MCP.SubstrateActor
	templateNamespace := strings.TrimSpace(actorSpec.TemplateRef.Namespace)
	if templateNamespace == "" {
		templateNamespace = tool.Namespace
	}
	cfg := r.SubstrateConfig.WithDefaults()
	return &ExecutionWorkspaceRequest{
		TemplateName:                 strings.TrimSpace(actorSpec.TemplateRef.Name),
		TemplateNamespace:            templateNamespace,
		SubstrateBootstrapSecretName: cfg.BootstrapSecretName,
		SubstrateBootstrapSecretKey:  cfg.BootstrapSecretKey,
	}
}

func (r *ToolReconciler) reconcileSubstrateMCPTool(ctx context.Context, tool *corev1alpha1.Tool) (ctrl.Result, error) {
	actorSpec := tool.Spec.MCP.SubstrateActor
	templateRequest := r.substrateMCPTemplateRequest(tool)
	actorID := deterministicSubstrateToolActorID(tool.Namespace, tool.Name, templateRequest.TemplateNamespace, templateRequest.TemplateName)
	poolName, poolNamespace, pool, err := r.resolveSubstrateMCPActorPool(ctx, tool, templateRequest)
	if err != nil {
		return r.updateStatus(ctx, tool, false, err.Error())
	}
	var poolRef *corev1alpha1.SubstrateActorPoolReference
	if poolName != "" {
		prefix := deterministicSubstratePoolActorPrefix(poolNamespace, poolName)
		ordinal := deterministicSubstratePoolActorOrdinal(
			pool.Spec.TargetActors,
			prefix,
			tool.Namespace,
			tool.Name,
			templateRequest.TemplateNamespace,
			templateRequest.TemplateName,
		)
		actorID = deterministicSubstratePoolActorID(prefix, ordinal)
		poolRef = &corev1alpha1.SubstrateActorPoolReference{Name: poolName, Namespace: poolNamespace}
		if assignedActorID := assignedSubstrateMCPPoolActorID(tool, poolName, poolNamespace, prefix, int(pool.Spec.TargetActors)); assignedActorID != "" {
			actorID = assignedActorID
		}
	}
	cleanupRef, err := r.substrateMCPReplacementCleanupRef(ctx, tool, actorID)
	if err != nil {
		return r.updateStatus(ctx, tool, false, err.Error())
	}
	ownershipChanged := ensureSubstrateMCPToolActorOwnership(tool, actorID, poolRef)
	pendingCleanupChanged := ensureSubstrateMCPToolCleanupState(tool, actorID, cleanupRef)
	if ownershipChanged || pendingCleanupChanged {
		if err := r.Update(ctx, tool); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if poolRef != nil {
		reservedActorID, reserved, err := r.reserveSubstrateMCPPoolActor(ctx, tool, poolNamespace, actorID, int(pool.Spec.TargetActors))
		if err != nil {
			return r.updateStatus(ctx, tool, false, err.Error())
		}
		if !reserved {
			return r.updateStatus(ctx, tool, false, fmt.Sprintf("all substrate pool actors in %q/%q are leased", poolNamespace, poolName))
		}
		if reservedActorID != actorID {
			actorID = reservedActorID
			cleanupRef, err = r.substrateMCPReplacementCleanupRef(ctx, tool, actorID)
			if err != nil {
				return r.updateStatus(ctx, tool, false, err.Error())
			}
			ownershipChanged = ensureSubstrateMCPToolActorOwnership(tool, actorID, poolRef)
			pendingCleanupChanged = ensureSubstrateMCPToolCleanupState(tool, actorID, cleanupRef)
			if ownershipChanged || pendingCleanupChanged {
				if err := r.Update(ctx, tool); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
	}
	if poolRef == nil {
		if err := r.ensureSubstrateMCPToolActorLease(ctx, tool, actorID); err != nil {
			return r.updateStatus(ctx, tool, false, err.Error())
		}
	}
	cfg := r.SubstrateConfig.WithDefaults()
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(workspace.SubstrateConfig{
				APIEndpoint:           cfg.APIEndpoint,
				APICAFile:             cfg.APICAFile,
				APIInsecureSkipVerify: cfg.APIInsecureSkipVerify,
				RouterURL:             cfg.RouterURL,
				ActorDNSSuffix:        cfg.ActorDNSSuffix,
			})
		}
	}
	executor, err := executorFactory(cfg)
	if err != nil {
		return r.updateStatus(ctx, tool, false, err.Error())
	}
	defer closeWorkspaceExecutor(ctx, executor)
	claim, err := executor.Claim(ctx, workspace.ClaimRequest{
		Namespace:       templateRequest.TemplateNamespace,
		ClaimName:       actorID,
		CreateIfMissing: true,
		Template: workspace.TemplateRef{
			Namespace: templateRequest.TemplateNamespace,
			Name:      templateRequest.TemplateName,
		},
		Timeout: cfg.ClaimTimeout,
	})
	if err != nil {
		return r.updateStatus(ctx, tool, false, err.Error())
	}
	if result, done, err := r.waitForSubstrateMCPToolActor(ctx, tool, executor, claim, actorID, actorSpec.Boot, cfg.ClaimTimeout); done || err != nil {
		return result, err
	}

	path := strings.TrimSpace(tool.Spec.MCP.Path)
	if path == "" {
		path = "/mcp"
	}
	endpoint := strings.TrimRight(cfg.RouterURL, "/") + "/" + strings.TrimLeft(path, "/")
	routeHost := actorID + "." + strings.Trim(strings.TrimSpace(cfg.ActorDNSSuffix), ".")
	templateRef := &corev1alpha1.WorkspaceTemplateReference{Name: templateRequest.TemplateName, Namespace: templateRequest.TemplateNamespace}
	actorStatus := &corev1alpha1.ToolActorStatus{
		Provider:    corev1alpha1.WorkspaceProviderSubstrate,
		ActorID:     actorID,
		RouteHost:   routeHost,
		TemplateRef: templateRef,
		PoolRef:     poolRef,
	}
	if err := r.healthCheckMCPActorEndpoint(ctx, endpoint, routeHost); err != nil {
		if cleanupRef != nil && tool.Status.Actor != nil {
			previousActor := *tool.Status.Actor
			return r.updateStatusWithActor(ctx, tool, false, err.Error(), tool.Status.Endpoint, &previousActor)
		}
		return r.updateStatusWithActor(ctx, tool, false, err.Error(), endpoint, actorStatus)
	}
	if err := r.cleanupPendingSubstrateMCPActor(ctx, executor, tool, cleanupRef, cfg.ClaimTimeout); err != nil {
		return ctrl.Result{}, err
	}
	return r.updateStatusWithActor(ctx, tool, true, "", endpoint, actorStatus)
}

func (r *ToolReconciler) waitForSubstrateMCPToolActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	executor workspace.WorkspaceExecutor,
	claim *workspace.ClaimResult,
	actorID string,
	bootRequested bool,
	timeout time.Duration,
) (ctrl.Result, bool, error) {
	bootActor := shouldBootSubstrateMCPToolActor(tool, actorID, bootRequested, claim.Created)
	if _, err := executor.WaitReady(ctx, workspace.WaitReadyRequest{
		Ref:     claim.Ref,
		Timeout: timeout,
		Boot:    bootActor,
	}); err != nil {
		result, updateErr := r.updateStatus(ctx, tool, false, err.Error())
		return result, true, updateErr
	}
	if bootRequested && !substrateMCPToolActorBootRecorded(tool, actorID) {
		canContinue, err := r.recordSubstrateMCPToolActorBooted(ctx, tool, actorID)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if !canContinue {
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
	}
	return ctrl.Result{}, false, nil
}

func shouldBootSubstrateMCPToolActor(tool *corev1alpha1.Tool, actorID string, requested bool, created bool) bool {
	actorID = strings.TrimSpace(actorID)
	if !requested || actorID == "" {
		return false
	}
	if created {
		return true
	}
	if tool == nil {
		return true
	}
	if strings.TrimSpace(tool.Annotations[substrateMCPToolBootedIDAnno]) == actorID {
		return false
	}
	return tool.Status.Actor == nil ||
		tool.Status.Actor.Provider != corev1alpha1.WorkspaceProviderSubstrate ||
		strings.TrimSpace(tool.Status.Actor.ActorID) != actorID
}

func substrateMCPToolActorBootRecorded(tool *corev1alpha1.Tool, actorID string) bool {
	if tool == nil {
		return false
	}
	return strings.TrimSpace(tool.Annotations[substrateMCPToolBootedIDAnno]) == strings.TrimSpace(actorID)
}

func markSubstrateMCPToolActorBooted(tool *corev1alpha1.Tool, actorID string) bool {
	actorID = strings.TrimSpace(actorID)
	if tool == nil || actorID == "" {
		return false
	}
	if tool.Annotations == nil {
		tool.Annotations = map[string]string{}
	}
	if strings.TrimSpace(tool.Annotations[substrateMCPToolBootedIDAnno]) == actorID {
		return false
	}
	tool.Annotations[substrateMCPToolBootedIDAnno] = actorID
	return true
}

func (r *ToolReconciler) recordSubstrateMCPToolActorBooted(ctx context.Context, tool *corev1alpha1.Tool, actorID string) (bool, error) {
	actorID = strings.TrimSpace(actorID)
	if tool == nil || actorID == "" {
		return true, nil
	}
	key := types.NamespacedName{Namespace: tool.Namespace, Name: tool.Name}
	var latestForStatus *corev1alpha1.Tool
	specChanged := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Tool{}
		if getErr := r.Get(ctx, key, latest); getErr != nil {
			if errors.IsNotFound(getErr) {
				return nil
			}
			return getErr
		}
		if latest.Generation != tool.Generation || !reflect.DeepEqual(latest.Spec, tool.Spec) {
			specChanged = true
			return nil
		}
		if !markSubstrateMCPToolActorBooted(latest, actorID) {
			latestForStatus = latest.DeepCopy()
			return nil
		}
		if updateErr := r.Update(ctx, latest); updateErr != nil {
			return updateErr
		}
		latestForStatus = latest.DeepCopy()
		return nil
	})
	if err != nil {
		return false, err
	}
	if specChanged || latestForStatus == nil {
		return false, nil
	}
	latestForStatus.DeepCopyInto(tool)
	return true, nil
}

func (r *ToolReconciler) resolveSubstrateMCPActorPool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	templateRequest *ExecutionWorkspaceRequest,
) (string, string, *corev1alpha1.SubstrateActorPool, error) {
	if tool == nil || tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil {
		return "", "", nil, nil
	}
	poolName, poolNamespace := substrateActorPoolReference(tool.Spec.MCP.SubstrateActor.PoolRef, tool.Namespace)
	if poolName == "" {
		return "", "", nil, nil
	}
	if r.EnforceNamespaceIsolation && poolNamespace != tool.Namespace {
		return "", "", nil, fmt.Errorf(
			"cross-namespace substrate actor poolRef not allowed when namespace isolation is enforced: pool %q in namespace %q, tool in %q",
			poolName,
			poolNamespace,
			tool.Namespace,
		)
	}
	pool, err := resolveSubstrateActorPoolReference(
		ctx,
		r.Client,
		poolNamespace,
		poolName,
		templateRequest.TemplateNamespace,
		templateRequest.TemplateName,
	)
	if err != nil {
		return "", "", nil, err
	}
	return poolName, poolNamespace, pool, nil
}

func (r *ToolReconciler) finalizeSubstrateMCPTool(ctx context.Context, tool *corev1alpha1.Tool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
		return ctrl.Result{}, nil
	}
	cfg := r.SubstrateConfig.WithDefaults()
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(workspace.SubstrateConfig{
				APIEndpoint:           cfg.APIEndpoint,
				APICAFile:             cfg.APICAFile,
				APIInsecureSkipVerify: cfg.APIInsecureSkipVerify,
				RouterURL:             cfg.RouterURL,
				ActorDNSSuffix:        cfg.ActorDNSSuffix,
			})
		}
	}
	poolRefs := substrateMCPPoolActorLeaseRefs(tool)
	heldPoolRefs, err := r.heldSubstrateMCPPoolActorLeaseRefs(ctx, tool, poolRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	deleteRefs := r.substrateMCPFinalizerActorDeleteRefs(tool)
	leasedDeleteRefs, err := r.substrateMCPToolActorLeaseDeleteRefs(ctx, tool, "MCP tool deleted")
	if err != nil {
		return ctrl.Result{}, err
	}
	deleteRefs = appendSubstrateMCPActorDeleteRefs(deleteRefs, leasedDeleteRefs...)
	if len(deleteRefs) > 0 || len(heldPoolRefs) > 0 {
		executor, err := executorFactory(cfg)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer closeWorkspaceExecutor(ctx, executor)
		for _, ref := range deleteRefs {
			if err := deleteSubstrateMCPActor(ctx, executor, ref.actorID, ref.reason, cfg.ClaimTimeout); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.deleteSubstrateMCPToolActorLease(ctx, tool, tool.Namespace, ref.actorID); err != nil {
				return ctrl.Result{}, err
			}
		}
		for _, ref := range heldPoolRefs {
			if err := r.cleanupSubstrateMCPPoolActor(ctx, executor, tool, ref.poolNamespace, ref.actorID, "MCP pooled tool actor deleted", cfg.ClaimTimeout); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	controllerutil.RemoveFinalizer(tool, substrateMCPToolActorFinalizer)
	removeSubstrateMCPToolActorOwnership(tool)
	if err := r.Update(ctx, tool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ToolReconciler) cleanupPendingSubstrateMCPActor(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	tool *corev1alpha1.Tool,
	trustedRef *substrateMCPActorCleanupRef,
	timeout time.Duration,
) error {
	ref := trustedRef
	if ref == nil {
		ref = pendingSubstrateMCPActorCleanupRef(tool)
		if ref == nil {
			return nil
		}
		if ref.poolName == substrateMCPToolCleanupNonPooledValueAnno {
			_, held, err := r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, tool.Namespace, ref.actorID)
			if err != nil {
				return err
			}
			if !held && clearSubstrateMCPToolPendingCleanup(tool) {
				if err := r.Update(ctx, tool); err != nil && !errors.IsNotFound(err) {
					return err
				}
				return nil
			}
		}
	}
	if ref.poolName == substrateMCPToolCleanupNonPooledValueAnno {
		if executor == nil {
			return fmt.Errorf("workspace executor is required to delete pending MCP actor %q", ref.actorID)
		}
		if err := deleteSubstrateMCPActor(ctx, executor, ref.actorID, "MCP tool actor replaced", timeout); err != nil {
			return err
		}
		if err := r.deleteSubstrateMCPToolActorLease(ctx, tool, tool.Namespace, ref.actorID); err != nil {
			return err
		}
	} else {
		if err := r.cleanupSubstrateMCPPoolActor(ctx, executor, tool, ref.poolNamespace, ref.actorID, "MCP pooled tool actor replaced", timeout); err != nil {
			return err
		}
	}
	if clearSubstrateMCPToolPendingCleanup(tool) {
		if err := r.Update(ctx, tool); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (r *ToolReconciler) cleanupSubstrateMCPPoolActor(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
	reason string,
	timeout time.Duration,
) error {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return nil
	}
	if executor == nil {
		return fmt.Errorf("workspace executor is required to delete pooled MCP actor %q", actorID)
	}
	lease, held, err := r.substrateMCPPoolActorLeaseHeldByTool(ctx, tool, leaseNamespace, actorID)
	if err != nil {
		return err
	}
	if lease == nil {
		return nil
	}
	if !held {
		return nil
	}
	if err := deleteSubstrateMCPActor(ctx, executor, actorID, reason, timeout); err != nil {
		return err
	}
	if err := r.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !errors.IsNotFound(err) {
		if errors.IsConflict(err) {
			stillHeld, verifyErr := substrateLeaseStillMatchesAfterDeleteConflict(ctx, r.Client, lease, func(latest *coordinationv1.Lease) bool {
				return substratePoolActorLeaseHeldByTool(latest, tool)
			})
			if verifyErr != nil {
				return verifyErr
			}
			if !stillHeld {
				return nil
			}
		}
		return err
	}
	return nil
}

func (r *ToolReconciler) substrateMCPPoolActorLeaseHeldByTool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) (*coordinationv1.Lease, bool, error) {
	actorID = strings.TrimSpace(actorID)
	leaseNamespace = strings.TrimSpace(leaseNamespace)
	if actorID == "" || leaseNamespace == "" {
		return nil, false, nil
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: leaseNamespace, Name: substratePoolActorLeaseName(actorID)}
	if err := r.Get(ctx, key, lease); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !substratePoolActorLeaseHeldByTool(lease, tool) {
		return lease, false, nil
	}
	return lease, true, nil
}

func (r *ToolReconciler) ensureSubstrateMCPToolActorLease(ctx context.Context, tool *corev1alpha1.Tool, actorID string) error {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return nil
	}
	key := types.NamespacedName{Namespace: tool.Namespace, Name: substrateMCPToolActorLeaseName(actorID)}
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, key, lease); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		lease = newSubstrateMCPToolActorLease(tool, tool.Namespace, actorID)
		if err := r.Create(ctx, lease); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}
			return err
		}
		return nil
	}
	if !substrateMCPToolActorLeaseHeldByTool(lease, tool) {
		return fmt.Errorf("substrate MCP tool actor lease %q in namespace %q is held by another owner", lease.Name, lease.Namespace)
	}
	patch := client.MergeFromWithOptions(lease.DeepCopy(), client.MergeFromWithOptimisticLock{})
	setSubstrateMCPToolActorLeaseHolder(lease, tool, actorID)
	if err := r.Patch(ctx, lease, patch); err != nil {
		if errors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

func (r *ToolReconciler) substrateMCPToolActorLeaseReplacementCleanupRef(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	desiredActorID string,
) (*substrateMCPActorCleanupRef, error) {
	if tool == nil || tool.Annotations == nil {
		return nil, nil
	}
	oldActorID := strings.TrimSpace(tool.Annotations[substrateMCPToolActorIDAnno])
	desiredActorID = strings.TrimSpace(desiredActorID)
	if oldActorID == "" || oldActorID == desiredActorID {
		return nil, nil
	}
	_, held, err := r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, tool.Namespace, oldActorID)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	return &substrateMCPActorCleanupRef{
		actorID:  oldActorID,
		poolName: substrateMCPToolCleanupNonPooledValueAnno,
	}, nil
}

func (r *ToolReconciler) substrateMCPToolActorLeaseDeleteRefs(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	reason string,
) ([]substrateMCPActorDeleteRef, error) {
	if tool == nil {
		return nil, nil
	}
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.InNamespace(tool.Namespace), client.MatchingLabels{
		labels.LabelPurpose: substrateMCPToolActorLeasePurpose,
	}); err != nil {
		return nil, err
	}
	refs := make([]substrateMCPActorDeleteRef, 0, len(leases.Items))
	for i := range leases.Items {
		lease := &leases.Items[i]
		if !substrateMCPToolActorLeaseHeldByTool(lease, tool) {
			continue
		}
		actorID := substrateMCPToolActorLeaseActorID(lease)
		if actorID == "" {
			continue
		}
		refs = append(refs, substrateMCPActorDeleteRef{actorID: actorID, reason: reason})
	}
	return refs, nil
}

func (r *ToolReconciler) substrateMCPToolActorLeaseHeldByTool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) (*coordinationv1.Lease, bool, error) {
	actorID = strings.TrimSpace(actorID)
	leaseNamespace = strings.TrimSpace(leaseNamespace)
	if actorID == "" || leaseNamespace == "" {
		return nil, false, nil
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: leaseNamespace, Name: substrateMCPToolActorLeaseName(actorID)}
	if err := r.Get(ctx, key, lease); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if lease.Labels[labels.LabelPurpose] != substrateMCPToolActorLeasePurpose ||
		!substrateMCPToolActorLeaseHeldByTool(lease, tool) {
		return lease, false, nil
	}
	return lease, true, nil
}

func (r *ToolReconciler) deleteSubstrateMCPToolActorLease(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) error {
	lease, held, err := r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, leaseNamespace, actorID)
	if err != nil {
		return err
	}
	if lease == nil || !held {
		return nil
	}
	if err := r.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !errors.IsNotFound(err) {
		if errors.IsConflict(err) {
			stillHeld, verifyErr := substrateLeaseStillMatchesAfterDeleteConflict(ctx, r.Client, lease, func(latest *coordinationv1.Lease) bool {
				return substrateMCPToolActorLeaseHeldByTool(latest, tool)
			})
			if verifyErr != nil {
				return verifyErr
			}
			if !stillHeld {
				return nil
			}
		}
		return err
	}
	return nil
}

type substrateMCPActorCleanupRef struct {
	actorID       string
	poolName      string
	poolNamespace string
}

func (r *ToolReconciler) substrateMCPReplacementCleanupRef(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	desiredActorID string,
) (*substrateMCPActorCleanupRef, error) {
	if ref := substrateMCPStatusReplacementCleanupRef(tool, desiredActorID); ref != nil {
		return ref, nil
	}
	if ref, err := r.substrateMCPToolActorLeaseReplacementCleanupRef(ctx, tool, desiredActorID); err != nil {
		return nil, err
	} else if ref != nil {
		return ref, nil
	}
	return r.substrateMCPLeasedAnnotationReplacementCleanupRef(ctx, tool, desiredActorID)
}

func substrateMCPStatusReplacementCleanupRef(tool *corev1alpha1.Tool, desiredActorID string) *substrateMCPActorCleanupRef {
	if tool == nil || tool.Status.Actor == nil {
		return nil
	}
	actor := tool.Status.Actor
	oldActorID := strings.TrimSpace(actor.ActorID)
	desiredActorID = strings.TrimSpace(desiredActorID)
	if actor.Provider != corev1alpha1.WorkspaceProviderSubstrate ||
		oldActorID == "" ||
		oldActorID == desiredActorID {
		return nil
	}
	if actor.PoolRef == nil {
		return &substrateMCPActorCleanupRef{
			actorID:  oldActorID,
			poolName: substrateMCPToolCleanupNonPooledValueAnno,
		}
	}
	poolName := strings.TrimSpace(actor.PoolRef.Name)
	poolNamespace := strings.TrimSpace(actor.PoolRef.Namespace)
	if poolNamespace == "" {
		poolNamespace = tool.Namespace
	}
	return &substrateMCPActorCleanupRef{
		actorID:       oldActorID,
		poolName:      poolName,
		poolNamespace: poolNamespace,
	}
}

func (r *ToolReconciler) substrateMCPLeasedAnnotationReplacementCleanupRef(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	desiredActorID string,
) (*substrateMCPActorCleanupRef, error) {
	if tool == nil || tool.Annotations == nil {
		return nil, nil
	}
	oldActorID := strings.TrimSpace(tool.Annotations[substrateMCPToolActorIDAnno])
	desiredActorID = strings.TrimSpace(desiredActorID)
	if oldActorID == "" || oldActorID == desiredActorID {
		return nil, nil
	}
	poolName := strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNameAnno])
	if poolName == "" {
		return nil, nil
	}
	poolNamespace := strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNamespaceAnno])
	if poolNamespace == "" {
		poolNamespace = tool.Namespace
	}
	_, held, err := r.substrateMCPPoolActorLeaseHeldByTool(ctx, tool, poolNamespace, oldActorID)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	return &substrateMCPActorCleanupRef{
		actorID:       oldActorID,
		poolName:      poolName,
		poolNamespace: poolNamespace,
	}, nil
}

func (r *ToolReconciler) tryReserveSubstrateMCPPoolActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) (bool, error) {
	leaseName := substratePoolActorLeaseName(actorID)
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: leaseNamespace, Name: leaseName}
	err := r.Get(ctx, key, lease)
	if errors.IsNotFound(err) {
		lease = newSubstrateMCPPoolActorLease(tool, leaseNamespace, leaseName, actorID)
		if err := r.Create(ctx, lease); err != nil {
			if errors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if substratePoolActorLeaseHeldByTool(lease, tool) {
		return true, nil
	}
	busy, err := substratePoolActorLeaseHasActiveHolder(ctx, r.Client, lease)
	if err != nil || busy {
		return false, err
	}
	patch := client.MergeFromWithOptions(lease.DeepCopy(), client.MergeFromWithOptimisticLock{})
	setSubstrateMCPPoolActorLeaseHolder(lease, tool, actorID)
	if err := r.Patch(ctx, lease, patch); err != nil {
		if errors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *ToolReconciler) reserveSubstrateMCPPoolActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	startActorID string,
	target int,
) (string, bool, error) {
	if target <= 0 {
		return "", false, fmt.Errorf("substrate actor pool in namespace %q has no target actors", leaseNamespace)
	}
	prefix, startOrdinal, ok := substratePoolActorPrefixAndOrdinal(startActorID)
	if !ok {
		return "", false, fmt.Errorf("substrate pool actor id %q is not deterministic", startActorID)
	}
	if actorID, found, err := r.substrateMCPPoolActorLeaseForTool(ctx, tool, leaseNamespace, prefix, target); err != nil {
		return "", false, err
	} else if found {
		return actorID, true, nil
	}
	for offset := range target {
		ordinal := (startOrdinal + offset) % target
		actorID := deterministicSubstratePoolActorID(prefix, ordinal)
		reserved, err := r.tryReserveSubstrateMCPPoolActor(ctx, tool, leaseNamespace, actorID)
		if err != nil {
			return "", false, err
		}
		if reserved {
			return actorID, true, nil
		}
	}
	return "", false, nil
}

func (r *ToolReconciler) substrateMCPPoolActorLeaseForTool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	prefix string,
	target int,
) (string, bool, error) {
	if tool == nil {
		return "", false, nil
	}
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.InNamespace(leaseNamespace), client.MatchingLabels{
		labels.LabelPurpose: substratePoolActorLeasePurpose,
	}); err != nil {
		return "", false, err
	}
	for i := range leases.Items {
		lease := &leases.Items[i]
		if !substratePoolActorLeaseHeldByTool(lease, tool) {
			continue
		}
		actorID := substratePoolActorLeaseActorID(lease)
		if ordinal, ok := substratePoolActorOrdinalFromID(actorID, prefix); ok && ordinal < target {
			return actorID, true, nil
		}
	}
	return "", false, nil
}

func (r *ToolReconciler) heldSubstrateMCPPoolActorLeaseRefs(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	refs []substrateMCPPoolActorLeaseRef,
) ([]substrateMCPPoolActorLeaseRef, error) {
	heldRefs := make([]substrateMCPPoolActorLeaseRef, 0, len(refs))
	for _, ref := range refs {
		_, held, err := r.substrateMCPPoolActorLeaseHeldByTool(ctx, tool, ref.poolNamespace, ref.actorID)
		if err != nil {
			return nil, err
		}
		if held {
			heldRefs = append(heldRefs, ref)
		}
	}
	return heldRefs, nil
}

func substrateMCPPoolActorLeaseRefs(tool *corev1alpha1.Tool) []substrateMCPPoolActorLeaseRef {
	if tool == nil {
		return nil
	}
	refs := make([]substrateMCPPoolActorLeaseRef, 0, 2)
	seen := map[substrateMCPPoolActorLeaseRef]struct{}{}
	add := func(namespace, actorID string) {
		ref := substrateMCPPoolActorLeaseRef{
			poolNamespace: strings.TrimSpace(namespace),
			actorID:       strings.TrimSpace(actorID),
		}
		if ref.poolNamespace == "" || ref.actorID == "" {
			return
		}
		if _, ok := seen[ref]; ok {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	if tool.Status.Actor != nil && tool.Status.Actor.PoolRef != nil {
		namespace := strings.TrimSpace(tool.Status.Actor.PoolRef.Namespace)
		if namespace == "" {
			namespace = tool.Namespace
		}
		add(namespace, tool.Status.Actor.ActorID)
	}
	if tool.Annotations != nil {
		add(tool.Annotations[substrateMCPToolActorPoolNamespaceAnno], tool.Annotations[substrateMCPToolActorIDAnno])
		if tool.Annotations[substrateMCPToolCleanupPoolNameAnno] != substrateMCPToolCleanupNonPooledValueAnno {
			add(tool.Annotations[substrateMCPToolCleanupPoolNamespaceAnno], tool.Annotations[substrateMCPToolCleanupActorIDAnno])
		}
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
		poolName, poolNamespace := substrateActorPoolReference(tool.Spec.MCP.SubstrateActor.PoolRef, tool.Namespace)
		if poolName != "" {
			add(poolNamespace, tool.Annotations[substrateMCPToolActorIDAnno])
		}
	}
	return refs
}

func assignedSubstrateMCPPoolActorID(tool *corev1alpha1.Tool, poolName string, poolNamespace string, prefix string, target int) string {
	if tool == nil {
		return ""
	}
	if tool.Annotations != nil &&
		strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNameAnno]) == strings.TrimSpace(poolName) &&
		strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNamespaceAnno]) == strings.TrimSpace(poolNamespace) {
		actorID := strings.TrimSpace(tool.Annotations[substrateMCPToolActorIDAnno])
		if ordinal, ok := substratePoolActorOrdinalFromID(actorID, prefix); ok && ordinal < target {
			return actorID
		}
	}
	if tool.Status.Actor != nil &&
		tool.Status.Actor.Provider == corev1alpha1.WorkspaceProviderSubstrate &&
		tool.Status.Actor.PoolRef != nil {
		name, namespace := substrateActorPoolReference(tool.Status.Actor.PoolRef, tool.Namespace)
		if name == strings.TrimSpace(poolName) && namespace == strings.TrimSpace(poolNamespace) {
			actorID := strings.TrimSpace(tool.Status.Actor.ActorID)
			if ordinal, ok := substratePoolActorOrdinalFromID(actorID, prefix); ok && ordinal < target {
				return actorID
			}
		}
	}
	return ""
}

func substratePoolActorPrefixAndOrdinal(actorID string) (string, int, bool) {
	actorID = strings.TrimSpace(actorID)
	if len(actorID) < 7 {
		return "", 0, false
	}
	separator := len(actorID) - 6
	if actorID[separator] != '-' {
		return "", 0, false
	}
	prefix := strings.TrimSpace(actorID[:separator])
	if prefix == "" {
		return "", 0, false
	}
	ordinal := 0
	for _, ch := range actorID[separator+1:] {
		if ch < '0' || ch > '9' {
			return "", 0, false
		}
		ordinal = ordinal*10 + int(ch-'0')
	}
	return prefix, ordinal, true
}

type substrateMCPPoolActorLeaseRef struct {
	poolNamespace string
	actorID       string
}

func newSubstrateMCPPoolActorLease(
	tool *corev1alpha1.Tool,
	namespace string,
	name string,
	actorID string,
) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
	setSubstrateMCPPoolActorLeaseHolder(lease, tool, actorID)
	return lease
}

func setSubstrateMCPPoolActorLeaseHolder(lease *coordinationv1.Lease, tool *corev1alpha1.Tool, actorID string) {
	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	lease.Labels[labels.LabelManaged] = managedLabelValue
	lease.Labels[labels.LabelPurpose] = substratePoolActorLeasePurpose
	lease.Labels[substratePoolActorLeaseActorIDLabel] = labels.SelectorValue(actorID)
	lease.Labels[substratePoolActorLeaseHolderUIDLabel] = labels.SelectorValue(string(tool.UID))
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	delete(lease.Annotations, substratePoolActorLeaseTaskNSAnno)
	delete(lease.Annotations, substratePoolActorLeaseTaskNameAnno)
	delete(lease.Annotations, substratePoolActorLeaseTaskUIDAnno)
	lease.Annotations[substratePoolActorLeaseToolNSAnno] = tool.Namespace
	lease.Annotations[substratePoolActorLeaseToolNameAnno] = tool.Name
	lease.Annotations[substratePoolActorLeaseToolUIDAnno] = string(tool.UID)
	now := metav1.NewMicroTime(time.Now())
	holder := fmt.Sprintf("tool/%s/%s/%s", tool.Namespace, tool.Name, tool.UID)
	lease.Spec.HolderIdentity = &holder
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
}

func substrateMCPToolActorLeaseName(actorID string) string {
	return strings.TrimSpace(actorID)
}

func newSubstrateMCPToolActorLease(
	tool *corev1alpha1.Tool,
	namespace string,
	actorID string,
) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      substrateMCPToolActorLeaseName(actorID),
		},
	}
	setSubstrateMCPToolActorLeaseHolder(lease, tool, actorID)
	return lease
}

func setSubstrateMCPToolActorLeaseHolder(lease *coordinationv1.Lease, tool *corev1alpha1.Tool, actorID string) {
	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	lease.Labels[labels.LabelManaged] = managedLabelValue
	lease.Labels[labels.LabelPurpose] = substrateMCPToolActorLeasePurpose
	lease.Labels[substratePoolActorLeaseActorIDLabel] = labels.SelectorValue(actorID)
	lease.Labels[substratePoolActorLeaseHolderUIDLabel] = labels.SelectorValue(string(tool.UID))
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	delete(lease.Annotations, substratePoolActorLeaseTaskNSAnno)
	delete(lease.Annotations, substratePoolActorLeaseTaskNameAnno)
	delete(lease.Annotations, substratePoolActorLeaseTaskUIDAnno)
	lease.Annotations[substratePoolActorLeaseToolNSAnno] = tool.Namespace
	lease.Annotations[substratePoolActorLeaseToolNameAnno] = tool.Name
	lease.Annotations[substratePoolActorLeaseToolUIDAnno] = string(tool.UID)
	now := metav1.NewMicroTime(time.Now())
	holder := fmt.Sprintf("tool/%s/%s/%s", tool.Namespace, tool.Name, tool.UID)
	lease.Spec.HolderIdentity = &holder
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
}

func substratePoolActorLeaseHeldByTool(lease *coordinationv1.Lease, tool *corev1alpha1.Tool) bool {
	if lease == nil || tool == nil || lease.Annotations == nil {
		return false
	}
	if lease.Annotations[substratePoolActorLeaseToolNSAnno] != tool.Namespace ||
		lease.Annotations[substratePoolActorLeaseToolNameAnno] != tool.Name {
		return false
	}
	leaseUID := lease.Annotations[substratePoolActorLeaseToolUIDAnno]
	return leaseUID == "" || string(tool.UID) == "" || leaseUID == string(tool.UID)
}

func substrateMCPToolActorLeaseHeldByTool(lease *coordinationv1.Lease, tool *corev1alpha1.Tool) bool {
	if lease == nil || tool == nil || lease.Annotations == nil {
		return false
	}
	if lease.Annotations[substratePoolActorLeaseToolNSAnno] != tool.Namespace ||
		lease.Annotations[substratePoolActorLeaseToolNameAnno] != tool.Name {
		return false
	}
	leaseUID := lease.Annotations[substratePoolActorLeaseToolUIDAnno]
	return leaseUID == "" || string(tool.UID) == "" || leaseUID == string(tool.UID)
}

func substrateMCPToolActorLeaseActorID(lease *coordinationv1.Lease) string {
	if lease == nil {
		return ""
	}
	if lease.Labels != nil {
		if actorID := strings.TrimSpace(lease.Labels[substratePoolActorLeaseActorIDLabel]); actorID != "" {
			return actorID
		}
	}
	return strings.TrimSpace(lease.Name)
}

func pendingSubstrateMCPActorCleanupRef(tool *corev1alpha1.Tool) *substrateMCPActorCleanupRef {
	if tool == nil || tool.Annotations == nil {
		return nil
	}
	actorID := strings.TrimSpace(tool.Annotations[substrateMCPToolCleanupActorIDAnno])
	if actorID == "" {
		return nil
	}
	poolName := strings.TrimSpace(tool.Annotations[substrateMCPToolCleanupPoolNameAnno])
	if poolName == "" {
		poolName = substrateMCPToolCleanupNonPooledValueAnno
	}
	poolNamespace := strings.TrimSpace(tool.Annotations[substrateMCPToolCleanupPoolNamespaceAnno])
	if poolName != substrateMCPToolCleanupNonPooledValueAnno && poolNamespace == "" {
		poolNamespace = tool.Namespace
	}
	return &substrateMCPActorCleanupRef{
		actorID:       actorID,
		poolName:      poolName,
		poolNamespace: poolNamespace,
	}
}

func ensureSubstrateMCPToolPendingCleanup(tool *corev1alpha1.Tool, ref *substrateMCPActorCleanupRef) bool {
	if tool == nil || ref == nil || strings.TrimSpace(ref.actorID) == "" {
		return false
	}
	if tool.Annotations == nil {
		tool.Annotations = map[string]string{}
	}
	changed := false
	set := func(key, value string) {
		value = strings.TrimSpace(value)
		if tool.Annotations[key] != value {
			tool.Annotations[key] = value
			changed = true
		}
	}
	poolName := strings.TrimSpace(ref.poolName)
	if poolName == "" {
		poolName = substrateMCPToolCleanupNonPooledValueAnno
	}
	set(substrateMCPToolCleanupActorIDAnno, ref.actorID)
	set(substrateMCPToolCleanupPoolNameAnno, poolName)
	if poolName == substrateMCPToolCleanupNonPooledValueAnno {
		if _, ok := tool.Annotations[substrateMCPToolCleanupPoolNamespaceAnno]; ok {
			delete(tool.Annotations, substrateMCPToolCleanupPoolNamespaceAnno)
			changed = true
		}
		return changed
	}
	set(substrateMCPToolCleanupPoolNamespaceAnno, ref.poolNamespace)
	return changed
}

func ensureSubstrateMCPToolCleanupState(
	tool *corev1alpha1.Tool,
	desiredActorID string,
	ref *substrateMCPActorCleanupRef,
) bool {
	desiredActorID = strings.TrimSpace(desiredActorID)
	if ref != nil {
		if strings.TrimSpace(ref.actorID) == "" || strings.TrimSpace(ref.actorID) == desiredActorID {
			return clearSubstrateMCPToolPendingCleanup(tool)
		}
		return ensureSubstrateMCPToolPendingCleanup(tool, ref)
	}
	pending := pendingSubstrateMCPActorCleanupRef(tool)
	if pending != nil && strings.TrimSpace(pending.actorID) == desiredActorID {
		return clearSubstrateMCPToolPendingCleanup(tool)
	}
	return false
}

func clearSubstrateMCPToolPendingCleanup(tool *corev1alpha1.Tool) bool {
	if tool == nil || tool.Annotations == nil {
		return false
	}
	changed := false
	for _, key := range []string{
		substrateMCPToolCleanupActorIDAnno,
		substrateMCPToolCleanupPoolNameAnno,
		substrateMCPToolCleanupPoolNamespaceAnno,
	} {
		if _, ok := tool.Annotations[key]; ok {
			delete(tool.Annotations, key)
			changed = true
		}
	}
	if len(tool.Annotations) == 0 {
		tool.Annotations = nil
	}
	return changed
}

type substrateMCPActorDeleteRef struct {
	actorID string
	reason  string
}

func appendSubstrateMCPActorDeleteRefs(
	refs []substrateMCPActorDeleteRef,
	extraRefs ...substrateMCPActorDeleteRef,
) []substrateMCPActorDeleteRef {
	if len(extraRefs) == 0 {
		return refs
	}
	seen := make(map[string]struct{}, len(refs)+len(extraRefs))
	for _, ref := range refs {
		actorID := strings.TrimSpace(ref.actorID)
		if actorID == "" {
			continue
		}
		seen[actorID] = struct{}{}
	}
	for _, ref := range extraRefs {
		ref.actorID = strings.TrimSpace(ref.actorID)
		if ref.actorID == "" {
			continue
		}
		if _, ok := seen[ref.actorID]; ok {
			continue
		}
		seen[ref.actorID] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func (r *ToolReconciler) substrateMCPFinalizerActorDeleteRefs(tool *corev1alpha1.Tool) []substrateMCPActorDeleteRef {
	if tool == nil {
		return nil
	}
	refs := make([]substrateMCPActorDeleteRef, 0, 3)
	seen := map[string]struct{}{}
	add := func(actorID, reason string) {
		actorID = strings.TrimSpace(actorID)
		if actorID == "" {
			return
		}
		if _, ok := seen[actorID]; ok {
			return
		}
		seen[actorID] = struct{}{}
		refs = append(refs, substrateMCPActorDeleteRef{actorID: actorID, reason: reason})
	}
	if tool.Status.Actor != nil &&
		tool.Status.Actor.Provider == corev1alpha1.WorkspaceProviderSubstrate &&
		tool.Status.Actor.PoolRef == nil {
		add(tool.Status.Actor.ActorID, "MCP tool deleted")
	}
	if tool.Spec.MCP != nil &&
		tool.Spec.MCP.SubstrateActor != nil &&
		tool.Spec.MCP.SubstrateActor.PoolRef == nil {
		templateRequest := r.substrateMCPTemplateRequest(tool)
		if strings.TrimSpace(templateRequest.TemplateName) != "" {
			add(
				deterministicSubstrateToolActorID(tool.Namespace, tool.Name, templateRequest.TemplateNamespace, templateRequest.TemplateName),
				"MCP tool deleted",
			)
		}
	}
	return refs
}

func ensureSubstrateMCPToolActorOwnership(tool *corev1alpha1.Tool, actorID string, poolRef *corev1alpha1.SubstrateActorPoolReference) bool {
	changed := false
	if !controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
		controllerutil.AddFinalizer(tool, substrateMCPToolActorFinalizer)
		changed = true
	}
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return changed
	}
	if tool.Annotations == nil {
		tool.Annotations = map[string]string{}
	}
	if tool.Annotations[substrateMCPToolActorIDAnno] != actorID {
		tool.Annotations[substrateMCPToolActorIDAnno] = actorID
		changed = true
	}
	if poolRef == nil {
		if _, ok := tool.Annotations[substrateMCPToolActorPoolNameAnno]; ok {
			delete(tool.Annotations, substrateMCPToolActorPoolNameAnno)
			changed = true
		}
		if _, ok := tool.Annotations[substrateMCPToolActorPoolNamespaceAnno]; ok {
			delete(tool.Annotations, substrateMCPToolActorPoolNamespaceAnno)
			changed = true
		}
		return changed
	}
	if tool.Annotations[substrateMCPToolActorPoolNameAnno] != strings.TrimSpace(poolRef.Name) {
		tool.Annotations[substrateMCPToolActorPoolNameAnno] = strings.TrimSpace(poolRef.Name)
		changed = true
	}
	if tool.Annotations[substrateMCPToolActorPoolNamespaceAnno] != strings.TrimSpace(poolRef.Namespace) {
		tool.Annotations[substrateMCPToolActorPoolNamespaceAnno] = strings.TrimSpace(poolRef.Namespace)
		changed = true
	}
	return changed
}

func removeSubstrateMCPToolActorOwnership(tool *corev1alpha1.Tool) {
	if tool == nil || tool.Annotations == nil {
		return
	}
	delete(tool.Annotations, substrateMCPToolActorIDAnno)
	delete(tool.Annotations, substrateMCPToolBootedIDAnno)
	delete(tool.Annotations, substrateMCPToolActorPoolNameAnno)
	delete(tool.Annotations, substrateMCPToolActorPoolNamespaceAnno)
	delete(tool.Annotations, substrateMCPToolCleanupActorIDAnno)
	delete(tool.Annotations, substrateMCPToolCleanupPoolNameAnno)
	delete(tool.Annotations, substrateMCPToolCleanupPoolNamespaceAnno)
	if len(tool.Annotations) == 0 {
		tool.Annotations = nil
	}
}

func deleteSubstrateMCPActor(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	actorID string,
	reason string,
	timeout time.Duration,
) error {
	actorID = strings.TrimSpace(actorID)
	_, err := executor.Delete(ctx, workspace.DeleteRequest{
		Ref:       workspace.WorkspaceRef{ClaimName: actorID, ID: actorID},
		Reason:    reason,
		Timeout:   timeout,
		SkipScrub: true,
	})
	return err
}

func closeWorkspaceExecutor(ctx context.Context, executor workspace.WorkspaceExecutor) {
	closer, ok := executor.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil {
		log.FromContext(ctx).Error(err, "failed to close workspace executor")
	}
}

// healthCheck performs an HTTP health check against the tool endpoint.
func (r *ToolReconciler) healthCheck(ctx context.Context, tool *corev1alpha1.Tool) error {
	httpClient := r.getHTTPClient()
	if tool == nil || tool.Spec.HTTP == nil {
		return fmt.Errorf("http is required unless mcp.substrateActor is set")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, tool.Spec.HTTP.URL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("endpoint unreachable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Any response (even 4xx/5xx) means the endpoint is reachable.
	// We only mark unavailable if the connection itself fails.
	return nil
}

func toolHTTPURL(tool *corev1alpha1.Tool) string {
	if tool == nil || tool.Spec.HTTP == nil {
		return ""
	}
	return tool.Spec.HTTP.URL
}

func (r *ToolReconciler) healthCheckMCPActorEndpoint(ctx context.Context, endpoint string, routeHost string) error {
	httpClient := r.getHTTPClient()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create MCP endpoint check request: %w", err)
	}
	if routeHost != "" {
		req.Host = routeHost
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("MCP endpoint unreachable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("MCP endpoint returned HTTP %d", resp.StatusCode)
	case resp.StatusCode >= http.StatusInternalServerError:
		return fmt.Errorf("MCP endpoint returned HTTP %d", resp.StatusCode)
	default:
		return nil
	}
}

// getHTTPClient returns the HTTP client for health checks.
func (r *ToolReconciler) getHTTPClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{
		Timeout: toolHealthCheckTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// updateStatus updates the Tool status and conditions.
func (r *ToolReconciler) updateStatus(ctx context.Context, tool *corev1alpha1.Tool, available bool, errMsg string) (ctrl.Result, error) {
	return r.updateStatusWithActor(ctx, tool, available, errMsg, "", nil)
}

func (r *ToolReconciler) updateStatusWithActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	available bool,
	errMsg string,
	endpoint string,
	actor *corev1alpha1.ToolActorStatus,
) (ctrl.Result, error) {
	now := metav1.Now()
	if actor == nil && !available && tool.Status.Actor != nil {
		preserved := *tool.Status.Actor
		actor = &preserved
	}

	tool.Status.Available = available
	tool.Status.LastCheck = &now
	tool.Status.Error = errMsg
	tool.Status.Endpoint = endpoint
	tool.Status.Actor = actor

	condition := metav1.Condition{
		Type:               "Available",
		LastTransitionTime: now,
		ObservedGeneration: tool.Generation,
	}

	if available {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "EndpointReachable"
		condition.Message = "Tool endpoint is reachable"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "EndpointUnreachable"
		condition.Message = errMsg
	}

	meta.SetStatusCondition(&tool.Status.Conditions, condition)

	if err := r.Status().Update(ctx, tool); err != nil {
		return ctrl.Result{}, err
	}

	// Re-check periodically
	return ctrl.Result{RequeueAfter: toolHealthCheckInterval}, nil
}

func deterministicSubstrateToolActorID(namespace, name, templateNamespace, templateName string) string {
	parts := []string{
		strings.TrimSpace(namespace),
		strings.TrimSpace(name),
		strings.TrimSpace(templateNamespace),
		strings.TrimSpace(templateName),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "orka-tool-" + hex.EncodeToString(sum[:])[:32]
}

// SetupWithManager sets up the controller with the Manager.
func (r *ToolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Tool{}).
		Named("tool").
		Complete(r)
}
