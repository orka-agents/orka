package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

func TestBrokeredDelegateTaskSubjectTokenResolverUsesOwnedIncomingSecret(t *testing.T) {
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "parent", Namespace: "team-a", UID: types.UID("parent-uid"),
		Annotations: map[string]string{labels.AnnotationTransactionTokenSecret: "parent-token"},
	}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent-token", Namespace: parent.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind, Name: parent.Name, UID: parent.UID,
			}},
		},
		Data: map[string][]byte{"token": []byte(" request-scoped-token ")},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	resolver := newBrokeredDelegateTaskSubjectTokenResolver(reader, "")

	token, err := resolver(context.Background(), parent, contexttoken.TTSTokenSourceIncoming)
	if err != nil {
		t.Fatalf("resolve incoming subject token: %v", err)
	}
	if token != "request-scoped-token" {
		t.Fatalf("resolved token = %q, want request-scoped token", token)
	}
}

func TestControllerHolderIDIsProcessIncarnationUnique(t *testing.T) {
	first := controllerHolderIDForIncarnation("workstation", "first-process")
	second := controllerHolderIDForIncarnation("workstation", "second-process")
	if first == second {
		t.Fatalf("holder IDs for distinct process incarnations match: %q", first)
	}
	if first == "workstation" || second == "workstation" {
		t.Fatal("holder ID omitted its process incarnation")
	}
}

func TestValidateEnabledSubstrateConfigSelectsActivePathRequirements(t *testing.T) {
	cfg := controller.SubstrateConfig{
		APIEndpoint:           "api.ate-system.svc:443",
		APIInsecureSkipVerify: true,
		RouterURL:             "http://atenet-router.ate-system.svc",
		ActorDNSSuffix:        "actors.resources.substrate.ate.dev",
	}
	if err := validateEnabledSubstrateConfig(cfg, false); err != nil {
		t.Fatalf("ACP-only Substrate configuration rejected: %v", err)
	}
	if err := validateEnabledSubstrateConfig(cfg, true); err == nil ||
		!strings.Contains(err.Error(), "bootstrap token secret name") {
		t.Fatalf("legacy workspace-provider configuration error = %v, want bootstrap Secret requirement", err)
	}
}

func TestValidateStaticTrustedServiceReferences(t *testing.T) {
	sameNamespace, err := outboundaccess.ParseTrustedServiceReferences("team-a/gateway:8443")
	if err != nil {
		t.Fatal(err)
	}
	crossNamespace, err := outboundaccess.ParseTrustedServiceReferences("shared/gateway:8443")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStaticTrustedServiceReferences("team-a", outboundaccess.TrustConfig{
		Gateways: sameNamespace,
	}); err != nil {
		t.Fatalf("same-namespace trust rejected: %v", err)
	}
	if err := validateStaticTrustedServiceReferences("team-a", outboundaccess.TrustConfig{
		TokenEndpoints: crossNamespace,
	}); err == nil || !strings.Contains(err.Error(), `must be in controller watch namespace "team-a"`) {
		t.Fatalf("cross-namespace trust error = %v", err)
	}
}

func TestCurrentControllerHolderIDPreservesExplicitOverride(t *testing.T) {
	t.Setenv("ORKA_CONTROLLER_HOLDER_ID", " explicit-controller ")
	if got := currentControllerHolderID(); got != "explicit-controller" {
		t.Fatalf("currentControllerHolderID() = %q, want explicit-controller", got)
	}
}

func TestBrokeredDelegateTaskSubjectTokenResolverRejectsUnownedIncomingSecret(t *testing.T) {
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "parent", Namespace: "team-a", UID: types.UID("parent-uid"),
		Annotations: map[string]string{labels.AnnotationTransactionTokenSecret: "other-token"},
	}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-token", Namespace: parent.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind, Name: "other", UID: types.UID("other-uid"),
			}},
		},
		Data: map[string][]byte{"token": []byte("must-not-be-used")},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	resolver := newBrokeredDelegateTaskSubjectTokenResolver(reader, "")

	_, err := resolver(context.Background(), parent, contexttoken.TTSTokenSourceIncoming)
	if err == nil || !strings.Contains(err.Error(), "not owned by the parent Task") {
		t.Fatalf("resolve incoming subject token error = %v, want owner rejection", err)
	}
}

func TestBrokeredDelegateTaskSubjectTokenResolverReadsControllerServiceAccountPerRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service-account-token")
	if err := os.WriteFile(path, []byte(" controller-service-account-token "), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := newBrokeredDelegateTaskSubjectTokenResolver(nil, path)

	token, err := resolver(context.Background(), &corev1alpha1.Task{}, contexttoken.TTSTokenSourceServiceAccount)
	if err != nil {
		t.Fatalf("resolve service account subject token: %v", err)
	}
	if token != "controller-service-account-token" {
		t.Fatalf("resolved service account token = %q", token)
	}
}

func TestWorkspacePublisherClientFromEnvUsesBoundedFallbackWithoutPublisher(t *testing.T) {
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_URL", "")
	t.Setenv("ORKA_ACP_ARTIFACT_CAPABILITY_SECRET_FILE", "")

	publisherClient, artifactSecret, gotLimit, err := workspacePublisherClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if publisherClient != nil {
		t.Fatal("Workspace/Publisher client is non-nil")
	}
	if len(artifactSecret) != 0 {
		t.Fatalf("artifact capability secret length = %d, want 0", len(artifactSecret))
	}
	if gotLimit != artifactcap.DefaultWorkspaceArtifactMaxBytes {
		t.Fatalf("workspace artifact fallback = %d, want %d", gotLimit, artifactcap.DefaultWorkspaceArtifactMaxBytes)
	}
}

func TestWorkspacePublisherClientFromEnvNegotiatesWorkspaceArtifactLimit(t *testing.T) {
	const wantLimit = int64(192 << 20)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != publisherservice.CapabilitiesPath {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(publisherservice.CapabilitiesResponse{
			Protocol: publisherservice.ProtocolVersion,
			Limits: publisherservice.CapabilityLimits{
				MaxWorkspaceArtifactBytes: wantLimit,
			},
		}); err != nil {
			t.Errorf("encode capabilities: %v", err)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	bearerPath := filepath.Join(dir, "controller-token")
	capabilityPath := filepath.Join(dir, "capability-secret")
	if err := os.WriteFile(bearerPath, []byte("controller-token-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capabilityPath, []byte("capability-secret-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_URL", server.URL)
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_CONTROLLER_TOKEN_FILE", bearerPath)
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_CAPABILITY_SECRET_FILE", capabilityPath)
	t.Setenv("ORKA_ACP_ARTIFACT_CAPABILITY_SECRET_FILE", "")

	publisherClient, artifactSecret, gotLimit, err := workspacePublisherClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if publisherClient == nil {
		t.Fatal("Workspace/Publisher client is nil")
	}
	if len(artifactSecret) != 0 {
		t.Fatalf("artifact capability secret length = %d, want 0", len(artifactSecret))
	}
	if gotLimit != wantLimit {
		t.Fatalf("workspace artifact limit = %d, want %d", gotLimit, wantLimit)
	}
}

func TestWorkspacePublisherClientFromEnvRejectsInvalidArtifactCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(publisherservice.CapabilitiesResponse{
			Protocol: publisherservice.ProtocolVersion,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	bearerPath := filepath.Join(dir, "controller-token")
	capabilityPath := filepath.Join(dir, "capability-secret")
	if err := os.WriteFile(bearerPath, []byte("controller-token-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capabilityPath, []byte("capability-secret-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_URL", server.URL)
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_CONTROLLER_TOKEN_FILE", bearerPath)
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_CAPABILITY_SECRET_FILE", capabilityPath)
	t.Setenv("ORKA_ACP_ARTIFACT_CAPABILITY_SECRET_FILE", "")

	if _, _, _, err := workspacePublisherClientFromEnv(); err == nil {
		t.Fatal("workspacePublisherClientFromEnv() error = nil, want invalid Publisher limit")
	}
}

func TestACPControlNamespace(t *testing.T) {
	tests := []struct {
		name                string
		runtimeEnabled      bool
		controllerNamespace string
		want                string
		wantErr             bool
	}{
		{
			name: "disabled runtime does not require controller namespace",
		},
		{
			name:                "disabled runtime keeps discovered controller namespace for cleanup",
			controllerNamespace: "orka-system",
			want:                "orka-system",
		},
		{
			name:           "enabled runtime fails closed without controller namespace",
			runtimeEnabled: true,
			wantErr:        true,
		},
		{
			name:                "enabled runtime rejects blank controller namespace",
			runtimeEnabled:      true,
			controllerNamespace: "  ",
			wantErr:             true,
		},
		{
			name:                "enabled runtime uses controller namespace",
			runtimeEnabled:      true,
			controllerNamespace: " orka-system ",
			want:                "orka-system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := acpControlNamespace(tt.runtimeEnabled, tt.controllerNamespace)
			if (err != nil) != tt.wantErr {
				t.Fatalf("acpControlNamespace() error = %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("acpControlNamespace() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestACPArtifactRetentionWiring(t *testing.T) {
	tests := []struct {
		name                    string
		runtimeEnabled          bool
		wantCollector           bool
		wantRuntimeReservations bool
	}{
		{
			name: "harness v1 has no ACP artifact wiring",
		},
		{
			name:                    "enabled runtime exposes reservation recorder",
			runtimeEnabled:          true,
			wantCollector:           true,
			wantRuntimeReservations: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wiring, err := newACPArtifactRetentionWiring(
				tt.runtimeEnabled,
				filepath.Join(t.TempDir(), "artifacts"),
			)
			if err != nil {
				t.Fatalf("newACPArtifactRetentionWiring() error = %v", err)
			}
			if got := wiring.collector != nil; got != tt.wantCollector {
				t.Fatalf("collector present = %t, want %t", got, tt.wantCollector)
			}
			if wiring.collector == nil {
				if wiring.taskCleanup != nil || wiring.runtimeReservations != nil {
					t.Fatal("disabled ACP artifact wiring retained active components")
				}
				return
			}
			if wiring.taskCleanup != wiring.collector {
				t.Fatal("Task cleanup retirer does not preserve the collector")
			}
			if got := wiring.runtimeReservations != nil; got != tt.wantRuntimeReservations {
				t.Fatalf("runtime reservation recorder present = %t, want %t", got, tt.wantRuntimeReservations)
			}
			if wiring.runtimeReservations != nil && wiring.runtimeReservations != wiring.collector {
				t.Fatal("runtime reservation recorder does not preserve the collector")
			}
			if !wiring.collector.NeedLeaderElection() {
				t.Fatal("collector must remain a leader-elected cleanup runnable")
			}
		})
	}
}

func TestACPArtifactRetentionWiringFailsClosedForUnsafeV2Root(t *testing.T) {
	if _, err := newACPArtifactRetentionWiring(true, "relative/artifacts"); err == nil {
		t.Fatal("newACPArtifactRetentionWiring() error = nil, want unsafe-root error")
	}
}

func TestACPControlStoreWiring(t *testing.T) {
	tests := []struct {
		name            string
		runtimeEnabled  bool
		withStore       bool
		wantTaskCleanup bool
		wantRuntime     bool
		wantErr         bool
	}{
		{
			name: "disabled runtime without controller namespace has no control store",
		},
		{
			name:      "harness v1 does not receive ACP cleanup wiring",
			withStore: true,
		},
		{
			name:            "enabled runtime shares store with Task cleanup",
			runtimeEnabled:  true,
			withStore:       true,
			wantTaskCleanup: true,
			wantRuntime:     true,
		},
		{
			name:           "enabled runtime fails closed without control store",
			runtimeEnabled: true,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var kubeControlStore *storekube.Store
			if tt.withStore {
				kubeControlStore = &storekube.Store{}
			}

			wiring, err := newACPControlStoreWiring(tt.runtimeEnabled, kubeControlStore)
			if (err != nil) != tt.wantErr {
				t.Fatalf("newACPControlStoreWiring() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got := wiring.taskCleanup; (got != nil) != tt.wantTaskCleanup {
				t.Fatalf("task cleanup store present = %t, want %t", got != nil, tt.wantTaskCleanup)
			} else if got != nil && got != kubeControlStore {
				t.Fatal("task cleanup store does not preserve the Kubernetes store")
			}
			if got := wiring.runtime; (got != nil) != tt.wantRuntime {
				t.Fatalf("runtime store present = %t, want %t", got != nil, tt.wantRuntime)
			} else if got != nil && got != kubeControlStore {
				t.Fatal("runtime store does not preserve the Kubernetes store")
			}
		})
	}
}

func TestManagerCacheOptions(t *testing.T) {
	childTypes := []client.Object{
		&appsv1.Deployment{},
		&appsv1.ReplicaSet{},
		&corev1.Pod{},
		&corev1.Service{},
		&corev1.Secret{},
		&networkingv1.NetworkPolicy{},
		&policyv1.PodDisruptionBudget{},
	}
	tests := []struct {
		name               string
		watchNamespace     string
		runtimeNamespace   string
		wantDefault        []string
		wantRuntimeChild   []string
		wantChildOverrides bool
	}{
		{
			name:             "cluster-wide watch is unrestricted",
			runtimeNamespace: "orka-runtimes",
		},
		{
			name:               "tenant defaults and distinct runtime child namespace",
			watchNamespace:     "tenant-a",
			runtimeNamespace:   "orka-runtimes",
			wantDefault:        []string{"tenant-a"},
			wantRuntimeChild:   []string{"orka-runtimes", "tenant-a"},
			wantChildOverrides: true,
		},
		{
			name:               "identical tenant and runtime namespaces are deduplicated",
			watchNamespace:     "tenant-a",
			runtimeNamespace:   "tenant-a",
			wantDefault:        []string{"tenant-a"},
			wantRuntimeChild:   []string{"tenant-a"},
			wantChildOverrides: true,
		},
		{
			name:               "v2 runtime children use the isolated runtime namespace",
			watchNamespace:     "tenant-a",
			runtimeNamespace:   "orka-runtimes",
			wantDefault:        []string{"tenant-a"},
			wantRuntimeChild:   []string{"orka-runtimes", "tenant-a"},
			wantChildOverrides: true,
		},
		{
			name:             "blank runtime namespace keeps tenant defaults",
			watchNamespace:   "tenant-a",
			runtimeNamespace: " ",
			wantDefault:      []string{"tenant-a"},
			wantRuntimeChild: []string{"tenant-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := managerCacheOptions(tt.watchNamespace, tt.runtimeNamespace)
			assertCacheNamespaces(t, options.DefaultNamespaces, tt.wantDefault)

			for _, object := range []client.Object{
				&corev1alpha1.Task{},
				&corev1alpha1.Agent{},
				&corev1.ConfigMap{},
			} {
				if _, ok := cacheByObjectForType(options, object); ok {
					t.Fatalf("default-cached object %T unexpectedly has a ByObject override", object)
				}
				assertCacheNamespaces(t, effectiveCacheNamespaces(options, object), tt.wantDefault)
			}

			wantOverrides := 0
			if tt.wantChildOverrides {
				wantOverrides += len(childTypes)
			}
			if got := len(options.ByObject); got != wantOverrides {
				t.Fatalf("ByObject override count = %d, want %d", got, wantOverrides)
			}
			for _, object := range childTypes {
				_, overridden := cacheByObjectForType(options, object)
				if overridden != tt.wantChildOverrides {
					t.Fatalf("ByObject override for %T = %t, want %t", object, overridden, tt.wantChildOverrides)
				}
				assertCacheNamespaces(t, effectiveCacheNamespaces(options, object), tt.wantRuntimeChild)
			}
		})
	}
}

func effectiveCacheNamespaces(options cache.Options, object client.Object) map[string]cache.Config {
	if byObject, ok := cacheByObjectForType(options, object); ok {
		return byObject.Namespaces
	}
	return options.DefaultNamespaces
}

func cacheByObjectForType(options cache.Options, object client.Object) (cache.ByObject, bool) {
	objectType := reflect.TypeOf(object)
	for candidate, byObject := range options.ByObject {
		if reflect.TypeOf(candidate) == objectType {
			return byObject, true
		}
	}
	return cache.ByObject{}, false
}

func assertCacheNamespaces(t *testing.T, namespaces map[string]cache.Config, want []string) {
	t.Helper()
	got := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		got = append(got, namespace)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("cache namespaces = %v, want %v", got, want)
	}
}

func TestWorkspaceCleanupAPIsInstalled(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{workspacev1alpha1.GroupVersion})
	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspaceProvider"),
		meta.RESTScopeRoot,
	)

	installed, err := workspaceCleanupAPIsInstalled(mapper)
	if err != nil {
		t.Fatalf("partial discovery returned error: %v", err)
	}
	if installed {
		t.Fatal("partial workspace API discovery reported installed")
	}

	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace"),
		meta.RESTScopeNamespace,
	)
	installed, err = workspaceCleanupAPIsInstalled(mapper)
	if err != nil {
		t.Fatalf("provider/workspace discovery returned error: %v", err)
	}
	if installed {
		t.Fatal("cleanup discovery ignored missing class and pool APIs")
	}

	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspaceClass"),
		meta.RESTScopeNamespace,
	)
	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspacePool"),
		meta.RESTScopeNamespace,
	)
	installed, err = workspaceCleanupAPIsInstalled(mapper)
	if err != nil {
		t.Fatalf("complete discovery returned error: %v", err)
	}
	if !installed {
		t.Fatal("complete workspace API discovery reported missing")
	}
}

func TestManagerWebhookAdmissionEnabled(t *testing.T) {
	tests := []struct {
		name              string
		taskProvenance    bool
		workspaceClassUse bool
		want              bool
	}{
		{name: "separate admission runtime", want: false},
		{name: "task provenance", taskProvenance: true, want: true},
		{name: "workspace class use", workspaceClassUse: true, want: true},
		{name: "all manager admission", taskProvenance: true, workspaceClassUse: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := managerWebhookAdmissionEnabled(tt.taskProvenance, tt.workspaceClassUse); got != tt.want {
				t.Fatalf("managerWebhookAdmissionEnabled(%t, %t) = %t, want %t",
					tt.taskProvenance, tt.workspaceClassUse, got, tt.want)
			}
		})
	}
}

func TestValidateWorkspaceProviderSecurityConfig(t *testing.T) {
	if err := validateWorkspaceProviderSecurityConfig(false, false); err != nil {
		t.Fatalf("disabled API validation: %v", err)
	}
	if err := validateWorkspaceProviderSecurityConfig(true, true); err != nil {
		t.Fatalf("enabled secure API validation: %v", err)
	}
	if err := validateWorkspaceProviderSecurityConfig(true, false); err == nil {
		t.Fatal("workspace API enabled without class-use admission")
	}
}

func TestValidateAgentExecutionSnapshotOptions(t *testing.T) {
	tests := []struct {
		name      string
		mode      executionmode.Mode
		keyFile   string
		retention time.Duration
		interval  time.Duration
		wantError bool
	}{
		{
			name: "harness v1 requires key", mode: executionmode.HarnessV1,
			retention: time.Hour, interval: time.Minute, wantError: true,
		},
		{
			name: "harness v2 requires key", mode: executionmode.HarnessV2,
			retention: time.Hour, interval: time.Minute, wantError: true,
		},
		{
			name: "harness v1 enabled", mode: executionmode.HarnessV1,
			keyFile: "/var/run/orka/snapshot/key", retention: 30 * 24 * time.Hour, interval: time.Hour,
		},
		{
			name: "harness v2 enabled", mode: executionmode.HarnessV2,
			keyFile: "/var/run/orka/snapshot/key", retention: 30 * 24 * time.Hour, interval: time.Hour,
		},
		{
			name: "zero retention", mode: executionmode.HarnessV2,
			keyFile: "/var/run/orka/snapshot/key", retention: 0, interval: time.Hour, wantError: true,
		},
		{
			name: "negative retention", mode: executionmode.HarnessV2, keyFile: "/var/run/orka/snapshot/key",
			retention: -time.Hour, interval: time.Hour, wantError: true,
		},
		{
			name: "zero interval", mode: executionmode.HarnessV2,
			keyFile: "/var/run/orka/snapshot/key", retention: time.Hour, interval: 0, wantError: true,
		},
		{
			name: "negative interval", mode: executionmode.HarnessV2, keyFile: "/var/run/orka/snapshot/key",
			retention: time.Hour, interval: -time.Minute, wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAgentExecutionSnapshotOptions(tt.mode, tt.keyFile, tt.retention, tt.interval)
			if (err != nil) != tt.wantError {
				t.Fatalf("validation error = %v, wantError = %t", err, tt.wantError)
			}
		})
	}
}

func TestLoadAgentExecutionSnapshotCipherAcceptsDeploymentKeyFormats(t *testing.T) {
	raw := []byte(strings.Repeat("k", 32))
	rawWithWhitespaceEdges := append([]byte{' '}, []byte(strings.Repeat("r", 30))...)
	rawWithWhitespaceEdges = append(rawWithWhitespaceEdges, '\n')
	encoded := base64.StdEncoding.EncodeToString(raw)

	tests := []struct {
		name      string
		contents  []byte
		wantError bool
	}{
		{name: "exact raw bytes", contents: raw},
		{name: "exact raw bytes with whitespace edges", contents: rawWithWhitespaceEdges},
		{name: "base64", contents: []byte(encoded)},
		{name: "base64 with normal trailing newline", contents: []byte(encoded + "\n")},
		{name: "base64 with surrounding whitespace", contents: []byte(" \t" + encoded + "\r\n")},
		{
			name:     "trimmed raw bytes are not silently accepted",
			contents: []byte(" " + strings.Repeat("x", 31) + " "), wantError: true,
		},
		{name: "malformed", contents: []byte("not-a-snapshot-key"), wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot-key")
			if err := os.WriteFile(path, tt.contents, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadAgentExecutionSnapshotCipher(path)
			if (err != nil) != tt.wantError {
				t.Fatalf("loadAgentExecutionSnapshotCipher() error = %v, wantError = %t", err, tt.wantError)
			}
		})
	}
}
