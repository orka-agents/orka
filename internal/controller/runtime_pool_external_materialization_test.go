// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	volumes := map[string]bool{}
	for _, volume := range request.Runtime.Template.Spec.Volumes {
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit != nil || native {
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
	for _, name := range []string{"native capability", "replaced UID", "changed route", "unavailable"} {
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
			}
			f.r.APIReader = reader
			_, err = f.r.publishExternalWorkspaceWorkload(t.Context(), f.pool, cfg, f.currentWorkspace(t), auth, providerSecret)
			w := f.currentWorkspace(t)
			if reader.reads == 0 {
				t.Fatal("publication used cached provider capabilities")
			}
			if name == "native capability" {
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
