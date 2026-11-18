package applicationexec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestStartupTLSIsSyntheticReadOnlyAndMetadataOnly(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	f.backend.config.ResourcePolicy.SubjectTLS = []pv.ApplicationSubjectTLSBundle{{ID: "server", DNSNames: []string{"localhost"}}}
	f.request.ApplicationPolicy = f.backend.ApplicationPolicy()
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	require.NoError(t, f.backend.ensureSubjectTLS(t.Context(), r, pv.Original))
	identities, err := f.backend.subjectTLSIdentities(t.Context(), r, pv.Original)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	secret := &corev1.Secret{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: identities[0].Namespace, Name: identities[0].Name}, secret))
	require.Equal(t, corev1.SecretTypeTLS, secret.Type)
	require.Len(t, secret.Data, 3)
	require.True(t, ptr.Deref(secret.Immutable, false))
	require.Empty(t, secret.Data["ca.key"])
	digest, err := pv.ValidateApplicationTLSBundle(f.backend.config.ResourcePolicy.SubjectTLS[0], identities[0].Namespace,
		secret.Data["ca.crt"], secret.Data["tls.crt"], secret.Data["tls.key"])
	require.NoError(t, err)
	require.Equal(t, digest, identities[0].Digest)
	encoded, err := json.Marshal(r.state)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(encoded), "BEGIN PRIVATE KEY"), "key material must not enter retained control state")
	require.False(t, strings.Contains(string(encoded), string(secret.Data["tls.key"])), "raw key material must not enter retained control state")
	job, err := f.backend.job(r, pv.Original, "tls-render", jobOptions{role: "subject", mode: "subject",
		image: f.backend.config.RuntimeImage, taskUID: string(r.state.Task.UID), serviceAccount: "subject",
		subjectTLS: identities})
	require.NoError(t, err)
	found := false
	for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.MountPath == "/subject-config/server" {
			found = true
			require.True(t, mount.ReadOnly)
		}
	}
	require.True(t, found)
	require.NoError(t, f.backend.ensureSubjectTLS(t.Context(), r, pv.Original), "resume must retain the exact generated identity, not mint a new key")
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
}

func TestSelectedFixtureSecretsAreOnlyProtectedCopies(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	profile := pv.ApplicationFixtureProfile{ID: "service", Image: f.config.WorkerImage, ImageID: f.config.WorkerImageID,
		Args: []string{"/usr/bin/service"}, Ports: []int{8080},
		SecretFiles: []pv.ApplicationFixtureSecretFile{{ID: "credential", SecretName: "synthetic-credential", Key: "value"}}}
	f.backend.config.ResourcePolicy.Fixtures = []pv.ApplicationFixtureProfile{profile}
	f.request.ApplicationPolicy = f.backend.ApplicationPolicy()
	fixture := pv.ApplicationFixture{ID: "real-service", Kind: "image", Profile: profile.ID, Port: 8080}
	f.request.Checks.Application.Fixtures = []pv.ApplicationFixture{fixture}
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	require.NoError(t, f.backend.ensureProtected(t.Context(), r, pv.Original))
	files, err := f.backend.fixtureSecretFiles(t.Context(), r, pv.Original, fixture, &profile)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, namespaceName(r, protectedSide(pv.Original)), files[0].Namespace)
	require.NotEqual(t, namespaceName(r, pv.Original), files[0].Namespace)
	copy := &corev1.Secret{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: files[0].Namespace, Name: files[0].Name}, copy))
	require.True(t, ptr.Deref(copy.Immutable, false))
	require.Len(t, copy.Data, 1)
	require.Equal(t, pv.Digest(copy.Data[files[0].Key]), files[0].Digest)
	r.state.Deadline = time.Now().Add(time.Minute)
	job, err := f.backend.job(r, protectedSide(pv.Original), "service-render", jobOptions{role: "fixture", mode: "fixture",
		image: profile.Image, taskUID: string(r.state.Task.UID), serviceAccount: "worker", fixtureSecrets: files})
	require.NoError(t, err)
	mounted := false
	for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.MountPath == "/fixture-secrets" {
			mounted = true
			require.True(t, mount.ReadOnly)
		}
	}
	require.True(t, mounted)
	for _, volume := range job.Spec.Template.Spec.Volumes {
		require.Nil(t, volume.HostPath)
	}
	unapproved := profile
	unapproved.SecretFiles = []pv.ApplicationFixtureSecretFile{{ID: "credential", SecretName: "orka-marker", Key: "sensitive"}}
	_, err = f.backend.fixtureSecretFiles(t.Context(), r, pv.Original, fixture, &unapproved)
	require.Error(t, err)
}

func TestRuntimeRoleRenderingUsesMemoryWithoutBudgetInflation(t *testing.T) {
	f := newFixture(t)
	r := &run{object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "run-uid"}},
		state: state{TargetUID: f.config.TargetClusterUID, Target: targetIdentity{NodeName: "target-node"}, Deadline: time.Now().Add(time.Minute)}}
	f.backend.config.RuntimeClassName = "gvisor-bounded"
	f.backend.config.RuntimeQualification = &pv.ApplicationRuntimeQualification{Profile: "gvisor-bounded-v1"}
	for _, mode := range []string{"prepare", "build", "subject", "observe", "boundary", "marker", "fixture"} {
		t.Run(mode, func(t *testing.T) {
			job, err := f.backend.job(r, pv.Original, "role-"+mode, jobOptions{role: mode, mode: mode,
				image: f.config.ToolchainImage, taskUID: "task-uid", serviceAccount: "worker", markerPort: 8080})
			require.NoError(t, err)
			for _, volume := range job.Spec.Template.Spec.Volumes {
				if volume.EmptyDir != nil {
					require.Equal(t, corev1.StorageMediumMemory, volume.EmptyDir.Medium)
					require.NotNil(t, volume.EmptyDir.SizeLimit)
				}
			}
			container := job.Spec.Template.Spec.Containers[0]
			memory := container.Resources.Limits.Memory().Value()
			if mode == "prepare" || mode == "build" || mode == "subject" {
				require.Equal(t, int64(2<<30), memory)
			} else {
				require.Equal(t, int64(256<<20), memory)
			}
			role := jobFileSizeRole(jobOptions{mode: mode})
			config, err := f.backend.runtimeConfig(container.Resources, role)
			require.NoError(t, err)
			require.Equal(t, uint64(memory), config.Limits.MemoryMax)
			cap, err := pv.ApplicationFileSizeLimit(role)
			require.NoError(t, err)
			require.Equal(t, cap, config.Limits.FileSizeMax)
		})
	}
	f.backend.config.RuntimeClassName, f.backend.config.RuntimeQualification = "", nil
	job, err := f.backend.job(r, pv.Original, "native-render", jobOptions{role: "subject", mode: "subject",
		image: f.config.WorkerImage, taskUID: "task-uid", serviceAccount: "subject"})
	require.NoError(t, err)
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.EmptyDir != nil {
			require.Empty(t, volume.EmptyDir.Medium, "native-v2 rendering is not silently changed by a runtime name")
		}
	}
}
