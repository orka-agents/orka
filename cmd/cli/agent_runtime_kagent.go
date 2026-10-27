/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	kagentProviderKind = "kagent"
	kagentAdapterName  = "kagent-serve-acp"
	// kagentBrokerLabel marks wrapper pods, which kagent's NetworkPolicies
	// admit to the controller and, for v0, to the Agent pods.
	kagentBrokerLabel = "orka.ai/kagent-broker"
	// kagentRuntimeLabel and kagentAgentLabel let a re-pin find each
	// runtime's objects and the runtimes of one Agent.
	kagentRuntimeLabel = "orka.ai/kagent-runtime"
	kagentAgentLabel   = "orka.ai/kagent-agent"
	// kagentRelayLabel names the relay Service a governed runtime serves, which
	// its relay NetworkPolicy selects: one relay's policy never covers the
	// runtimes of another kagent Agent, even one of the same name.
	kagentRelayLabel = "orka.ai/kagent-relay"
	// kagentRuntimeNamespaceLabel marks a v0 runtime's pod reader in the
	// Agent's namespace, which runtimes of other Orka namespaces share.
	kagentRuntimeNamespaceLabel = "orka.ai/kagent-runtime-namespace"
	kagentRelayPort             = 8092
	kagentControlPort           = 8080
	kagentControlPortName       = "control"
	kagentBrokerContainer       = "broker"
	// kagentSupervisorContainer is the container Orka's Kubernetes recovery
	// restarts, the one whose boot it drains.
	kagentSupervisorContainer = "supervisor"
	// kagentBrokerUID is the wrapper image's non-root user.
	kagentBrokerUID        = 65532
	kagentPartWorkload     = "workload"
	kagentPartRelay        = "relay"
	kagentPartRegistration = "registration"
)

var kagentPinnedImage = regexp.MustCompile(`^[^\s@]+@(sha256:[0-9a-f]{64})$`)

// kagentRenderOptions are the inputs that are not in the bundle.
type kagentRenderOptions struct {
	ConfiguredImage string
	RuntimeImage    string
	ControllerEpoch int64
	Namespace       string
	OrkaAPIURL      string
	StorageClass    string
	Part            string
	Capabilities    string
	DeploymentUID   string
	// SubstrateNamespace is where kagent v1's Substrate egress runs.
	SubstrateNamespace string
}

func newAgentRuntimeRenderKagentCmd() *cobra.Command {
	var options kagentRenderOptions
	cmd := &cobra.Command{
		Use:   "render-kagent <bundle>",
		Short: "Render the Kubernetes objects of a wrapped kagent Agent's runtime",
		Long: `Render one part of the runtime that serves a wrapped kagent Agent, from the
deployment bundle "orka agent import kagent --bundle-dir" wrote for it. The
output is a JSON List for kubectl apply.

  workload      The ServiceAccount, broker ledger volume, Deployment, and
                Service of this runtime, plus, for kagent v0, read access to
                the Agent's pods. Apply it, then wait for the Deployment.
  relay         A governed Agent's relay Service and NetworkPolicy. Their name
                is part of the kagent Agent's config, so it is the same for
                every runtime of the Agent; applying it moves the relay to
                this runtime. Apply it before the registration: Orka's
                conformance prompt reaches kagent, which calls the relay.
  registration  The AgentRuntime. It needs --capabilities, the runtime's
                GET /v2/capabilities answer, and --deployment-uid, the UID of
                the runtime's Deployment, and is created once the Deployment
                is ready. It opts into Orka's Kubernetes recovery, which
                drains and restarts the runtime after an Orka controller
                restart: annotate the Deployment with
                orka.ai/agent-runtime-recovery-uid=<the AgentRuntime's UID>.

The Secret <runtime>-auth must exist with the keys controller-token,
capability-secret, and provider-token, and, for a governed Agent,
model-upstream-token. It is never rendered.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, _ := cmd.Flags().GetString(cliNamespaceQuery)
			options.Namespace = strings.TrimSpace(namespace)
			list, err := renderKagentRuntime(args[0], options)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(list)
		},
	}
	cmd.Flags().StringVar(&options.ConfiguredImage, "configured-image", "", "Configured kagent source image, by digest (<repo>@sha256:<digest>); its digest is the profile's adapter digest")
	cmd.Flags().StringVar(&options.RuntimeImage, "runtime-image", "", "Orka kagent runtime image composed from it, by digest (<repo>@sha256:<digest>)")
	cmd.Flags().Int64Var(&options.ControllerEpoch, "controller-epoch", 0, "Current Orka controller epoch (the orka-controller cepoch record)")
	cmd.Flags().StringVar(&options.OrkaAPIURL, "orka-api-url", "", "Orka API base URL the supervisor calls (default http://orka-api.<namespace>.svc:8080)")
	cmd.Flags().StringVar(&options.StorageClass, "storage-class", "", "StorageClass of the broker ledger volume (default: the cluster default)")
	cmd.Flags().StringVar(&options.Part, "part", kagentPartWorkload, "Part to render: workload, relay, or registration")
	cmd.Flags().StringVar(&options.Capabilities, "capabilities", "", "With --part registration, the runtime's GET /v2/capabilities answer (file)")
	cmd.Flags().StringVar(&options.DeploymentUID, "deployment-uid", "", "With --part registration, the UID of the runtime's Deployment, which Orka recovers")
	cmd.Flags().StringVar(&options.SubstrateNamespace, "substrate-namespace", "ate-system", "With --part relay, the namespace of kagent v1's Substrate egress, which carries the Agent's calls to the relay")
	return cmd
}

// kagentRuntimeProfile is the immutable registration input for one runtime.
type kagentRuntimeProfile struct {
	Profile       corev1alpha1.AgentRuntimeProfileSpec
	MCPPolicy     corev1alpha1.AgentRuntimeMCPPolicySpec
	SupervisorEnv map[string]string
}

func readKagentBundle(dir string) (kagentBundle, []byte, error) {
	var bundle kagentBundle
	raw, err := os.ReadFile(filepath.Join(dir, kagentBundleRuntimeFile))
	if err != nil {
		return bundle, nil, fmt.Errorf("reading the bundle: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return bundle, nil, fmt.Errorf("%s is not a deployment bundle: %w", kagentBundleRuntimeFile, err)
	}
	config, err := os.ReadFile(filepath.Join(dir, kagentBundleConfigFile))
	if err != nil {
		return bundle, nil, fmt.Errorf("reading the bundle: %w", err)
	}
	sum := sha256.Sum256(config)
	if digest := "sha256:" + hex.EncodeToString(sum[:]); digest != bundle.ConfigDigest ||
		kagentBundleRuntimeName(bundle.Agent, config, bundle.AllowedTools, bundle.ApprovalRequiredTools, bundle.Revision) != bundle.Runtime {
		return bundle, nil, fmt.Errorf("%s does not match the configuration the bundle pinned; re-run the import", kagentBundleConfigFile)
	}
	if bundle.KagentVersion != kagentVersionV0 && bundle.KagentVersion != "v1" {
		return bundle, nil, fmt.Errorf("the bundle names kagent version %q", bundle.KagentVersion)
	}
	if bundle.Source == "" {
		return bundle, nil, fmt.Errorf("%s names no kagent source; re-run the import", kagentBundleRuntimeFile)
	}
	return bundle, config, nil
}

func renderKagentRuntime(dir string, options kagentRenderOptions) (map[string]any, error) {
	bundle, config, err := readKagentBundle(dir)
	if err != nil {
		return nil, err
	}
	if options.Namespace == "" {
		return nil, fmt.Errorf("--namespace names where the runtime and its AgentRuntime live")
	}
	match := kagentPinnedImage.FindStringSubmatch(options.ConfiguredImage)
	if match == nil || kagentPinnedImage.FindStringSubmatch(options.RuntimeImage) == nil {
		return nil, fmt.Errorf("--configured-image and --runtime-image must be pinned by digest (<repo>@sha256:<digest>)")
	}
	profile, err := kagentRuntimeProfileFor(bundle, config, match[1])
	if err != nil {
		return nil, err
	}
	var items []any
	switch options.Part {
	case kagentPartWorkload:
		if options.ControllerEpoch <= 0 {
			return nil, fmt.Errorf("--controller-epoch must be the current Orka controller epoch")
		}
		if items, err = kagentWorkload(bundle, profile, options); err != nil {
			return nil, err
		}
	case kagentPartRelay:
		if !bundle.Governed {
			return nil, fmt.Errorf("kagent Agent %s/%s is observed; only a governed Agent has a relay", bundle.KagentNamespace, bundle.KagentName)
		}
		if items, err = kagentRelay(bundle, options); err != nil {
			return nil, err
		}
	case kagentPartRegistration:
		registration, err := kagentRegistration(bundle, profile, options)
		if err != nil {
			return nil, err
		}
		items = []any{registration}
	default:
		return nil, fmt.Errorf("--part must be workload, relay, or registration")
	}
	return map[string]any{kagentFieldAPIVersion: "v1", kagentFieldKind: "List", "items": items}, nil
}

// kagentRuntimeProfileFor computes the v2 profile, its MCP policy, and the
// matching supervisor environment. Observed mode brokers no Orka tools;
// governed mode lists the Orka tools the Agent calls through the relay.
func kagentRuntimeProfileFor(bundle kagentBundle, config []byte, adapter string) (kagentRuntimeProfile, error) {
	var configuration struct {
		Model    string          `json:"model"`
		Governed json.RawMessage `json:"governed"`
	}
	if err := json.Unmarshal(config, &configuration); err != nil || configuration.Model == "" || configuration.Model != bundle.Model {
		return kagentRuntimeProfile{}, fmt.Errorf("the bundle's configuration must name its profile model")
	}
	allowed := slices.Clone(bundle.AllowedTools)
	approvalRequired := slices.Clone(bundle.ApprovalRequiredTools)
	slices.Sort(allowed)
	slices.Sort(approvalRequired)
	// The wrapper reads a missing or null governed stanza as observed mode.
	governed := strings.TrimSpace(string(configuration.Governed))
	if (governed != "" && governed[0] == '{') != bundle.Governed {
		return kagentRuntimeProfile{}, fmt.Errorf("the bundle and its configuration disagree on governed mode")
	}
	if len(allowed) != 0 && !bundle.Governed {
		return kagentRuntimeProfile{}, fmt.Errorf("only a governed kagent configuration may list Orka tools")
	}
	for _, tool := range approvalRequired {
		if !slices.Contains(allowed, tool) {
			return kagentRuntimeProfile{}, fmt.Errorf("approval-required tool %q is not allowed", tool)
		}
	}
	// The CRD lists stay explicit, while the approval digest uses the empty
	// policy the controller derives from an empty approvalRequiredTools list.
	disallowed := []string{}
	approval := harnessv2.MCPApprovalPolicy{}
	if len(approvalRequired) != 0 {
		approval.RequiredTools = approvalRequired
	}
	toolDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(allowed, disallowed, false)
	if err != nil {
		return kagentRuntimeProfile{}, err
	}
	approvalDigest, err := harnessv2.CanonicalMCPApprovalPolicyDigest(approval)
	if err != nil {
		return kagentRuntimeProfile{}, err
	}
	mcpDigest, err := harnessv2.CanonicalMCPConfigurationDigest(allowed)
	if err != nil {
		return kagentRuntimeProfile{}, err
	}
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: map[string]string{kagentAdapterName: adapter},
		ProviderKind: kagentProviderKind, Model: configuration.Model, AgentConfigurationDigest: bundle.ConfigDigest,
		ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
		WorkspaceIntent: harnessv2.WorkspaceIntentRead, ProxyCredentialRole: "operator-managed",
		ProxyCredentialScope: "external-runtime", ResourceClass: "external",
	}
	if err := profile.Validate(); err != nil {
		return kagentRuntimeProfile{}, err
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		return kagentRuntimeProfile{}, err
	}
	out := kagentRuntimeProfile{
		Profile: corev1alpha1.AgentRuntimeProfileSpec{
			Digest: string(digest), DigestSchemaVersion: int32(harnessv2.ProfileDigestSchemaVersion),
			ACPProfile: profile.ACPProfile, AdapterName: kagentAdapterName, AdapterDigest: adapter,
			ProviderKind: kagentProviderKind, Model: configuration.Model, AgentConfigurationDigest: bundle.ConfigDigest,
			ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			WorkspaceIntent: corev1alpha1.WorkspaceIntentRead, ProxyCredentialRole: profile.ProxyCredentialRole,
			ProxyCredentialScope: profile.ProxyCredentialScope, ResourceClass: profile.ResourceClass,
		},
		MCPPolicy: corev1alpha1.AgentRuntimeMCPPolicySpec{
			AllowedTools: allowed, DisallowedTools: disallowed, ApprovalRequiredTools: approvalRequired,
		},
		SupervisorEnv: map[string]string{
			"ORKA_ACP_PROVIDER":                   kagentProviderKind,
			"ORKA_ACP_MODEL":                      configuration.Model,
			"ORKA_ACP_WORKSPACE_INTENT":           "read",
			"ORKA_ACP_KAGENT_ADAPTER_DIGEST":      adapter,
			"ORKA_ACP_AGENT_CONFIGURATION_DIGEST": bundle.ConfigDigest,
			"ORKA_ACP_TOOL_POLICY_DIGEST":         toolDigest,
			"ORKA_ACP_APPROVAL_POLICY_DIGEST":     approvalDigest,
			"ORKA_ACP_MCP_CONFIGURATION_DIGEST":   mcpDigest,
			"ORKA_ACP_PROXY_CREDENTIAL_ROLE":      profile.ProxyCredentialRole,
			"ORKA_ACP_PROXY_CREDENTIAL_SCOPE":     profile.ProxyCredentialScope,
			"ORKA_ACP_RESOURCE_CLASS":             profile.ResourceClass,
			// The kagent broker serves the identity and retire-boot recovery
			// endpoints, so the supervisor can retire a restarted boot.
			"ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST": string(digest),
		},
	}
	if len(approvalRequired) != 0 {
		// Approval-gated tools need the supervisor qualified for this exact profile.
		out.SupervisorEnv["ORKA_ACP_BROKERED_TOOL_APPROVAL_PROFILE_DIGEST"] = string(digest)
	}
	return out, nil
}

func kagentEnv(values map[string]string) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		env = append(env, corev1.EnvVar{Name: name, Value: values[name]})
	}
	return env
}

func kagentSecurityContext(uid int64, add ...corev1.Capability) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: new(uid), RunAsGroup: new(uid), RunAsNonRoot: new(uid != 0),
		AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true), Privileged: new(false),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: add},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// kagentWithProbes sets the startup, readiness, and liveness probes of a
// container from one handler.
func kagentWithProbes(container *corev1.Container, handler corev1.ProbeHandler) {
	probe := func(failures, period int32) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: handler, FailureThreshold: failures, PeriodSeconds: period, TimeoutSeconds: 3}
	}
	container.StartupProbe, container.ReadinessProbe, container.LivenessProbe = probe(60, 2), probe(3, 5), probe(3, 10)
}

func kagentResources(cpuRequest, memoryRequest, cpuLimit, memoryLimit string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuRequest), corev1.ResourceMemory: resource.MustParse(memoryRequest)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuLimit), corev1.ResourceMemory: resource.MustParse(memoryLimit)},
	}
}

// kagentMeta names an object of a runtime. Its annotation names the kagent
// Agent the runtime wraps: kagent Agents of one name in different namespaces
// or API versions map to one Orka Agent, so a retirement must tell their
// runtimes apart.
func kagentMeta(bundle kagentBundle, name, namespace string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels,
		Annotations: map[string]string{kagentSourceAnnotation: bundle.Source}}
}

func kagentSecretVolume(name, secret string, keys ...string) corev1.Volume {
	items := make([]corev1.KeyToPath, 0, len(keys))
	for _, key := range keys {
		items = append(items, corev1.KeyToPath{Key: key, Path: key})
	}
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: secret, DefaultMode: ptr.To[int32](0o400), Items: items}}}
}

func kagentEmptyDir(name, size string) corev1.Volume {
	limit := resource.MustParse(size)
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}}}
}

func kagentSecretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}}}
}

// kagentWorkload renders the runtime's own objects. The pod runs exactly the
// supervisor and broker containers, mounts no service account token except
// v0's into the broker, and keeps the ledger on a broker-only volume.
func kagentWorkload(bundle kagentBundle, profile kagentRuntimeProfile, options kagentRenderOptions) ([]any, error) {
	name, namespace := bundle.Runtime, options.Namespace
	secret, account, claim := name+"-auth", name+"-broker", name+"-ledger"
	orkaAPI := options.OrkaAPIURL
	if orkaAPI == "" {
		orkaAPI = "http://orka-api." + namespace + ".svc:8080"
	}
	labels := map[string]string{kagentRuntimeLabel: name, kagentAgentLabel: bundle.Agent}
	podLabels := map[string]string{kagentRuntimeLabel: name, kagentAgentLabel: bundle.Agent, kagentBrokerLabel: "true"}
	if bundle.Governed {
		relay, _, _, err := kagentRelayService(bundle.Relay)
		if err != nil {
			return nil, err
		}
		podLabels[kagentRelayLabel] = relay
	}
	values := maps.Clone(profile.SupervisorEnv)
	maps.Copy(values, map[string]string{
		"ORKA_ACP_LISTEN_ADDRESS":               ":" + strconv.Itoa(kagentControlPort),
		"ORKA_ACP_CONTROLLER_EPOCH":             strconv.FormatInt(options.ControllerEpoch, 10),
		"ORKA_ACP_RUNTIME_INSTANCE_ID":          name + "-instance",
		"ORKA_ACP_RUNTIME_POOL_UID":             name + "-external-pool",
		"ORKA_ACP_RUNTIME_POOL_GENERATION":      "1",
		"ORKA_ACP_CONTROLLER_TOKEN_FILE":        "/var/run/secrets/orka/auth/controller-token",
		"ORKA_ACP_CAPABILITY_SECRET_FILE":       "/var/run/secrets/orka/auth/capability-secret",
		"ORKA_ACP_PROVIDER_TOKEN_FILE":          "/var/run/secrets/orka/provider/provider-token",
		"ORKA_ACP_PROVIDER_PROXY_BASE_URL":      "http://127.0.0.1:8091/v1",
		"ORKA_ACP_ARTIFACT_API_URL":             orkaAPI,
		"ORKA_ACP_MCP_BROKER_URL":               orkaAPI,
		"ORKA_ACP_WORKSPACE_MAX_ARTIFACT_BYTES": "104857600",
		"ORKA_ACP_TRUST_NAMESPACE":              namespace,
		"ORKA_ACP_SESSION_BASE_DIR":             "/sessions",
	})
	supervisor := corev1.Container{
		Name: kagentSupervisorContainer, Image: options.RuntimeImage, ImagePullPolicy: corev1.PullIfNotPresent, Env: kagentEnv(values),
		Ports:           []corev1.ContainerPort{{Name: kagentControlPortName, ContainerPort: kagentControlPort}},
		SecurityContext: kagentSecurityContext(0, "CHOWN", "KILL", "SETGID", "SETUID"),
		Resources:       kagentResources("100m", "256Mi", "2", "2Gi"),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "supervisor-auth", MountPath: "/var/run/secrets/orka/auth", ReadOnly: true},
			{Name: "supervisor-provider", MountPath: "/var/run/secrets/orka/provider", ReadOnly: true},
			{Name: "supervisor-sessions", MountPath: "/sessions"}, {Name: "supervisor-tmp", MountPath: "/tmp"},
			{Name: "supervisor-home", MountPath: "/home/worker"},
		},
	}
	for _, field := range []string{"uid", cliNameKey, cliNamespaceQuery} {
		supervisor.Env = append(supervisor.Env, corev1.EnvVar{Name: "ORKA_ACP_POD_" + strings.ToUpper(field),
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata." + field}}})
	}
	kagentWithProbes(&supervisor, corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/v2/health", Port: intstr.FromString(kagentControlPortName)}})
	broker := corev1.Container{
		Name: kagentBrokerContainer, Image: options.ConfiguredImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{"--protocol", kagentBrokerContainer, "--config", "/agent/kagent.json"},
		Env: append(kagentEnv(map[string]string{
			"ORKA_KAGENT_BROKER_ADDR":                    "127.0.0.1:8091",
			"ORKA_KAGENT_BROKER_STATE_DIR":               "/broker-state/ledger",
			"ORKA_KAGENT_ACP_MODEL":                      bundle.Model,
			"ORKA_KAGENT_ACP_AGENT_CONFIGURATION_DIGEST": bundle.ConfigDigest,
		}), kagentSecretEnv("ORKA_KAGENT_BROKER_BEARER_TOKEN", secret, "provider-token")),
		SecurityContext: kagentSecurityContext(kagentBrokerUID),
		Resources:       kagentResources("50m", "64Mi", "1", "256Mi"),
		VolumeMounts:    []corev1.VolumeMount{{Name: "broker-state", MountPath: "/broker-state"}},
	}
	kagentWithProbes(&broker, corev1.ProbeHandler{Exec: &corev1.ExecAction{
		Command: []string{"/orka-runtime-kagent", "--protocol", kagentBrokerContainer, "--health-check"}}})
	if bundle.Governed {
		broker.Env = append(broker.Env, corev1.EnvVar{Name: "ORKA_KAGENT_RELAY_ADDR", Value: ":" + strconv.Itoa(kagentRelayPort)},
			kagentSecretEnv("ORKA_KAGENT_MODEL_UPSTREAM_TOKEN", secret, "model-upstream-token"))
		broker.Ports = []corev1.ContainerPort{{Name: kagentPartRelay, ContainerPort: kagentRelayPort}}
	}
	volumes := []corev1.Volume{
		kagentSecretVolume("supervisor-auth", secret, "controller-token", "capability-secret"),
		kagentSecretVolume("supervisor-provider", secret, "provider-token"),
		kagentEmptyDir("supervisor-sessions", "4Gi"), kagentEmptyDir("supervisor-tmp", "512Mi"), kagentEmptyDir("supervisor-home", "256Mi"),
		{Name: "broker-state", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}},
	}
	items := []any{&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: kagentMeta(bundle, account, namespace, labels), AutomountServiceAccountToken: new(false)}}
	if bundle.KagentVersion == kagentVersionV0 {
		// The v0 broker pins the Agent's pod, so it alone reads pods in the
		// Agent's namespace with its pod's service account.
		volumes = append(volumes, corev1.Volume{Name: "broker-kube-api", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o444),
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To[int64](3600)}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
					Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			}}}})
		broker.VolumeMounts = append(broker.VolumeMounts, corev1.VolumeMount{Name: "broker-kube-api",
			MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
		// The same bundle may run in several Orka namespaces, so the pod
		// reader's name and labels name the runtime's namespace too. Neither
		// name contains a dot.
		role := name + "." + namespace + ".agent-pods"
		readerLabels := maps.Clone(labels)
		readerLabels[kagentRuntimeNamespaceLabel] = namespace
		items = append(items,
			&rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
				ObjectMeta: kagentMeta(bundle, role, bundle.KagentNamespace, readerLabels),
				Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}}},
			&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
				ObjectMeta: kagentMeta(bundle, role, bundle.KagentNamespace, readerLabels),
				RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: namespace}}},
		)
	}
	pvc := &corev1.PersistentVolumeClaim{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: kagentMeta(bundle, claim, namespace, labels),
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	if options.StorageClass != "" {
		pvc.Spec.StorageClassName = new(options.StorageClass)
	}
	selector := map[string]string{kagentRuntimeLabel: name}
	deployment := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: kagentMeta(bundle, name, namespace, labels),
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			RevisionHistoryLimit: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: podLabels}, Spec: corev1.PodSpec{
				ServiceAccountName: account, AutomountServiceAccountToken: new(false), EnableServiceLinks: new(false),
				ShareProcessNamespace: new(false),
				// The broker, a non-root user, creates its ledger on the volume;
				// fsGroup makes a root-owned volume writable to it. ACP children
				// start with their supplementary groups cleared.
				SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					FSGroup: new(int64(kagentBrokerUID)), FSGroupChangePolicy: new(corev1.FSGroupChangeOnRootMismatch)},
				TerminationGracePeriodSeconds: ptr.To[int64](120), Containers: []corev1.Container{supervisor, broker}, Volumes: volumes,
			}}}}
	service := &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: kagentMeta(bundle, name, namespace, labels),
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: selector,
			Ports: []corev1.ServicePort{{Name: kagentControlPortName, Port: kagentControlPort, TargetPort: intstr.FromString(kagentControlPortName)}}}}
	// The broker ledger must survive pod restarts, or a restart drops every
	// session it owns. Each runtime has its own ledger.
	return append(items, pvc, deployment, service), nil
}

// kagentRelay renders a governed Agent's relay Service, the one its pinned
// config names, selecting this runtime's pod, and the NetworkPolicy that
// admits only the kagent Agent's traffic to the relay.
func kagentRelay(bundle kagentBundle, options kagentRenderOptions) ([]any, error) {
	name, namespace, servicePort, err := kagentRelayService(bundle.Relay)
	if err != nil {
		return nil, err
	}
	if namespace != options.Namespace {
		return nil, fmt.Errorf("the Agent's relay %s is in namespace %q; render the runtime there", bundle.Relay, namespace)
	}
	labels := map[string]string{kagentAgentLabel: bundle.Agent}
	meta := kagentMeta(bundle, name, options.Namespace, labels)
	tcp := corev1.ProtocolTCP
	port := func(n int) []networkingv1.NetworkPolicyPort {
		return []networkingv1.NetworkPolicyPort{{Port: new(intstr.FromInt(n)), Protocol: &tcp}}
	}
	return []any{
		&corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: meta,
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{kagentRuntimeLabel: bundle.Runtime},
				Ports: []corev1.ServicePort{{Name: kagentPartRelay, Port: int32(servicePort), TargetPort: intstr.FromString(kagentPartRelay)}}}},
		&networkingv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
			ObjectMeta: meta,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{kagentRelayLabel: name}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					// Orka's controller reaches every runtime's control port.
					{Ports: port(kagentControlPort)},
					{From: []networkingv1.NetworkPolicyPeer{kagentRelayPeer(bundle, options)}, Ports: port(kagentRelayPort)},
				},
			}},
	}, nil
}

// kagentRelayPeer selects what calls the relay for the kagent Agent: v0's
// Agent pods themselves; for v1, Substrate's egress, which carries every
// actor's outbound calls.
func kagentRelayPeer(bundle kagentBundle, options kagentRenderOptions) networkingv1.NetworkPolicyPeer {
	namespace, pods := bundle.KagentNamespace, map[string]string{"app": kagentV0AppLabel, kagentV0AppLabel: bundle.KagentName}
	if bundle.KagentVersion != kagentVersionV0 {
		namespace, pods = cmp.Or(options.SubstrateNamespace, "ate-system"), map[string]string{"app": "atenet-egress"}
	}
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: pods},
	}
}

// kagentRegistration renders the AgentRuntime once the runtime answers.
func kagentRegistration(bundle kagentBundle, profile kagentRuntimeProfile, options kagentRenderOptions) (*corev1alpha1.AgentRuntime, error) {
	if options.Capabilities == "" {
		return nil, fmt.Errorf("--part registration needs --capabilities, the runtime's GET /v2/capabilities answer")
	}
	file, err := os.Open(options.Capabilities)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return nil, err
	}
	var capabilities struct {
		RuntimeProfileDigest            string                                                    `json:"runtimeProfileDigest"`
		Limits                          *corev1alpha1.AgentRuntimeProtocolLimits                  `json:"limits"`
		SupportsDrain                   bool                                                      `json:"supportsDrain"`
		SupportsPublicationFinalization bool                                                      `json:"supportsPublicationFinalization"`
		WorkspaceGovernance             *corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities `json:"workspaceGovernance"`
	}
	if err := json.Unmarshal(raw, &capabilities); err != nil {
		return nil, fmt.Errorf("--capabilities is not a capabilities answer: %w", err)
	}
	if capabilities.RuntimeProfileDigest != profile.Profile.Digest {
		return nil, fmt.Errorf("the runtime serves profile %q, not the rendered %s; it runs other images or another configuration", capabilities.RuntimeProfileDigest, profile.Profile.Digest)
	}
	if capabilities.Limits == nil || capabilities.WorkspaceGovernance == nil {
		return nil, fmt.Errorf("the runtime reports no limits or workspaceGovernance")
	}
	if options.DeploymentUID == "" {
		return nil, fmt.Errorf("--part registration needs --deployment-uid, the UID of the runtime's Deployment")
	}
	name, namespace := bundle.Runtime, options.Namespace
	contract := corev1alpha1.AgentRuntimeContractHarnessV2
	return &corev1alpha1.AgentRuntime{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "AgentRuntime"},
		ObjectMeta: kagentMeta(bundle, name, namespace, map[string]string{kagentRuntimeLabel: name, kagentAgentLabel: bundle.Agent}),
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: &contract,
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint,
				Endpoint: "http://" + name + "." + namespace + ".svc.cluster.local:" + strconv.Itoa(kagentControlPort),
				// The supervisor reads Orka's controller epoch once, at start.
				// Recovery lets Orka drain it and restart it with the new
				// epoch after the controller restarts, which a runtime
				// outside it would wait for by hand.
				KubernetesRecovery: &corev1alpha1.AgentRuntimeKubernetesRecoverySpec{
					DeploymentName: name, DeploymentUID: options.DeploymentUID, ContainerName: kagentSupervisorContainer,
				}},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{
				ControllerBearerTokenSecretRef: &corev1alpha1.AgentRuntimeSecretKeyReference{Name: name + "-auth", Key: "controller-token"},
				OperationCapabilitySecretRef:   &corev1alpha1.AgentRuntimeSecretKeyReference{Name: name + "-auth", Key: "capability-secret"},
			},
			Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
				RuntimeInstanceID: name + "-instance", Profile: &profile.Profile, MCPPolicy: &profile.MCPPolicy,
				Limits: capabilities.Limits, SupportsDrain: capabilities.SupportsDrain,
				SupportsPublicationFinalization: capabilities.SupportsPublicationFinalization,
				WorkspaceGovernance:             capabilities.WorkspaceGovernance,
			},
		},
	}, nil
}
