package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func runtimePoolToolboxTestObject(toolboxes ...harnessv2.RuntimeToolbox) *corev1alpha1.RuntimePool {
	pool := runtimePoolTestObject(1)
	profile, err := runtimePoolHarnessProfile(pool.Spec.Runtime.Profile)
	if err != nil {
		panic(err)
	}
	profile.Toolboxes = toolboxes
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		panic(err)
	}
	pool.Spec.Runtime.Profile.Digest = string(digest)
	pool.Spec.Runtime.Profile.Toolboxes = runtimePoolToolboxesFromProfile(toolboxes)
	return pool
}

func runtimePoolTestToolboxes() []harnessv2.RuntimeToolbox {
	return []harnessv2.RuntimeToolbox{
		{Image: "registry.example.com/tools/yq-jq@" + acpTestToolboxDigest, MountPath: "/opt/yq-jq", PathEntries: []string{"bin"}},
		{Image: "registry.example.com/tools/brew@" + acpTestToolboxDigest, MountPath: harnessv2.RuntimeToolboxHomebrewMountPath, PathEntries: []string{"bin", "sbin"}},
	}
}

func renderToolboxTemplate(t *testing.T, pool *corev1alpha1.RuntimePool, policy ACPToolboxPolicy) (*RuntimePoolReconciler, corev1.PodTemplateSpec) {
	t.Helper()
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	r.ToolboxPolicy = policy
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatalf("runtimePoolConfig: %v", err)
	}
	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	return r, r.runtimePoolPodTemplate(pool, cfg, selector, "auth", "provider")
}

func TestRuntimePoolPodTemplateUnchangedWithoutToolboxes(t *testing.T) {
	pool := runtimePoolTestObject(1)
	_, without := renderToolboxTemplate(t, pool, ACPToolboxPolicy{})
	_, enabled := renderToolboxTemplate(t, pool, acpTestToolboxPolicy())
	withoutJSON, _ := json.Marshal(without)
	enabledJSON, _ := json.Marshal(enabled)
	if string(withoutJSON) != string(enabledJSON) {
		t.Fatal("enabling the toolbox policy changed a template without toolboxes")
	}
	if len(enabled.Spec.InitContainers) != 0 || len(enabled.Spec.Volumes) != 5 || len(enabled.Spec.ImagePullSecrets) != 0 {
		t.Fatalf("template without toolboxes gained toolbox wiring: %#v", enabled.Spec)
	}
	for _, env := range enabled.Spec.Containers[0].Env {
		if env.Name == runtimePoolToolboxesEnv || env.Name == runtimePoolToolboxMountMethod {
			t.Fatalf("template without toolboxes projected %s", env.Name)
		}
	}
	if enabled.Spec.Containers[0].TerminationMessagePolicy != "" {
		t.Fatal("template without toolboxes changed the termination message policy")
	}
}

func TestRuntimePoolPodTemplateCopyModeWiresToolboxes(t *testing.T) {
	toolboxes := runtimePoolTestToolboxes()
	pool := runtimePoolToolboxTestObject(toolboxes...)
	policy := acpTestToolboxPolicy()
	policy.ImagePullSecrets = []string{"toolbox-pull", " ", "second-pull"}
	policy.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64", "kubernetes.io/os": "windows"}
	r, template := renderToolboxTemplate(t, pool, policy)
	assertRuntimePoolEnvironment(t, r, pool, template.Spec.Containers[0].Env)

	if len(template.Spec.InitContainers) != 3 {
		t.Fatalf("init containers = %d, want handoff plus two copies", len(template.Spec.InitContainers))
	}
	assertToolboxHandoffContainer(t, pool, template.Spec.InitContainers[0])
	for i, toolboxSpec := range toolboxes {
		assertToolboxCopyContainer(t, template, i, toolboxSpec)
	}
	assertToolboxInitHardening(t, template)
	assertToolboxCopyVolumes(t, template, len(toolboxes))
	assertToolboxProjection(t, template)
	if len(template.Spec.ImagePullSecrets) != 2 || template.Spec.ImagePullSecrets[0].Name != "toolbox-pull" || template.Spec.ImagePullSecrets[1].Name != "second-pull" {
		t.Fatalf("image pull secrets %#v", template.Spec.ImagePullSecrets)
	}
	if template.Spec.NodeSelector["kubernetes.io/arch"] != "amd64" || template.Spec.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Fatalf("node selector %#v (the Linux selector must never be overridden)", template.Spec.NodeSelector)
	}
	if err := ValidateACPToolboxNodeSelector(map[string]string{"kubernetes.io/os": "windows"}); err == nil {
		t.Fatal("a non-linux OS selector must be rejected at startup")
	}
	if err := ValidateACPToolboxNodeSelector(map[string]string{"kubernetes.io/os": "linux", "zone": "a"}); err != nil {
		t.Fatalf("linux OS selector must be accepted: %v", err)
	}
	// Pull secrets are Pod-level references, never container mounts or env.
	for _, container := range append(append([]corev1.Container(nil), template.Spec.InitContainers...), template.Spec.Containers...) {
		for _, mount := range container.VolumeMounts {
			if strings.Contains(mount.Name, "pull") {
				t.Fatalf("container %s mounts a pull secret", container.Name)
			}
		}
	}
	assertRuntimeContainerProtections(t, template)
}

func assertToolboxHandoffContainer(t *testing.T, pool *corev1alpha1.RuntimePool, handoff corev1.Container) {
	t.Helper()
	if handoff.Name != runtimePoolToolboxHandoffContainer || handoff.Image != pool.Spec.Runtime.Image ||
		strings.Join(handoff.Command, " ") != toolbox.SupervisorBinaryPath+" "+toolbox.HandoffSubcommand+" --dst /handoff" ||
		len(handoff.VolumeMounts) != 1 || handoff.VolumeMounts[0].Name != runtimePoolToolboxHandoffVolume || handoff.VolumeMounts[0].ReadOnly {
		t.Fatalf("unexpected handoff container %#v", handoff)
	}
}

func assertToolboxCopyContainer(t *testing.T, template corev1.PodTemplateSpec, i int, toolboxSpec harnessv2.RuntimeToolbox) {
	t.Helper()
	copier := template.Spec.InitContainers[i+1]
	if copier.Name != runtimePoolToolboxCopyContainerName(i) || copier.Image != toolboxSpec.Image || copier.WorkingDir != "/" ||
		copier.ImagePullPolicy != corev1.PullIfNotPresent || copier.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("unexpected copy container %#v", copier)
	}
	wantCommand := "/handoff/" + toolbox.HandoffBinaryName + " " + toolbox.CopySubcommand + " --src " + toolboxSpec.MountPath +
		" --dst /out --path-entries " + strings.Join(toolboxSpec.PathEntries, ",")
	if strings.Join(copier.Command, " ") != wantCommand {
		t.Fatalf("copy command = %q, want %q", strings.Join(copier.Command, " "), wantCommand)
	}
	if len(copier.VolumeMounts) != 2 || copier.VolumeMounts[0].Name != runtimePoolToolboxHandoffVolume || !copier.VolumeMounts[0].ReadOnly ||
		copier.VolumeMounts[1].Name != runtimePoolToolboxVolumeName(i) || copier.VolumeMounts[1].MountPath != "/out" || copier.VolumeMounts[1].ReadOnly {
		t.Fatalf("unexpected copy mounts %#v", copier.VolumeMounts)
	}
	for _, mount := range template.Spec.Containers[0].VolumeMounts {
		if mount.Name != runtimePoolToolboxVolumeName(i) {
			continue
		}
		if mount.MountPath != toolboxSpec.MountPath || mount.SubPath != toolbox.OutputRootName || !mount.ReadOnly {
			t.Fatalf("unexpected runtime mount %#v", mount)
		}
		return
	}
	t.Fatalf("runtime container is missing the mount for %s", toolboxSpec.MountPath)
}

func assertToolboxInitHardening(t *testing.T, template corev1.PodTemplateSpec) {
	t.Helper()
	for _, init := range template.Spec.InitContainers {
		sc := init.SecurityContext
		if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser == 0 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
			sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
			sc.Privileged == nil || *sc.Privileged || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || len(sc.Capabilities.Drop) != 1 ||
			sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Fatalf("init container %s is not hardened: %#v", init.Name, sc)
		}
		if *sc.RunAsUser >= 20000 && *sc.RunAsUser <= 29999 {
			t.Fatalf("init container %s uses a session identity", init.Name)
		}
		if init.Resources.Limits.Cpu().IsZero() || init.Resources.Limits.Memory().IsZero() {
			t.Fatalf("init container %s has no resource limits", init.Name)
		}
	}
	if *template.Spec.InitContainers[0].SecurityContext.RunAsUser == *template.Spec.InitContainers[1].SecurityContext.RunAsUser {
		t.Fatal("handoff and copy containers must use different identities")
	}
}

func assertToolboxCopyVolumes(t *testing.T, template corev1.PodTemplateSpec, count int) {
	t.Helper()
	volumes := map[string]corev1.Volume{}
	for _, volume := range template.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	if volumes[runtimePoolToolboxHandoffVolume].EmptyDir == nil || volumes[runtimePoolToolboxHandoffVolume].EmptyDir.SizeLimit.String() != "64Mi" {
		t.Fatalf("handoff volume %#v", volumes[runtimePoolToolboxHandoffVolume])
	}
	for i := range count {
		volume := volumes[runtimePoolToolboxVolumeName(i)]
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit.Value() <= toolbox.DefaultMaxTotalBytes {
			t.Fatalf("toolbox volume %d = %#v", i, volume)
		}
	}
}

func assertToolboxProjection(t *testing.T, template corev1.PodTemplateSpec) {
	t.Helper()
	environment := runtimePoolLiteralEnvironment(template.Spec.Containers[0].Env)
	var projected []harnessv2.RuntimeToolbox
	if err := json.Unmarshal([]byte(environment[runtimePoolToolboxesEnv]), &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected) != 2 || projected[1].PathEntries[1] != "sbin" || environment[runtimePoolToolboxMountMethod] != "copy" {
		t.Fatalf("projected toolboxes %#v method %q", projected, environment[runtimePoolToolboxMountMethod])
	}
	if template.Spec.Containers[0].TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatal("runtime container must fall back to logs for the toolbox FAIL line")
	}
}

func TestRuntimePoolPodTemplateImageVolumeMode(t *testing.T) {
	toolboxes := runtimePoolTestToolboxes()
	pool := runtimePoolToolboxTestObject(toolboxes...)
	policy := acpTestToolboxPolicy()
	policy.MountMethod = ACPToolboxMountImageVolume
	_, template := renderToolboxTemplate(t, pool, policy)
	if len(template.Spec.InitContainers) != 0 {
		t.Fatalf("image volume mode must not add init containers: %#v", template.Spec.InitContainers)
	}
	volumes := map[string]corev1.Volume{}
	for _, volume := range template.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	if _, present := volumes[runtimePoolToolboxHandoffVolume]; present {
		t.Fatal("image volume mode must not add the handoff volume")
	}
	for i, toolboxSpec := range toolboxes {
		volume := volumes[runtimePoolToolboxVolumeName(i)]
		if volume.Image == nil || volume.Image.Reference != toolboxSpec.Image || volume.Image.PullPolicy != corev1.PullIfNotPresent {
			t.Fatalf("toolbox volume %d = %#v", i, volume)
		}
		found := false
		for _, mount := range template.Spec.Containers[0].VolumeMounts {
			if mount.Name == runtimePoolToolboxVolumeName(i) {
				found = true
				if mount.MountPath != toolboxSpec.MountPath || mount.SubPath != strings.TrimPrefix(toolboxSpec.MountPath, "/") || !mount.ReadOnly {
					t.Fatalf("unexpected runtime mount %#v", mount)
				}
			}
		}
		if !found {
			t.Fatalf("runtime container is missing the mount for %s", toolboxSpec.MountPath)
		}
	}
	environment := runtimePoolLiteralEnvironment(template.Spec.Containers[0].Env)
	if environment[runtimePoolToolboxMountMethod] != "imageVolume" {
		t.Fatalf("mount method env = %q", environment[runtimePoolToolboxMountMethod])
	}
	assertRuntimeContainerProtections(t, template)
	// The mount method changes the template, never the profile digest.
	copyPolicy := acpTestToolboxPolicy()
	_, copyTemplate := renderToolboxTemplate(t, pool, copyPolicy)
	if copyTemplate.Annotations[runtimePoolTemplateRevisionAnnotation] == template.Annotations[runtimePoolTemplateRevisionAnnotation] {
		t.Fatal("changing the mount method must change the template revision")
	}
	if copyTemplate.Annotations[runtimePoolProfileAnnotation] != template.Annotations[runtimePoolProfileAnnotation] {
		t.Fatal("changing the mount method must not change the profile digest")
	}
}

// assertRuntimeContainerProtections guards the properties image volumes rely
// on: the runtime container stays non-privileged with no privilege
// escalation, and toolbox wiring never adds capabilities.
func assertRuntimeContainerProtections(t *testing.T, template corev1.PodTemplateSpec) {
	t.Helper()
	sc := template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.Privileged == nil || *sc.Privileged {
		t.Fatalf("runtime container lost allowPrivilegeEscalation=false or privileged=false: %#v", sc)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("runtime container must drop ALL capabilities: %#v", sc.Capabilities)
	}
	for _, capability := range sc.Capabilities.Add {
		switch capability {
		case "CHOWN", "KILL", "SETGID", "SETUID":
		default:
			t.Fatalf("toolbox wiring added capability %s", capability)
		}
	}
	if template.Spec.SecurityContext == nil || template.Spec.SecurityContext.SeccompProfile == nil ||
		template.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("runtime Pod lost its seccomp profile")
	}
	if template.Spec.HostPID || template.Spec.HostNetwork || template.Spec.HostIPC {
		t.Fatal("runtime Pod gained host namespaces")
	}
}

func TestRuntimePoolToolboxAdmissionFailsClosed(t *testing.T) {
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		policy ACPToolboxPolicy
		pool   *corev1alpha1.RuntimePool
		want   string
	}{
		"disabled":         {policy: ACPToolboxPolicy{}, pool: pool, want: "disabled"},
		"unavailable":      {policy: ACPToolboxPolicy{Enabled: true, UnavailableReason: "image volumes need Kubernetes 1.36 or newer"}, pool: pool, want: "1.36"},
		"bad method":       {policy: ACPToolboxPolicy{Enabled: true, MountMethod: "hostPath"}, pool: pool, want: "mount method"},
		"registry removed": {policy: ACPToolboxPolicy{Enabled: true, AllowedRegistries: []string{"ghcr.io/other"}, MountMethod: ACPToolboxMountCopy}, pool: pool, want: "no longer from an allowed toolbox registry"},
		"empty allowlist":  {policy: ACPToolboxPolicy{Enabled: true, MountMethod: ACPToolboxMountCopy}, pool: pool, want: "no longer from an allowed toolbox registry"},
		"workspace pool": {policy: acpTestToolboxPolicy(), pool: func() *corev1alpha1.RuntimePool {
			workspacePool := pool.DeepCopy()
			workspacePool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: corev1alpha1.WorkspaceProviderAgentSandbox}
			return workspacePool
		}(), want: "execution-workspace"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r.ToolboxPolicy = tc.policy
			err := r.runtimePoolToolboxAdmission(tc.pool, cfg)
			if err == nil || !strings.HasPrefix(err.Error(), acpToolboxUnavailablePrefix) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want ToolboxUnavailable with %q", err, tc.want)
			}
		})
	}
	r.ToolboxPolicy = acpTestToolboxPolicy()
	if err := r.runtimePoolToolboxAdmission(pool, cfg); err != nil {
		t.Fatalf("enabled policy: %v", err)
	}
	plain := runtimePoolTestObject(1)
	r.ToolboxPolicy = ACPToolboxPolicy{}
	plainCfg, err := r.runtimePoolConfig(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.runtimePoolToolboxAdmission(plain, plainCfg); err != nil {
		t.Fatalf("pool without toolboxes must not be gated: %v", err)
	}
}

func TestRuntimePoolReconcileReportsToolboxUnavailableWhenDisabled(t *testing.T) {
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	// Admit the pool first so a Deployment exists, then disable toolboxes.
	r.ToolboxPolicy = acpTestToolboxPolicy()
	runtimePoolReconcile(t, r, pool)
	deployment := runtimePoolTestDeployment(t, r, pool.Namespace, runtimePoolResourceName(pool.Namespace, pool.Name))
	if ptr.Deref(deployment.Spec.Replicas, 0) != 1 {
		t.Fatalf("admitted pool Deployment replicas = %d, want 1", ptr.Deref(deployment.Spec.Replicas, 0))
	}
	r.ToolboxPolicy = ACPToolboxPolicy{}
	runtimePoolReconcile(t, r, pool)
	deployment = runtimePoolTestDeployment(t, r, pool.Namespace, runtimePoolResourceName(pool.Namespace, pool.Name))
	if ptr.Deref(deployment.Spec.Replicas, 0) != 0 {
		t.Fatalf("Deployment of a pool whose toolboxes are no longer admitted must scale to zero, got %d replicas", ptr.Deref(deployment.Spec.Replicas, 0))
	}
	var current corev1alpha1.RuntimePool
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &current); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, corev1alpha1.RuntimePoolConditionRolloutReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != corev1alpha1.RuntimePoolReasonToolboxUnavailable {
		t.Fatalf("rollout condition = %#v", condition)
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
		t.Fatalf("status = %#v", current.Status)
	}
	message, ok := runtimePoolToolboxUnavailableMessage(&current)
	if !ok || !strings.Contains(message, "disabled") {
		t.Fatalf("message = %q ok=%v", message, ok)
	}
}

// A pool that loses toolbox admission while its supervisor still serves a
// session keeps its workload until the live probe reports it idle; the probe
// is consulted directly because the toolbox gate runs before the rollout
// path that refreshes the pool's capacity counters.
func TestRuntimePoolToolboxFailureScalesDownOnlyWhenSupervisorIsIdle(t *testing.T) {
	scheme := runtimePoolTestScheme(t)
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	pod := runtimePoolReadyPod(pool, pool.Namespace, "codex-pod", "pod-uid-1", "10.0.0.21")
	supervisor := &fakeRuntimePoolSupervisorClient{probe: runtimePoolValidProbe(pool, &pod, "boot-1", false)}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool, &pod)
	r.ToolboxPolicy = acpTestToolboxPolicy()
	runtimePoolReconcile(t, r, pool)
	name := runtimePoolResourceName(pool.Namespace, pool.Name)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 1 {
		t.Fatal("admitted pool must run one replica")
	}

	// The supervisor is busy: toolboxes get disabled, admission closes, but
	// the workload stays up.
	supervisor.probe.Status.Pressure.ActivePrompts = 1
	r.ToolboxPolicy = ACPToolboxPolicy{}
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 1 {
		t.Fatal("a busy supervisor must not be scaled away")
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || !strings.Contains(current.Status.Message, "once its sessions and prompts finish") {
		t.Fatalf("status = %#v", current.Status)
	}

	// The prompt finishes: the next reconcile observes the live idle probe
	// and scales the workload to zero.
	supervisor.probe.Status.Pressure.ActivePrompts = 0
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 0 {
		t.Fatal("an idle supervisor must be scaled to zero")
	}
	if current := runtimePoolTestGetPool(t, r, pool); !strings.Contains(current.Status.Message, "scaled to zero") {
		t.Fatalf("status = %#v", current.Status)
	}
}

func TestRuntimePoolToolboxFailureClassification(t *testing.T) {
	toolboxes := runtimePoolTestToolboxes()
	pod := func(mutate func(*corev1.Pod)) []corev1.Pod {
		p := corev1.Pod{}
		mutate(&p)
		return []corev1.Pod{p}
	}
	cases := map[string]struct {
		pods      []corev1.Pod
		toolboxes []harnessv2.RuntimeToolbox
		want      string
		ok        bool
	}{
		"copy init container failed with stable line": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name: runtimePoolToolboxCopyContainerName(0),
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1, Message: "noise\nFAIL reason=TOOLBOX_ARCH_MISMATCH msg=/opt/yq-jq/bin/yq is built for arm64 but this node runs amd64\n",
					}},
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_ARCH_MISMATCH: toolbox-copy-0: /opt/yq-jq/bin/yq is built for arm64", ok: true,
		},
		"handoff image pull is a runtime-image failure, not a toolbox failure": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimePoolToolboxHandoffContainer,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
				}}
			}),
			toolboxes: toolboxes, ok: false,
		},
		"copy init container image pull": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimePoolToolboxCopyContainerName(1),
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_IMAGE_PULL: toolbox-copy-1: ImagePullBackOff", ok: true,
		},
		"handoff failed": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimePoolToolboxHandoffContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "FAIL reason=TOOLBOX_COPY_FAILED msg=disk full"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_COPY_FAILED: toolbox-handoff: disk full", ok: true,
		},
		"supervisor startup check failed": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: runtimeField,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1, Message: "FAIL reason=TOOLBOX_MISSING_PATH_ENTRY msg=toolbox /opt/yq-jq path entry bin: not a folder",
					}},
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_MISSING_PATH_ENTRY: runtime:", ok: true,
		},
		"image volume mount failed": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimeField,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerError", Message: "failed to generate container spec: ImageVolumeMountFailed: subPath opt/yq-jq does not exist"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_MOUNT_FAILED:", ok: true,
		},
		"image volume pull failed": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimeField,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "failed to pull " + toolboxes[0].Image}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_IMAGE_PULL:", ok: true,
		},
		"copy init container OOM killed without stable line": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:                 runtimePoolToolboxCopyContainerName(0),
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
					State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_COPY_FAILED: toolbox-copy-0: exited 137 (OOMKilled)", ok: true,
		},
		"copy init container cannot be created": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimePoolToolboxCopyContainerName(0),
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerError", Message: "mount /handoff: not a directory"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_MOUNT_FAILED: toolbox-copy-0: CreateContainerError mount /handoff", ok: true,
		},
		"copy init container recovered after an earlier failure": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:                 runtimePoolToolboxCopyContainerName(0),
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "FAIL reason=TOOLBOX_COPY_FAILED msg=interrupted"}},
					State:                corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
				}}
			}),
			toolboxes: toolboxes, ok: false,
		},
		"runtime container running after an earlier toolbox failure": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:                 runtimeField,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "FAIL reason=TOOLBOX_MISSING_MOUNT msg=old"}},
					State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}
			}),
			toolboxes: toolboxes, ok: false,
		},
		"runtime image pull is not a toolbox failure": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimeField,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "failed to pull docker.io/sozercan/orka-acp"}},
				}}
			}),
			toolboxes: toolboxes, ok: false,
		},
		"runtime crash without stable line": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:                 runtimeField,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Message: "panic"}},
				}}
			}),
			toolboxes: toolboxes, ok: false,
		},
		"no toolboxes": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimePoolToolboxCopyContainerName(0),
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "FAIL reason=TOOLBOX_TOO_DEEP msg=x"}},
				}}
			}),
			toolboxes: nil, ok: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reason, message, ok := runtimePoolToolboxFailure(tc.pods, tc.toolboxes)
			if ok != tc.ok {
				t.Fatalf("ok = %v (%q), want %v", ok, message, tc.ok)
			}
			if !ok {
				return
			}
			if reason != corev1alpha1.RuntimePoolReasonToolboxUnavailable || !strings.HasPrefix(message, tc.want) {
				t.Fatalf("reason=%s message=%q, want prefix %q", reason, message, tc.want)
			}
		})
	}
}

func TestRuntimePoolToolboxesFromEnvironment(t *testing.T) {
	if toolboxes, err := runtimePoolToolboxesFromEnvironment(map[string]string{}); err != nil || toolboxes != nil {
		t.Fatalf("absent env: %v %#v", err, toolboxes)
	}
	if toolboxes, err := runtimePoolToolboxesFromEnvironment(map[string]string{runtimePoolToolboxesEnv: "[]"}); err != nil || toolboxes != nil {
		t.Fatalf("empty list: %v %#v", err, toolboxes)
	}
	if _, err := runtimePoolToolboxesFromEnvironment(map[string]string{runtimePoolToolboxesEnv: "{"}); err == nil {
		t.Fatal("malformed JSON must fail")
	}
	if _, err := runtimePoolToolboxesFromEnvironment(map[string]string{runtimePoolToolboxesEnv: `[{"image":"x","mountPath":"/opt/x"}]`}); err == nil {
		t.Fatal("invalid toolbox must fail")
	}
	encoded, err := runtimePoolToolboxesJSON(runtimePoolTestToolboxes())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := runtimePoolToolboxesFromEnvironment(map[string]string{runtimePoolToolboxesEnv: encoded})
	if err != nil || len(decoded) != 2 || decoded[0].MountPath != "/opt/yq-jq" {
		t.Fatalf("round trip: %v %#v", err, decoded)
	}
}

// A previously active runtime Pod that restarts and fails its toolbox checks
// must report ToolboxUnavailable (not the generic RolloutFailed) so waiting
// Tasks fail instead of retrying against a fence that can never serve again.
func TestRuntimePoolReconcileReportsToolboxUnavailableForRestartedActivePod(t *testing.T) {
	scheme := runtimePoolTestScheme(t)
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	pod := runtimePoolReadyPod(pool, pool.Namespace, "codex-pod", "pod-uid-1", "10.0.0.21")
	supervisor := &fakeRuntimePoolSupervisorClient{probe: runtimePoolValidProbe(pool, &pod, "boot-1", false)}
	r := runtimePoolTestReconciler(t, scheme, supervisor, pool, &pod)
	r.ToolboxPolicy = acpTestToolboxPolicy()

	runtimePoolReconcile(t, r, pool)
	var current corev1alpha1.RuntimePool
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.ActiveInstance == nil || current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("pool with toolboxes did not become serving: %#v", current.Status)
	}

	// The active Pod restarts and the supervisor's startup toolbox check fails.
	runtimePoolTestSetPodReady(t, r, &pod, false)
	currentPod := &corev1.Pod{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(&pod), currentPod); err != nil {
		t.Fatal(err)
	}
	currentPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: runtimeField,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Message: "FAIL reason=TOOLBOX_MISSING_PATH_ENTRY msg=toolbox /opt/yq-jq path entry bin: not a folder",
		}},
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	if err := r.Status().Update(context.Background(), currentPod); err != nil {
		t.Fatal(err)
	}

	runtimePoolReconcile(t, r, pool)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &current); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, corev1alpha1.RuntimePoolConditionRolloutReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != corev1alpha1.RuntimePoolReasonToolboxUnavailable ||
		!strings.Contains(condition.Message, "TOOLBOX_MISSING_PATH_ENTRY") {
		t.Fatalf("rollout condition = %#v", condition)
	}
	if current.Status.ActiveInstance == nil {
		t.Fatal("the exact active-instance fence must be preserved while admission is closed")
	}
	if _, ok := runtimePoolToolboxUnavailableMessage(&current); !ok {
		t.Fatal("dispatcher settlement must see the ToolboxUnavailable condition")
	}
}
