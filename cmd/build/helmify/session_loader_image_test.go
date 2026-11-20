package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestSessionLoaderImageRejectsTagOnly(t *testing.T) {
	output, err := helmTemplateStaticChart(t,
		"--set", "workers.sessionLoader.image.digest=",
		"--set", "workers.sessionLoader.image.tag=1.37")
	require.Error(t, err)
	require.Contains(t, output, "workers.sessionLoader.image.digest is required")
}

func TestSessionLoaderImage(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: "docker.io/library/busybox@" +
			"sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"},
		{name: "mirror", args: []string{
			"--set", "workers.sessionLoader.image.repository=registry.example.com/platform/loader",
			"--set", "workers.sessionLoader.image.digest=" + digest,
		}, want: "registry.example.com/platform/loader@" + digest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := requireHelmRender(t, append([]string{"--show-only", "templates/deployment.yaml"}, tc.args...)...)
			var deployment appsv1.Deployment
			require.NoError(t, yaml.Unmarshal([]byte(rendered), &deployment))
			require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
			var images []string
			for _, arg := range deployment.Spec.Template.Spec.Containers[0].Args {
				if image, ok := strings.CutPrefix(arg, "--session-loader-image="); ok {
					images = append(images, image)
				}
			}
			require.Equal(t, []string{tc.want}, images)
		})
	}
}
