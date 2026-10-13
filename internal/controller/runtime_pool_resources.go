package controller

import (
	"encoding/json"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func (r *RuntimePoolReconciler) runtimePoolEnvironment(pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) []corev1.EnvVar {
	adapterDigestsJSON, _ := json.Marshal(cfg.profile.AdapterDigests)
	modelContextLimit := ""
	modelOutputLimit := ""
	if cfg.profile.ModelLimits != nil {
		modelContextLimit = strconv.FormatInt(cfg.profile.ModelLimits.Context, 10)
		modelOutputLimit = strconv.FormatInt(cfg.profile.ModelLimits.Output, 10)
	}
	nativeSessionMaxBytes := cfg.nativeSessionMaxBytes
	if nativeSessionMaxBytes == 0 {
		nativeSessionMaxBytes = harnessv2.DefaultMaxNativeSessionBytes
	}
	return []corev1.EnvVar{
		{Name: "ORKA_ACP_LISTEN_ADDRESS", Value: ":8080"},
		{Name: "ORKA_ACP_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "ORKA_ACP_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "ORKA_ACP_POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "ORKA_ACP_CONTROLLER_EPOCH", Value: strconv.FormatInt(cfg.controllerEpoch, 10)},
		{Name: "ORKA_ACP_RUNTIME_POOL_UID", Value: string(pool.UID)},
		{Name: "ORKA_ACP_RUNTIME_POOL_GENERATION", Value: strconv.FormatInt(pool.Generation, 10)},
		{Name: "ORKA_ACP_RUNTIME_PROFILE_DIGEST", Value: pool.Spec.Runtime.Profile.Digest},
		{Name: "ORKA_ACP_PROFILE_DIGEST_SCHEMA_VERSION", Value: pool.Spec.Runtime.Profile.DigestSchemaVersion},
		{Name: "ORKA_ACP_ACP_PROFILE", Value: cfg.profile.ACPProfile},
		{Name: "ORKA_ACP_ADAPTER_DIGESTS_JSON", Value: string(adapterDigestsJSON)},
		{Name: "ORKA_ACP_PROVIDER", Value: cfg.profile.ProviderKind},
		{Name: "ORKA_ACP_PROVIDER_PROXY_BASE_URL", Value: cfg.providerProxy.baseURL},
		{Name: "ORKA_ACP_MODEL", Value: cfg.profile.Model},
		{Name: "ORKA_ACP_MODEL_CONTEXT_LIMIT", Value: modelContextLimit},
		{Name: "ORKA_ACP_MODEL_OUTPUT_LIMIT", Value: modelOutputLimit},
		{Name: "ORKA_ACP_WORKSPACE_INTENT", Value: string(cfg.profile.WorkspaceIntent)},
		{Name: "ORKA_ACP_AGENT_CONFIGURATION_DIGEST", Value: cfg.profile.AgentConfigurationDigest},
		{Name: "ORKA_ACP_TOOL_POLICY_DIGEST", Value: cfg.profile.ToolPolicyDigest},
		{Name: "ORKA_ACP_APPROVAL_POLICY_DIGEST", Value: cfg.profile.ApprovalPolicyDigest},
		{Name: "ORKA_ACP_MCP_CONFIGURATION_DIGEST", Value: cfg.profile.MCPConfigurationDigest},
		{Name: "ORKA_ACP_PROXY_CREDENTIAL_ROLE", Value: cfg.profile.ProxyCredentialRole},
		{Name: "ORKA_ACP_PROXY_CREDENTIAL_SCOPE", Value: cfg.profile.ProxyCredentialScope},
		{Name: "ORKA_ACP_RESOURCE_CLASS", Value: cfg.profile.ResourceClass},
		{Name: runtimePoolControllerTokenFileEnv, Value: runtimePoolControllerTokenPath},
		{Name: runtimePoolCapabilitySecretFileEnv, Value: runtimePoolCapabilitySecretPath},
		{Name: runtimePoolProviderTokenFileEnv, Value: runtimePoolProviderTokenPath},
		{Name: "ORKA_ACP_PROVIDER_TOKEN_GENERATION", Value: cfg.providerProxy.tokenGeneration},
		{Name: "ORKA_ACP_ARTIFACT_API_URL", Value: strings.TrimRight(r.ControllerAPIURL, "/")},
		{Name: "ORKA_ACP_WORKSPACE_MAX_ARTIFACT_BYTES", Value: strconv.FormatInt(r.WorkspaceArtifactMaxBytes, 10)},
		{Name: "ORKA_ACP_MCP_BROKER_URL", Value: strings.TrimRight(r.ControllerAPIURL, "/")},
		{Name: "ORKA_ACP_TRUST_NAMESPACE", Value: pool.Spec.TrustDomain.Namespace},
		{Name: "ORKA_ACP_SESSION_BASE_DIR", Value: "/sessions"},
		{Name: "ORKA_NATIVE_SESSION_MAX_BYTES", Value: strconv.Itoa(nativeSessionMaxBytes)},
	}
}
