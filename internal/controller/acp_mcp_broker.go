package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

const (
	errorField = "error"
)

type ACPMCPAuthenticatedTask struct {
	Name         string
	Namespace    string
	UID          string
	ParentTaskID string
	AgentName    string
	AgentUID     string
	SessionName  string
	Deadline     time.Time
}

type acpMCPAuthenticatedTaskContextKey struct{}

func withACPMCPAuthenticatedTask(ctx context.Context, task ACPMCPAuthenticatedTask) context.Context {
	return context.WithValue(ctx, acpMCPAuthenticatedTaskContextKey{}, task)
}

func ACPMCPAuthenticatedTaskFromContext(ctx context.Context) (ACPMCPAuthenticatedTask, bool) {
	task, ok := ctx.Value(acpMCPAuthenticatedTaskContextKey{}).(ACPMCPAuthenticatedTask)
	return task, ok && task.Name != "" && task.Namespace != "" && task.UID != ""
}

type ACPMCPBrokerCredentials struct {
	ControllerBearerToken    string
	CapabilitySecret         []byte
	ExpectedFence            harnessv2.Fence
	RuntimeProfile           harnessv2.RuntimeProfile
	ExpectedMCPConfiguration *harnessv2.MCPPolicyConfiguration
	ControllerFence          store.ControllerEpochFence
	Task                     ACPMCPAuthenticatedTask
}

type ACPMCPBrokerCredentialResolver interface {
	ResolveACPMCPBrokerCredentials(context.Context, harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error)
}

type ACPMCPBrokerPreAuthenticator interface {
	PreAuthenticateACPMCPBroker(ctx context.Context, poolNamespace, poolUID, authorizationHeader string) error
}

type ACPMCPBrokerCredentialResolverFunc func(context.Context, harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error)

func (f ACPMCPBrokerCredentialResolverFunc) ResolveACPMCPBrokerCredentials(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
	return f(ctx, request)
}

type ACPMCPPromptAuthorizer interface {
	AuthorizeACPMCPPrompt(context.Context, harnessv2.MCPBrokerCallRequest) error
}

type ACPMCPPromptAuthorizerFunc func(context.Context, harnessv2.MCPBrokerCallRequest) error

func (f ACPMCPPromptAuthorizerFunc) AuthorizeACPMCPPrompt(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
	return f(ctx, request)
}

type ACPMCPToolExecutor interface {
	ExecuteACPMCPTool(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error)
}

type acpMCPPreparedToolCall func(context.Context) (json.RawMessage, error)

type ACPMCPToolExecutorFunc func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error)

func (f ACPMCPToolExecutorFunc) ExecuteACPMCPTool(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
	return f(ctx, request, descriptor)
}

type RegistryACPMCPToolExecutor struct {
	TaskProvenanceProtected bool
	GatewayService          *gatewayruntime.Service
	Registry                *tools.Registry
	Reader                  client.Reader
	KubeClient              kubernetes.Interface
	HTTPClient              *http.Client
	OutboundAccess          outboundaccess.Resolver
	TransactionExchange     *workerexecutor.TransactionExchangeConfig
	ContextFactory          func(context.Context, harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error)
	SubstrateMCPRouter      *workerexecutor.SubstrateMCPRouter

	EnforceTransactionCredentialAuth bool
	ConnectorReadScopes              []string
	TransactionCredentialReadScopes  []string
	AgentExecutionSnapshots          store.AgentExecutionSnapshotStore
	Connections                      outboundaccess.ConnectionCredentialSource
}

type acpMCPConnectionDigester interface {
	ConnectionDigest(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (string, error)
}

func (e RegistryACPMCPToolExecutor) ConnectionDigest(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (string, error) {
	if descriptor.Source == harnessv2.MCPToolSourceBrokeredBuiltin {
		if _, linked := connectors.BuiltinConnectorToolClass(descriptor.Name); !linked {
			return "", nil
		}
		_, frozen, _, err := e.taskConnectionAuthority(ctx, request)
		if err != nil {
			return "", err
		}
		key := outboundaccess.BuiltinConnectionKey(descriptor.Name)
		digest := frozenConnectionDigest(frozen, key)
		if digest == "" {
			return "", fmt.Errorf("linked built-in %q has no Connection frozen for it", descriptor.Name)
		}
		return digest, nil
	}
	if descriptor.Source != harnessv2.MCPToolSourceBrokeredCustom {
		return "", nil
	}
	if e.Reader == nil {
		return "", errors.New("connector digest resolution requires a reader")
	}
	name := customACPMCPToolResource(descriptor)
	infos, err := connectorToolsFor(ctx, e.Reader, e.Registry, request.Namespace, []string{name})
	if err != nil {
		return "", fmt.Errorf("classify connector tool %q: %w", name, err)
	}
	info, ok := infos[name]
	if !ok {
		return "", e.refuseConnectorClassificationDrift(ctx, request, name)
	}
	frozen, err := e.taskFrozenConnections(ctx, request)
	if err != nil {
		return "", err
	}
	digest := frozenConnectionDigest(frozen, info.PolicyName)
	if digest == "" {
		return "", fmt.Errorf("connector tool %q has no Connection frozen for policy %q", descriptor.Name, info.PolicyName)
	}
	return connectorBindingDigest(digest, info.SpecDigest), nil
}

func (e RegistryACPMCPToolExecutor) taskFrozenConnections(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (map[string]outboundaccess.FrozenConnection, error) {
	_, frozen, _, err := e.taskConnectionAuthority(ctx, request)
	return frozen, err
}

func brokerConnectorReadAuthorizer(transaction *corev1alpha1.TaskTransaction, readScopes []string, enforce bool) func() *tools.ChatToolError {
	if !enforce || transaction == nil || len(readScopes) == 0 {
		return nil
	}
	scopes := transaction.Scopes
	if len(scopes) == 0 {
		scopes = strings.Fields(transaction.Scope)
	}
	for _, scope := range scopes {
		if slices.Contains(readScopes, scope) {
			return nil
		}
	}
	return func() *tools.ChatToolError {
		return &tools.ChatToolError{
			Type:       "unauthorized_tool",
			Message:    fmt.Sprintf("the task's transaction lacks one of the scopes %q needed to read linked accounts", strings.Join(readScopes, ",")),
			Suggestion: "Create the task with a token that carries the connector-read scope",
		}
	}
}

func (e RegistryACPMCPToolExecutor) taskConnectionAuthority(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (*corev1alpha1.RequestedBy, map[string]outboundaccess.FrozenConnection, *corev1alpha1.TaskTransaction, error) {
	if e.AgentExecutionSnapshots == nil {
		return nil, nil, nil, errors.New("connector digest resolution requires execution snapshots")
	}
	if e.Reader == nil {
		return nil, nil, nil, errors.New("connector binding resolution requires a reader")
	}
	authenticated, ok := ACPMCPAuthenticatedTaskFromContext(ctx)
	if !ok || authenticated.Namespace != request.Namespace || authenticated.UID != string(request.Metadata.TaskUID) {
		return nil, nil, nil, errors.New("authenticated ACP MCP task authority is unavailable")
	}
	task := &corev1alpha1.Task{}
	if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: authenticated.Namespace, Name: authenticated.Name}, task); err != nil {
		return nil, nil, nil, fmt.Errorf("load authenticated task for connector digest: %w", err)
	}
	if string(task.UID) != authenticated.UID {
		return nil, nil, nil, errors.New("authenticated ACP MCP task identity changed")
	}
	executor := workerexecutor.NewToolExecutorForNamespace(request.Namespace, nil, nil)
	if err := bindFrozenConnections(ctx, e.Reader, e.AgentExecutionSnapshots, task, executor); err != nil {
		return nil, nil, nil, err
	}
	verified, err := requesterProvenanceVerified(ctx, e.Reader, task)
	if err != nil {
		return nil, nil, nil, err
	}
	var requester *corev1alpha1.RequestedBy
	if verified {
		requester = task.Spec.RequestedBy.DeepCopy()
	}
	return requester, executor.FrozenConnections(), task.Spec.Transaction.DeepCopy(), nil
}

func (e RegistryACPMCPToolExecutor) refuseConnectorClassificationDrift(ctx context.Context, request harnessv2.MCPBrokerCallRequest, toolName string) error {
	tool := &corev1alpha1.Tool{}
	if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: toolName}, tool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("load tool %q: %w", toolName, err)
	}
	if tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil || e.AgentExecutionSnapshots == nil {
		return nil
	}
	if _, ok := ACPMCPAuthenticatedTaskFromContext(ctx); !ok {
		return nil
	}
	frozen, err := e.taskFrozenConnections(ctx, request)
	if err != nil {
		return err
	}
	return connectorClassificationDrift(frozen, tool)
}

func connectorClassificationDrift(frozen map[string]outboundaccess.FrozenConnection, tool *corev1alpha1.Tool) error {
	if tool == nil || tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
		return nil
	}
	policyName := tool.Spec.HTTP.OutboundAccessPolicyRef.Name
	if _, ok := frozen[policyName]; ok {
		return fmt.Errorf("tool %q: policy %q was connection mode when the task was dispatched and is not any more; re-dispatch the task", tool.Name, policyName)
	}
	return nil
}

func connectorPolicyPin(ctx context.Context, reader client.Reader, registry *tools.Registry, frozen map[string]outboundaccess.FrozenConnection, tool *corev1alpha1.Tool) (string, *outboundaccess.PolicyIdentity, error) {
	if tool == nil || tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
		return "", nil, nil
	}
	infos, err := connectorToolsFor(ctx, reader, registry, tool.Namespace, []string{tool.Name})
	if err != nil {
		return "", nil, fmt.Errorf("classify connector tool %q: %w", tool.Name, err)
	}
	info, ok := infos[tool.Name]
	if !ok {
		return "", nil, connectorClassificationDrift(frozen, tool)
	}
	return info.PolicyName, &outboundaccess.PolicyIdentity{UID: info.PolicyUID, Generation: info.PolicyGeneration}, nil
}

func forwardedWriteKeptResult(descriptor harnessv2.MCPToolDescriptor, result json.RawMessage) json.RawMessage {
	if descriptor.Forward == nil || !descriptor.Forward.Write || mcpToolResultIsError(result) {
		return result
	}
	status, keptStatus := "an unreadable status", http.StatusConflict
	headers := map[string]string{"Content-Type": "application/json"}
	if response, err := harnessv2.DecodeMCPForwardResponse(result); err == nil {
		status = "status " + strconv.Itoa(response.Status)
		if azureRetryableStatus(response.Status) {
			keptStatus = response.Status
			if after := response.Headers["Retry-After"]; after != "" {
				headers["Retry-After"] = after
			}
		}
	}
	type azureError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	body, _ := json.Marshal(struct {
		Error azureError `json:"error"`
	}{azureError{
		Code: "OrkaResultNotKept",
		Message: "Azure already answered this request, with " + status + ". Orka does not keep the answers to Azure writes, " +
			"which can carry secrets; check the resource, or run the command again to read the result.",
	}})
	kept, err := harnessv2.CanonicalValue(harnessv2.MCPForwardResponse{
		Status: keptStatus, Headers: headers, Body: string(body),
	})
	if err != nil {
		return json.RawMessage(`{"isError":true,"code":"tool_outcome_unknown","error":"The tool may have run. Do not repeat it automatically."}`)
	}
	return kept
}

func azureRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func maskForwardedWriteArguments(arguments json.RawMessage) json.RawMessage {
	request, err := harnessv2.DecodeMCPForwardRequest(arguments)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	var body any
	decoder := json.NewDecoder(bytes.NewReader(request.Body))
	decoder.UseNumber()
	var shown map[string]azurePreviewKind
	if request.Method == http.MethodPut || request.Method == http.MethodPatch {
		shown = azurePreviewSchemas[azureResourceType(request.Path)]
	}
	if len(request.Body) > 0 && decoder.Decode(&body) == nil {
		body = maskJSONValues(body, "", shown)
	} else {
		body = nil
	}
	masked, err := json.Marshal(struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		Query       string `json:"query,omitempty"`
		Body        any    `json:"body,omitempty"`
		IfMatch     string `json:"ifMatch,omitempty"`
		IfNoneMatch string `json:"ifNoneMatch,omitempty"`
	}{request.Method, request.Path, maskForwardedQuery(request.Query), body, request.IfMatch, request.IfNoneMatch})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return masked
}

func maskForwardedQuery(query string) string {
	if query == "" {
		return ""
	}
	parameters := strings.Split(query, "&")
	for index, parameter := range parameters {
		key, _, hasValue := strings.Cut(parameter, "=")
		if !hasValue {
			continue
		}
		if name, err := url.QueryUnescape(key); err == nil && strings.EqualFold(name, "api-version") {
			continue
		}
		parameters[index] = key + "=[masked]"
	}
	return strings.Join(parameters, "&")
}

var azurePreviewSchemas = func() map[string]map[string]azurePreviewKind {
	pool := "mode:s ostype:s ossku:s orchestratorversion:s count:n vmsize:s enableautoscaling:b mincount:n maxcount:n"
	schemas := map[string]string{
		azureResourceGroupType: "location:s",
		"microsoft.containerservice/managedclusters": "location:s zones.*:s sku.name:s sku.tier:s identity.type:s " +
			"properties.kubernetesversion:s properties.agentpoolprofiles.*.name:s " + prefixed("properties.agentpoolprofiles.*.", pool),
		"microsoft.containerservice/managedclusters/agentpools": prefixed("properties.", pool),
		"microsoft.compute/virtualmachines":                     "location:s zones.*:s properties.hardwareprofile.vmsize:s",
	}
	kinds := map[string]azurePreviewKind{"s": azurePreviewString, "n": azurePreviewNumber, "b": azurePreviewBool}
	out := make(map[string]map[string]azurePreviewKind, len(schemas))
	for resourceType, paths := range schemas {
		out[resourceType] = map[string]azurePreviewKind{}
		for field := range strings.FieldsSeq(paths) {
			path, kind, _ := strings.Cut(field, ":")
			out[resourceType][path] = kinds[kind]
		}
	}
	return out
}()

type azurePreviewKind int

const (
	azurePreviewString azurePreviewKind = iota + 1
	azurePreviewNumber
	azurePreviewBool
)

const azureResourceGroupType = "resourcegroups"

func prefixed(prefix, fields string) string {
	var out []string
	for field := range strings.FieldsSeq(fields) {
		out = append(out, prefix+field)
	}
	return strings.Join(out, " ")
}

func azureResourceType(path string) string {
	segments := strings.Split(strings.Trim(strings.ToLower(path), "/"), "/")
	if len(segments) == 4 && segments[0] == "subscriptions" && segments[2] == azureResourceGroupType {
		return azureResourceGroupType
	}
	providers := -1
	for index, segment := range segments {
		if segment == "providers" {
			providers = index
		}
	}
	rest := segments[providers+1:]
	if providers < 0 || len(rest) < 3 || len(rest)%2 == 0 {
		return ""
	}
	resourceTypes := []string{rest[0]}
	for index := 1; index < len(rest); index += 2 {
		resourceTypes = append(resourceTypes, rest[index])
	}
	return strings.Join(resourceTypes, "/")
}

const maxAzurePreviewString = 64

var azurePreviewText = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]*$`)

func maskJSONValues(value any, path string, shown map[string]azurePreviewKind) any {
	child := func(segment string) string {
		if path == "" {
			return segment
		}
		return path + "." + segment
	}
	switch typed := value.(type) {
	case map[string]any:
		for field, item := range typed {
			segment := strings.ToLower(field)
			if segment == "" || strings.ContainsAny(segment, ".*\x00") {
				segment = "\x00"
			}
			typed[field] = maskJSONValues(item, child(segment), shown)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = maskJSONValues(item, child("*"), shown)
		}
		return typed
	}
	if azurePreviewShows(shown[path], value) {
		return value
	}
	return "[masked]"
}

func azurePreviewShows(kind azurePreviewKind, value any) bool {
	switch typed := value.(type) {
	case string:
		return kind == azurePreviewString && len(typed) <= maxAzurePreviewString && azurePreviewText.MatchString(typed)
	case json.Number:
		return kind == azurePreviewNumber
	case bool:
		return kind == azurePreviewBool
	}
	return false
}

func connectorBindingDigest(connectionDigest, specDigest string) string {
	sum := sha256.Sum256([]byte(connectionDigest + "\x00" + specDigest))
	return hex.EncodeToString(sum[:])
}

func (e RegistryACPMCPToolExecutor) ValidateACPMCPTool(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) error {
	if descriptor.Source != harnessv2.MCPToolSourceBrokeredCustom {
		return nil
	}
	if e.Reader == nil {
		return errors.New("custom MCP tool reader is unavailable")
	}
	tool := &corev1alpha1.Tool{}
	if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: customACPMCPToolResource(descriptor)}, tool); err != nil {
		return acpMCPAuthorityReadError(err)
	}
	return matchCurrentCustomACPMCPTool(tool, descriptor, request.Authorization.ApprovalPolicy)
}

func customACPMCPToolResource(descriptor harnessv2.MCPToolDescriptor) string {
	if descriptor.Forward != nil && descriptor.Forward.Write {
		return harnessv2.MCPForwardToolResource(descriptor.Name)
	}
	return descriptor.Name
}

func matchCurrentCustomACPMCPTool(tool *corev1alpha1.Tool, descriptor harnessv2.MCPToolDescriptor, approval harnessv2.MCPApprovalPolicy) error {
	current, err := customACPMCPToolDescriptor(tool)
	if err == nil && descriptor.Forward != nil && descriptor.Forward.Write {
		current, err = forwardingWriteDescriptor(tool, current)
		if err == nil && tool.Spec.HTTP.Forward.Writes.WriteApprovalRequired() && !approval.Requires(descriptor.Name) {
			return fmt.Errorf("forwarding write tool %q now requires approval", descriptor.Name)
		}
	}
	if err != nil {
		return err
	}
	expected, expectedErr := harnessv2.CanonicalValue(descriptor)
	currentJSON, currentErr := harnessv2.CanonicalValue(current)
	if expectedErr != nil || currentErr != nil || !bytes.Equal(expected, currentJSON) {
		return fmt.Errorf("custom MCP tool %q changed after prompt authorization", descriptor.Name)
	}
	return nil
}

func (e RegistryACPMCPToolExecutor) ExecuteACPMCPTool(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	descriptor harnessv2.MCPToolDescriptor,
) (json.RawMessage, error) {
	call, err := e.prepareACPMCPTool(ctx, request, descriptor)
	if err != nil {
		if descriptor.Forward != nil && descriptor.Forward.Write {
			return nil, &tools.NoEffectError{Summary: "The tool refused the request before sending it.", Err: err}
		}
		return nil, err
	}
	result, err := call(ctx)
	if refusal, ok := errors.AsType[*acpMCPRefusedWithoutEffect](err); ok {
		return refusal.Result, nil
	}
	return result, err
}

type acpMCPRefusedWithoutEffect struct {
	Result json.RawMessage
	Code   string
	Reason string
}

func (e *acpMCPRefusedWithoutEffect) Error() string { return e.Reason }

//nolint:gocyclo // Tool preparation enforces every frozen-binding and policy check before execution.
func (e RegistryACPMCPToolExecutor) prepareACPMCPTool(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	descriptor harnessv2.MCPToolDescriptor,
) (acpMCPPreparedToolCall, error) {
	var execute func(context.Context) (string, error)
	switch descriptor.Source {
	case harnessv2.MCPToolSourceBrokeredBuiltin:
		registry := e.Registry
		if registry == nil {
			registry = tools.DefaultRegistry
		}
		if _, ok := registry.Get(descriptor.Name); !ok {
			return nil, fmt.Errorf("MCP tool %q is not registered", descriptor.Name)
		}
		_, linkedBuiltin := connectors.BuiltinConnectorToolClass(descriptor.Name)
		needsRequester := linkedBuiltin || descriptor.Name == tools.ListConnectionsToolName
		var toolContext *tools.ToolContext
		if e.ContextFactory != nil {
			var contextErr error
			toolContext, contextErr = e.ContextFactory(ctx, request)
			if contextErr != nil {
				return nil, contextErr
			}
			if toolContext != nil {
				copy := *toolContext
				copy.Namespace = request.Namespace
				copy.TaskUID = string(request.Metadata.TaskUID)
				copy.ToolCallID = request.Call.CallID
				copy.OperationID = string(request.Metadata.OperationID)
				if copy.Tenant == "" {
					copy.Tenant = request.Namespace
				}
				if needsRequester {
					requester, frozen, transaction, err := e.taskConnectionAuthority(ctx, request)
					if err != nil {
						return nil, err
					}
					copy.Requester = requester
					if descriptor.Name == tools.ListConnectionsToolName {
						copy.AuthorizeConnectorRead = brokerConnectorReadAuthorizer(transaction, e.ConnectorReadScopes, e.EnforceTransactionCredentialAuth)
					}
					if linkedBuiltin {
						authenticated, _ := ACPMCPAuthenticatedTaskFromContext(ctx)
						if copy.TaskID != "" && copy.TaskID != authenticated.Name {
							return nil, fmt.Errorf("built-in tool %q: the tool context names task %q, not the authenticated task", descriptor.Name, copy.TaskID)
						}
						copy.TaskID = authenticated.Name
						copy.LinkedAccounts = linkedBuiltinAccounts{
							source: e.Connections, namespace: request.Namespace, requester: requester, frozen: frozen, required: true,
						}
					}
				}
				toolContext = &copy
			}
		}
		if linkedBuiltin && (toolContext == nil || toolContext.LinkedAccounts == nil) {
			return nil, fmt.Errorf("built-in tool %q runs only under the requester's linked account, and no authenticated task context is available", descriptor.Name)
		}
		execute = func(callCtx context.Context) (string, error) {
			if toolContext != nil {
				callCtx = tools.WithToolContext(callCtx, toolContext)
			}
			if timeout, bounded := connectors.BuiltinConnectorToolTimeout(descriptor.Name); linkedBuiltin && bounded {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithTimeout(callCtx, timeout)
				defer cancel()
			}
			return registry.Execute(callCtx, descriptor.Name, request.Call.Arguments)
		}
	case harnessv2.MCPToolSourceBrokeredCustom:
		if e.Reader == nil || e.KubeClient == nil {
			return nil, fmt.Errorf("custom MCP tool executor is not configured")
		}
		e.Reader = acpMCPAuthorityReader{e.Reader}
		tool := &corev1alpha1.Tool{}
		resource := customACPMCPToolResource(descriptor)
		if getErr := e.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: resource}, tool); getErr != nil {
			return nil, fmt.Errorf("load custom MCP tool %q: %w", resource, getErr)
		}
		if matchErr := matchCurrentCustomACPMCPTool(tool, descriptor, request.Authorization.ApprovalPolicy); matchErr != nil {
			return nil, matchErr
		}
		executor := workerexecutor.NewToolExecutorForNamespace(request.Namespace, e.KubeClient, e.HTTPClient, e.OutboundAccess)
		executor.SetSubstrateMCPRouter(e.SubstrateMCPRouter)
		executor.SetTransactionExchangeConfig(e.TransactionExchange)
		if descriptor.Forward != nil {
			executor.AllowForwarding()
			if descriptor.Forward.Write {
				tool = tool.DeepCopy()
				tool.Spec.BrokeredToolClass = corev1alpha1.AgentRuntimeBrokeredToolClassWrite
			}
		}
		if tool.Spec.HTTP != nil && tool.Spec.HTTP.GatewayTaskPass {
			if err := e.bindGatewayTaskGrant(ctx, request, tool, executor); err != nil {
				return nil, err
			}
		} else if bindErr := e.bindTaskTransactionAuthority(ctx, request, executor); bindErr != nil {
			return nil, bindErr
		}
		policyName, identity, pinErr := connectorPolicyPin(ctx, e.Reader, e.Registry, executor.FrozenConnections(), tool)
		if pinErr != nil {
			return nil, pinErr
		}
		if identity != nil {
			executor.SetCheckedPolicy(policyName, *identity)
		}
		authenticated, _ := ACPMCPAuthenticatedTaskFromContext(ctx)
		execute = func(callCtx context.Context) (string, error) {
			execCtx := workerexecutor.WithToolCallID(callCtx, request.Call.CallID)
			execCtx = workerexecutor.WithToolIdempotencyKey(execCtx, string(request.Metadata.OperationID))
			execCtx = workerexecutor.WithAgentUID(execCtx, authenticated.AgentUID)
			result, err := executor.Execute(execCtx, tool, request.Call.Arguments)
			return result, customToolNoEffectError(err)
		}
	default:
		return nil, fmt.Errorf("MCP tool %q is not broker-executable", descriptor.Name)
	}
	return func(callCtx context.Context) (json.RawMessage, error) {
		result, err := execute(callCtx)
		return acpMCPToolExecutionResult(callCtx, descriptor, result, err)
	}, nil
}

func customToolNoEffectError(err error) error {
	if err == nil {
		return nil
	}
	if _, delivered := errors.AsType[workerexecutor.ToolExecutionError](err); delivered {
		return err
	}
	if !workerexecutor.ToolRequestWasAttempted(err) {
		return &tools.NoEffectError{Summary: "The tool stopped before sending its request.", Err: err}
	}
	if status, ok := errors.AsType[workerexecutor.ToolHTTPStatusError](err); ok && !status.Redirected &&
		status.StatusCode >= 400 && status.StatusCode < 500 && status.StatusCode != http.StatusRequestTimeout {
		return &tools.NoEffectError{Summary: fmt.Sprintf("The tool endpoint refused the request (HTTP %d).", status.StatusCode), Err: err}
	}
	return err
}

func acpMCPToolExecutionResult(ctx context.Context, descriptor harnessv2.MCPToolDescriptor, result string, err error) (json.RawMessage, error) {
	if err != nil {
		if rejection, ok := errors.AsType[*tools.GatewayReplyRejection](err); ok &&
			descriptor.Source == harnessv2.MCPToolSourceBrokeredBuiltin && descriptor.Name == "reply_in_conversation" {
			return json.Marshal(struct {
				IsError bool   `json:"isError"`
				Error   string `json:"error"`
			}{true, rejection.Error()})
		}
		if argErr, ok := errors.AsType[*tools.ToolArgumentError](err); ok {
			return json.Marshal(struct {
				IsError bool   `json:"isError"`
				Code    string `json:"code"`
				Error   string `json:"error"`
			}{true, "invalid_arguments", argErr.Error()})
		}
		if required, ok := errors.AsType[*outboundaccess.ConnectionRequiredError](err); ok {
			log.FromContext(ctx).Info("ACP MCP tool call refused: the requester's linked account is unavailable",
				"tool", descriptor.Name, "provider", required.Provider, "kind", required.Kind, "reason", required.Error())
			result, err := json.Marshal(struct {
				IsError bool   `json:"isError"`
				Code    string `json:"code"`
				Error   string `json:"error"`
			}{true, harnessv2.MCPToolErrorCodeConnectionRequired, required.Error()})
			if err != nil {
				return nil, err
			}
			return result, &acpMCPRefusedWithoutEffect{Result: result, Code: harnessv2.MCPToolErrorCodeConnectionRequired, Reason: required.Error()}
		}
		_, executionFailed := errors.AsType[workerexecutor.ToolExecutionError](err)
		readTimedOut := descriptor.Effect == harnessv2.MCPToolEffectReadOnly && ctx.Err() == nil &&
			workerexecutor.ToolRequestWasAttempted(err) && errors.Is(err, context.DeadlineExceeded)
		if executionFailed || readTimedOut {
			return json.RawMessage(`{"isError":true,"code":"tool_execution_failed","error":"MCP tool execution failed"}`), nil
		}
		return nil, err
	}
	raw := json.RawMessage(result)
	if _, err := harnessv2.CanonicalJSON(raw); err != nil {
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return nil, encodeErr
		}
		raw = encoded
	}
	if len(raw) > harnessv2.MaxMCPResultBytes {
		return nil, fmt.Errorf("MCP tool result exceeds %d bytes", harnessv2.MaxMCPResultBytes)
	}
	return raw, nil
}

func (e RegistryACPMCPToolExecutor) bindTaskTransactionAuthority(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	executor *workerexecutor.ToolExecutor,
) error {
	authenticated, ok := ACPMCPAuthenticatedTaskFromContext(ctx)
	if !ok || authenticated.Namespace != request.Namespace || authenticated.UID != string(request.Metadata.TaskUID) {
		return errors.New("authenticated ACP MCP task authority is unavailable")
	}
	if e.Reader == nil {
		return errors.New("ACP MCP task authority requires a Kubernetes reader")
	}
	task := &corev1alpha1.Task{}
	if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: authenticated.Namespace, Name: authenticated.Name}, task); err != nil {
		return fmt.Errorf("load authenticated ACP MCP task authority: %w", err)
	}
	if string(task.UID) != authenticated.UID {
		return errors.New("authenticated ACP MCP task identity changed")
	}
	if err := bindFrozenConnections(ctx, e.Reader, e.AgentExecutionSnapshots, task, executor); err != nil {
		return err
	}
	return bindVerifiedTaskTransactionAuthority(
		ctx, e.Reader, task, e.TransactionCredentialReadScopes,
		e.EnforceTransactionCredentialAuth, executor,
	)
}

type ACPMCPBrokerDependencies struct {
	TaskProvenanceProtected bool
	GatewayService          *gatewayruntime.Service
	ConnectorReadScopes     []string
	Reader                  client.Reader
	Epochs                  *ControllerEpochManager
	ControlStore            store.DurableControlStore
	AgentExecutionSnapshots store.AgentExecutionSnapshotStore
	ExecutionEvents         store.DeduplicatingExecutionEventStore
	PromptLeases            *ACPMCPPromptLeaseRegistry
	KubeClient              kubernetes.Interface
	HTTPClient              *http.Client
	Registry                *tools.Registry
	OutboundAccess          outboundaccess.Resolver
	TransactionExchange     *workerexecutor.TransactionExchangeConfig
	ContextFactory          func(context.Context, harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error)
	SubstrateMCPRouter      *workerexecutor.SubstrateMCPRouter
	Connections             outboundaccess.ConnectionCredentialSource

	EnforceTransactionCredentialAuth bool
	TransactionCredentialReadScopes  []string
}

func NewProductionACPMCPBroker(dependencies ACPMCPBrokerDependencies) (*ACPMCPBroker, error) {
	if dependencies.PromptLeases == nil {
		return nil, fmt.Errorf("production ACP MCP broker requires a prompt lease registry")
	}
	if dependencies.Reader == nil || dependencies.Epochs == nil || dependencies.ControlStore == nil ||
		dependencies.AgentExecutionSnapshots == nil || dependencies.ExecutionEvents == nil || dependencies.KubeClient == nil {
		return nil, fmt.Errorf("production ACP MCP broker dependencies are incomplete")
	}
	epochMutations, ok := dependencies.ControlStore.(store.ControllerEpochMutationStore)
	if !ok {
		return nil, fmt.Errorf("production ACP MCP broker requires the authoritative epoch mutation guard")
	}
	broker := &ACPMCPBroker{
		Credentials: KubernetesACPMCPBrokerCredentialResolver{
			Reader: dependencies.Reader, Epochs: dependencies.Epochs,
			AgentExecutionSnapshots: dependencies.AgentExecutionSnapshots,
		},
		Prompts: DurableACPMCPPromptAuthorizer{Attempts: dependencies.ControlStore, PromptLeases: dependencies.PromptLeases},
		Executor: RegistryACPMCPToolExecutor{
			TaskProvenanceProtected: dependencies.TaskProvenanceProtected,
			GatewayService:          dependencies.GatewayService,
			AgentExecutionSnapshots: dependencies.AgentExecutionSnapshots,
			Registry:                dependencies.Registry, Reader: dependencies.Reader, KubeClient: dependencies.KubeClient,
			HTTPClient: dependencies.HTTPClient, OutboundAccess: dependencies.OutboundAccess,
			TransactionExchange: dependencies.TransactionExchange, ContextFactory: dependencies.ContextFactory,
			SubstrateMCPRouter:               dependencies.SubstrateMCPRouter,
			Connections:                      dependencies.Connections,
			ConnectorReadScopes:              append([]string(nil), dependencies.ConnectorReadScopes...),
			EnforceTransactionCredentialAuth: dependencies.EnforceTransactionCredentialAuth,
			TransactionCredentialReadScopes: append(
				[]string(nil),
				dependencies.TransactionCredentialReadScopes...,
			),
		},
		Effects: dependencies.ControlStore, EpochMutations: epochMutations,
		ApprovalEvents: dependencies.ExecutionEvents, ApprovalSecrets: dependencies.KubeClient,
	}
	if err := broker.Validate(); err != nil {
		return nil, err
	}
	return broker, nil
}

type ACPMCPBroker struct {
	Credentials          ACPMCPBrokerCredentialResolver
	Prompts              ACPMCPPromptAuthorizer
	Executor             ACPMCPToolExecutor
	Effects              store.ExternalEffectStore
	EpochMutations       store.ControllerEpochMutationStore
	MaxBodyBytes         int64
	ApprovalEvents       store.DeduplicatingExecutionEventStore
	ApprovalSecrets      kubernetes.Interface
	ApprovalWaitTimeout  time.Duration
	ApprovalPollInterval time.Duration
}

func (b *ACPMCPBroker) Validate() error {
	if b == nil || b.Credentials == nil || b.Prompts == nil || b.Executor == nil || b.Effects == nil {
		return fmt.Errorf("ACP MCP broker requires credentials, prompt authorization, tool executor, and external-effect store")
	}
	return nil
}

//nolint:gocyclo // The MCP request dispatcher keeps authentication and per-operation authorization explicit.
func (b *ACPMCPBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != harnessv2.MCPBrokerCallPath {
		http.NotFound(w, r)
		return
	}
	if err := b.Validate(); err != nil {
		writeACPMCPError(w, http.StatusServiceUnavailable, "MCP broker is unavailable")
		return
	}
	if preAuth, ok := b.Credentials.(ACPMCPBrokerPreAuthenticator); ok {
		poolNamespace := r.Header.Get(harnessv2.MCPBrokerPoolNamespaceHeader)
		poolUID := r.Header.Get(harnessv2.MCPBrokerPoolUIDHeader)
		if err := preAuth.PreAuthenticateACPMCPBroker(r.Context(), poolNamespace, poolUID, r.Header.Get("Authorization")); err != nil {
			writeACPMCPError(w, http.StatusUnauthorized, "MCP broker authentication failed")
			return
		}
	}
	limit := b.MaxBodyBytes
	if limit <= 0 || limit > harnessv2.MaxCanonicalJSONBytes {
		limit = harnessv2.MaxCanonicalJSONBytes
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close() //nolint:errcheck
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var request harnessv2.MCPBrokerCallRequest
	if err := decoder.Decode(&request); err != nil {
		writeACPMCPError(w, http.StatusBadRequest, "invalid MCP broker request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeACPMCPError(w, http.StatusBadRequest, "invalid MCP broker request")
		return
	}
	now := time.Now().UTC()
	descriptor, err := request.ValidateAt(now)
	if err != nil {
		writeACPMCPError(w, http.StatusBadRequest, "invalid MCP broker request")
		return
	}
	if _, ok := b.Credentials.(ACPMCPBrokerPreAuthenticator); ok {
		if request.Namespace != r.Header.Get(harnessv2.MCPBrokerPoolNamespaceHeader) ||
			string(request.Metadata.Fence.RuntimePoolUID) != r.Header.Get(harnessv2.MCPBrokerPoolUIDHeader) {
			writeACPMCPError(w, http.StatusBadRequest, "MCP broker request does not match its authenticated pool identity")
			return
		}
	}
	credentials, err := b.Credentials.ResolveACPMCPBrokerCredentials(r.Context(), request)
	if err != nil || !constantTimeBearerMatch(r.Header.Get("Authorization"), credentials.ControllerBearerToken) {
		writeACPMCPError(w, http.StatusUnauthorized, "MCP broker authentication failed")
		return
	}
	if mismatch := harnessv2.CompareFence(credentials.ExpectedFence, request.Metadata.Fence, true); mismatch != harnessv2.FenceMatch {
		writeACPMCPError(w, http.StatusGone, "MCP broker fence is stale")
		return
	}
	if err := request.Authorization.ValidateProfile(credentials.RuntimeProfile); err != nil {
		writeACPMCPError(w, http.StatusGone, "MCP broker policy is stale")
		return
	}
	if credentials.ExpectedMCPConfiguration != nil &&
		!request.Authorization.Configuration().Matches(*credentials.ExpectedMCPConfiguration) {
		writeACPMCPError(w, http.StatusGone, "MCP broker policy is stale")
		return
	}
	if err := harnessv2.VerifyOperationCapability(
		credentials.CapabilitySecret, r.Header.Get(harnessv2.OperationCapabilityHeader), request.Metadata, true, time.Now().UTC(),
	); err != nil {
		writeACPMCPError(w, http.StatusForbidden, "MCP broker operation authorization failed")
		return
	}
	promptCtx := withACPMCPAuthenticatedTask(r.Context(), credentials.Task)
	if err := b.authorizeMCPPromptAdmission(promptCtx, request); err != nil {
		writeACPMCPError(w, http.StatusForbidden, "MCP prompt is not active")
		return
	}
	promptCtx = context.WithValue(promptCtx, acpMCPTaskDataGuardContextKey{}, b.taskDataGuard(request, credentials))
	approvalRequired := request.Authorization.ApprovalPolicy.Requires(request.Call.ToolName)
	promptCtx, stopPrompt := b.watchPromptAuthority(promptCtx, request, approvalRequired)
	defer stopPrompt()
	if approvalRequired {
		b.serveApprovedCall(w, promptCtx, request, descriptor, credentials)
		return
	}
	call := func(ctx context.Context) (json.RawMessage, error) {
		ctx = withACPMCPAuthenticatedTask(ctx, credentials.Task)
		result, executeErr := b.Executor.ExecuteACPMCPTool(ctx, request, descriptor)
		if executeErr != nil {
			return nil, executeErr
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := b.Prompts.AuthorizeACPMCPPrompt(ctx, request); err != nil {
			return nil, fmt.Errorf("MCP prompt authority was revoked during tool execution")
		}
		return result, nil
	}
	var result json.RawMessage
	replayed := false
	var effectIdentity *store.ExternalEffectIdentity
	if descriptor.Effect == harnessv2.MCPToolEffectConsequential {
		identity := store.ExternalEffectIdentity{
			Kind: acpMCPToolEffectKind, Namespace: request.Namespace,
			AggregateID: string(request.Authorization.RuntimeSessionUID),
			OperationID: string(request.Metadata.OperationID),
		}
		effectIdentity = &identity
		effectRequest := map[string]any{"call": request.Call, "descriptor": descriptor}
		if digester, ok := b.Executor.(acpMCPConnectionDigester); ok {
			digest, digestErr := digester.ConnectionDigest(promptCtx, request, descriptor)
			if digestErr != nil {
				writeACPMCPError(w, http.StatusBadGateway, "MCP tool connection binding is unavailable")
				return
			}
			if digest != "" {
				effectRequest["connection"] = digest
			}
		}
		run, execute := runExternalEffectWithReplay[json.RawMessage], call
		var live json.RawMessage
		if descriptor.Forward != nil && descriptor.Forward.Write {
			run = runExternalEffectOnceWithReplay[json.RawMessage]
			execute = func(ctx context.Context) (json.RawMessage, error) {
				answer, callErr := call(ctx)
				if callErr != nil {
					return nil, callErr
				}
				live = answer
				return forwardedWriteKeptResult(descriptor, answer), nil
			}
		}
		result, replayed, err = run(promptCtx, b.Effects, credentials.ControllerFence, identity, effectRequest, execute)
		if err == nil && !replayed && live != nil {
			result = live
		}
		if errors.Is(err, errExternalEffectAlreadyStarted) {
			result, err = json.RawMessage(`{"isError":true,"code":"tool_outcome_unknown","error":"The tool may have run. Do not repeat it automatically."}`), nil
		}
		if noEffect, ok := errors.AsType[*tools.NoEffectError](err); ok && descriptor.Forward != nil && descriptor.Forward.Write {
			failure := acpApprovalResult("", acpApprovalCodeNoEffect, acpApprovalFailureSummary(noEffect.Summary))
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			if settleErr := settleExternalEffectStore(settleCtx, b.Effects, credentials.ControllerFence, identity, store.ExternalEffectFailed, failure); settleErr == nil {
				result, err = failure, nil
			}
			cancel()
		}
	} else {
		result, err = call(promptCtx)
	}
	if err != nil {
		if effectIdentity != nil && !errors.Is(err, store.ErrConflict) {
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			_ = settleExternalEffectStore(settleCtx, b.Effects, credentials.ControllerFence, *effectIdentity, store.ExternalEffectOutcomeUnknown, nil)
			cancel()
		}
		writeACPMCPError(w, http.StatusBadGateway, "MCP tool execution failed")
		return
	}
	response := harnessv2.MCPBrokerCallResponse{
		Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: result,
		IsError: mcpToolResultIsError(result), Replayed: replayed,
	}
	if err := response.Validate(); err != nil {
		writeACPMCPError(w, http.StatusBadGateway, "MCP tool returned an invalid result")
		return
	}
	writeACPMCPJSON(w, http.StatusOK, response)
}

func (b *ACPMCPBroker) authorizeMCPPromptAdmission(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
	if !request.Authorization.ApprovalPolicy.Requires(request.Call.ToolName) {
		return b.Prompts.AuthorizeACPMCPPrompt(ctx, request)
	}
	waitCtx, cancel := context.WithDeadline(ctx, request.Metadata.ExpiresAt)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := waitCtx.Err(); err != nil {
			return err
		}
		err := b.Prompts.AuthorizeACPMCPPrompt(waitCtx, request)
		if !errors.Is(err, errACPMCPPromptAwaitingAcceptance) && !errors.Is(err, errACPMCPAuthorityUnavailable) {
			if err == nil {
				if !time.Now().UTC().Before(request.Metadata.ExpiresAt) {
					return context.DeadlineExceeded
				}
				return waitCtx.Err()
			}
			return err
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (b *ACPMCPBroker) watchPromptAuthority(ctx context.Context, request harnessv2.MCPBrokerCallRequest, allowUnavailable bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkCtx, stopCheck := context.WithTimeout(ctx, 5*time.Second)
				err := b.Prompts.AuthorizeACPMCPPrompt(checkCtx, request)
				stopCheck()
				if err != nil && (!allowUnavailable || !errors.Is(err, errACPMCPAuthorityUnavailable)) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func mcpToolResultIsError(result json.RawMessage) bool {
	var envelope struct {
		Success *bool `json:"success"`
		IsError bool  `json:"isError"`
	}
	if json.Unmarshal(result, &envelope) != nil {
		return false
	}
	return envelope.IsError || (envelope.Success != nil && !*envelope.Success)
}

func constantTimeBearerMatch(header, expected string) bool {
	value := strings.TrimSpace(header)
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		value = strings.TrimSpace(value[len("Bearer "):])
	}
	expected = strings.TrimSpace(expected)
	return len(value) == len(expected) && len(expected) >= 32 && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

type DurableACPMCPPromptAuthorizer struct {
	Attempts     store.PromptAttemptStore
	PromptLeases *ACPMCPPromptLeaseRegistry
}

var (
	errACPMCPTaskCancelled            = errors.New("MCP task was cancelled")
	errACPMCPTaskExpired              = errors.New("MCP task deadline has expired")
	errACPMCPPromptAwaitingAcceptance = errors.New("MCP prompt acceptance has not been recorded")
)

func (a DurableACPMCPPromptAuthorizer) AuthorizeACPMCPPrompt(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
	if a.Attempts == nil {
		return fmt.Errorf("prompt-attempt store is required")
	}
	key := store.PromptAttemptKey{
		Namespace: request.Namespace, TaskUID: string(request.Metadata.TaskUID),
		Attempt: int64(request.Metadata.TaskAttempt), PromptID: string(request.Metadata.PromptID),
	}
	id, err := key.CanonicalID()
	if err != nil {
		return err
	}
	approvalRequired := request.Authorization.ApprovalPolicy.Requires(request.Call.ToolName)
	if approvalRequired {
		if err := a.PromptLeases.authorize(request); err != nil {
			return err
		}
	}
	attempt, err := a.Attempts.GetPromptAttempt(ctx, id)
	if err != nil {
		return acpMCPAuthorityReadError(err)
	}
	if attempt.ExecutionState == store.PromptExecutionSubmitting {
		descriptor, ok := request.Authorization.ToolPolicy.Descriptor(request.Call.ToolName)
		if !ok || descriptor.Effect != harnessv2.MCPToolEffectReadOnly && !approvalRequired {
			return fmt.Errorf("consequential or approval-required MCP calls require an accepted prompt attempt")
		}
	} else if attempt.ExecutionState != store.PromptExecutionAccepted && attempt.ExecutionState != store.PromptExecutionRunning {
		return fmt.Errorf("prompt attempt is in state %s", attempt.ExecutionState)
	}
	if attempt.SessionUID != string(request.Authorization.RuntimeSessionUID) ||
		attempt.RuntimeInstanceID != string(request.Metadata.Fence.RuntimeInstanceID) ||
		attempt.ControllerEpoch != int64(request.Metadata.Fence.ControllerEpoch) {
		return fmt.Errorf("prompt attempt identity does not match MCP authorization")
	}
	if approvalRequired {
		if err := a.PromptLeases.authorize(request); err != nil {
			return err
		}
		if attempt.ExecutionState == store.PromptExecutionSubmitting {
			return errACPMCPPromptAwaitingAcceptance
		}
	}
	return nil
}

type KubernetesACPMCPBrokerCredentialResolver struct {
	Reader                  client.Reader
	Epochs                  *ControllerEpochManager
	AgentExecutionSnapshots store.AgentExecutionSnapshotStore
}

var errACPMCPRuntimePoolNotFound = errors.New("runtime pool active instance was not found")

func (r KubernetesACPMCPBrokerCredentialResolver) PreAuthenticateACPMCPBroker(
	ctx context.Context,
	poolNamespace, poolUID, authorizationHeader string,
) error {
	if r.Reader == nil {
		return fmt.Errorf("kubernetes MCP credential resolver is not configured")
	}
	if strings.TrimSpace(poolNamespace) == "" || strings.TrimSpace(poolUID) == "" {
		return fmt.Errorf("MCP broker pool identity headers are required")
	}
	identity := harnessv2.RuntimePoolUID(poolUID)
	pool, err := findACPMCPRuntimePoolIdentity(ctx, r.Reader, poolNamespace, identity)
	if err != nil {
		return err
	}
	runtime, err := findACPMCPExternalRuntimeIdentity(ctx, r.Reader, poolNamespace, identity)
	if err != nil {
		return err
	}
	if (pool == nil) == (runtime == nil) {
		return fmt.Errorf("MCP runtime provider identity is missing or ambiguous")
	}
	if pool != nil {
		if pool.Status.ActiveInstance == nil {
			return fmt.Errorf("runtime pool has no active instance")
		}
		bearer, _, authErr := runtimePoolACPMCPAuthMaterial(ctx, r.Reader, pool)
		if authErr != nil {
			return authErr
		}
		if !constantTimeBearerMatch(authorizationHeader, bearer) {
			return fmt.Errorf("MCP broker bearer does not match the pool")
		}
		return nil
	}
	if r.Epochs == nil {
		return fmt.Errorf("kubernetes MCP credential resolver is not configured")
	}
	controllerFence, err := r.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	bearer, err := externalRuntimeACPMCPPreAuthBearer(ctx, r.Reader, runtime, controllerFence, identity)
	if err != nil {
		return err
	}
	if !constantTimeBearerMatch(authorizationHeader, bearer) {
		return fmt.Errorf("MCP broker bearer does not match the runtime identity")
	}
	return nil
}

func externalRuntimeACPMCPPreAuthBearer(
	ctx context.Context,
	reader client.Reader,
	runtime *corev1alpha1.AgentRuntime,
	controllerFence store.ControllerEpochFence,
	poolUID harnessv2.RuntimePoolUID,
) (string, error) {
	if runtime == nil {
		return "", fmt.Errorf("external AgentRuntime identity is required")
	}
	observed := runtime.Status.ObservedCapabilities
	if runtime.Status.ObservedGeneration != runtime.Generation ||
		runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 || runtime.Spec.Capabilities == nil ||
		runtime.Spec.Capabilities.WorkspaceGovernance == nil || runtime.Spec.Capabilities.Profile == nil ||
		!runtime.Spec.Capabilities.WorkspaceGovernance.Strict() || observed == nil {
		return "", fmt.Errorf("external AgentRuntime authority is incomplete for MCP preauthentication")
	}
	if observed.ControllerEpoch != controllerFence.Epoch || observed.RuntimePoolUID != string(poolUID) ||
		observed.RuntimeProfileDigest != runtime.Spec.Capabilities.Profile.Digest ||
		observed.ProtocolVersion != string(corev1alpha1.RuntimePoolProtocolHarnessV2) {
		return "", fmt.Errorf("external AgentRuntime preauthentication identity is stale")
	}
	if runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef == nil {
		return "", fmt.Errorf("external AgentRuntime controller bearer reference is required")
	}
	reference := *runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef
	secret, err := readAgentRuntimeMCPSecret(ctx, reader, runtime.Namespace, reference)
	if err != nil {
		return "", err
	}
	if secret.ResourceVersion != runtime.Status.ObservedControllerAuthRefResourceVersion {
		return "", fmt.Errorf("external AgentRuntime bearer changed after conformance")
	}
	bearer := strings.TrimSpace(string(secret.Data[reference.Key]))
	if len(bearer) < 32 {
		return "", fmt.Errorf("external AgentRuntime bearer is incomplete")
	}
	return bearer, nil
}

func (r KubernetesACPMCPBrokerCredentialResolver) ResolveACPMCPBrokerCredentials(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
) (ACPMCPBrokerCredentials, error) {
	if r.Reader == nil || r.Epochs == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("kubernetes MCP credential resolver is not configured")
	}
	r.Reader = acpMCPAuthorityReader{Reader: r.Reader}
	if r.AgentExecutionSnapshots != nil {
		r.AgentExecutionSnapshots = acpMCPAuthoritySnapshots{AgentExecutionSnapshotStore: r.AgentExecutionSnapshots}
	}
	controllerFence, err := r.Epochs.CurrentFence(ctx)
	if err != nil {
		return ACPMCPBrokerCredentials{}, acpMCPAuthorityReadError(err)
	}
	if uint64(controllerFence.Epoch) != request.Metadata.Fence.ControllerEpoch {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("MCP request uses a stale controller epoch")
	}
	task, execution, err := findACPMCPTaskExecution(ctx, r.Reader, request, controllerFence)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	var credentials ACPMCPBrokerCredentials
	switch {
	case strings.TrimSpace(execution.RuntimePoolName) != "":
		credentials, err = r.resolveRuntimePoolCredentials(ctx, request, execution, controllerFence)
	case strings.TrimSpace(execution.AgentRuntimeName) != "":
		credentials, err = r.resolveExternalRuntimeCredentials(ctx, request, task, execution, controllerFence)
	default:
		return ACPMCPBrokerCredentials{}, fmt.Errorf("active MCP task has no runtime target")
	}
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	credentials.Task = ACPMCPAuthenticatedTask{
		Name: task.Name, Namespace: task.Namespace, UID: string(task.UID),
		ParentTaskID: labels.ParentTaskName(task.Labels, task.Annotations),
	}
	if task.Spec.SessionRef != nil {
		credentials.Task.SessionName = task.Spec.SessionRef.Name
	}
	credentials.Task.Deadline, _ = acpTaskDeadline(task, time.Now().UTC())
	if task.Spec.AgentRef != nil {
		credentials.Task.AgentName = strings.TrimSpace(task.Spec.AgentRef.Name)
	}
	if binding := task.Status.AgentExecutionBinding; binding != nil && binding.Agent != nil {
		credentials.Task.AgentUID = string(binding.Agent.UID)
	}
	return credentials, nil
}

func (r KubernetesACPMCPBrokerCredentialResolver) resolveRuntimePoolCredentials(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	execution *corev1alpha1.TaskExecutionStatus,
	controllerFence store.ControllerEpochFence,
) (ACPMCPBrokerCredentials, error) {
	pool, err := findACPMCPRuntimePool(ctx, r.Reader, request.Namespace, request.Metadata.Fence.RuntimePoolUID)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	if execution.RuntimePoolUID != string(pool.UID) || execution.RuntimePoolName != pool.Name {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("active MCP task is bound to a different runtime pool")
	}
	active := pool.Status.ActiveInstance
	if active == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("runtime pool has no active instance")
	}
	if active.ControllerEpoch != controllerFence.Epoch || active.ProfileDigest != pool.Spec.Runtime.Profile.Digest ||
		active.ProtocolVersion != corev1alpha1.RuntimePoolProtocolHarnessV2 {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("runtime pool active instance is stale")
	}
	expected := expectedACPMCPFence(pool, execution, controllerFence)
	if mismatch := harnessv2.CompareFence(expected, request.Metadata.Fence, true); mismatch != harnessv2.FenceMatch {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("runtime pool active instance does not match MCP request")
	}
	bearer, capability, err := runtimePoolACPMCPAuthMaterial(ctx, r.Reader, pool)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	return ACPMCPBrokerCredentials{
		ControllerBearerToken: bearer, CapabilitySecret: capability, ExpectedFence: expected,
		RuntimeProfile: runtimeProfileFromPool(pool.Spec.Runtime.Profile), ControllerFence: controllerFence,
	}, nil
}

func (r KubernetesACPMCPBrokerCredentialResolver) resolveExternalRuntimeCredentials(
	ctx context.Context,
	request harnessv2.MCPBrokerCallRequest,
	task *corev1alpha1.Task,
	execution *corev1alpha1.TaskExecutionStatus,
	controllerFence store.ControllerEpochFence,
) (ACPMCPBrokerCredentials, error) {
	verified, err := r.loadVerifiedExternalExecution(ctx, task)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	runtime := verified.externalRuntime
	if runtime == nil || runtime.Name != execution.AgentRuntimeName || string(runtime.UID) != execution.AgentRuntimeUID {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("active MCP task is bound to a different external AgentRuntime")
	}
	if verified.body.ExternalRuntime == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("frozen external AgentRuntime authority is unavailable")
	}
	frozen := verified.body.ExternalRuntime
	observed := runtime.Status.ObservedCapabilities
	if observed == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime observation is unavailable")
	}
	if strings.TrimSpace(execution.RuntimeSessionSupervisorBootID) == "" ||
		execution.RuntimeSessionSupervisorBootID != observed.SupervisorBootID {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime supervisor boot does not match the active MCP Task")
	}
	profile := verified.plan.Profile
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil || profileDigest != verified.plan.Digest {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("frozen external AgentRuntime profile digest is invalid")
	}
	expected := harnessv2.Fence{
		RuntimeInstanceID: harnessv2.RuntimeInstanceID(observed.RuntimeInstanceID),
		SupervisorBootID:  harnessv2.SupervisorBootID(observed.SupervisorBootID),
		ControllerEpoch:   uint64(controllerFence.Epoch), RuntimePoolUID: harnessv2.RuntimePoolUID(observed.RuntimePoolUID),
		RuntimePoolGeneration:      uint64(observed.RuntimePoolGeneration),
		RuntimeSessionUID:          harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
		RuntimeSessionGeneration:   uint64(execution.RuntimeSessionGeneration),
		RuntimeProfileDigest:       verified.plan.Digest,
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	if mismatch := harnessv2.CompareFence(expected, request.Metadata.Fence, true); mismatch != harnessv2.FenceMatch {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime fence does not match MCP request")
	}
	if runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef == nil || runtime.Spec.ClientAuth.OperationCapabilitySecretRef == nil {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime v2 client auth references are required")
	}
	controllerRef := *runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef
	capabilityRef := *runtime.Spec.ClientAuth.OperationCapabilitySecretRef
	bearerSecret, err := readAgentRuntimeMCPSecret(ctx, r.Reader, runtime.Namespace, controllerRef)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	capabilitySecret, err := readAgentRuntimeMCPSecret(ctx, r.Reader, runtime.Namespace, capabilityRef)
	if err != nil {
		return ACPMCPBrokerCredentials{}, err
	}
	if !frozenACPMCPSecretMatches(frozen.ControllerAuth, "controller-auth", runtime.Namespace, controllerRef, bearerSecret) ||
		!frozenACPMCPSecretMatches(frozen.OperationCapability, "operation-capability", runtime.Namespace, capabilityRef, capabilitySecret) {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime authentication authority changed after Task binding")
	}
	bearer := strings.TrimSpace(string(bearerSecret.Data[controllerRef.Key]))
	capability := append([]byte(nil), capabilitySecret.Data[capabilityRef.Key]...)
	if len(bearer) < 32 || len(capability) < harnessv2.MinCapabilitySecretBytes {
		return ACPMCPBrokerCredentials{}, fmt.Errorf("external AgentRuntime auth material is incomplete")
	}
	expectedMCPConfiguration := verified.mcpConfiguration
	return ACPMCPBrokerCredentials{
		ControllerBearerToken: bearer, CapabilitySecret: capability, ExpectedFence: expected,
		RuntimeProfile: profile, ExpectedMCPConfiguration: &expectedMCPConfiguration,
		ControllerFence: controllerFence,
	}, nil
}

func (r KubernetesACPMCPBrokerCredentialResolver) loadVerifiedExternalExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
) (*verifiedAgentExecution, error) {
	if r.AgentExecutionSnapshots == nil {
		return nil, fmt.Errorf("encrypted execution snapshot store is required for external MCP authorization")
	}
	if task == nil || task.Status.AgentExecutionBinding == nil {
		return nil, fmt.Errorf("external MCP Task has no immutable execution binding")
	}
	verifier := &TaskReconciler{
		APIReader:               r.Reader,
		ControllerEpochManager:  r.Epochs,
		AgentExecutionSnapshots: r.AgentExecutionSnapshots,
	}
	verified, err := verifier.loadVerifiedBoundExecutionForActiveSession(ctx, task, task.Status.AgentExecutionBinding)
	if err != nil {
		return nil, fmt.Errorf("verify external MCP execution binding: %w", err)
	}
	if verified.binding.Backend != corev1alpha1.AgentExecutionBackendExternalEndpoint || verified.externalRuntime == nil {
		return nil, fmt.Errorf("external MCP Task binding does not select an external AgentRuntime")
	}
	return verified, nil
}

func frozenACPMCPSecretMatches(
	frozen agentExecutionSnapshotSecretRef,
	role string,
	namespace string,
	reference corev1alpha1.AgentRuntimeSecretKeyReference,
	secret *corev1.Secret,
) bool {
	return secret != nil && frozen.Role == role && frozen.Namespace == namespace && frozen.Name == reference.Name &&
		frozen.UID == string(secret.UID) && frozen.ResourceVersion == secret.ResourceVersion &&
		len(frozen.Keys) == 1 && frozen.Keys[0] == reference.Key
}

func readAgentRuntimeMCPSecret(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	reference corev1alpha1.AgentRuntimeSecretKeyReference,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: reference.Name}, secret); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reference.Key) == "" {
		return nil, fmt.Errorf("external AgentRuntime auth key is missing")
	}
	return secret, nil
}

func findACPMCPRuntimePool(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	poolUID harnessv2.RuntimePoolUID,
) (*corev1alpha1.RuntimePool, error) {
	pool, err := findACPMCPRuntimePoolIdentity(ctx, reader, namespace, poolUID)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, errACPMCPRuntimePoolNotFound
	}
	if pool.Status.ActiveInstance == nil {
		return nil, fmt.Errorf("runtime pool has no active instance")
	}
	return pool, nil
}

func findACPMCPRuntimePoolIdentity(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	poolUID harnessv2.RuntimePoolUID,
) (*corev1alpha1.RuntimePool, error) {
	var pools corev1alpha1.RuntimePoolList
	if err := reader.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var pool *corev1alpha1.RuntimePool
	for index := range pools.Items {
		candidate := &pools.Items[index]
		if string(candidate.UID) != string(poolUID) {
			continue
		}
		if pool != nil {
			return nil, fmt.Errorf("runtime pool UID is ambiguous")
		}
		pool = candidate.DeepCopy()
	}
	return pool, nil
}

func findACPMCPExternalRuntimeIdentity(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	poolUID harnessv2.RuntimePoolUID,
) (*corev1alpha1.AgentRuntime, error) {
	var runtimes corev1alpha1.AgentRuntimeList
	if err := reader.List(ctx, &runtimes, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var runtime *corev1alpha1.AgentRuntime
	for index := range runtimes.Items {
		candidate := &runtimes.Items[index]
		observed := candidate.Status.ObservedCapabilities
		if observed == nil || observed.RuntimePoolUID != string(poolUID) {
			continue
		}
		if runtime != nil {
			return nil, fmt.Errorf("external AgentRuntime pool UID is ambiguous")
		}
		runtime = candidate.DeepCopy()
	}
	return runtime, nil
}

func findACPMCPTaskExecution(
	ctx context.Context,
	reader client.Reader,
	request harnessv2.MCPBrokerCallRequest,
	controllerFence store.ControllerEpochFence,
) (*corev1alpha1.Task, *corev1alpha1.TaskExecutionStatus, error) {
	var tasks corev1alpha1.TaskList
	if err := reader.List(ctx, &tasks, client.InNamespace(request.Namespace)); err != nil {
		return nil, nil, err
	}
	var task *corev1alpha1.Task
	var execution *corev1alpha1.TaskExecutionStatus
	for index := range tasks.Items {
		candidate := &tasks.Items[index]
		if string(candidate.UID) != string(request.Metadata.TaskUID) {
			continue
		}
		if task != nil {
			return nil, nil, fmt.Errorf("task UID is ambiguous")
		}
		task = candidate.DeepCopy()
		if candidate.Status.Execution != nil {
			copy := *candidate.Status.Execution
			execution = &copy
		}
	}
	if execution == nil {
		return nil, nil, fmt.Errorf("active MCP task was not found")
	}
	_, cancelRequested := task.Annotations[labels.AnnotationCancelRequested]
	if !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseCancelled || cancelRequested {
		return nil, nil, errACPMCPTaskCancelled
	}
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || task.Status.Phase == corev1alpha1.TaskPhaseFailed {
		return nil, nil, fmt.Errorf("MCP task is no longer active")
	}
	if deadline, ok := acpTaskDeadline(task, time.Now().UTC()); ok && !time.Now().UTC().Before(deadline) {
		return nil, nil, errACPMCPTaskExpired
	}
	if execution.State != corev1alpha1.TaskExecutionStateSubmitting &&
		execution.State != corev1alpha1.TaskExecutionStateAccepted && execution.State != corev1alpha1.TaskExecutionStateRunning {
		return nil, nil, fmt.Errorf("active MCP task is not running")
	}
	if execution.Attempt != int32(request.Metadata.TaskAttempt) || execution.PromptID != string(request.Metadata.PromptID) ||
		execution.RuntimeSessionUID != string(request.Authorization.RuntimeSessionUID) ||
		execution.RuntimeSessionGeneration != int64(request.Authorization.SessionGeneration) ||
		execution.RuntimeInstanceID != string(request.Metadata.Fence.RuntimeInstanceID) ||
		execution.ControllerEpoch != controllerFence.Epoch {
		return nil, nil, fmt.Errorf("active MCP task identity does not match request")
	}
	return task, execution, nil
}

func expectedACPMCPFence(
	pool *corev1alpha1.RuntimePool,
	execution *corev1alpha1.TaskExecutionStatus,
	controllerFence store.ControllerEpochFence,
) harnessv2.Fence {
	active := pool.Status.ActiveInstance
	return harnessv2.Fence{
		RuntimeInstanceID: harnessv2.RuntimeInstanceID(active.RuntimeInstanceID),
		SupervisorBootID:  harnessv2.SupervisorBootID(active.BootID),
		ControllerEpoch:   uint64(controllerFence.Epoch), RuntimePoolUID: harnessv2.RuntimePoolUID(pool.UID),
		RuntimePoolGeneration:      uint64(pool.Generation),
		RuntimeSessionUID:          harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
		RuntimeSessionGeneration:   uint64(execution.RuntimeSessionGeneration),
		RuntimeProfileDigest:       harnessv2.ProfileDigest(pool.Spec.Runtime.Profile.Digest),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
}

func runtimePoolACPMCPAuthMaterial(
	ctx context.Context,
	reader client.Reader,
	pool *corev1alpha1.RuntimePool,
) (string, []byte, error) {
	active := pool.Status.ActiveInstance
	namespace := pool.Spec.RuntimeNamespace
	if namespace == "" {
		namespace = active.PodNamespace
	}
	secret, err := resolveRuntimePoolAuthSecret(ctx, reader, pool, namespace, active.ControllerEpoch)
	if err != nil {
		return "", nil, err
	}
	bearer := strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKey]))
	capability := append([]byte(nil), secret.Data[runtimePoolCapabilitySecretKey]...)
	if len(bearer) < 32 || len(capability) < harnessv2.MinCapabilitySecretBytes {
		return "", nil, fmt.Errorf("runtime pool auth Secret is incomplete")
	}
	return bearer, capability, nil
}

func resolveRuntimePoolAuthSecret(
	ctx context.Context,
	reader client.Reader,
	pool *corev1alpha1.RuntimePool,
	namespace string,
	epoch int64,
) (*corev1.Secret, error) {
	if reader == nil || pool == nil || strings.TrimSpace(namespace) == "" || epoch <= 0 {
		return nil, fmt.Errorf("runtime pool auth Secret lookup is incomplete")
	}
	if pool.Spec.ExecutionWorkspace != nil {
		binding := strings.TrimSpace(pool.Annotations[runtimePoolPrivateAuthSecretBindingAnnotation(epoch)])
		if binding == "" {
			return nil, fmt.Errorf("private RuntimePool auth Secret binding for controller epoch %d is missing", epoch)
		}
		name, uid, err := parseRuntimePoolPrivateSecretBinding(binding)
		if err != nil {
			return nil, err
		}
		secret := &corev1.Secret{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
			return nil, fmt.Errorf("read bound private RuntimePool auth Secret: %w", err)
		}
		if secret.UID != uid {
			return nil, fmt.Errorf("bound private RuntimePool auth Secret UID changed")
		}
		cfg := runtimePoolConfig{
			namespace: namespace,
			baseName:  runtimePoolResourceName(pool.Namespace, pool.Name),
			labels: map[string]string{
				runtimePoolManagedByLabel:   runtimePoolManagedByLabelValue,
				runtimePoolApplicationLabel: runtimePoolApplicationLabelValue,
				runtimePoolKeyLabel:         runtimePoolKey(pool.Namespace, pool.Name),
				runtimePoolNameLabel:        pool.Name,
				runtimePoolNamespaceLabel:   pool.Namespace,
				runtimePoolUIDLabel:         string(pool.UID),
				runtimePoolNetworkRoleLabel: "provider-client",
			},
			controllerEpoch: epoch,
		}
		if !runtimePoolPrivateAuthSecretMatchesPool(secret, pool, cfg) {
			return nil, fmt.Errorf("bound private RuntimePool auth Secret does not carry the exact immutable RuntimePool ownership identity")
		}
		return secret.DeepCopy(), nil
	}

	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(namespace), client.MatchingLabels{
		runtimePoolAuthLabel: booleanTrueValue, runtimePoolUIDLabel: string(pool.UID),
	}); err != nil {
		return nil, err
	}
	return runtimePoolAuthSecretForEpoch(secrets.Items, epoch)
}

func runtimePoolAuthSecretForEpoch(secrets []corev1.Secret, epoch int64) (*corev1.Secret, error) {
	matched := runtimePoolAuthSecretsForEpoch(secrets, epoch)
	if len(matched) != 1 {
		return nil, fmt.Errorf("runtime pool requires exactly one auth Secret for controller epoch %d", epoch)
	}
	return &matched[0], nil
}

func runtimePoolAuthSecretsForEpoch(secrets []corev1.Secret, epoch int64) []corev1.Secret {
	epochValue := strconv.FormatInt(epoch, 10)
	legacySuffix := "auth-e" + epochValue
	randomSuffixPrefix := legacySuffix + "-"
	matched := make([]corev1.Secret, 0, 1)
	for i := range secrets {
		secretEpoch := strings.TrimSpace(secrets[i].Labels[runtimePoolCredentialEpochLabel])
		if secretEpoch == epochValue || (secretEpoch == "" &&
			(strings.HasSuffix(secrets[i].Name, legacySuffix) ||
				strings.HasPrefix(runtimePoolAuthSuffixPattern.FindString(secrets[i].Name), randomSuffixPrefix))) {
			matched = append(matched, *secrets[i].DeepCopy())
		}
	}
	return matched
}

func writeACPMCPError(w http.ResponseWriter, status int, message string) {
	writeACPMCPJSON(w, status, map[string]any{errorField: message})
}

func writeACPMCPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
