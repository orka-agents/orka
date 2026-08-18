package api

import (
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func (h *Handlers) ListRuntimePools(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRuntimePools", h.contextTokenAuthorization.AgentReadScopes); err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "list", "runtimepools", namespace, ""); err != nil {
		return err
	}
	pagination, err := ParsePagination(c.Query("limit", "100"), c.Query("continue", ""))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	list := &corev1alpha1.RuntimePoolList{}
	if err := h.client.List(c.Context(), list, &client.ListOptions{
		Namespace: namespace,
		Limit:     pagination.Limit,
		Continue:  pagination.Continue,
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list runtime pools: %v", err))
	}
	return c.JSON(list)
}

func (h *Handlers) GetRuntimePool(c fiber.Ctx) error {
	if err := h.authorizeContextTokenAction(c, "getRuntimePool", h.contextTokenAuthorization.AgentReadScopes); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "get", "runtimepools", namespace, c.Params("name")); err != nil {
		return err
	}
	pool, err := h.fetchRuntimePool(c, c.Params("name"))
	if err != nil {
		return err
	}
	return c.JSON(pool)
}

func (h *Handlers) CreateRuntimePool(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "runtime pool"); err != nil {
		return err
	}
	var pool corev1alpha1.RuntimePool
	if err := c.Bind().JSON(&pool); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid runtime pool manifest")
	}
	name := strings.TrimSpace(pool.Name)
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "metadata.name is required")
	}
	namespace, err := h.resolveNamespace(c, pool.Namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "create", "runtimepools", namespace, name); err != nil {
		return err
	}
	pool.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RuntimePool"}
	pool.Namespace = namespace
	pool.ResourceVersion = ""
	pool.UID = ""
	pool.Status = corev1alpha1.RuntimePoolStatus{}
	if err := h.client.Create(c.Context(), &pool); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "runtime pool already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create runtime pool: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(&pool)
}

func (h *Handlers) UpdateRuntimePool(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "runtime pool"); err != nil {
		return err
	}
	existing, err := h.fetchRuntimePool(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "update", "runtimepools", existing.Namespace, existing.Name); err != nil {
		return err
	}
	var desired corev1alpha1.RuntimePool
	if err := c.Bind().JSON(&desired); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid runtime pool manifest")
	}
	existing.Spec = desired.Spec
	if err := h.client.Update(c.Context(), existing); err != nil {
		if apierrors.IsConflict(err) {
			return fiber.NewError(fiber.StatusConflict, "runtime pool was updated concurrently")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update runtime pool: %v", err))
	}
	return c.JSON(existing)
}

func (h *Handlers) DeleteRuntimePool(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "runtime pool"); err != nil {
		return err
	}
	pool, err := h.fetchRuntimePool(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "delete", "runtimepools", pool.Namespace, pool.Name); err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), pool); err != nil && !apierrors.IsNotFound(err) {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete runtime pool: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// authorizeRuntimeResourceAction enforces Kubernetes RBAC for RuntimePool and
// AgentRuntime reads and writes, like the Task and gateway paths: a
// TokenReview-authenticated identity must pass a SubjectAccessReview for the
// exact verb, resource, and name before the controller client acts on its
// behalf. It is a no-op for non-TokenReview auth (e.g. context tokens, which
// carry their own scope checks).
func (h *Handlers) authorizeRuntimeResourceAction(c fiber.Ctx, verb, resource, namespace, name string) error {
	return authorizeKubernetesResourceAction(
		c.Context(), h.clientset, GetUserInfo(c), namespace, verb, corev1alpha1.GroupVersion.Group, resource, name,
	)
}

func (h *Handlers) fetchRuntimePool(c fiber.Ctx, name string) (*corev1alpha1.RuntimePool, error) {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	pool := &corev1alpha1.RuntimePool{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "runtime pool not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get runtime pool: %v", err))
	}
	return pool, nil
}

func (h *Handlers) ListAgentRuntimes(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listAgentRuntimes", h.contextTokenAuthorization.AgentReadScopes); err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "list", "agentruntimes", namespace, ""); err != nil {
		return err
	}
	pagination, err := ParsePagination(c.Query("limit", "100"), c.Query("continue", ""))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	list := &corev1alpha1.AgentRuntimeList{}
	if err := h.client.List(c.Context(), list, &client.ListOptions{
		Namespace: namespace,
		Limit:     pagination.Limit,
		Continue:  pagination.Continue,
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list agent runtimes: %v", err))
	}
	return c.JSON(list)
}

func (h *Handlers) GetAgentRuntime(c fiber.Ctx) error {
	if err := h.authorizeContextTokenAction(c, "getAgentRuntime", h.contextTokenAuthorization.AgentReadScopes); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "get", "agentruntimes", namespace, c.Params("name")); err != nil {
		return err
	}
	runtime, err := h.fetchAgentRuntime(c, c.Params("name"))
	if err != nil {
		return err
	}
	return c.JSON(runtime)
}

func (h *Handlers) CreateAgentRuntime(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "agent runtime"); err != nil {
		return err
	}
	var runtime corev1alpha1.AgentRuntime
	if err := c.Bind().JSON(&runtime); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid agent runtime manifest")
	}
	if strings.TrimSpace(runtime.Name) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "metadata.name is required")
	}
	namespace, err := h.resolveNamespace(c, runtime.Namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "create", "agentruntimes", namespace, strings.TrimSpace(runtime.Name)); err != nil {
		return err
	}
	runtime.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "AgentRuntime"}
	runtime.Namespace = namespace
	runtime.ResourceVersion = ""
	runtime.UID = ""
	runtime.Status = corev1alpha1.AgentRuntimeStatus{}
	if err := h.client.Create(c.Context(), &runtime); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "agent runtime already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create agent runtime: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(&runtime)
}

func (h *Handlers) UpdateAgentRuntime(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "agent runtime"); err != nil {
		return err
	}
	existing, err := h.fetchAgentRuntime(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "update", "agentruntimes", existing.Namespace, existing.Name); err != nil {
		return err
	}
	var desired corev1alpha1.AgentRuntime
	if err := c.Bind().JSON(&desired); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid agent runtime manifest")
	}
	existing.Spec = desired.Spec
	if err := h.client.Update(c.Context(), existing); err != nil {
		if apierrors.IsConflict(err) {
			return fiber.NewError(fiber.StatusConflict, "agent runtime was updated concurrently")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update agent runtime: %v", err))
	}
	return c.JSON(existing)
}

func (h *Handlers) DeleteAgentRuntime(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "agent runtime"); err != nil {
		return err
	}
	runtime, err := h.fetchAgentRuntime(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeRuntimeResourceAction(c, "delete", "agentruntimes", runtime.Namespace, runtime.Name); err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), runtime); err != nil && !apierrors.IsNotFound(err) {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete agent runtime: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handlers) fetchAgentRuntime(c fiber.Ctx, name string) (*corev1alpha1.AgentRuntime, error) {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	runtime := &corev1alpha1.AgentRuntime{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: name}, runtime); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "agent runtime not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent runtime: %v", err))
	}
	return runtime, nil
}
