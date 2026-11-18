// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func (f *externalRuntimePoolFixture) advertiseNativeProcess(t *testing.T) {
	t.Helper()
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.provider), provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.SupportedFeatures = append(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess)
	if err := f.r.Update(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	if f.currentWorkspace(t).Spec.Workload == nil {
		// This fixture represents the native kind selected at pool creation.
		f.pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures = append(f.pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess)
		pool := runtimePoolTestGetPool(t, f.r, f.pool)
		pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures = slices.Clone(f.pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures)
		if err := f.r.Update(t.Context(), &pool); err != nil {
			t.Fatal(err)
		}
	}
}

// The native backend admits at most 32 environment entries. Main's in-tree
// native runtime fit its live conformance flags in that budget; keep it.
func TestExternalRuntimePoolNativeEnvironmentFitsBackendLimit(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.r.E2EPromptWriteAmbiguityMarker = "ORKA_E2E_WS_LC_AMBIGUOUS_OK"
	f.advertiseNativeProcess(t)
	w := f.currentWorkspace(t)
	w.Spec.Lifecycle.AllowedOnDetach = append(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	if err := f.r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	env := f.publish(t).Spec.Workload.Runtime.Template.Spec.Containers[0].Env
	names := map[string]bool{}
	for _, variable := range env {
		names[variable.Name] = true
	}
	if len(env) > 32 || !names["ORKA_ACP_POD_UID"] || !names[runtimePoolE2EPromptWriteAmbiguity] || names["ORKA_ACP_POD_NAME"] || names["ORKA_ACP_POD_NAMESPACE"] {
		t.Fatalf("native environment has %d entries (backend limit 32) or wrong identity variables: %v", len(env), names)
	}
}

// Supervisor telemetry shares that budget. A native runtime that cannot
// suspend has three entries to spare, which every endpoint and protocol
// combination, and the enable flag, one collector endpoint and one exporter
// setting shared by traces and metrics, must fit.
func TestExternalRuntimePoolNativeEnvironmentFitsBackendLimitWithTelemetry(t *testing.T) {
	const collector = "http://collector.otel.svc:4318"
	combinations := map[string]map[string]string{
		"generic endpoint, insecure":     {"OTEL_EXPORTER_OTLP_ENDPOINT": collector, "OTEL_EXPORTER_OTLP_INSECURE": "true"},
		"generic endpoint, compression":  {"OTEL_EXPORTER_OTLP_ENDPOINT": collector, "OTEL_EXPORTER_OTLP_COMPRESSION": "gzip"},
		"equal protocols set per signal": {"OTEL_EXPORTER_OTLP_ENDPOINT": collector, "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf"},
	}
	for name, combination := range runtimePoolTelemetryCombinations() {
		combinations[name] = combination.values
	}
	for name, values := range combinations {
		t.Run(name, func(t *testing.T) {
			for _, signal := range []string{"", "TRACES_", "METRICS_"} {
				for _, suffix := range append([]string{otlpEndpointSuffix}, runtimePoolTelemetryScalarSuffixes...) {
					t.Setenv("OTEL_EXPORTER_OTLP_"+signal+suffix, values["OTEL_EXPORTER_OTLP_"+signal+suffix])
				}
			}
			f := newExternalRuntimePoolFixture(t)
			f.r.EnableTelemetry = true
			f.advertiseNativeProcess(t)
			env := f.publish(t).Spec.Workload.Runtime.Template.Spec.Containers[0].Env
			names := make([]string, 0, len(env))
			for _, variable := range env {
				names = append(names, variable.Name)
			}
			if !slices.Contains(names, "ORKA_ENABLE_TELEMETRY") {
				t.Fatalf("native environment carries no telemetry settings: %v", names)
			}
			if len(env) > 32 {
				t.Fatalf("native environment has %d entries (backend limit 32): %v", len(env), names)
			}
		})
	}
}

func TestExternalRuntimePoolFreezesMaterializationLayoutBeforeRevision(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/workspace.orka.ai_executionworkspaces.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		t.Fatal(err)
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	schema, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range []bool{false, true} {
		for _, durable := range []bool{false, true} {
			name := "pod"
			if native {
				name = "native"
			}
			if durable {
				name += " durable"
			}
			t.Run(name, func(t *testing.T) {
				f := newExternalRuntimePoolFixture(t)
				if native {
					f.advertiseNativeProcess(t)
				}
				if durable {
					w := f.currentWorkspace(t)
					w.Spec.Lifecycle.AllowedOnDetach = append(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
					if err := f.r.Update(t.Context(), w); err != nil {
						t.Fatal(err)
					}
				}
				w := f.publish(t)
				request := w.Spec.Workload
				assertExternalRuntimePoolMaterializationLayout(t, request, native, durable)
				assertExternalRuntimePoolMaterializationNetwork(t, request, native)
				if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil {
					t.Fatal(err)
				}
				object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(w)
				if err != nil {
					t.Fatal(err)
				}
				pruning.Prune(object, schema, true)
				defaulting.Default(object, schema)
				var persisted workspacev1alpha1.ExecutionWorkspace
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object, &persisted); err != nil {
					t.Fatal(err)
				}
				if err := workspaceprovider.ValidateWorkspaceWorkload(&persisted); err != nil {
					t.Fatalf("API pruning/defaulting changed frozen materialization revision: %v", err)
				}
				if request.Revision != persisted.Spec.Workload.Revision {
					t.Fatal("API persistence changed the admitted materialization revision")
				}
			})
		}
	}
}

func assertExternalRuntimePoolMaterializationNetwork(t *testing.T, request *workspacev1alpha1.WorkloadRequest, native bool) {
	t.Helper()
	policy := request.Runtime.NetworkPolicy
	if policy == nil || len(policy.Egress) == 0 {
		t.Fatal("runtime egress permissions were not frozen")
	}
	wantTypes := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	if native {
		if len(request.Runtime.Template.Spec.NodeSelector) != 0 {
			t.Fatal("native intent carries a Kubernetes node selector")
		}
		wantTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	}
	if !slices.Equal(policy.PolicyTypes, wantTypes) || (len(policy.Ingress) == 0) != native {
		t.Fatal("public network intent does not match provider materialization")
	}
}

//nolint:gocyclo // Explicit field checks keep the frozen materialization contract auditable.
func assertExternalRuntimePoolMaterializationLayout(t *testing.T, request *workspacev1alpha1.WorkloadRequest, native, durable bool) {
	t.Helper()
	container := request.Runtime.Template.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || *container.SecurityContext.ReadOnlyRootFilesystem == native {
		t.Fatal("public root-filesystem intent does not match provider materialization")
	}
	assertExternalRuntimePoolMaterializationSecurity(t, request, native)
	if slices.Contains(request.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) != native {
		t.Fatal("request did not freeze native-process materialization capability")
	}
	wantResources := runtimePoolResourceRequirements(runtimePoolResourceClassStandard)
	if native {
		wantResources = corev1.ResourceRequirements{}
	}
	if !reflect.DeepEqual(request.Resources, wantResources) || !reflect.DeepEqual(container.Resources, wantResources) {
		t.Fatalf("public resources do not match provider materialization: %#v", request.Resources)
	}
	volumes := map[string]bool{}
	for _, volume := range request.Runtime.Template.Spec.Volumes {
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.IsZero() || native {
			t.Fatalf("unexpected public volume: %s", volume.Name)
		}
		volumes[volume.Name] = true
	}
	if native && len(volumes) != 0 || !native && len(volumes) != 3 {
		t.Fatal("scratch declarations differ from admitted materialization")
	}
	mounts := 0
	for _, mount := range container.VolumeMounts {
		if mount.Name == runtimePoolDurableWorkspaceVolume && durable {
			if mount.MountPath != runtimePoolDurableWorkspaceMountPath || mount.ReadOnly || volumes[mount.Name] {
				t.Fatal("durable workspace layout was rewritten as Pod scratch")
			}
			continue
		}
		if native || !volumes[mount.Name] {
			t.Fatal("native scratch mount survived or Pod scratch mount lost its declaration")
		}
		mounts++
	}
	if !native && mounts != 3 || native && mounts != 0 {
		t.Fatal("scratch mounts differ from admitted materialization")
	}
	env := runtimePoolLiteralEnvironment(container.Env)
	wantPort, wantListener := runtimePoolPort, ":8080"
	if native {
		wantPort, wantListener = 80, ":80"
		if container.StartupProbe != nil || container.ReadinessProbe != nil || container.LivenessProbe != nil || container.Lifecycle != nil || request.Runtime.Template.Spec.TerminationGracePeriodSeconds != nil {
			t.Fatal("native intent declares Kubernetes health or shutdown guarantees")
		}
	} else if container.StartupProbe == nil || container.ReadinessProbe == nil || container.LivenessProbe == nil || container.Lifecycle == nil || request.Runtime.Template.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatal("Pod health or shutdown guarantees changed")
	}
	if request.Runtime.BootstrapPort != wantPort || env["ORKA_ACP_LISTEN_ADDRESS"] != wantListener || len(container.Ports) != 1 || container.Ports[0].ContainerPort != wantPort {
		t.Fatal("public bootstrap listener differs from provider materialization")
	}
	for _, name := range []string{"ORKA_ACP_SESSION_BASE_DIR", "ORKA_ACP_MCP_BROKER_URL", "ORKA_ACP_POD_NAMESPACE"} {
		present := slices.ContainsFunc(container.Env, func(variable corev1.EnvVar) bool { return variable.Name == name })
		if present == native {
			t.Fatalf("public environment override %s differs from provider materialization", name)
		}
	}
	if env[runtimePoolBootstrapNonceEnv] == "" || env["ORKA_ACP_CREDENTIAL_BOOTSTRAP_PUBLIC_KEY"] == "" ||
		(env["ORKA_ACP_DURABLE_WORKSPACE_DIR"] == runtimePoolDurableWorkspaceMountPath) != durable ||
		(env["ORKA_ACP_DURABLE_WORKSPACE_KEY"] == "shared") != durable {
		t.Fatal("public bootstrap or durable identity configuration changed")
	}
}

func assertExternalRuntimePoolMaterializationSecurity(t *testing.T, request *workspacev1alpha1.WorkloadRequest, native bool) {
	t.Helper()
	pod := request.Runtime.Template.Spec.SecurityContext
	container := request.Runtime.Template.Spec.Containers[0].SecurityContext
	if pod == nil || container == nil || pod.RunAsUser == nil || *pod.RunAsUser != 0 || pod.RunAsGroup == nil || *pod.RunAsGroup != 0 || container.RunAsUser == nil || *container.RunAsUser != 0 || container.RunAsGroup == nil || *container.RunAsGroup != 0 {
		t.Fatal("supervisor root identity was not frozen")
	}
	if native {
		if pod.SeccompProfile != nil || container.SeccompProfile != nil || container.AllowPrivilegeEscalation != nil || container.Privileged == nil || *container.Privileged || container.RunAsNonRoot == nil || *container.RunAsNonRoot {
			t.Fatal("native request declares unsupported security guarantees")
		}
	} else if pod.SeccompProfile == nil || container.SeccompProfile == nil || container.AllowPrivilegeEscalation == nil || *container.AllowPrivilegeEscalation {
		t.Fatal("Pod security guarantees changed")
	}
	if container.Capabilities == nil || !slices.Equal(container.Capabilities.Drop, []corev1.Capability{"ALL"}) || !slices.Equal(container.Capabilities.Add, []corev1.Capability{"CHOWN", "KILL", "SETGID", "SETUID"}) {
		t.Fatal("supervisor admitted capability set changed")
	}
}

func TestExternalRuntimePoolCapabilityDoesNotRewriteAdmittedPodIntent(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	w := f.publish(t)
	previous := w.Spec.Workload.DeepCopy()
	f.advertiseNativeProcess(t)
	runtimePoolReconcile(t, f.r, f.pool)
	if !reflect.DeepEqual(previous, f.currentWorkspace(t).Spec.Workload) {
		t.Fatal("provider capability change rewrote an existing Pod materialization request")
	}
}

type externalMaterializationProviderReader struct {
	client.Reader
	provider *workspacev1alpha1.ExecutionWorkspaceProvider
	err      error
	reads    int
}

func (r *externalMaterializationProviderReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if provider, ok := object.(*workspacev1alpha1.ExecutionWorkspaceProvider); ok {
		r.reads++
		if r.err != nil {
			return r.err
		}
		if key.Name != r.provider.Name || key.Namespace != "" {
			return errors.New("provider lookup lost the bound registration name or scope")
		}
		*provider = *r.provider.DeepCopy()
		return nil
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestExternalRuntimePoolPublicationFencesAuthoritativeProvider(t *testing.T) {
	for _, name := range []string{"native capability", "replaced UID", "changed route", "unavailable", "advanced generation", "draining advanced generation"} {
		t.Run(name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			cfg, err := f.r.runtimePoolConfig(f.pool)
			if err != nil {
				t.Fatal(err)
			}
			auth, providerSecret, err := f.r.ensureRuntimePoolSecrets(t.Context(), f.pool, cfg)
			if err != nil {
				t.Fatal(err)
			}
			reader := &externalMaterializationProviderReader{Reader: f.r.Client, provider: f.provider.DeepCopy()}
			reader.provider.Status.SupportedFeatures = append(reader.provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess)
			switch name {
			case "replaced UID":
				reader.provider.UID = "replacement-provider"
			case "changed route":
				reader.provider.Spec.ControllerName = "other.workspace.orka.ai"
			case "unavailable":
				reader.err = errors.New("provider observation unavailable")
			case "advanced generation", "draining advanced generation":
				// The provider published current status for a newer generation
				// after the reconcile-time fence.
				reader.provider.Generation++
				reader.provider.Status.ObservedGeneration = reader.provider.Generation
				for i := range reader.provider.Status.Conditions {
					reader.provider.Status.Conditions[i].ObservedGeneration = reader.provider.Generation
				}
				if name == "draining advanced generation" {
					reader.provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
				}
			}
			f.r.APIReader = reader
			_, err = f.r.publishExternalWorkspaceWorkload(t.Context(), f.pool, cfg, f.currentWorkspace(t), auth, providerSecret)
			w := f.currentWorkspace(t)
			if reader.reads == 0 {
				t.Fatal("publication used cached provider capabilities")
			}
			if name == "draining advanced generation" {
				if err != nil || w.Spec.Workload == nil {
					t.Fatalf("draining provider stopped serving its bound workspace: %v", err)
				}
			} else if name == "native capability" {
				if err != nil || w.Spec.Workload == nil || slices.Contains(w.Spec.Workload.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) {
					t.Fatalf("late authoritative capability changed the frozen Pod kind: %v", err)
				}
			} else if err == nil || w.Spec.Workload != nil {
				t.Fatalf("unproven provider identity published workload: %v", err)
			}
		})
	}
}

func TestExternalRuntimePoolAttestsOnlyFrozenMaterializationKind(t *testing.T) {
	for _, tc := range []struct {
		name            string
		native, process bool
		valid           bool
	}{
		{name: "Pod intent and Pod evidence", valid: true},
		{name: "native intent and Process evidence", native: true, process: true, valid: true},
		{name: "old Pod intent and Process evidence", process: true},
		{name: "native intent and Pod evidence", native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			if tc.native {
				f.advertiseNativeProcess(t)
			}
			w, worker := f.materialize(t)
			evidence := w.Status.Allocation.Startup
			if tc.process {
				evidence.Pod = nil
				evidence.Endpoint = "http://native-router.example:80/runtime"
				evidence.Process = &workspacev1alpha1.NativeProcessEvidence{Namespace: "native", Name: "process", UID: w.Status.Allocation.Identity.InstanceID, Version: 1,
					Worker: workspacev1alpha1.PodReference{Namespace: worker.Namespace, Name: worker.Name, UID: worker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}
			}
			pod, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, evidence)
			if tc.valid {
				if err != nil || pod == nil {
					t.Fatalf("matching frozen materialization rejected: %v", err)
				}
				return
			}
			if err == nil || pod != nil {
				t.Fatal("startup changed the frozen materialization kind")
			}
			if err := f.r.Status().Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			runtimePoolReconcile(t, f.r, f.pool)
			status := runtimePoolTestGetPool(t, f.r, f.pool).Status
			if f.seeds != 0 || f.supervisor.probeCalls != 0 || status.ActiveInstance != nil || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
				t.Fatal("mismatched materialization received runtime credentials or admission")
			}
		})
	}
}

// A suspendable native runtime adds durable-workspace entries to the same
// 32-entry budget. Telemetry must fit what remains, and the pool must say
// when it cannot, instead of publishing a request the provider refuses.
func TestExternalRuntimePoolSuspendableNativeEnvironmentFitsWithTelemetry(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.otel.svc.cluster.local:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	for name, test := range map[string]struct {
		marker    string
		telemetry bool
		status    metav1.ConditionStatus
		reason    string
	}{
		"traces and metrics fit":         {telemetry: true, status: metav1.ConditionTrue, reason: corev1alpha1.RuntimePoolReasonTelemetryProjected},
		"no room left for any telemetry": {marker: "ORKA_E2E_WS_LC_AMBIGUOUS_OK", status: metav1.ConditionFalse, reason: corev1alpha1.RuntimePoolReasonNativeEnvironmentLimit},
	} {
		t.Run(name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			f.r.EnableTelemetry = true
			f.r.E2EPromptWriteAmbiguityMarker = test.marker
			f.advertiseNativeProcess(t)
			w := f.currentWorkspace(t)
			w.Spec.Lifecycle.AllowedOnDetach = append(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
			if err := f.r.Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			env := f.publish(t).Spec.Workload.Runtime.Template.Spec.Containers[0].Env
			names := map[string]bool{}
			for _, variable := range env {
				names[variable.Name] = true
			}
			if len(env) > nativeRuntimeEnvironmentLimit || !names["ORKA_ACP_DURABLE_WORKSPACE_KEY"] {
				t.Fatalf("suspendable native environment has %d entries (backend limit %d): %v", len(env), nativeRuntimeEnvironmentLimit, names)
			}
			if names["ORKA_ENABLE_TELEMETRY"] != test.telemetry {
				t.Fatalf("telemetry projected = %t, want %t: %v", names["ORKA_ENABLE_TELEMETRY"], test.telemetry, names)
			}
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionTelemetryReady)
			if condition == nil || condition.Status != test.status || condition.Reason != test.reason {
				t.Fatalf("TelemetryReady = %+v, want %s/%s", condition, test.status, test.reason)
			}
		})
	}
}

func TestFitNativeRuntimeTelemetryDropsMetricsThenTraces(t *testing.T) {
	// Separate OTLP/HTTP collectors need four entries: the Pod projection's
	// fixed budget of three drops metrics, while a native runtime with room
	// for four keeps both signals.
	values := map[string]string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  "http://traces.otel.svc.cluster.local:4318/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://metrics.otel.svc.cluster.local:4318/v1/metrics",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
	}
	split := func(name string) string { return values[name] }
	base := func(entries int) []corev1.EnvVar {
		env := make([]corev1.EnvVar, entries)
		for i := range env {
			env[i] = corev1.EnvVar{Name: fmt.Sprintf("ORKA_ACP_FIXTURE_%d", i), Value: "x"}
		}
		return env
	}
	pod := runtimePoolTelemetryEnv(true, split)
	if len(pod) != 3 || slices.ContainsFunc(pod, func(v corev1.EnvVar) bool { return v.Name == "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT" }) {
		t.Fatalf("Pod projection = %v, want trace-only within three entries", pod)
	}
	both := []string{"ORKA_ENABLE_TELEMETRY", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL"}
	traces := []string{"ORKA_ENABLE_TELEMETRY", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"}
	for name, test := range map[string]struct {
		base      int
		fits      bool
		entries   int
		telemetry []string
	}{
		"room for every signal": {base: nativeRuntimeEnvironmentLimit - 4, fits: true, entries: 32, telemetry: both},
		"room for traces":       {base: nativeRuntimeEnvironmentLimit - 3, entries: 32, telemetry: traces},
		"no room":               {base: nativeRuntimeEnvironmentLimit - 2, entries: 30},
		"already over":          {base: nativeRuntimeEnvironmentLimit + 1, entries: 33},
	} {
		t.Run(name, func(t *testing.T) {
			// The runtime template arrives carrying the Pod projection.
			env, fits, message := fitNativeRuntimeTelemetry(append(base(test.base), pod...), true, split)
			if fits != test.fits || message == "" || len(env) != test.entries {
				t.Fatalf("fit = %t %q with %d entries, want %t with %d", fits, message, len(env), test.fits, test.entries)
			}
			var telemetry []string
			for _, variable := range env {
				if !strings.HasPrefix(variable.Name, "ORKA_ACP_FIXTURE_") {
					telemetry = append(telemetry, variable.Name)
				}
			}
			if !slices.Equal(telemetry, test.telemetry) {
				t.Fatalf("telemetry entries = %v, want %v", telemetry, test.telemetry)
			}
		})
	}
	// Readiness compares against every configured signal: a trace-only Pod
	// projection is not reported as complete.
	if _, fits, _ := fitNativeRuntimeTelemetry(append(base(nativeRuntimeEnvironmentLimit-3), pod...), true, split); fits {
		t.Fatal("trace-only telemetry reported every signal projected")
	}
	// An unchanged projection that fits keeps its place in the environment.
	tracesSignal, metricsSignal, _ := runtimePoolTelemetrySignals(true, split)
	unchanged := append(append(base(2), encodeRuntimePoolTelemetryEnv(tracesSignal, metricsSignal)...), base(1)...)
	if env, fits, _ := fitNativeRuntimeTelemetry(unchanged, true, split); !fits || !reflect.DeepEqual(env, unchanged) {
		t.Fatalf("unchanged projection was rewritten: %v", env)
	}
	env, fits, message := fitNativeRuntimeTelemetry(append(base(31), pod...), false, split)
	if len(env) != 31 || !fits || message != "" {
		t.Fatalf("disabled telemetry kept %d entries or reported %q", len(env), message)
	}
}

// Native fitting decides which collectors the runtime exports to, so the
// gateway allowlist admits only those: a dropped metrics collector is not
// reachable from the sandbox.
func TestExternalRuntimePoolNativeCollectorEgressFollowsFittedTelemetry(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://traces.otel.svc.cluster.local:4318/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://metrics.otel.svc.cluster.local:4318/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	f := newExternalRuntimePoolFixture(t)
	f.r.EnableTelemetry, f.r.TelemetryCollectorEgress = true, true
	f.advertiseNativeProcess(t)
	f.advertiseSubstrateV1Backend(t)
	w := f.currentWorkspace(t)
	w.Spec.Lifecycle.AllowedOnDetach = append(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	if err := f.r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	workload := f.publish(t).Spec.Workload.Runtime
	hosts := make([]string, 0, len(workload.Egress.Rules))
	for _, rule := range workload.Egress.Rules {
		hosts = append(hosts, rule.Host)
	}
	env := workload.Template.Spec.Containers[0].Env
	if len(env) > nativeRuntimeEnvironmentLimit || !slices.Contains(hosts, "traces.otel.svc.cluster.local") || slices.Contains(hosts, "metrics.otel.svc.cluster.local") ||
		!slices.Equal(telemetryCollectorEndpoints(env), []string{"http://traces.otel.svc.cluster.local:4318/v1/traces"}) {
		t.Fatalf("native runtime with %d entries exports to %v and may reach %v", len(env), telemetryCollectorEndpoints(env), hosts)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionTelemetryReady); condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != corev1alpha1.RuntimePoolReasonNativeEnvironmentLimit {
		t.Fatalf("TelemetryReady = %+v, want False/NativeEnvironmentLimit", condition)
	}
}

// A request without supervisor telemetry clears readiness left by an earlier
// request instead of describing a runtime that no longer exists.
func TestExternalRuntimePoolClearsTelemetryReadyWithoutTelemetry(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	f.r.setRuntimePoolCondition(&pool, &pool.Status, corev1alpha1.RuntimePoolConditionTelemetryReady, metav1.ConditionTrue,
		corev1alpha1.RuntimePoolReasonTelemetryProjected, "earlier request")
	if err := f.r.Status().Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	f.publish(t)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionTelemetryReady); condition != nil {
		t.Fatalf("stale TelemetryReady kept: %+v", condition)
	}
}

// loseNextPublishStatusWrite makes the API server refuse the next RuntimePool
// status write that reports a published workload request, as when the drain,
// session or supervisor probe path advances the pool first. It counts the
// writes it refused.
func (f *externalRuntimePoolFixture) loseNextPublishStatusWrite() *int {
	lost := new(int)
	f.r.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			if pool, ok := object.(*corev1alpha1.RuntimePool); ok && *lost == 0 && strings.HasPrefix(pool.Status.Message, "public workload request published") {
				*lost++
				return apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("runtimepools").GroupResource(), pool.Name, errors.New("the object has been modified"))
			}
			return c.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	return lost
}

// reducedNativeTelemetryEnv configures separate OTLP/HTTP collectors whose
// exporter settings differ per signal: six entries, more than the room a
// native runtime that cannot suspend leaves, while traces alone fit.
var reducedNativeTelemetryEnv = map[string]string{
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  "http://traces.otel.svc.cluster.local:4318/v1/traces",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://metrics.otel.svc.cluster.local:4318/v1/metrics",
	"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
	"OTEL_EXPORTER_OTLP_TRACES_INSECURE":  "true",
	"OTEL_EXPORTER_OTLP_METRICS_INSECURE": "false",
}

func assertTelemetryReady(t *testing.T, pool corev1alpha1.RuntimePool, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionTelemetryReady)
	if status == "" {
		if condition != nil {
			t.Fatalf("TelemetryReady = %+v, want none", condition)
		}
		return
	}
	if condition == nil || condition.Status != status || condition.Reason != reason || condition.Message == "" {
		t.Fatalf("TelemetryReady = %+v, want %s/%s", condition, status, reason)
	}
}

// Only the publishing reconcile fits telemetry. When its status write loses
// the optimistic lock, the requeued reconcile does not publish again, so it
// must re-derive TelemetryReady from the frozen request for the pool to serve
// with the condition.
func TestExternalRuntimePoolTelemetryReadySurvivesLostPublishStatusWrite(t *testing.T) {
	for name, test := range map[string]struct {
		env    map[string]string
		status metav1.ConditionStatus
		reason string
	}{
		"every signal projected": {
			env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.svc.cluster.local:4317", "OTEL_EXPORTER_OTLP_INSECURE": "true"},
			status: metav1.ConditionTrue, reason: corev1alpha1.RuntimePoolReasonTelemetryProjected,
		},
		"metrics reduced to fit": {
			env:    reducedNativeTelemetryEnv,
			status: metav1.ConditionFalse, reason: corev1alpha1.RuntimePoolReasonNativeEnvironmentLimit,
		},
	} {
		t.Run(name, func(t *testing.T) {
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			f := newExternalRuntimePoolFixture(t)
			f.r.EnableTelemetry, f.r.TelemetryCollectorEgress = true, true
			f.advertiseNativeProcess(t)
			f.advertiseSubstrateV1Backend(t)
			lost := f.loseNextPublishStatusWrite()
			routeRuntimeFixture(t, f, nil)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if *lost != 1 {
				t.Fatalf("lost %d publishing status writes, want 1", *lost)
			}
			assertTelemetryReady(t, pool, "", "")
			w := f.currentWorkspace(t)
			if err := f.r.bindExternalRuntimeInstanceEvidence(t.Context(), &pool, w); err != nil {
				t.Fatal(err)
			}
			attested, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
			if err != nil {
				t.Fatal(err)
			}
			f.supervisor.probe = runtimePoolValidProbe(&pool, attested, "routed-boot", false)
			for range 3 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
				t.Fatalf("routed runtime did not serve: %s: %s", pool.Status.Lifecycle, pool.Status.Message)
			}
			assertTelemetryReady(t, pool, test.status, test.reason)
		})
	}
}

// A republished request re-derives TelemetryReady even when its own publishing
// status write is lost: the predecessor's condition must not describe the new
// runtime.
func TestExternalRuntimePoolRepublishRederivesTelemetryReady(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.otel.svc.cluster.local:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	for name, test := range map[string]struct {
		change func(*testing.T, *externalRuntimePoolFixture)
		status metav1.ConditionStatus
		reason string
	}{
		"telemetry disabled": {change: func(_ *testing.T, f *externalRuntimePoolFixture) { f.r.EnableTelemetry = false }},
		"metrics reduced to fit": {
			change: func(t *testing.T, _ *externalRuntimePoolFixture) {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
				for key, value := range reducedNativeTelemetryEnv {
					t.Setenv(key, value)
				}
			},
			status: metav1.ConditionFalse, reason: corev1alpha1.RuntimePoolReasonNativeEnvironmentLimit,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			f.r.EnableTelemetry = true
			f.advertiseNativeProcess(t)
			w := f.publish(t)
			assertTelemetryReady(t, runtimePoolTestGetPool(t, f.r, f.pool), metav1.ConditionTrue, corev1alpha1.RuntimePoolReasonTelemetryProjected)
			// The provider reports the first, never-started instance stopped.
			identity := workspacev1alpha1.InstanceIdentity{AllocationID: "first-allocation", InstanceID: "first-instance", RequestRevision: w.Spec.Workload.Revision}
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: w.Spec.Workload.Sequence, Key: w.Spec.Workload.Key, Identity: identity, State: workspacev1alpha1.AllocationStopped}
			w.Status.ObservedGeneration = w.Generation
			if err := f.r.Status().Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			test.change(t, f)
			lost := f.loseNextPublishStatusWrite()
			for range 6 {
				runtimePoolReconcile(t, f.r, f.pool)
				if w = f.currentWorkspace(t); w.Spec.Workload.Sequence == 2 {
					break
				}
			}
			if w.Spec.Workload.Sequence != 2 || *lost != 1 {
				t.Fatalf("republished sequence %d, lost %d publishing status writes; want 2 and 1", w.Spec.Workload.Sequence, *lost)
			}
			assertTelemetryReady(t, runtimePoolTestGetPool(t, f.r, f.pool), metav1.ConditionTrue, corev1alpha1.RuntimePoolReasonTelemetryProjected)
			runtimePoolReconcile(t, f.r, f.pool)
			assertTelemetryReady(t, runtimePoolTestGetPool(t, f.r, f.pool), test.status, test.reason)
		})
	}
}

// A request published under another controller telemetry configuration keeps
// the condition recorded with it: re-fitting it to the current configuration
// would describe a projection the runtime does not have.
func TestExternalRuntimePoolKeepsTelemetryReadyAcrossConfigurationChange(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.otel.svc.cluster.local:4317")
	f := newExternalRuntimePoolFixture(t)
	f.r.EnableTelemetry = true
	f.advertiseNativeProcess(t)
	f.publish(t)
	assertTelemetryReady(t, runtimePoolTestGetPool(t, f.r, f.pool), metav1.ConditionTrue, corev1alpha1.RuntimePoolReasonTelemetryProjected)
	f.r.EnableTelemetry = false
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Message == "public workload request published; waiting for provider materialization" {
		t.Fatal("reconcile did not recompute status after publication")
	}
	assertTelemetryReady(t, pool, metav1.ConditionTrue, corev1alpha1.RuntimePoolReasonTelemetryProjected)
}

// A configuration change that leaves the frozen projection unchanged is judged
// against the signals now configured: once metrics are turned off, the
// trace-only runtime exports every configured signal and does not report
// metrics as dropped for the environment limit.
func TestExternalRuntimePoolTelemetryReadyFollowsConfiguredSignalsForUnchangedProjection(t *testing.T) {
	for key, value := range reducedNativeTelemetryEnv {
		t.Setenv(key, value)
	}
	f := newExternalRuntimePoolFixture(t)
	f.r.EnableTelemetry = true
	f.advertiseNativeProcess(t)
	before := f.publish(t).Spec.Workload.Revision
	assertTelemetryReady(t, runtimePoolTestGetPool(t, f.r, f.pool), metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonNativeEnvironmentLimit)
	t.Setenv(runtimePoolMetricsExporterEnv, "none")
	runtimePoolReconcile(t, f.r, f.pool)
	if f.currentWorkspace(t).Spec.Workload.Revision != before {
		t.Fatal("configuration change republished the request")
	}
	assertTelemetryReady(t, runtimePoolTestGetPool(t, f.r, f.pool), metav1.ConditionTrue, corev1alpha1.RuntimePoolReasonTelemetryProjected)
}

// Empty model limits are equivalent to unset ones for both the supervisor
// and the controller, so the native template omits them; set limits stay.
func TestRuntimePoolNativeProcessTemplateOmitsOnlyEmptyModelLimits(t *testing.T) {
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Env: []corev1.EnvVar{
		{Name: "ORKA_ACP_MODEL_CONTEXT_LIMIT", Value: ""},
		{Name: "ORKA_ACP_MODEL_OUTPUT_LIMIT", Value: ""},
	}}}}}
	if env := runtimePoolNativeProcessTemplate(template).Spec.Containers[0].Env; len(env) != 0 {
		t.Fatalf("empty model limits kept: %v", env)
	}
	template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "ORKA_ACP_MODEL_CONTEXT_LIMIT", Value: "200000"},
		{Name: "ORKA_ACP_MODEL_OUTPUT_LIMIT", Value: "8192"},
	}
	if env := runtimePoolNativeProcessTemplate(template).Spec.Containers[0].Env; len(env) != 2 {
		t.Fatalf("set model limits dropped: %v", env)
	}
	environment := map[string]string{}
	if limits, err := runtimePoolModelLimitsFromEnvironment(environment); err != nil || limits != nil {
		t.Fatalf("absent model limits = %v, %v; want unset", limits, err)
	}
}
