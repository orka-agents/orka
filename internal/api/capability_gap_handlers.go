/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/capabilitygap"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
)

const capabilityGapTaskAction = "listTasks"

// GetCapabilityGaps handles GET /api/v1/capability-gaps. It returns the
// namespace's capability-gap counts for the current UTC day and the six
// before it: categories, closed reasons, runtimes, catalog tool names and the
// names of recent Tasks the caller may list. It never returns Task content.
func (h *Handlers) GetCapabilityGaps(c fiber.Ctx) error {
	recorder := capabilitygap.Default()
	if recorder == nil {
		return fiber.NewError(fiber.StatusNotFound, "capability-gap signals are not enabled")
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if err := h.authorizeContextTokenAction(c, capabilityGapTaskAction, h.contextTokenAuthorization.TaskListScopes); err != nil {
		return err
	}
	// Counts outlive their Tasks, so they cannot be filtered per Task. A token
	// limited to particular objects therefore cannot read the aggregate.
	if err := h.rejectObjectConstrainedContextToken(c, capabilityGapTaskAction, "capability-gap signals require a token without object constraints"); err != nil {
		return err
	}

	// Counts are kept per namespace incarnation; read only the current one so
	// a recreated namespace never sees its predecessor's counts.
	namespaceUID, err := h.capabilityGapNamespaceUID(c, namespace)
	if err != nil {
		return err
	}

	gatewayAuthorizations := map[gatewayTaskAuthorizationKey]bool{}
	visibleGateways := map[capabilitygap.GatewayOwner]bool{}
	for _, owner := range recorder.GatewayOwners(namespace, namespaceUID) {
		ok, err := h.capabilityGapGatewayVisible(c, owner, gatewayAuthorizations)
		if err != nil {
			return err
		}
		visibleGateways[owner] = ok
	}
	summary := recorder.Summary(namespace, namespaceUID, func(owner capabilitygap.GatewayOwner) bool { return visibleGateways[owner] })
	for i := range summary.Signals {
		visible := make([]string, 0, len(summary.Signals[i].Tasks))
		for _, name := range summary.Signals[i].Tasks {
			ok, err := h.capabilityGapTaskVisible(c, namespace, name, gatewayAuthorizations)
			if err != nil {
				return err
			}
			if ok {
				visible = append(visible, name)
			}
		}
		summary.Signals[i].Tasks = visible
	}
	return c.JSON(summary)
}

// capabilityGapTaskVisible applies the per-Task checks ListTasks applies, so
// a Task reference appears only to callers who could list that Task. Deleted
// Tasks are dropped; their counts remain.
func (h *Handlers) capabilityGapTaskVisible(
	c fiber.Ctx, namespace, name string, gatewayAuthorizations map[gatewayTaskAuthorizationKey]bool,
) (bool, error) {
	task := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fiber.NewError(fiber.StatusInternalServerError, "failed to load capability-gap Task reference")
	}
	if h.contextTokenAuthorization.Enabled() {
		allowed, err := h.contextTokenAllowsLoadedTask(c, capabilityGapTaskAction, task)
		if err != nil || !allowed {
			return false, err
		}
	}
	return h.taskAccess().gatewayTaskReadableCached(c, capabilityGapTaskAction, task, gatewayAuthorizations)
}

// capabilityGapGatewayVisible reports whether the caller may read Tasks owned
// by owner, using the check that guards those Tasks themselves. Signals from
// Gateways the caller cannot read are left out of every count.
func (h *Handlers) capabilityGapGatewayVisible(
	c fiber.Ctx, owner capabilitygap.GatewayOwner, cache map[gatewayTaskAuthorizationKey]bool,
) (bool, error) {
	key := gatewayTaskAuthorizationKey{
		GatewayNamespace: owner.Namespace, NamespaceUID: owner.NamespaceUID, GatewayName: owner.Name, GatewayUID: owner.UID,
	}
	if allowed, ok := cache[key]; ok {
		return allowed, nil
	}
	identity := gatewayruntime.TaskOwnerIdentity{
		GatewayNamespace: owner.Namespace, NamespaceUID: owner.NamespaceUID, GatewayName: owner.Name, GatewayUID: owner.UID,
	}
	err := h.taskAccess().authorizeGatewayTaskIdentity(c, capabilityGapTaskAction, &corev1alpha1.Task{}, identity, false)
	var status *fiber.Error
	if err != nil && (!errors.As(err, &status) || (status.Code != fiber.StatusForbidden && status.Code != fiber.StatusNotFound)) {
		return false, err
	}
	cache[key] = err == nil
	return err == nil, nil
}

// capabilityGapNamespaceUID returns the namespace's current UID with one
// uncached read. A missing namespace has no current incarnation, so its
// summary is empty.
func (h *Handlers) capabilityGapNamespaceUID(c fiber.Ctx, namespace string) (string, error) {
	reader := h.uncachedReader()
	if reader == nil {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, "namespace identity lookup is unavailable")
	}
	current := &corev1.Namespace{}
	if err := reader.Get(c.Context(), client.ObjectKey{Name: namespace}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fiber.NewError(fiber.StatusInternalServerError, "failed to resolve namespace identity")
	}
	return string(current.UID), nil
}
