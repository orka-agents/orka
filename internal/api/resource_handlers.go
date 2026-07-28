package api

import (
	"fmt"
	neturl "net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type CreateProviderRequest struct {
	Name      string                    `json:"name"`
	Namespace string                    `json:"namespace"`
	Metadata  MetadataRequest           `json:"metadata"`
	Spec      corev1alpha1.ProviderSpec `json:"spec"`
}

type UpdateProviderRequest struct {
	Spec corev1alpha1.ProviderSpec `json:"spec"`
}

type CreateToolRequest struct {
	Name      string                `json:"name"`
	Namespace string                `json:"namespace"`
	Metadata  MetadataRequest       `json:"metadata"`
	Spec      corev1alpha1.ToolSpec `json:"spec"`
}

type UpdateToolRequest struct {
	Spec corev1alpha1.ToolSpec `json:"spec"`
}

type CreateSubstrateActorPoolRequest struct {
	Name      string                              `json:"name"`
	Namespace string                              `json:"namespace"`
	Metadata  MetadataRequest                     `json:"metadata"`
	Spec      corev1alpha1.SubstrateActorPoolSpec `json:"spec"`
}

type UpdateSubstrateActorPoolRequest struct {
	Spec corev1alpha1.SubstrateActorPoolSpec `json:"spec"`
}

func rejectContextTokenResourceMutation(c fiber.Ctx, resource string) error {
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken {
		return nil
	}
	return fiber.NewError(
		fiber.StatusForbidden,
		fmt.Sprintf("%s mutations are not allowed with transaction tokens", resource),
	)
}

func validateProviderRESTCreate(spec corev1alpha1.ProviderSpec) error {
	if spec.BaseURL != "" {
		return fiber.NewError(
			fiber.StatusBadRequest,
			"spec.baseURL is not allowed through the REST API; create providers with Kubernetes RBAC instead",
		)
	}
	return nil
}

func validateProviderRESTUpdate(spec, existing corev1alpha1.ProviderSpec) error {
	if spec.BaseURL != "" && spec.BaseURL != existing.BaseURL {
		return fiber.NewError(
			fiber.StatusBadRequest,
			"spec.baseURL cannot be changed through the REST API; update providers with Kubernetes RBAC instead",
		)
	}
	return nil
}

func toolSpecHasProtectedAuth(spec corev1alpha1.ToolSpec) bool {
	if spec.HTTP == nil {
		return false
	}
	if spec.HTTP.AuthSecretRef != nil || spec.HTTP.OutboundAccessPolicyRef != nil {
		return true
	}
	for name := range spec.HTTP.Headers {
		if isCredentialHeader(name) {
			return true
		}
	}
	return false
}

func validateToolRESTMutation(spec corev1alpha1.ToolSpec) error {
	if spec.HTTP == nil {
		return nil
	}
	if spec.HTTP.AuthSecretRef != nil {
		return fiber.NewError(
			fiber.StatusBadRequest,
			"spec.http.authSecretRef is not allowed through the REST API; create tools with Kubernetes RBAC instead",
		)
	}
	if spec.HTTP.OutboundAccessPolicyRef != nil {
		return fiber.NewError(
			fiber.StatusBadRequest,
			"spec.http.outboundAccessPolicyRef is not allowed through the REST API; create tools with Kubernetes RBAC instead",
		)
	}
	if err := validateToolRESTURL(spec.HTTP.URL); err != nil {
		return err
	}
	for name := range spec.HTTP.Headers {
		if isCredentialHeader(name) {
			return fiber.NewError(
				fiber.StatusBadRequest,
				fmt.Sprintf("spec.http.headers[%q] is not allowed through the REST API", name),
			)
		}
	}
	return nil
}

func validateToolRESTURL(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return nil
	}
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "spec.http.url is invalid")
	}
	if parsed.User != nil {
		return fiber.NewError(fiber.StatusBadRequest, "spec.http.url must not contain embedded credentials")
	}
	for name := range parsed.Query() {
		if isCredentialHeader(name) {
			return fiber.NewError(
				fiber.StatusBadRequest,
				fmt.Sprintf("spec.http.url query parameter %q is not allowed through the REST API", name),
			)
		}
	}
	return nil
}

func isCredentialHeader(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "x-auth-token", "api-key":
		return true
	}
	return strings.Contains(normalized, "api-key") ||
		strings.Contains(normalized, "apikey") ||
		strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "authorization") ||
		strings.Contains(normalized, "cookie")
}

func isContextTokenRequest(c fiber.Ctx) bool {
	ui := GetUserInfo(c)
	return ui != nil && ui.AuthType == AuthTypeContextToken
}

func providerReadItems(c fiber.Ctx, providers []corev1alpha1.Provider) any {
	if !isContextTokenRequest(c) {
		return providers
	}
	items := make([]fiber.Map, 0, len(providers))
	for i := range providers {
		items = append(items, providerReadItem(&providers[i]))
	}
	return items
}

func providerReadItem(provider *corev1alpha1.Provider) fiber.Map {
	if provider == nil {
		return fiber.Map{}
	}
	return fiber.Map{
		"name":         provider.Name,
		"namespace":    provider.Namespace,
		"type":         provider.Spec.Type,
		"defaultModel": provider.Spec.DefaultModel,
		"ready":        provider.Status.Ready,
	}
}

// ListProviders lists configured LLM providers.
func (h *Handlers) ListProviders(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(
		c,
		"listProviders",
		h.contextTokenAuthorization.ProviderUseScopes,
	); err != nil {
		return err
	}
	pagination, err := ParsePagination(c.Query("limit", "100"), c.Query("continue", ""))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	list := &corev1alpha1.ProviderList{}
	if err := h.client.List(c.Context(), list, &client.ListOptions{
		Namespace: namespace,
		Limit:     pagination.Limit,
		Continue:  pagination.Continue,
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list providers: %v", err))
	}
	items := list.Items
	filteredList := false
	if h.contextTokenAuthorization.Enabled() {
		filtered := make([]corev1alpha1.Provider, 0, len(items))
		for i := range items {
			provider := &items[i]
			allowed := contextTokenAllowsListedProviderModel(
				c,
				h.contextTokenAuthorization,
				"listProviders",
				namespace,
				providerResolutionInfo(provider),
				provider.Spec.DefaultModel,
			)
			if allowed {
				filtered = append(filtered, *provider)
			}
		}
		filteredList = len(filtered) != len(items)
		items = filtered
	}
	remainingItemCount := list.RemainingItemCount
	if filteredList {
		remainingItemCount = nil
	}
	return c.JSON(ListResponse{
		Items: providerReadItems(c, items),
		Metadata: ListMeta{
			Continue:           list.Continue,
			RemainingItemCount: remainingItemCount,
		},
	})
}

// GetProvider returns a configured LLM provider.
func (h *Handlers) GetProvider(c fiber.Ctx) error {
	provider, err := h.fetchProvider(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := authorizeContextTokenProviderUse(
		c,
		h.contextTokenAuthorization,
		"getProvider",
		provider.Namespace,
		providerResolutionInfo(provider),
		provider.Spec.DefaultModel,
	); err != nil {
		return err
	}
	if isContextTokenRequest(c) {
		return c.JSON(providerReadItem(provider))
	}
	return c.JSON(provider)
}

// CreateProvider creates a provider.
func (h *Handlers) CreateProvider(c fiber.Ctx) error {
	var req CreateProviderRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	name := req.Name
	if name == "" {
		name = req.Metadata.Name
	}
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	explicitNS := req.Namespace
	if explicitNS == "" {
		explicitNS = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNS)
	if err != nil {
		return err
	}
	if err := rejectContextTokenResourceMutation(c, "provider"); err != nil {
		return err
	}
	if err := validateProviderRESTCreate(req.Spec); err != nil {
		return err
	}
	provider := &corev1alpha1.Provider{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata),
		Spec:       req.Spec,
	}
	if err := h.client.Create(c.Context(), provider); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "provider already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create provider: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(provider)
}

// UpdateProvider updates a provider spec.
func (h *Handlers) UpdateProvider(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "provider"); err != nil {
		return err
	}
	provider, err := h.fetchProvider(c, c.Params("name"))
	if err != nil {
		return err
	}
	var req UpdateProviderRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := validateProviderRESTUpdate(req.Spec, provider.Spec); err != nil {
		return err
	}
	// REST updates cannot set protected routing fields, but they also must not
	// clear values that were created through the Kubernetes API/RBAC path.
	req.Spec.BaseURL = provider.Spec.BaseURL
	provider.Spec = req.Spec
	if err := h.client.Update(c.Context(), provider); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update provider: %v", err))
	}
	return c.JSON(provider)
}

// DeleteProvider deletes a provider.
func (h *Handlers) DeleteProvider(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "provider"); err != nil {
		return err
	}
	provider, err := h.fetchProvider(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), provider); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete provider: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handlers) fetchProvider(c fiber.Ctx, name string) (*corev1alpha1.Provider, error) {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	provider := &corev1alpha1.Provider{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: name, Namespace: namespace}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "provider not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get provider: %v", err))
	}
	return provider, nil
}

// CreateTool creates a Tool CRD.
func (h *Handlers) CreateTool(c fiber.Ctx) error {
	var req CreateToolRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	name := req.Name
	if name == "" {
		name = req.Metadata.Name
	}
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	if _, builtin := builtinToolsMap[name]; builtin {
		return fiber.NewError(fiber.StatusConflict, "tool name is reserved for a built-in tool")
	}
	explicitNS := req.Namespace
	if explicitNS == "" {
		explicitNS = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNS)
	if err != nil {
		return err
	}
	if err := rejectContextTokenResourceMutation(c, "tool"); err != nil {
		return err
	}
	if err := validateToolRESTMutation(req.Spec); err != nil {
		return err
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata),
		Spec:       req.Spec,
	}
	if err := h.client.Create(c.Context(), tool); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "tool already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create tool: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(tool)
}

// UpdateTool updates a Tool CRD.
func (h *Handlers) UpdateTool(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "tool"); err != nil {
		return err
	}
	name := c.Params("name")
	if _, builtin := builtinToolsMap[name]; builtin {
		return fiber.NewError(fiber.StatusConflict, "built-in tools cannot be updated")
	}
	tool, err := h.fetchToolCRD(c, name)
	if err != nil {
		return err
	}
	if toolSpecHasProtectedAuth(tool.Spec) {
		return fiber.NewError(
			fiber.StatusBadRequest,
			"tools with protected HTTP auth configuration must be updated with Kubernetes RBAC",
		)
	}
	var req UpdateToolRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := validateToolRESTMutation(req.Spec); err != nil {
		return err
	}
	tool.Spec = req.Spec
	if err := h.client.Update(c.Context(), tool); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update tool: %v", err))
	}
	return c.JSON(tool)
}

// DeleteTool deletes a Tool CRD.
func (h *Handlers) DeleteTool(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "tool"); err != nil {
		return err
	}
	name := c.Params("name")
	if _, builtin := builtinToolsMap[name]; builtin {
		return fiber.NewError(fiber.StatusConflict, "built-in tools cannot be deleted")
	}
	tool, err := h.fetchToolCRD(c, name)
	if err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), tool); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete tool: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handlers) fetchToolCRD(c fiber.Ctx, name string) (*corev1alpha1.Tool, error) {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	tool := &corev1alpha1.Tool{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: name, Namespace: namespace}, tool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "tool not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get tool: %v", err))
	}
	return tool, nil
}

// ListSubstrateActorPools lists Orka Substrate actor pools.
func (h *Handlers) ListSubstrateActorPools(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(
		c,
		"listSubstrateActorPools",
		h.contextTokenAuthorization.ToolReadScopes,
	); err != nil {
		return err
	}
	pagination, err := ParsePagination(c.Query("limit", "100"), c.Query("continue", ""))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	list := &corev1alpha1.SubstrateActorPoolList{}
	if err := h.client.List(c.Context(), list, &client.ListOptions{
		Namespace: namespace,
		Limit:     pagination.Limit,
		Continue:  pagination.Continue,
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list substrate actor pools: %v", err))
	}
	return c.JSON(ListResponse{
		Items: list.Items,
		Metadata: ListMeta{
			Continue:           list.Continue,
			RemainingItemCount: list.RemainingItemCount,
		},
	})
}

// GetSubstrateActorPool gets an Orka Substrate actor pool.
func (h *Handlers) GetSubstrateActorPool(c fiber.Ctx) error {
	pool, err := h.fetchSubstrateActorPool(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(
		c,
		"getSubstrateActorPool",
		h.contextTokenAuthorization.ToolReadScopes,
	); err != nil {
		return err
	}
	return c.JSON(pool)
}

// CreateSubstrateActorPool creates an Orka Substrate actor pool.
func (h *Handlers) CreateSubstrateActorPool(c fiber.Ctx) error {
	var req CreateSubstrateActorPoolRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	name := req.Name
	if name == "" {
		name = req.Metadata.Name
	}
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	explicitNS := req.Namespace
	if explicitNS == "" {
		explicitNS = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNS)
	if err != nil {
		return err
	}
	if err := rejectContextTokenResourceMutation(c, "substrate actor pool"); err != nil {
		return err
	}
	pool := &corev1alpha1.SubstrateActorPool{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata),
		Spec:       req.Spec,
	}
	if err := h.client.Create(c.Context(), pool); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "substrate actor pool already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create substrate actor pool: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(pool)
}

// UpdateSubstrateActorPool updates an Orka Substrate actor pool.
func (h *Handlers) UpdateSubstrateActorPool(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "substrate actor pool"); err != nil {
		return err
	}
	var req UpdateSubstrateActorPoolRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	var updated *corev1alpha1.SubstrateActorPool
	var fetchErr error
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pool, err := h.fetchSubstrateActorPool(c, c.Params("name"))
		if err != nil {
			fetchErr = err
			return nil
		}
		pool.Spec = req.Spec
		if err := h.client.Update(c.Context(), pool); err != nil {
			return err
		}
		updated = pool
		return nil
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update substrate actor pool: %v", err))
	}
	if fetchErr != nil {
		return fetchErr
	}
	return c.JSON(updated)
}

// DeleteSubstrateActorPool deletes an Orka Substrate actor pool.
func (h *Handlers) DeleteSubstrateActorPool(c fiber.Ctx) error {
	if err := rejectContextTokenResourceMutation(c, "substrate actor pool"); err != nil {
		return err
	}
	pool, err := h.fetchSubstrateActorPool(c, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), pool); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete substrate actor pool: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handlers) fetchSubstrateActorPool(c fiber.Ctx, name string) (*corev1alpha1.SubstrateActorPool, error) {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	pool := &corev1alpha1.SubstrateActorPool{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: name, Namespace: namespace}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "substrate actor pool not found")
		}
		return nil, fiber.NewError(
			fiber.StatusInternalServerError,
			fmt.Sprintf("failed to get substrate actor pool: %v", err),
		)
	}
	return pool, nil
}

// handleAuthWhoAmI returns a sanitized authenticated identity.
func (s *Server) handleAuthWhoAmI(c fiber.Ctx) error {
	ui := GetUserInfo(c)
	if ui == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "missing authenticated identity")
	}
	identity := fiber.Map{
		"authenticated": true,
		"authType":      ui.AuthType,
		"username":      ui.Username,
		"uid":           ui.UID,
		"groups":        ui.Groups,
		"namespace":     ui.Namespace,
		"subject":       ui.Subject,
		"email":         ui.Email,
		"issuer":        ui.Issuer,
		"roles":         ui.Roles,
	}
	if ui.ContextToken != nil {
		identity["transaction"] = fiber.Map{
			"profile":            ui.ContextToken.Profile,
			"type":               ui.ContextToken.Type,
			"id":                 ui.ContextToken.TransactionID,
			"issuer":             ui.ContextToken.Issuer,
			"subject":            ui.ContextToken.Subject,
			"audience":           ui.ContextToken.Audience,
			"scope":              ui.ContextToken.Scope,
			"scopes":             ui.ContextToken.Scopes,
			"requestingWorkload": ui.ContextToken.RequestingWorkload,
		}
	}
	return c.JSON(identity)
}
