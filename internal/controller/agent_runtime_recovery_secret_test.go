package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type recoverySecretTypeReader struct {
	client.Reader
	secretReads []client.ObjectKey
	secretError error
}

func (r *recoverySecretTypeReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*corev1.Secret); ok {
		r.secretReads = append(r.secretReads, key)
		if r.secretError != nil {
			return r.secretError
		}
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func recoverySecretTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func recoverySecretTestPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		AutomountServiceAccountToken: new(false),
		Containers: []corev1.Container{{
			Name: "supervisor", Image: "docker.io/example/supervisor@" + testControllerDigest("recovery-secret-image"),
			Env: []corev1.EnvVar{{Name: agentRuntimeEpochEnvironment, Value: "1"}},
		}},
	}
}

func addRecoverySupervisorSecret(spec *corev1.PodSpec, source string) {
	const secretName = "configuration"
	container, err := recoverySupervisorContainer(spec, "supervisor")
	if err != nil {
		panic(err)
	}
	if source == "environment" {
		container.Env = append(container.Env, corev1.EnvVar{Name: "CONFIG", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: "config", Optional: new(true)},
		}})
		return
	}
	volume := corev1.Volume{Name: source}
	if source == "direct" {
		volume.Secret = &corev1.SecretVolumeSource{SecretName: secretName, Optional: new(true), Items: []corev1.KeyToPath{{Key: "config", Path: "config"}}}
	} else {
		volume.Projected = &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Optional: new(true), Items: []corev1.KeyToPath{{Key: "config", Path: "config"}},
		}}}}
	}
	spec.Volumes = append(spec.Volumes, volume)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: source, MountPath: "/" + source, ReadOnly: true})
}

func TestAgentRuntimeRecoverySupervisorSecretTypes(t *testing.T) {
	for _, source := range []string{"direct", "projected", "environment"} {
		for _, secretType := range []corev1.SecretType{corev1.SecretTypeServiceAccountToken, corev1.SecretTypeOpaque} {
			t.Run(source+"/"+string(secretType), func(t *testing.T) {
				spec := recoverySecretTestPodSpec()
				addRecoverySupervisorSecret(&spec, source)
				// Secret type is unavailable to the pure PodSpec validator. These
				// references must be resolved before enrollment or retirement use.
				if err := validateAgentRuntimeRecoveryPodSpec(spec, "supervisor", "agentkit"); err != nil {
					t.Fatalf("pure topology rejected a supported reference: %v", err)
				}
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "configuration"}, Type: secretType}
				base := recoverySecretTestClient(t, secret)
				reader := &recoverySecretTypeReader{Reader: base}
				r := &AgentRuntimeReconciler{Client: base, APIReader: reader}
				err := r.validateAgentRuntimeRecoveryPodSpec(t.Context(), defaultNS, spec, "supervisor", "agentkit")
				if secretType == corev1.SecretTypeServiceAccountToken {
					if err == nil || !strings.Contains(err.Error(), "service account identity token Secret") {
						t.Fatalf("legacy token Secret was not refused: %v", err)
					}
				} else if err != nil {
					t.Fatalf("ordinary configuration Secret was rejected: %v", err)
				}
				if !reflect.DeepEqual(reader.secretReads, []client.ObjectKey{{Namespace: defaultNS, Name: "configuration"}}) {
					t.Fatalf("wrong Secret lookup scope: %v", reader.secretReads)
				}
			})
		}
	}
}

func TestAgentRuntimeRecoverySupervisorSecretsFailClosed(t *testing.T) {
	for _, source := range []string{"direct", "projected", "environment"} {
		t.Run(source, func(t *testing.T) {
			spec := recoverySecretTestPodSpec()
			addRecoverySupervisorSecret(&spec, source)
			// A same-name Secret in another namespace cannot satisfy the read.
			base := recoverySecretTestClient(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "configuration"}, Type: corev1.SecretTypeOpaque})
			reader := &recoverySecretTypeReader{Reader: base}
			r := &AgentRuntimeReconciler{Client: base, APIReader: reader}
			if err := r.validateAgentRuntimeRecoveryPodSpec(t.Context(), defaultNS, spec, "supervisor", "agentkit"); !apierrors.IsNotFound(err) {
				t.Fatalf("missing optional Secret did not fail closed: %v", err)
			}
			unavailable := errors.New("Secret type read unavailable")
			reader.secretError = unavailable
			if err := r.validateAgentRuntimeRecoveryPodSpec(t.Context(), defaultNS, spec, "supervisor", "agentkit"); !errors.Is(err, unavailable) {
				t.Fatalf("read error did not fail closed: %v", err)
			}
		})
	}
}

func TestAgentRuntimeRecoverySupervisorSecretsUseUncachedReader(t *testing.T) {
	for _, authoritativeType := range []corev1.SecretType{corev1.SecretTypeServiceAccountToken, corev1.SecretTypeOpaque} {
		t.Run(string(authoritativeType), func(t *testing.T) {
			cachedType := corev1.SecretTypeOpaque
			if authoritativeType == corev1.SecretTypeOpaque {
				cachedType = corev1.SecretTypeServiceAccountToken
			}
			spec := recoverySecretTestPodSpec()
			addRecoverySupervisorSecret(&spec, "direct")
			cached := recoverySecretTestClient(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "configuration"}, Type: cachedType})
			authoritative := recoverySecretTestClient(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "configuration"}, Type: authoritativeType})
			reader := &recoverySecretTypeReader{Reader: authoritative}
			r := &AgentRuntimeReconciler{Client: cached, APIReader: reader}
			err := r.validateAgentRuntimeRecoveryPodSpec(t.Context(), defaultNS, spec, "supervisor", "agentkit")
			if (err != nil) != (authoritativeType == corev1.SecretTypeServiceAccountToken) || len(reader.secretReads) != 1 {
				t.Fatalf("admission used stale Secret type: error=%v reads=%d", err, len(reader.secretReads))
			}
		})
	}
}

func TestAgentRuntimeRecoverySupervisorSecretsKeepBrokerIdentityAndDeduplicateConfig(t *testing.T) {
	spec := foundryRecoveryPodSpec(recoverySecretTestPodSpec())
	for _, source := range []string{"direct", "projected", "environment"} {
		addRecoverySupervisorSecret(&spec, source)
	}
	spec.Volumes = append(spec.Volumes,
		corev1.Volume{Name: "broker-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "broker-identity"}}},
		corev1.Volume{Name: "broker-projected", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "broker-identity"},
		}}}}}},
	)
	broker, err := recoverySupervisorContainer(&spec, agentRuntimeFoundryBroker)
	if err != nil {
		t.Fatal(err)
	}
	broker.VolumeMounts = append(broker.VolumeMounts, corev1.VolumeMount{Name: "broker-secret", MountPath: "/legacy", ReadOnly: true},
		corev1.VolumeMount{Name: "broker-projected", MountPath: "/legacy-projected", ReadOnly: true})
	broker.Env = append(broker.Env, corev1.EnvVar{Name: "BROKER_CONFIG", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "broker-identity"}, Key: "config"},
	}})
	before := spec.DeepCopy()
	base := recoverySecretTestClient(t,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "configuration"}, Type: corev1.SecretTypeOpaque},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "broker-identity"}, Type: corev1.SecretTypeServiceAccountToken},
	)
	reader := &recoverySecretTypeReader{Reader: base}
	r := &AgentRuntimeReconciler{Client: base, APIReader: reader}
	if err := r.validateAgentRuntimeRecoveryPodSpec(t.Context(), defaultNS, spec, "supervisor", "foundry"); err != nil {
		t.Fatalf("broker-only workload identity or supervisor configuration rejected: %v", err)
	}
	if !reflect.DeepEqual(reader.secretReads, []client.ObjectKey{{Namespace: defaultNS, Name: "configuration"}}) {
		t.Fatalf("read broker-only identity or duplicated config lookups: %v", reader.secretReads)
	}
	if !reflect.DeepEqual(before, &spec) {
		t.Fatal("validation changed the original Pod specification")
	}
	// Sharing that same volume with the supervisor must restore the refusal.
	supervisor, err := recoverySupervisorContainer(&spec, "supervisor")
	if err != nil {
		t.Fatal(err)
	}
	supervisor.VolumeMounts = append(supervisor.VolumeMounts, corev1.VolumeMount{Name: "broker-secret", MountPath: "/identity", ReadOnly: true})
	if err := r.validateAgentRuntimeRecoveryPodSpec(t.Context(), defaultNS, spec, "supervisor", "foundry"); err == nil {
		t.Fatal("broker identity became accessible to the supervisor")
	}
}

func TestAgentRuntimeRecoverySupervisorSecretsKeepPureTopologyGuards(t *testing.T) {
	spec := recoverySecretTestPodSpec()
	addRecoverySupervisorSecret(&spec, "direct")
	spec.HostPID = true
	base := recoverySecretTestClient(t)
	reader := &recoverySecretTypeReader{Reader: base, secretError: errors.New("unexpected Secret read")}
	r := &AgentRuntimeReconciler{Client: base, APIReader: reader}
	if err := r.validateAgentRuntimeRecoveryPodSpec(t.Context(), defaultNS, spec, "supervisor", "agentkit"); err == nil || !strings.Contains(err.Error(), "private Linux process namespaces") {
		t.Fatalf("pure topology guard was bypassed: %v", err)
	}
	if len(reader.secretReads) != 0 {
		t.Fatal("read Secret before rejecting the unsupported topology")
	}
}

func TestAgentRuntimeRecoverySupervisorSecretsBlockBackendEnrollment(t *testing.T) {
	for _, source := range []string{"direct", "projected", "environment"} {
		t.Run(source, func(t *testing.T) {
			registered := &corev1alpha1.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime", UID: "runtime-uid"},
				Spec: corev1alpha1.AgentRuntimeRegistrySpec{Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
					Endpoint:           "http://runtime.default.svc.cluster.local:8080",
					KubernetesRecovery: &corev1alpha1.AgentRuntimeKubernetesRecoverySpec{DeploymentName: "runtime", DeploymentUID: "deployment-uid", ContainerName: "supervisor"},
				}},
			}
			deployment, rs, pod, service, slice := runtimeRecoveryObjects(t, registered, "http://127.0.0.1:8080")
			addRecoverySupervisorSecret(&deployment.Spec.Template.Spec, source)
			rs.Spec.Template.Spec = *deployment.Spec.Template.Spec.DeepCopy()
			pod.Spec = *deployment.Spec.Template.Spec.DeepCopy()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "configuration"}, Type: corev1.SecretTypeServiceAccountToken}
			// recoveryBackend rejects at the Deployment check before endpoint
			// admission or a boot witness. No server, provider, or credential is used.
			base := recoverySecretTestClient(t, deployment, rs, pod, service, slice, secret)
			reader := &recoverySecretTypeReader{Reader: base}
			r := &AgentRuntimeReconciler{Client: base, APIReader: reader}
			backend, err := r.recoveryBackend(t.Context(), registered)
			if backend != nil || err == nil || !strings.Contains(err.Error(), "service account identity token Secret") {
				t.Fatalf("backend enrollment admitted supervisor identity: backendPresent=%t error=%v", backend != nil, err)
			}
			if !reflect.DeepEqual(reader.secretReads, []client.ObjectKey{{Namespace: defaultNS, Name: "configuration"}}) {
				t.Fatalf("backend did not resolve the original Secret: %v", reader.secretReads)
			}
		})
	}
}
