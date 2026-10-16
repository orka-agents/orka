package main

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestStaticChartRuntimeFeedbackDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "defaults"},
		{
			name: "configured_but_disabled",
			args: []string{
				"--set-string", "controller.runtimeFeedback.url=https://gkr.example.internal:9444",
				"--set-string", "controller.runtimeFeedback.nodeURLsKey=node-urls.json",
				"--set-string", "controller.runtimeFeedback.existingSecret=gkr-feedback-client",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := requireHelmRender(t, tc.args...)
			for _, forbidden := range []string{
				"--acp-runtime-feedback-", "runtime-feedback-credentials", "gkr-feedback-client",
			} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("disabled feedback rendered %q", forbidden)
				}
			}
		})
	}
}

func TestStaticChartRuntimeFeedbackProjectsOnlyControllerCredentials(t *testing.T) {
	for _, mode := range []string{"url", "node-map"} {
		for _, customKeys := range []bool{false, true} {
			name := mode + "/default_keys"
			if customKeys {
				name = mode + "/custom_keys"
			}
			t.Run(name, func(t *testing.T) {
				const secretName = "gkr-feedback-client"
				args := []string{
					"--set", "controller.runtimeFeedback.enabled=true",
					"--set-string", "controller.runtimeFeedback.existingSecret=" + secretName,
				}
				caKey, certKey, privateKeyKey, nodeURLsKey := "ca.crt", "tls.crt", "tls.key", "node-urls.json"
				if customKeys {
					caKey, certKey, privateKeyKey, nodeURLsKey = "server-ca", "controller-cert", "controller-key", "node-routes"
					args = append(args,
						"--set-string", "controller.runtimeFeedback.caKey="+caKey,
						"--set-string", "controller.runtimeFeedback.certKey="+certKey,
						"--set-string", "controller.runtimeFeedback.privateKeyKey="+privateKeyKey,
					)
				}
				wantItems := map[string]string{caKey: "ca.crt", certKey: "tls.crt", privateKeyKey: "tls.key"}
				wantArgs := []string{
					"--acp-runtime-feedback-ca-file=/var/run/secrets/gkr-feedback/ca.crt",
					"--acp-runtime-feedback-cert-file=/var/run/secrets/gkr-feedback/tls.crt",
					"--acp-runtime-feedback-key-file=/var/run/secrets/gkr-feedback/tls.key",
				}
				if mode == "url" {
					args = append(args, "--set-string", "controller.runtimeFeedback.url=https://gkr.example.internal:9444")
					wantArgs = append(wantArgs, "--acp-runtime-feedback-url=https://gkr.example.internal:9444")
				} else {
					args = append(args, "--set-string", "controller.runtimeFeedback.nodeURLsKey="+nodeURLsKey)
					wantArgs = append(wantArgs, "--acp-runtime-feedback-node-urls-file=/var/run/secrets/gkr-feedback/node-urls.json")
					wantItems[nodeURLsKey] = "node-urls.json"
				}

				output := requireHelmRender(t, args...)
				var controller *appsv1.Deployment
				for document := range strings.SplitSeq(output, "\n---\n") {
					if !strings.Contains(document, secretName) && !strings.Contains(document, "--acp-runtime-feedback-") &&
						!strings.Contains(document, "runtime-feedback-credentials") {
						continue
					}
					var deployment appsv1.Deployment
					if err := yaml.Unmarshal([]byte(document), &deployment); err != nil {
						t.Fatal(err)
					}
					if deployment.Kind != "Deployment" || deployment.Labels["app.kubernetes.io/component"] != "controller" {
						t.Fatal("feedback configuration escaped the controller Deployment or created an operator-owned Secret")
					}
					if controller != nil {
						t.Fatal("feedback configuration appeared in more than one resource")
					}
					controller = &deployment
				}
				if controller == nil {
					t.Fatal("controller has no runtime-feedback configuration")
				}
				pod := controller.Spec.Template.Spec
				if len(pod.Containers) != 1 || pod.Containers[0].Name != "controller" {
					t.Fatal("unexpected container can access the feedback projection")
				}
				container := pod.Containers[0]
				var feedbackArgs []string
				for _, arg := range container.Args {
					if strings.HasPrefix(arg, "--acp-runtime-feedback-") {
						feedbackArgs = append(feedbackArgs, arg)
					}
				}
				slices.Sort(feedbackArgs)
				slices.Sort(wantArgs)
				if !slices.Equal(feedbackArgs, wantArgs) {
					t.Fatalf("feedback args = %v, want %v", feedbackArgs, wantArgs)
				}
				requireRuntimeFeedbackSecretProjection(t, pod, secretName, wantItems)
			})
		}
	}
}

func requireRuntimeFeedbackSecretProjection(
	t *testing.T, pod corev1.PodSpec, secretName string, wantItems map[string]string,
) {
	t.Helper()
	container := pod.Containers[0]
	mountIndex := slices.IndexFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == "runtime-feedback-credentials"
	})
	if mountIndex < 0 {
		t.Fatal("controller has no feedback Secret mount")
	}
	mount := container.VolumeMounts[mountIndex]
	if mount.MountPath != "/var/run/secrets/gkr-feedback" || !mount.ReadOnly ||
		mount.SubPath != "" || mount.SubPathExpr != "" {
		t.Fatal("feedback Secret mount is writable, uses an unexpected path, or prevents projected updates")
	}
	volumeIndex := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == mount.Name })
	if volumeIndex < 0 {
		t.Fatal("feedback mount has no volume")
	}
	secret := pod.Volumes[volumeIndex].Secret
	if secret == nil || secret.SecretName != secretName || secret.DefaultMode == nil || *secret.DefaultMode != 0400 ||
		(secret.Optional != nil && *secret.Optional) || len(secret.Items) != len(wantItems) {
		t.Fatal("feedback volume does not require exactly the selected operator Secret items")
	}
	for _, item := range secret.Items {
		if wantItems[item.Key] != item.Path || item.Mode != nil {
			t.Fatalf("unexpected projection for feedback Secret item %q", item.Key)
		}
	}
	// Secret volumes apply fsGroup ownership and group-read permission.
	// Keep the existing non-root identity and its projected-volume group.
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 ||
		container.SecurityContext == nil || container.SecurityContext.RunAsUser == nil ||
		*container.SecurityContext.RunAsUser != 65532 ||
		container.SecurityContext.RunAsNonRoot == nil || !*container.SecurityContext.RunAsNonRoot {
		t.Fatal("feedback changed the default non-root identity or removed its Secret-volume group access")
	}
}

func TestStaticChartRuntimeFeedbackRejectsIncompleteConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no_route", []string{"--set-string", "controller.runtimeFeedback.url="}},
		{"both_routes", []string{"--set-string", "controller.runtimeFeedback.nodeURLsKey=node-urls.json"}},
		{"no_secret", []string{"--set-string", "controller.runtimeFeedback.existingSecret="}},
		{"no_ca_key", []string{"--set-string", "controller.runtimeFeedback.caKey="}},
		{"no_certificate_key", []string{"--set-string", "controller.runtimeFeedback.certKey="}},
		{"no_private_key", []string{"--set-string", "controller.runtimeFeedback.privateKeyKey="}},
		{"blank_secret", []string{"--set-string", "controller.runtimeFeedback.existingSecret= "}},
		{"blank_node_key", []string{"--set-string", "controller.runtimeFeedback.nodeURLsKey= "}},
		{"non_string_key", []string{"--set", "controller.runtimeFeedback.caKey=42"}},
		{"non_boolean_enabled", []string{"--set-string", "controller.runtimeFeedback.enabled=false"}},
		{"non_object_configuration", []string{"--set-string", "controller.runtimeFeedback=invalid"}},
		{
			"harness_v1",
			[]string{
				"--set-string", "controller.mode=harness-v1",
				"--set-string", "harnessV1.image.digest=sha256:" + strings.Repeat("1", 64),
				"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{
				"--set", "controller.runtimeFeedback.enabled=true",
				"--set-string", "controller.runtimeFeedback.url=https://gkr.example.internal:9444",
				"--set-string", "controller.runtimeFeedback.existingSecret=gkr-feedback-client",
				"--show-only", "templates/deployment.yaml",
			}
			output, err := helmTemplateStaticChart(t, append(args, tc.args...)...)
			if err == nil || !strings.Contains(output, "controller.runtimeFeedback") {
				t.Fatal("Helm accepted invalid runtime-feedback configuration or rejected it for an unrelated reason")
			}
		})
	}
}
