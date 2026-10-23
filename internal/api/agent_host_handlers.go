/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

// AgentHostResponse is the signed-in person's own agent host (ADR 0036). It
// never carries token material.
type AgentHostResponse struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Environment is the name the Copilot app lists, as the chart's
	// template names it.
	Environment string `json:"environment"`
	Phase       string `json:"phase"`
	Ready       bool   `json:"ready"`
	Reason      string `json:"reason,omitempty"`
	Message     string `json:"message,omitempty"`
	GitHubLogin string `json:"githubLogin,omitempty"`
	// Suspended is true while the host is stopped, by request or for being
	// idle.
	Suspended bool `json:"suspended"`
	// OperatorSuspended is true when an operator stopped the host; waking it
	// is then theirs to do.
	OperatorSuspended bool   `json:"operatorSuspended,omitempty"`
	Connection        string `json:"connection"`
	Deleting          bool   `json:"deleting,omitempty"`
}

// GetOwnAgentHost serves GET /api/v1/agent-hosts/me.
func (h *Handlers) GetOwnAgentHost(c fiber.Ctx) error {
	host, err := h.ownAgentHost(c, connectorActionRead)
	if err != nil {
		return err
	}
	return c.JSON(agentHostResponse(host))
}

// WakeOwnAgentHost serves POST /api/v1/agent-hosts/me/wake. It wakes a host
// that idled out; one an operator suspended stays suspended.
func (h *Handlers) WakeOwnAgentHost(c fiber.Ctx) error {
	host, err := h.ownAgentHost(c, connectorActionManage)
	if err != nil {
		return err
	}
	if host.Spec.Suspended {
		return fiber.NewError(fiber.StatusConflict, "an operator suspended this host; ask them to resume it")
	}
	if idle := host.Status.IdleSuspendedAt; idle != nil {
		base := host.DeepCopy()
		// The controller wakes a host for a request dated after its
		// suspension, and both are stored to the second.
		wake := time.Now().UTC().Truncate(time.Second)
		if !wake.After(idle.Time) {
			wake = idle.Add(time.Second)
		}
		requested := metav1.NewTime(wake)
		host.Spec.WakeRequestedAt = &requested
		// Fenced to the object the ownership check read: a host replaced
		// under the same name since, which may be another person's, is
		// refused rather than woken.
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := h.client.Patch(c.Context(), host, patch); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return fiber.NewError(fiber.StatusConflict, "your host changed while it was being woken; try again")
			}
			return fiber.NewError(fiber.StatusInternalServerError, "could not wake the host")
		}
	}
	return c.JSON(agentHostResponse(host))
}

// DeleteOwnAgentHost serves DELETE /api/v1/agent-hosts/me. The host and its
// volume are deleted; while the person stays linked, a fresh host follows.
func (h *Handlers) DeleteOwnAgentHost(c fiber.Ctx) error {
	host, err := h.ownAgentHost(c, connectorActionManage)
	if err != nil {
		return err
	}
	// Fenced to the object the ownership check read: a host replaced under
	// the same name since, which may be another person's, is never deleted.
	uid, version := host.UID, host.ResourceVersion
	if err := h.client.Delete(c.Context(), host, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return fiber.NewError(fiber.StatusConflict, "your host changed while it was being deleted; try again")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "could not delete the host")
	}
	response := agentHostResponse(host)
	response.Deleting, response.Ready = true, false
	return c.Status(fiber.StatusAccepted).JSON(response)
}

// ownAgentHost finds the host fed by the caller's own link to a provider
// that serves agent hosts. Only the controller creates such hosts, and the
// link's name derives from the caller's verified identity, so no request can
// reach another person's host.
func (h *Handlers) ownAgentHost(c fiber.Ctx, action connectorAction) (*corev1alpha1.AgentHost, error) {
	ui, err := h.connectorIdentity(c, action)
	if err != nil {
		return nil, err
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return nil, err
	}
	ctx := c.Context()
	providers := &corev1alpha1.ConnectorProviderList{}
	if err := h.client.List(ctx, providers, client.InNamespace(namespace)); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "could not read connector providers")
	}
	// Every provider counts, not only those serving hosts now: a host kept
	// after its provider stopped serving them is still its person's to see
	// and delete. The host of a provider serving hosts now comes first.
	links := map[string]bool{}
	for i := range providers.Items {
		links[connectors.ConnectionName(providers.Items[i].Name, ui.Issuer, ui.Subject)] =
			providers.Items[i].Spec.AgentHost != nil && providers.Items[i].DeletionTimestamp.IsZero()
	}
	hosts := &corev1alpha1.AgentHostList{}
	if err := h.client.List(ctx, hosts, client.InNamespace(namespace)); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "could not read agent hosts")
	}
	var retained *corev1alpha1.AgentHost
	for i := range hosts.Items {
		from := hosts.Items[i].Spec.GitHubTokenFrom
		if from == nil {
			continue
		}
		current, mine := links[from.ConnectionRef.Name]
		if !mine {
			// A host kept after its person disconnected outlives its provider,
			// which nothing holds once the link is gone. Its link's name still
			// says whose it is.
			mine = connectionNameOf(from.ConnectionRef.Name, ui.Issuer, ui.Subject)
		}
		if mine && current {
			return &hosts.Items[i], nil
		} else if mine && retained == nil {
			retained = &hosts.Items[i]
		}
	}
	if retained != nil {
		return retained, nil
	}
	return nil, fiber.NewError(fiber.StatusNotFound, "you have no agent host yet; link GitHub first, or wait a moment after linking")
}

// connectionNameOf reports whether name is this person's link to the provider
// its prefix names. The name's digest covers the provider and the person's
// verified identity, so it matches no one else's.
func connectionNameOf(name, issuer, subject string) bool {
	cut := strings.LastIndex(name, "-")
	return cut > 0 && connectors.ConnectionName(name[:cut], issuer, subject) == name
}

func agentHostResponse(host *corev1alpha1.AgentHost) AgentHostResponse {
	response := AgentHostResponse{
		Name:              host.Name,
		Namespace:         host.Namespace,
		Environment:       "orka-" + host.Name,
		Phase:             host.Status.Phase,
		Suspended:         host.Spec.Suspended || host.Status.IdleSuspendedAt != nil,
		OperatorSuspended: host.Spec.Suspended,
		Deleting:          !host.DeletionTimestamp.IsZero(),
	}
	if from := host.Spec.GitHubTokenFrom; from != nil {
		response.Connection = from.ConnectionRef.Name
	}
	if account := host.Status.GitHub; account != nil {
		response.GitHubLogin = account.Login
	}
	if ready := meta.FindStatusCondition(host.Status.Conditions, "Ready"); ready != nil {
		// A host being deleted keeps its last Ready condition; it is not
		// ready for anyone.
		response.Ready = ready.Status == metav1.ConditionTrue && !response.Deleting
		response.Reason = ready.Reason
		response.Message = strings.TrimSpace(ready.Message)
	}
	return response
}
