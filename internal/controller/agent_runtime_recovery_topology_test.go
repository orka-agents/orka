package controller

import (
	"reflect"
	"testing"

	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func foundryRecoveryPodSpec(local corev1.PodSpec) corev1.PodSpec {
	result := *local.DeepCopy()
	result.Volumes = []corev1.Volume{
		{Name: "ledger", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "foundry-ledger"}}},
		{Name: "azure-identity", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
			ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "api://AzureADTokenExchange", Path: "token", ExpirationSeconds: new(int64(3600))},
		}}}}},
	}
	// Broker first also proves that recovery patches/selects the supervisor by
	// its configured name rather than accidentally changing the first container.
	result.Containers = append([]corev1.Container{{Name: "broker", Image: "docker.io/example/broker@" + testControllerDigest("broker"),
		Env:          []corev1.EnvVar{{Name: "AZURE_FEDERATED_TOKEN_FILE", Value: "/identity/token"}},
		VolumeMounts: []corev1.VolumeMount{{Name: "ledger", MountPath: "/ledger"}, {Name: "azure-identity", MountPath: "/identity", ReadOnly: true}},
	}}, result.Containers...)
	return result
}

func TestAgentRuntimeRecoveryTopologyAcceptsExactLocalAndFoundryPods(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	local := *f.pod.Spec.DeepCopy()
	local.Containers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
		Add: []corev1.Capability{"CHOWN", "KILL", "SETGID", "SETUID"}, Drop: []corev1.Capability{"ALL"},
	}}
	if err := validateAgentRuntimeRecoveryPodSpec(local, "supervisor", "agentkit"); err != nil {
		t.Fatalf("actual deployed local capability set was rejected: %v", err)
	}
	foundry := foundryRecoveryPodSpec(local)
	if err := validateAgentRuntimeRecoveryPodSpec(foundry, "supervisor", "foundry"); err != nil {
		t.Fatalf("explicit broker-only identity and ledger were rejected: %v", err)
	}
	for _, test := range []struct {
		name, provider string
		change         func(*corev1.PodSpec)
	}{
		{"local sidecar", "agentkit", func(*corev1.PodSpec) {}},
		{"shared PID", "foundry", func(spec *corev1.PodSpec) { spec.ShareProcessNamespace = new(true) }},
		{"host PID", "foundry", func(spec *corev1.PodSpec) { spec.HostPID = true }},
		{"ptrace", "foundry", func(spec *corev1.PodSpec) {
			spec.Containers[1].SecurityContext.Capabilities.Add = append(spec.Containers[1].SecurityContext.Capabilities.Add, "SYS_PTRACE")
		}},
		{"host mount", "foundry", func(spec *corev1.PodSpec) {
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}},
		{"supervisor PVC", "foundry", func(spec *corev1.PodSpec) {
			spec.Containers[1].VolumeMounts = []corev1.VolumeMount{{Name: "ledger", MountPath: "/ledger"}}
		}},
		{"supervisor token", "foundry", func(spec *corev1.PodSpec) {
			spec.Containers[1].VolumeMounts = []corev1.VolumeMount{{Name: "azure-identity", MountPath: "/identity"}}
		}},
		{"unpinned broker", "foundry", func(spec *corev1.PodSpec) { spec.Containers[0].Image = "broker:latest" }},
		{"fixed boot", "foundry", func(spec *corev1.PodSpec) {
			spec.Containers[1].Env = append(spec.Containers[1].Env, corev1.EnvVar{Name: agentRuntimeBootEnvironment, Value: "fixed"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrong := foundry.DeepCopy()
			test.change(wrong)
			if err := validateAgentRuntimeRecoveryPodSpec(*wrong, "supervisor", test.provider); err == nil {
				t.Fatal("unsupported physical topology was admitted")
			}
		})
	}
}

func TestAgentRuntimeRecoveryRejectsSupervisorServiceAccountToken(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	spec := *f.pod.Spec.DeepCopy()
	spec.Volumes = []corev1.Volume{{Name: "azure-identity", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
			ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: new(int64(3600))},
		}}},
	}}}
	spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "azure-identity", MountPath: "/identity", ReadOnly: true}}
	for _, provider := range []string{"codex", "claude", "copilot", "opencode", "agentkit", "foundry"} {
		t.Run(provider, func(t *testing.T) {
			candidate := spec
			if provider == agentRuntimeFoundryProvider {
				candidate = foundryRecoveryPodSpec(candidate)
			}
			if err := validateAgentRuntimeRecoveryPodSpec(candidate, "supervisor", provider); err == nil {
				t.Fatal("supervisor identity token was admitted despite automountServiceAccountToken=false")
			}
		})
	}

	// Exercise enrollment with matching Deployment, ReplicaSet, and Pod specs;
	// the token must prevent admission before any boot witness is published.
	deployment := &appsv1.Deployment{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Spec.Template.Spec = *spec.DeepCopy()
	f.rs.Spec.Template.Spec = *spec.DeepCopy()
	f.pod.Spec = *spec.DeepCopy()
	for _, object := range []client.Object{deployment, f.rs, f.pod} {
		if err := f.r.Update(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	f.reconcile(t)
	if f.runtime.Status.Ready {
		t.Error("local runtime with supervisor identity token became Ready")
	}
	witnesses, err := f.r.recoveryWitnesses(t.Context(), f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(witnesses) != 0 {
		t.Error("local runtime with supervisor identity token published a boot witness")
	}
}

func configureFoundryRuntimeRecoveryFixture(t *testing.T, f *runtimeRecoveryFixture) {
	t.Helper()
	f.server.Close()
	f.config.Profile.ProviderKind = "foundry"
	server, err := conformancetest.NewServer(f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	f.server = server
	updated, _ := testAgentRuntimeAndSecret(t, f.runtime.Spec.Deployment.Endpoint, f.config)
	f.runtime.Spec.Capabilities = updated.Spec.Capabilities
	if err := f.r.Update(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = f.r.recoveryDeployment(t.Context(), f.runtime)
	if err == nil {
		t.Fatal("a Foundry registration accepted the old one-container topology")
	}
	deployment := &appsv1.Deployment{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Spec.Template.Spec = foundryRecoveryPodSpec(deployment.Spec.Template.Spec)
	f.rs.Spec.Template = *deployment.Spec.Template.DeepCopy()
	f.rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "hash-1"
	f.pod.Spec = *deployment.Spec.Template.Spec.DeepCopy()
	f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, f.server.URL()))
	for _, object := range []client.Object{deployment, f.rs, f.pod, f.slice} {
		if err := f.r.Update(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	f.updateServiceTargetPort(t)
	status := f.pod.Status.ContainerStatuses[0]
	status.Name, status.ContainerID, status.ImageID = "broker", "containerd://broker-1", testControllerDigest("broker")
	f.pod.Status.ContainerStatuses = append(f.pod.Status.ContainerStatuses, status)
	if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRuntimeRecoveryFoundryUsesAuthenticatedDrainWithoutContainerFallback(t *testing.T) {
	for _, lostBoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "authenticated drain", true: "lost supervisor remains blocked"}[lostBoot], func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			configureFoundryRuntimeRecoveryFixture(t, f)
			f.reconcile(t)
			if !f.runtime.Status.Ready {
				t.Fatalf("Foundry topology did not conform: %s", f.runtime.Status.Message)
			}
			witness := f.witness(t)
			if lostBoot {
				if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
					t.Fatal(err)
				}
				f.pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: recoveryTerminal(witness)}
				if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
					t.Fatal(err)
				}
			}
			f.advanceEpoch(t)
			f.reconcile(t)
			f.reconcile(t)
			retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
			if err != nil || retired == lostBoot || f.runtime.Status.Ready {
				t.Fatalf("Foundry retirement=%t lostBoot=%t: %v", retired, lostBoot, err)
			}
			deployment, _, epoch, err := f.r.recoveryDeployment(t.Context(), f.runtime)
			if err != nil || epoch != map[bool]uint64{false: 2, true: 1}[lostBoot] {
				t.Fatalf("Foundry epoch transition=%d: %v", epoch, err)
			}
			if !reflect.DeepEqual(deployment.Spec.Template.Spec.Containers[0].Env, []corev1.EnvVar{{Name: "AZURE_FEDERATED_TOKEN_FILE", Value: "/identity/token"}}) {
				t.Fatal("recovery changed broker identity configuration")
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil || !controllerutil.ContainsFinalizer(f.pod, agentRuntimeRecoveryPodFinalizer) {
				t.Fatalf("live/missing-proof Foundry Pod lost retention: %v", err)
			}
		})
	}
}
