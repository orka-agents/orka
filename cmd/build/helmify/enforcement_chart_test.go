package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func enforcementArchive(t *testing.T, files map[string]string) (string, string, []byte) {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for name, body := range files {
		require.NoError(t, archive.WriteHeader(&tar.Header{
			Name: name, Mode: 0600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}))
		_, err := archive.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	require.NoError(t, compressed.Close())
	path := filepath.Join(t.TempDir(), "guardd.tgz")
	require.NoError(t, os.WriteFile(path, buffer.Bytes(), 0600))
	sum := sha256.Sum256(buffer.Bytes())
	return path, hex.EncodeToString(sum[:]), buffer.Bytes()
}

func TestEnforcementChartBundlePreservesVerifiedBytesAndAddsOnlyLocalDependency(t *testing.T) {
	archive, digest, original := enforcementArchive(t, map[string]string{
		"guardd/Chart.yaml": "apiVersion: v2\nname: guardd\ntype: application\nversion: 0.1.0-rc.1+local\n",
	})
	destination := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(destination, "Chart.yaml"), []byte(
		"apiVersion: v2\nname: orka\nversion: 0.0.0-dev\n"), 0600))
	require.NoError(t, bundleEnforcementChart(destination, archive, digest))
	retained, err := os.ReadFile(filepath.Join(destination, "charts", "guardd.tgz"))
	require.NoError(t, err)
	require.Equal(t, original, retained)
	body, err := os.ReadFile(filepath.Join(destination, "Chart.yaml"))
	require.NoError(t, err)
	var metadata struct {
		Name         string
		Dependencies []map[string]string
	}
	require.NoError(t, yaml.Unmarshal(body, &metadata))
	require.Equal(t, "orka", metadata.Name)
	require.Equal(t, []map[string]string{
		{"name": "guardd", "alias": "enforcement", "version": "0.1.0-rc.1+local"},
	}, metadata.Dependencies)
}

func TestEnforcementChartBundleRefusesUnverifiedOrInvalidInputs(t *testing.T) {
	for _, metadata := range []string{
		"apiVersion: v2\nname: other\ntype: application\nversion: 0.1.0\n",
		"apiVersion: v2\nname: guardd\ntype: application\nversion: 0.1.0\ndependencies: [{name: remote}]\n",
		"invalid: [",
	} {
		archive, digest, _ := enforcementArchive(t, map[string]string{"guardd/Chart.yaml": metadata})
		require.Error(t, bundleEnforcementChart(t.TempDir(), archive, digest))
	}
	archive, digest, _ := enforcementArchive(t, map[string]string{
		"guardd/Chart.yaml":    "apiVersion: v2\nname: guardd\ntype: application\nversion: 0.1.0\n",
		"guardd/../../outside": "not extracted",
	})
	require.Error(t, bundleEnforcementChart(t.TempDir(), archive, digest))
	require.Error(t, bundleEnforcementChart(t.TempDir(), archive, strings.Repeat("0", 64)))
	require.Error(t, bundleEnforcementChart(t.TempDir(), archive, ""))
	require.Error(t, bundleEnforcementChart(t.TempDir(), "", digest))
	require.NoError(t, bundleEnforcementChart(t.TempDir(), "", ""))
	nested, nestedDigest, _ := enforcementArchive(t, map[string]string{
		"guardd/Chart.yaml":       "apiVersion: v2\nname: guardd\ntype: application\nversion: 0.1.0\n",
		"guardd/charts/extra.tgz": "undeclared dependencies must not be loaded",
	})
	require.Error(t, bundleEnforcementChart(t.TempDir(), nested, nestedDigest))
}

func TestStaticChartRejectsUnbundledEnforcement(t *testing.T) {
	output, err := helmTemplateStaticChart(t, "--set", "enforcement.enabled=true")
	require.Error(t, err, output)
}

func TestControllerCanResolveEnforcementNodeWithoutNodeMutationAuthority(t *testing.T) {
	output, err := helmTemplateStaticChart(t, "--set-string", "controller.watchNamespace=orka-tasks")
	require.NoError(t, err, output)
	document := requireRenderedDocument(t, output, "kind: ClusterRole\n",
		"\n  name: test-orka-controller-cluster\n")
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal([]byte(document), &role))
	var permissions []string
	for _, rule := range role.Rules {
		if (slices.Contains(rule.APIGroups, "") || slices.Contains(rule.APIGroups, "*")) &&
			(slices.Contains(rule.Resources, "nodes") || slices.Contains(rule.Resources, "*")) {
			require.Empty(t, rule.ResourceNames, "the scheduled node is not known at install time")
			permissions = append(permissions, rule.Verbs...)
		}
	}
	require.Equal(t, []string{"get"}, permissions)
}

func TestBundledEnforcementProjectionSurvivesDisabledEnrollment(t *testing.T) {
	archivePath := os.Getenv("ORKA_TEST_GUARDD_CHART")
	if archivePath == "" {
		t.Skip("set ORKA_TEST_GUARDD_CHART to the actual locally packaged Guardd chart")
	}
	helm, err := exec.LookPath("helm")
	require.NoError(t, err)
	destination := t.TempDir()
	oldOutput := *outputDir
	*outputDir = destination
	t.Cleanup(func() { *outputDir = oldOutput })
	require.NoError(t, copyStaticFiles("static"))
	archive, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	sum := sha256.Sum256(archive)
	require.NoError(t, bundleEnforcementChart(destination, archivePath, hex.EncodeToString(sum[:])))
	var installed appsv1.Deployment
	for _, enabled := range []string{"false", "true"} {
		t.Run(enabled, func(t *testing.T) {
			arguments := make([]string, 0, 5+len(staticChartDefaultArgs())+8)
			arguments = append(arguments, "template", "test", destination, "--namespace", "orka-test")
			arguments = append(arguments, staticChartDefaultArgs()...)
			arguments = append(arguments, "--set", "enforcement.enabled="+enabled,
				"--set-string", "controller.watchNamespace=orka-tasks",
				"--set", "enforcement.installationName=local-enforcement",
				"--set", "enforcement.image=localhost/guardd@sha256:"+strings.Repeat("1", 64))
			output, err := exec.Command(helm, arguments...).CombinedOutput()
			require.NoError(t, err, string(output))
			document := requireRenderedDocument(t, string(output),
				"kind: Deployment", "app.kubernetes.io/component: controller")
			var deployment appsv1.Deployment
			require.NoError(t, yaml.Unmarshal([]byte(document), &deployment))
			installed = deployment
			container := deployment.Spec.Template.Spec.Containers[0]
			require.Contains(t, container.Args, "--enforcement-provider-registry-projected=true")
			require.Contains(t, container.Args, "--enforcement-provider-registry=/var/run/orka/enforcement/registry.json")
			mountIndex := slices.IndexFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
				return mount.Name == "enforcement-providers"
			})
			require.NotEqual(t, -1, mountIndex)
			require.Equal(t, "/var/run/orka/enforcement", container.VolumeMounts[mountIndex].MountPath)
			require.Empty(t, container.VolumeMounts[mountIndex].SubPath)
			require.True(t, container.VolumeMounts[mountIndex].ReadOnly)
			volumeIndex := slices.IndexFunc(deployment.Spec.Template.Spec.Volumes, func(volume corev1.Volume) bool {
				return volume.Name == "enforcement-providers"
			})
			require.NotEqual(t, -1, volumeIndex)
			volume := deployment.Spec.Template.Spec.Volumes[volumeIndex]
			require.NotNil(t, volume.Projected)
			require.EqualValues(t, 0440, *volume.Projected.DefaultMode)
			require.Len(t, volume.Projected.Sources, 2)
			require.Equal(t, "local-enforcement-registry", volume.Projected.Sources[0].ConfigMap.Name)
			require.True(t, *volume.Projected.Sources[0].ConfigMap.Optional)
			require.Equal(t, "local-enforcement-controller-identity", volume.Projected.Sources[1].Secret.Name)
			require.True(t, *volume.Projected.Sources[1].Secret.Optional)
			assertManagementIsolation(t, string(output), "orka-test", "orka-tasks")
		})
	}
	arguments := append([]string{"template", "test", destination, "--namespace", "orka-test"}, staticChartDefaultArgs()...)
	arguments = append(arguments, "--set", "enforcement.enabled=true",
		"--set", "enforcement.image=localhost/guardd@sha256:"+strings.Repeat("1", 64))
	output, err := exec.Command(helm, arguments...).CombinedOutput()
	require.Error(t, err, string(output))
	require.Contains(t, string(output), "management release namespace")
	arguments = append(arguments, "--set", "enforcement.enabled=false")
	output, err = exec.Command(helm, arguments...).CombinedOutput()
	require.NoError(t, err, string(output))
	config := helmLookupConfig(t, map[string]any{
		"/api/v1/namespaces/orka-test/configmaps/local-enforcement-installation": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]string{"name": "local-enforcement-installation", "namespace": "orka-test"},
		},
	})
	arguments = append(arguments, "--set-string", "enforcement.installationName=local-enforcement",
		"--dry-run=server", "--kubeconfig", config)
	output, err = exec.Command(helm, arguments...).CombinedOutput()
	require.Error(t, err, string(output))
	require.Contains(t, string(output), "management release namespace",
		"retained credentials must still require isolation after enrollment is disabled")
	upgradeConfig := helmLookupConfig(t, map[string]any{
		"/api/v1/namespaces/orka-tasks": map[string]any{
			"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{
				"name": "orka-tasks", "labels": map[string]string{"orka.ai/controller-mode": "harness-v2"},
			},
		},
		"/apis/apps/v1/namespaces/orka-test/deployments": map[string]any{
			"apiVersion": "apps/v1", "kind": "DeploymentList", "items": []appsv1.Deployment{installed},
		},
		"/api/v1/namespaces/orka-test/configmaps/local-enforcement-installation": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]string{"name": "local-enforcement-installation", "namespace": "orka-test"},
		},
	})
	for _, change := range []string{
		"", "enforcement.installationName=other-installation", "enforcement.controllerMountDirectory=/other-directory",
	} {
		t.Run("disabled-upgrade/"+change, func(t *testing.T) {
			args := append([]string{"template", "test", destination, "--namespace", "orka-test",
				"--is-upgrade", "--dry-run=server", "--kubeconfig", upgradeConfig}, staticChartDefaultArgs()...)
			args = append(args, "--set-string", "controller.watchNamespace=orka-tasks",
				"--set-string", "enforcement.installationName=local-enforcement",
				"--set", "enforcement.enabled=false",
				"--set-string", "enforcement.image=localhost/guardd@sha256:"+strings.Repeat("1", 64))
			if change != "" {
				args = append(args, "--set-string", change)
			}
			output, err := exec.Command(helm, args...).CombinedOutput()
			if change == "" {
				require.NoError(t, err, string(output))
				return
			}
			require.Error(t, err, string(output))
			require.Contains(t, string(output), "enforcement projection identity and directory are immutable")
		})
	}
}

func assertManagementIsolation(t *testing.T, rendered, management, tasks string) {
	t.Helper()
	workers := make(map[string]bool)
	var bindings []rbacv1.RoleBinding
	var clusterBindings []rbacv1.ClusterRoleBinding
	var managementRole *rbacv1.Role
	var webhookFound bool
	for document := range strings.SplitSeq(rendered, "\n---") {
		var metadata struct {
			metav1.TypeMeta `json:",inline"`
			Metadata        metav1.ObjectMeta `json:"metadata"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(document), &metadata))
		switch metadata.Kind {
		case "ServiceAccount":
			if metadata.Metadata.Labels["orka.ai/worker"] == "true" {
				require.Equal(t, tasks, metadata.Metadata.Namespace)
				workers[metadata.Metadata.Name] = true
			}
		case "RoleBinding":
			var binding rbacv1.RoleBinding
			require.NoError(t, yaml.Unmarshal([]byte(document), &binding))
			bindings = append(bindings, binding)
		case "ClusterRoleBinding":
			var binding rbacv1.ClusterRoleBinding
			require.NoError(t, yaml.Unmarshal([]byte(document), &binding))
			clusterBindings = append(clusterBindings, binding)
		case "Role":
			var role rbacv1.Role
			require.NoError(t, yaml.Unmarshal([]byte(document), &role))
			if role.Namespace == management && strings.HasSuffix(role.Name, "-management") {
				managementRole = &role
			}
		case "ValidatingWebhookConfiguration":
			var configuration admissionv1.ValidatingWebhookConfiguration
			require.NoError(t, yaml.Unmarshal([]byte(document), &configuration))
			require.NotEmpty(t, configuration.Webhooks)
			for _, webhook := range configuration.Webhooks {
				require.Equal(t, management, webhook.ClientConfig.Service.Namespace)
				require.Equal(t, tasks, webhook.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
			}
			webhookFound = true
		}
	}
	require.NotEmpty(t, workers)
	require.True(t, webhookFound)
	bound := make(map[string]bool)
	for _, binding := range bindings {
		for _, subject := range binding.Subjects {
			if subject.Kind == "ServiceAccount" && workers[subject.Name] {
				require.Equal(t, tasks, subject.Namespace)
				require.Equal(t, tasks, binding.Namespace)
				bound[subject.Name] = true
			}
		}
	}
	for worker := range workers {
		require.True(t, bound[worker], "worker %s has no tenant binding", worker)
	}
	for _, binding := range clusterBindings {
		for _, subject := range binding.Subjects {
			require.False(t, subject.Kind == "ServiceAccount" && workers[subject.Name],
				"worker must not receive cluster-wide authority")
		}
	}
	require.NotNil(t, managementRole)
	for _, rule := range managementRole.Rules {
		for _, resource := range rule.Resources {
			require.NotContains(t,
				[]string{"*", "pods", "jobs", "deployments", "serviceaccounts", "serviceaccounts/token"}, resource)
		}
	}
}
