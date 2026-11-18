package patchverification

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

func TestApplicationTCPNeedsActualSubjectAndPositiveNormalControl(t *testing.T) {
	manifest := applicationTestManifest(t)
	marker, err := ApplicationTCPExpectation(true)
	require.NoError(t, err)
	noMarker, err := ApplicationTCPExpectation(false)
	require.NoError(t, err)
	manifest.Application.Fixtures = []ApplicationFixture{{ID: "protected", Port: 8081}}
	for i := range manifest.Checks {
		manifest.Checks[i].Application = &ApplicationCheck{Version: ApplicationProtocolVersion,
			TCP: &ApplicationTCPCheck{Target: "subject", Port: 8080, MarkerID: "protected", MaxBytes: 512}}
	}
	manifest.Checks[0].Healthy, manifest.Checks[0].Failure = noMarker, marker
	manifest.Checks[1].Healthy, manifest.Checks[1].Failure = marker, noMarker
	require.NoError(t, ValidateManifest(manifest))
	binding, evidence := applicationTestEvidence(t, manifest)
	require.Equal(t, Verified, Evaluate(manifest, binding, evidence).Conclusion)
	manifest.Checks[1].Application.TCP.Target, manifest.Checks[1].Application.TCP.Port = "protected", 8081
	require.Error(t, ValidateManifest(manifest), "fixture-only normal checks cannot establish subject behavior")
	manifest.Checks[0].Application.TCP.Target, manifest.Checks[0].Application.TCP.Port = "protected", 8081
	require.Error(t, ValidateManifest(manifest), "directly reading a marker fixture is not application reproduction")
}

func TestApplicationTCPObserverProtectsMarkersAndRejectsIncompleteResponses(t *testing.T) {
	marker := []byte(strings.Repeat("M", 48))
	tests := []struct {
		name       string
		response   []byte
		incomplete bool
		wantMatch  bool
		wantError  bool
	}{
		{name: "present", response: append([]byte("prefix:"), marker...), wantMatch: true},
		{name: "absent complete", response: []byte("complete negative response")},
		{name: "oversized", response: []byte(strings.Repeat("X", 129)), wantError: true},
		{name: "partial positive", response: marker, incomplete: true, wantError: true},
		{name: "partial negative", response: []byte("partial"), incomplete: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() { _ = listener.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer func() { _ = connection.Close() }()
				_ = connection.SetDeadline(time.Now().Add(time.Second))
				_, _ = connection.Write(test.response)
				if test.incomplete {
					<-ctx.Done()
				}
			}()
			_, rawPort, err := net.SplitHostPort(listener.Addr().String())
			require.NoError(t, err)
			port, err := strconv.Atoi(rawPort)
			require.NoError(t, err)
			check := ApplicationTCPCheck{Target: "subject", Port: port, MarkerID: "protected", MaxBytes: 128}
			endpoint := ApplicationEndpoint{ID: "subject", Address: "127.0.0.1", Port: port}
			identity, content, err := ObserveApplicationTCP(ctx, check, endpoint, marker)
			if test.wantError {
				require.Error(t, err)
				require.Nil(t, content)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.wantMatch, identity.Result.MarkerMatched)
				require.Equal(t, Digest(marker), identity.MarkerDigest)
				require.NotContains(t, string(content), string(marker))
				require.Len(t, content, len(mustApplicationTCPOutput(t, test.wantMatch)))
			}
			cancel()
			_ = listener.Close()
			<-done
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := ObserveApplicationTCP(ctx, ApplicationTCPCheck{Target: "subject", Port: 8080, MaxBytes: 128},
		ApplicationEndpoint{ID: "subject", Address: "127.0.0.1", Port: 8080}, marker)
	require.Error(t, err, "cancellation is not marker absence")
}

func mustApplicationTCPOutput(t *testing.T, matched bool) string {
	t.Helper()
	expected, err := ApplicationTCPExpectation(matched)
	require.NoError(t, err)
	return expected.Stdout
}

func applicationStatusObject(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": namespace, "uid": "object-uid", "resourceVersion": "17"},
		"status":   map[string]any{"accepted": true},
	}}
}

func TestApplicationAPIObserverUsesActualStatusNotFixtureAssertions(t *testing.T) {
	object := applicationStatusObject("run-original", "case-unsafe")
	object.Object["spec"] = map[string]any{"passed": true}
	object.Object["status"] = map[string]any{"accepted": "protected-observed-value"}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), object)
	check := ApplicationKubernetesCheck{APIVersion: "v1", Resource: "services", Name: "case-unsafe", Pointer: "/status/accepted"}
	identity, content, err := ObserveApplicationAPI(t.Context(), client, "run-original", check)
	require.NoError(t, err)
	require.Equal(t, "object-uid", identity.UID)
	require.Equal(t, "17", identity.ResourceVersion)
	require.NotContains(t, string(content), "protected-observed-value")
	require.Equal(t, Digest([]byte(`"protected-observed-value"`)), identity.Result.ValueDigest)
	check.Pointer = "/spec/passed"
	_, _, err = ObserveApplicationAPI(t.Context(), client, "run-original", check)
	require.Error(t, err)
	check.Pointer, check.Resource = "/status/value", "secrets"
	_, _, err = ObserveApplicationAPI(t.Context(), client, "run-original", check)
	require.Error(t, err)
}

func applicationLiveTestPod(namespace string, identity ApplicationContainerIdentity, image string, artifact bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: identity.PodName, Namespace: namespace, UID: types.UID(identity.PodUID),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "job",
				UID: types.UID(identity.JobUID), Controller: new(true)}}},
		Spec: corev1.PodSpec{NodeName: "qualified-node", ServiceAccountName: identity.ServiceAccountName,
			AutomountServiceAccountToken: new(false),
			Containers: []corev1.Container{{Name: identity.ContainerName, Image: image,
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(65532)), RunAsGroup: new(int64(65532)), RunAsNonRoot: new(true),
					AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true),
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				}}}},
		Status: corev1.PodStatus{PodIP: "10.0.0.7", ContainerStatuses: []corev1.ContainerStatus{
			{Name: identity.ContainerName, ContainerID: identity.ContainerID, ImageID: identity.ImageID,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}
	if artifact {
		pod.Spec.Containers[0].Command = []string{ApplicationWorkerPath}
		pod.Spec.Containers[0].Args = []string{"subject", "--input", ApplicationInputPath}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "artifact", MountPath: ApplicationArtifactsPath, ReadOnly: true}}
	}
	return pod
}

func TestApplicationObserverRechecksActualPodAndContainerAfterObservation(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			manifest := applicationTestManifest(t)
			binding, evidence := applicationTestEvidence(t, manifest)
			proof := evidence[0]
			input := ApplicationWorkerInput{
				Version: ApplicationProtocolVersion, Manifest: manifest, Binding: binding, Side: Original,
				CheckID: manifest.Checks[0].ID, TaskUID: "original-task", Target: proof.Application.Target,
				Artifact: &proof.Application.Artifact, Subject: &proof.Application.Subject,
				Worker: applicationTestIdentity("observer-original-unsafe", manifest.Environment.Dependencies[ApplicationObserverImageIDKey]),
			}
			subject := applicationLiveTestPod(input.Target.Namespace, *input.Subject, manifest.Application.Runtime.Image, true)
			observer := applicationLiveTestPod(input.Target.Namespace, input.Worker, manifest.Environment.Dependencies[ApplicationObserverImageKey], false)
			objects := make([]runtime.Object, 0, 4)
			objects = append(objects, subject, observer)
			for _, identity := range []ApplicationContainerIdentity{*input.Subject, input.Worker} {
				objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
					Name: identity.ServiceAccountName, Namespace: input.Target.Namespace, UID: types.UID(identity.ServiceAccountUID),
				}})
			}
			kube := kubefake.NewSimpleClientset(objects...)
			if replace {
				reads := 0
				kube.PrependReactor("get", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
					if action.(kubetesting.GetAction).GetName() == subject.Name {
						reads++
						if reads > 1 {
							restarted := subject.DeepCopy()
							restarted.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
							return true, restarted, nil
						}
					}
					return false, nil, nil
				})
			}
			resources := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), applicationStatusObject(input.Target.Namespace, "case-unsafe"))
			observed, err := ObserveApplication(t.Context(), input, kube, resources, nil)
			if replace {
				require.Error(t, err)
				require.Nil(t, observed.Observation.Application)
			} else {
				require.NoError(t, err)
				require.NoError(t, ValidateObservation(manifest, binding, observed.Observation))
				require.Equal(t, proof.StdoutDigest, observed.Observation.StdoutDigest)
				require.NotEmpty(t, observed.Observation.Application.Artifact.BinaryDigest)
			}
		})
	}
}

func TestApplicationArtifactReceiptCannotRebindSourceOrBinary(t *testing.T) {
	manifest := applicationTestManifest(t)
	binding, evidence := applicationTestEvidence(t, manifest)
	artifact := evidence[0].Application.Artifact
	partial := artifact
	partial.Build.ContainerID, partial.Build.ServiceAccountName, partial.Build.ServiceAccountUID, partial.Build.JobUID = "", "", "", ""
	require.NoError(t, ValidateApplicationArtifactFile(artifact, partial))
	require.Error(t, ValidateApplicationArtifact(manifest, binding, Original, partial))
	partial.BinaryDigest = Digest([]byte("different bytes"))
	require.Error(t, ValidateApplicationArtifactFile(artifact, partial))
	content, err := json.Marshal(partial)
	require.NoError(t, err)
	_, err = DecodeApplicationArtifact(strings.NewReader(string(content) + `{"extra":true}`))
	require.Error(t, err)
	require.Equal(t, "b6fc4c620b67d95f953a5c1c1230aaab5db5a1b0", applicationObjectDigest("blob", []byte("hello"), 40))
}
