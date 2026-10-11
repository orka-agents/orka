package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
		strings.Join(handoff.Command, " ") != toolbox.SupervisorBinaryPath || len(handoff.Args) == 0 ||
		strings.Join(handoff.Args, " ") != toolbox.HandoffSubcommand+" --dst /handoff" ||
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
	wantArgs := toolbox.CopySubcommand + " --src " + toolboxSpec.MountPath +
		" --dst /out --path-entries " + strings.Join(toolboxSpec.PathEntries, ",")
	if strings.Join(copier.Command, " ") != "/handoff/"+toolbox.HandoffBinaryName || strings.Join(copier.Args, " ") != wantArgs {
		t.Fatalf("copy invocation = %q %q, want command %q args %q", copier.Command, copier.Args, "/handoff/"+toolbox.HandoffBinaryName, wantArgs)
	}
	if len(copier.Args) == 0 {
		t.Fatal("copy container must set non-empty Args so the image CMD cannot reach the copier")
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
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit.Value() < toolbox.DefaultMaxTotalBytes+toolbox.DefaultMaxEntries*runtimePoolToolboxBlockBytes {
			t.Fatalf("toolbox volume %d = %#v (must reserve a block per permitted entry)", i, volume)
		}
	}
	// The Pod's ephemeral-storage budget grows by the handoff volume plus one
	// full toolbox volume per toolbox, so kubelet never evicts a filling Pod.
	base := runtimePoolResourceRequirements(runtimePoolResourceClassStandard)
	want := base.Limits.StorageEphemeral().Value() + runtimePoolToolboxHandoffBytes + int64(count)*runtimePoolToolboxVolumeBytes
	if got := template.Spec.Containers[0].Resources.Limits.StorageEphemeral().Value(); got != want {
		t.Fatalf("runtime ephemeral-storage limit = %d, want %d", got, want)
	}
	wantRequest := base.Requests.StorageEphemeral().Value() + runtimePoolToolboxHandoffBytes + int64(count)*runtimePoolToolboxVolumeBytes
	if got := template.Spec.Containers[0].Resources.Requests.StorageEphemeral().Value(); got != wantRequest {
		t.Fatalf("runtime ephemeral-storage request = %d, want %d", got, wantRequest)
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
	// First reconcile persists the admission-closed barrier only.
	runtimePoolReconcile(t, r, pool)
	deployment = runtimePoolTestDeployment(t, r, pool.Namespace, runtimePoolResourceName(pool.Namespace, pool.Name))
	if ptr.Deref(deployment.Spec.Replicas, 0) != 1 {
		t.Fatalf("the barrier reconcile must not touch the Deployment, got %d replicas", ptr.Deref(deployment.Spec.Replicas, 0))
	}
	// With the barrier persisted and no Pod able to run anything, the next
	// reconcile scales the workload to zero.
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

	// Toolboxes get disabled while the supervisor is busy. The first reconcile
	// persists only the admission-closed barrier and keeps the exact fence.
	supervisor.probe.Status.Pressure.ActivePrompts = 1
	supervisor.probe.Status.Pressure.ResidentSessions = 1
	supervisor.probe.Status.Sessions = []harnessv2.RuntimeSessionStatus{{RuntimeSessionID: "busy-session", RuntimeSessionUID: "busy-session-uid", Generation: 1, State: harnessv2.RuntimeSessionStatePromptRunning, ActivePromptID: "busy-prompt", LastTransitionAt: runtimePoolTestNow}}
	supervisor.probe.Status.ActivePrompts = []harnessv2.ActivePromptStatus{{RuntimeSessionUID: "busy-session-uid", SessionGeneration: 1, TaskUID: "busy-task", TaskAttempt: 1, PromptID: "busy-prompt", LeaseExpiresAt: runtimePoolTestNow.Add(time.Minute), FrameSequence: 1, StartedAt: runtimePoolTestNow}}
	r.ToolboxPolicy = ACPToolboxPolicy{}
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || current.Status.ActiveInstance == nil ||
		meta.FindStatusCondition(current.Status.Conditions, corev1alpha1.RuntimePoolConditionRolloutReady).Reason != corev1alpha1.RuntimePoolReasonToolboxUnavailable {
		t.Fatalf("barrier status = %#v", current.Status)
	}
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 1 {
		t.Fatal("the barrier reconcile must not touch the Deployment")
	}

	// With the barrier persisted, a busy supervisor still keeps the workload
	// and the fence.
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 1 {
		t.Fatal("a busy supervisor must not be scaled away")
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil || !strings.Contains(current.Status.Message, "once its sessions and prompts finish") {
		t.Fatalf("busy status = %#v", current.Status)
	}

	// A live descendant alone also keeps it; the idle invariant is complete.
	supervisor.probe.Status.Pressure.ActivePrompts = 0
	supervisor.probe.Status.Pressure.ResidentSessions = 0
	supervisor.probe.Status.Sessions = nil
	supervisor.probe.Status.ActivePrompts = nil
	supervisor.probe.Status.Pressure.LiveDescendants = 1
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 1 {
		t.Fatal("a supervisor with live descendants must not be scaled away")
	}

	// A NotReady active Pod is a readiness blip, not idleness.
	supervisor.probe.Status.Pressure.LiveDescendants = 0
	runtimePoolTestSetPodReady(t, r, &pod, false)
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 1 {
		t.Fatal("a NotReady active Pod must not be treated as idle")
	}
	runtimePoolTestSetPodReady(t, r, &pod, true)

	// Even with nothing running, the first idle observation only requests a
	// supervisor drain; the workload stays until a later probe shows the
	// drained supervisor quiescent.
	drainCallsBefore := supervisor.drainCalls
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != drainCallsBefore+1 || supervisor.drainReason != harnessv2.DrainReasonToolboxUnavailable {
		t.Fatalf("drain calls = %d (reason %q), want one toolbox drain request", supervisor.drainCalls, supervisor.drainReason)
	}
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 1 {
		t.Fatal("the workload must stay until the drained supervisor is observed quiescent")
	}
	// The supervisor confirms the drain and reports the complete quiescence
	// invariant: the workload is scaled to zero and the fence cleared only now.
	supervisor.probe.Status.Drain.Requested = true
	supervisor.probe.Status.Drain.RequestedAt = runtimePoolTestNow
	supervisor.probe.Status.Drain.Reason = harnessv2.DrainReasonToolboxUnavailable
	supervisor.probe.Status.Lifecycle = harnessv2.SupervisorLifecycleDraining
	supervisor.probe.Status.Drain.AcceptingNewSessions = false
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, name).Spec.Replicas, 0) != 0 {
		t.Fatal("an idle supervisor must be scaled to zero")
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance != nil || !strings.Contains(current.Status.Message, "scaled to zero") {
		t.Fatalf("stopped status = %#v", current.Status)
	}
}

func TestRuntimePoolToolboxFailureClassification(t *testing.T) {
	toolboxes := runtimePoolTestToolboxes()
	pod := func(mutate func(*corev1.Pod)) []corev1.Pod {
		p := corev1.Pod{}
		mutate(&p)
		setToolboxPullObservation(t, &p, runtimePoolTestNow.Add(-runtimePoolToolboxPullRetryWindow))
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
		"image volume subPath reported as a config error": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  runtimeField,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: "failed to prepare subPath for volumeMount \"toolbox-0\""}},
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
		"copy init container OOM killed without stable line after bounded retries": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:                 runtimePoolToolboxCopyContainerName(0),
					RestartCount:         runtimePoolToolboxUnexplainedRestartLimit,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
					State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}}
			}),
			toolboxes: toolboxes, want: "ToolboxUnavailable: TOOLBOX_COPY_FAILED: toolbox-copy-0: exited 137 (OOMKilled)", ok: true,
		},
		"copy init container interrupted once still gets its idempotent retry": {
			pods: pod(func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:                 runtimePoolToolboxCopyContainerName(0),
					RestartCount:         1,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
					State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}}
			}),
			toolboxes: toolboxes, ok: false,
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
			reason, message, ok := runtimePoolToolboxFailure(tc.pods, tc.toolboxes, runtimePoolTestNow)
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

func TestRuntimePoolToolboxImagePullRecoveryWindow(t *testing.T) {
	for _, copyMode := range []bool{true, false} {
		for _, reason := range []string{podWaitingReasonErrImagePull, podWaitingReasonImagePullBackOff, podWaitingReasonRegistryUnavailable, podWaitingReasonInvalidImageName} {
			t.Run(fmt.Sprintf("copy=%t/%s", copyMode, reason), func(t *testing.T) {
				toolboxes := runtimePoolTestToolboxes()
				status := corev1.ContainerStatus{Name: runtimeField, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "registry timeout pulling " + toolboxes[0].Image}}}
				pod := corev1.Pod{Status: corev1.PodStatus{StartTime: new(metav1.NewTime(runtimePoolTestNow))}}
				if copyMode {
					status.Name = runtimePoolToolboxCopyContainerName(0)
					pod.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
				} else {
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
				}
				setToolboxPullObservation(t, &pod, runtimePoolTestNow)
				_, _, immediate := runtimePoolToolboxFailure([]corev1.Pod{pod}, toolboxes, runtimePoolTestNow)
				if immediate != (reason == podWaitingReasonInvalidImageName) {
					t.Fatalf("immediate failure = %t", immediate)
				}
				if reason != podWaitingReasonInvalidImageName {
					if _, _, failed := runtimePoolToolboxFailure([]corev1.Pod{pod}, toolboxes, runtimePoolTestNow.Add(runtimePoolToolboxPullRetryWindow-time.Nanosecond)); failed {
						t.Fatal("failed before retry window expired")
					}
				}
				if _, _, failed := runtimePoolToolboxFailure([]corev1.Pod{pod}, toolboxes, runtimePoolTestNow.Add(runtimePoolToolboxPullRetryWindow)); !failed {
					t.Fatal("persistent pull failure was not bounded")
				}
				if copyMode {
					pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
				} else {
					pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
				}
				if _, _, failed := runtimePoolToolboxFailure([]corev1.Pod{pod}, toolboxes, runtimePoolTestNow.Add(2*runtimePoolToolboxPullRetryWindow)); failed {
					t.Fatal("recovered pull retained a terminal failure")
				}
			})
		}
	}
}

func setToolboxPullObservation(t *testing.T, pod *corev1.Pod, first time.Time) {
	t.Helper()
	observations := map[string]runtimePoolToolboxPullFailure{}
	for _, status := range append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		observations[status.Name] = runtimePoolToolboxPullFailure{FirstObserved: first, RestartCount: status.RestartCount}
	}
	data, err := json.Marshal(observations)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[runtimePoolToolboxPullFailuresAnnotation] = string(data)
}

func TestRuntimePoolToolboxPullObservationsSurviveControllerRestart(t *testing.T) {
	for _, copyMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("copy=%t", copyMode), func(t *testing.T) {
			ctx := context.Background()
			pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
			pod := runtimePoolPendingPod(pool, pool.Namespace, "delayed-pull-pod", "delayed-pull-uid")
			pod.CreationTimestamp = metav1.NewTime(runtimePoolTestNow.Add(-time.Hour))
			pod.Status.StartTime = new(metav1.NewTime(runtimePoolTestNow.Add(-time.Hour)))
			status := corev1.ContainerStatus{Name: runtimeField, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: podWaitingReasonErrImagePull, Message: "timeout pulling " + runtimePoolTestToolboxes()[0].Image}}}
			setStatus := func(p *corev1.Pod, status corev1.ContainerStatus) {
				if copyMode {
					status.Name = runtimePoolToolboxCopyContainerName(0)
					p.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
				} else {
					p.Status.ContainerStatuses = []corev1.ContainerStatus{status}
				}
			}
			setStatus(&pod, status)
			r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool, &pod)
			pods := []corev1.Pod{pod}
			if err := r.observeRuntimePoolToolboxPullFailures(ctx, pods, runtimePoolTestToolboxes()); err != nil {
				t.Fatal(err)
			}
			if _, _, failed := runtimePoolToolboxFailure(pods, runtimePoolTestToolboxes(), runtimePoolTestNow); failed {
				t.Fatal("first pull failure inherited unrelated startup delay")
			}
			restarted := &RuntimePoolReconciler{Client: r.Client, ControllerEpoch: r.ControllerEpoch + 1,
				Now: func() time.Time { return runtimePoolTestNow.Add(runtimePoolToolboxPullRetryWindow) }}
			fresh := &corev1.Pod{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(&pod), fresh); err != nil {
				t.Fatal(err)
			}
			pods = []corev1.Pod{*fresh}
			if err := restarted.observeRuntimePoolToolboxPullFailures(ctx, pods, runtimePoolTestToolboxes()); err != nil {
				t.Fatal(err)
			}
			if _, _, failed := runtimePoolToolboxFailure(pods, runtimePoolTestToolboxes(), restarted.now()); !failed {
				t.Fatal("persistent pull was not bounded across controller restart")
			}
			fresh = &pods[0]
			status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			setStatus(fresh, status)
			if err := r.Status().Update(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			if err := restarted.observeRuntimePoolToolboxPullFailures(ctx, pods, runtimePoolTestToolboxes()); err != nil {
				t.Fatal(err)
			}
			if pods[0].Annotations[runtimePoolToolboxPullFailuresAnnotation] != "" {
				t.Fatal("recovered pull retained its failure episode")
			}
			status.RestartCount++
			status.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: podWaitingReasonImagePullBackOff, Message: "timeout pulling " + runtimePoolTestToolboxes()[0].Image}}
			setStatus(fresh, status)
			if err := r.Status().Update(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			if err := restarted.observeRuntimePoolToolboxPullFailures(ctx, pods, runtimePoolTestToolboxes()); err != nil {
				t.Fatal(err)
			}
			if _, _, failed := runtimePoolToolboxFailure(pods, runtimePoolTestToolboxes(), restarted.now()); failed {
				t.Fatal("new failure episode inherited an expired retry window")
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

type toolboxDeployedAuthSupervisor struct {
	*fakeRuntimePoolSupervisorClient
	token      []byte
	capability []byte
	rejected   int
}

func (s *toolboxDeployedAuthSupervisor) Probe(ctx context.Context, endpoint, token string, capability []byte) (RuntimePoolProbeResult, error) {
	if !bytes.Equal([]byte(token), s.token) || !bytes.Equal(capability, s.capability) {
		s.rejected++
		return RuntimePoolProbeResult{}, errors.New("credentials do not match deployed instance")
	}
	return s.fakeRuntimePoolSupervisorClient.Probe(ctx, endpoint, token, capability)
}

func TestRuntimePoolToolboxPolicyRemovalDrainsAcrossEpochChange(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("disabled=%t", disabled), func(t *testing.T) {
			pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
			supervisor := &fakeRuntimePoolSupervisorClient{}
			r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
			r.ToolboxPolicy = acpTestToolboxPolicy()
			deployment, _ := runtimePoolTestStartServing(t, r, pool, supervisor, "epoch-pod", "epoch-pod-uid", "10.0.0.82", "epoch-boot")
			auth, err := r.runtimePoolDeploymentAuthSecret(context.Background(), deployment)
			if err != nil {
				t.Fatal(err)
			}
			checked := &toolboxDeployedAuthSupervisor{fakeRuntimePoolSupervisorClient: supervisor, token: auth.Data[runtimePoolControllerTokenKey], capability: auth.Data[runtimePoolCapabilitySecretKey]}
			r.SupervisorClient = checked
			r.ControllerEpoch++
			if disabled {
				r.ToolboxPolicy = ACPToolboxPolicy{}
			} else {
				r.ToolboxPolicy.AllowedRegistries = []string{"registry.example.com/other"}
			}
			runtimePoolReconcile(t, r, pool)
			runtimePoolReconcile(t, r, pool)
			if checked.rejected != 0 || supervisor.drainCalls != 1 {
				t.Fatalf("deployed authentication rejected=%d drain calls=%d", checked.rejected, supervisor.drainCalls)
			}
			supervisor.probe.Status.Drain.Requested = true
			supervisor.probe.Status.Drain.RequestedAt = runtimePoolTestNow
			supervisor.probe.Status.Drain.Reason = harnessv2.DrainReasonToolboxUnavailable
			supervisor.probe.Status.Lifecycle = harnessv2.SupervisorLifecycleDraining
			supervisor.probe.Status.Drain.AcceptingNewSessions = false
			runtimePoolReconcile(t, r, pool)
			if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, deployment.Name).Spec.Replicas, 0) != 0 {
				t.Fatal("policy-rejected deployed instance did not retire")
			}
		})
	}
}

func TestRuntimePoolToolboxRetirementPreservesTaskCleanupReceipt(t *testing.T) {
	ctx := context.Background()
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
	r.ToolboxPolicy = acpTestToolboxPolicy()
	deployment, _ := runtimePoolTestStartServing(t, r, pool, supervisor, "receipt-pod", "receipt-pod-uid", "10.0.0.83", "receipt-boot")
	current := runtimePoolTestGetPool(t, r, pool)
	task := runtimePoolRetirementTask(t, &current, "toolbox-retirement-task")
	if err := r.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	r.ToolboxPolicy = ACPToolboxPolicy{}
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	supervisor.probe.Status.Drain.Requested = true
	supervisor.probe.Status.Drain.RequestedAt = runtimePoolTestNow
	supervisor.probe.Status.Drain.Reason = harnessv2.DrainReasonToolboxUnavailable
	supervisor.probe.Status.Lifecycle = harnessv2.SupervisorLifecycleDraining
	supervisor.probe.Status.Drain.AcceptingNewSessions = false
	// A lost receipt write must retain the authenticated instance for retry.
	failReceipt := true
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); isTask && failReceipt {
				failReceipt = false
				return errors.New("injected retirement receipt write failure")
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err == nil || !strings.Contains(err.Error(), "injected retirement receipt") {
		t.Fatalf("receipt write failure = %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil || ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, deployment.Name).Spec.Replicas, 0) != 1 {
		t.Fatal("lost receipt write discarded the runtime retirement fence")
	}
	runtimePoolReconcile(t, r, pool)
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
		t.Fatal("toolbox retirement lost exactly bound Session Task cleanup proof")
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance != nil || ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, deployment.Name).Spec.Replicas, 0) != 0 {
		t.Fatal("quiescent instance was not retired after its receipt persisted")
	}
}

func TestRuntimePoolToolboxDrainRejectsMismatchedProbeFence(t *testing.T) {
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
	r.ToolboxPolicy = acpTestToolboxPolicy()
	deployment, _ := runtimePoolTestStartServing(t, r, pool, supervisor, "fence-pod", "fence-pod-uid", "10.0.0.84", "fence-boot")
	r.ToolboxPolicy = ACPToolboxPolicy{}
	runtimePoolReconcile(t, r, pool)
	supervisor.probe.Status.Fence.SupervisorBootID = "different-boot"
	supervisor.probe.Status.Drain.Requested = true
	supervisor.probe.Status.Drain.RequestedAt = runtimePoolTestNow
	supervisor.probe.Status.Drain.Reason = harnessv2.DrainReasonToolboxUnavailable
	supervisor.probe.Status.Lifecycle = harnessv2.SupervisorLifecycleDraining
	supervisor.probe.Status.Drain.AcceptingNewSessions = false
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err == nil {
		t.Fatal("toolbox retirement accepted a mismatched authenticated probe")
	}
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, deployment.Name).Spec.Replicas, 0) != 1 {
		t.Fatal("mismatched probe scaled away the active instance")
	}
}

func TestRuntimePoolRecoversFromTransientToolboxImagePull(t *testing.T) {
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	pod := runtimePoolReadyPod(pool, pool.Namespace, "recovering-toolbox-pod", "recovering-toolbox-pod-uid", "10.0.0.85")
	pod.Status.StartTime = new(metav1.NewTime(runtimePoolTestNow))
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: runtimePoolToolboxCopyContainerName(0), State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: podWaitingReasonErrImagePull, Message: "registry request timed out"}}}}
	supervisor := &fakeRuntimePoolSupervisorClient{probe: runtimePoolValidProbe(pool, &pod, "recovering-boot", false)}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool, &pod)
	r.ToolboxPolicy = acpTestToolboxPolicy()
	runtimePoolReconcile(t, r, pool)
	current := runtimePoolTestGetPool(t, r, pool)
	if _, unavailable := runtimePoolToolboxUnavailableMessage(&current); unavailable {
		t.Fatal("transient pull failure would terminalize waiting Tasks")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(&pod), &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	if err := r.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("recovered pool = %s/%s", current.Status.Lifecycle, current.Status.AdmissionState)
	}
}

func TestRuntimePoolToolboxPolicyRemovalRecyclesInPlaceSupervisorRestart(t *testing.T) {
	ctx := context.Background()
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
	r.ToolboxPolicy = acpTestToolboxPolicy()
	deployment, pod := runtimePoolTestStartServing(t, r, pool, supervisor, "restarted-toolbox-pod", "restarted-toolbox-pod-uid", "10.0.0.86", "old-toolbox-boot")
	r.ToolboxPolicy = ACPToolboxPolicy{}
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "new-toolbox-boot", false)
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, deployment.Name).Spec.Replicas, 0) != 1 {
		t.Fatal("first reconcile skipped the durable toolbox admission barrier")
	}
	runtimePoolReconcile(t, r, pool)
	if ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, deployment.Name).Spec.Replicas, 0) != 0 {
		t.Fatal("legitimate in-place restart stranded a rejected workload")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(&pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("exact restarted Pod was not recycled: %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance != nil {
		t.Fatal("recycled instance retained stale fence")
	}
}

func TestRuntimePoolToolboxPullDeadlineSurvivesWaitingReasonChanges(t *testing.T) {
	for _, copyMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("copy=%t", copyMode), func(t *testing.T) {
			ctx := context.Background()
			pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
			pod := runtimePoolPendingPod(pool, pool.Namespace, "registry-pull-pod", "registry-pull-uid")
			status := corev1.ContainerStatus{Name: runtimeField, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: podWaitingReasonImagePullBackOff, Message: "pull " + runtimePoolTestToolboxes()[0].Image}}}
			if copyMode {
				status.Name = runtimePoolToolboxCopyContainerName(0)
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
			} else {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
			}
			r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool, &pod)
			pods := []corev1.Pod{pod}
			if err := r.observeRuntimePoolToolboxPullFailures(ctx, pods, runtimePoolTestToolboxes()); err != nil {
				t.Fatal(err)
			}
			first := pods[0].Annotations[runtimePoolToolboxPullFailuresAnnotation]
			r.Now = func() time.Time { return runtimePoolTestNow.Add(runtimePoolToolboxPullRetryWindow) }
			for _, reason := range []string{podWaitingReasonRegistryUnavailable, "ContainerCreating", "RuntimeSpecificPullFailure", podWaitingReasonImagePullBackOff, podWaitingReasonRegistryUnavailable} {
				current := &pods[0]
				if copyMode {
					current.Status.InitContainerStatuses[0].State.Waiting.Reason = reason
				} else {
					current.Status.ContainerStatuses[0].State.Waiting.Reason = reason
				}
				if err := r.Status().Update(ctx, current); err != nil {
					t.Fatal(err)
				}
				if err := r.observeRuntimePoolToolboxPullFailures(ctx, pods, runtimePoolTestToolboxes()); err != nil {
					t.Fatal(err)
				}
				if current.Annotations[runtimePoolToolboxPullFailuresAnnotation] != first {
					t.Fatalf("%s reset the same failure episode", reason)
				}
				if reason == podWaitingReasonImagePullBackOff || reason == podWaitingReasonRegistryUnavailable {
					if _, _, failed := runtimePoolToolboxFailure(pods, runtimePoolTestToolboxes(), r.now()); !failed {
						t.Fatalf("%s hid the expired pull deadline", reason)
					}
				}
			}
		})
	}
}

func TestRuntimePoolToolboxRestartRecycleRetriesAfterScaleToZero(t *testing.T) {
	ctx := context.Background()
	pool := runtimePoolToolboxTestObject(runtimePoolTestToolboxes()...)
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
	r.ToolboxPolicy = acpTestToolboxPolicy()
	deployment, pod := runtimePoolTestStartServing(t, r, pool, supervisor, "retry-toolbox-pod", "retry-toolbox-pod-uid", "10.0.0.87", "old-retry-boot")
	r.ToolboxPolicy = ACPToolboxPolicy{}
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "new-retry-boot", false)
	runtimePoolReconcile(t, r, pool)
	failedDelete := true
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod && failedDelete {
				failedDelete = false
				return errors.New("injected exact-Pod deletion failure")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err == nil || !strings.Contains(err.Error(), "injected exact-Pod") {
		t.Fatalf("exact Pod deletion failure = %v", err)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance == nil || ptr.Deref(runtimePoolTestDeployment(t, r, pool.Namespace, deployment.Name).Spec.Replicas, 0) != 0 {
		t.Fatal("failed Pod removal did not preserve fence after scale-to-zero")
	}
	runtimePoolReconcile(t, r, pool)
	if err := r.Get(ctx, client.ObjectKeyFromObject(&pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("zero-replica Deployment skipped exact Pod recycle retry: %v", err)
	}
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance != nil {
		t.Fatal("successful retry retained retired instance fence")
	}
}
