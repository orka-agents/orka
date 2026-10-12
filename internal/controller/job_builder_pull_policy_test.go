/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestJobBuilder_Build_NativeWorkerImagePullPolicies(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name  string
		image string
		want  corev1.PullPolicy
	}{
		{name: "latest", image: "ghcr.io/orka-agents/orka/worker:latest", want: corev1.PullAlways},
		{name: "rolling tag", image: "ghcr.io/orka-agents/orka/worker:dev", want: corev1.PullAlways},
		{name: "version tag", image: "ghcr.io/orka-agents/orka/worker:v1.2.3", want: corev1.PullAlways},
		{name: "untagged", image: "ghcr.io/orka-agents/orka/worker", want: corev1.PullAlways},
		{name: "registry port", image: "localhost:5000/worker:dev", want: corev1.PullAlways},
		{name: "digest", image: "ghcr.io/orka-agents/orka/worker@" + digest, want: corev1.PullIfNotPresent},
		{name: "tag and digest", image: "ghcr.io/orka-agents/orka/worker:latest@" + digest, want: corev1.PullIfNotPresent},
		{name: "short digest", image: "ghcr.io/orka-agents/orka/worker@sha256:abc", want: corev1.PullAlways},
		{name: "invalid digest", image: "ghcr.io/orka-agents/orka/worker@sha256:" + strings.Repeat("g", 64), want: corev1.PullAlways},
		{name: "invalid repository", image: "ghcr.io/orka-agents/orka/Worker@" + digest, want: corev1.PullAlways},
		{name: "trailing whitespace", image: "ghcr.io/orka-agents/orka/worker@" + digest + " ", want: corev1.PullAlways},
	}

	for _, taskType := range []corev1alpha1.TaskType{corev1alpha1.TaskTypeAI, corev1alpha1.TaskTypeContainer} {
		t.Run(string(taskType), func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					builder := setupJobBuilder()
					builder.AIWorkerImage = tt.image
					builder.GeneralWorkerImage = tt.image
					task := &corev1alpha1.Task{
						ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS},
						Spec:       corev1alpha1.TaskSpec{Type: taskType},
					}

					job, err := builder.Build(context.Background(), task, nil, nil)
					if err != nil {
						t.Fatalf("Build() error = %v", err)
					}
					container := job.Spec.Template.Spec.Containers[0]
					if container.Image != tt.image {
						t.Errorf("Image = %q, want %q", container.Image, tt.image)
					}
					if container.ImagePullPolicy != tt.want {
						t.Errorf("ImagePullPolicy = %q, want %q", container.ImagePullPolicy, tt.want)
					}
				})
			}
		})
	}
}

func TestJobBuilder_Build_PlatformHelperImagePullPolicies(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		name   string
		suffix string
		want   corev1.PullPolicy
	}{
		{name: "latest", suffix: ":latest", want: corev1.PullAlways},
		{name: "rolling tag", suffix: ":dev", want: corev1.PullAlways},
		{name: "version tag", suffix: ":v1.2.3", want: corev1.PullAlways},
		{name: "untagged", want: corev1.PullAlways},
		{name: "digest", suffix: "@" + digest, want: corev1.PullIfNotPresent},
		{name: "tag and digest", suffix: ":latest@" + digest, want: corev1.PullIfNotPresent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := setupJobBuilder()
			builder.GeneralWorkerImage = "ghcr.io/orka-agents/orka/general-worker" + tt.suffix
			builder.InitImage = "busybox" + tt.suffix
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testTask,
					Namespace: defaultNS,
					UID:       types.UID("pull-policy-task-uid"),
					Annotations: map[string]string{
						labels.AnnotationRepositoryValidationCommandDigest: digest,
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:  corev1alpha1.TaskTypeContainer,
					Image: testBusyboxImage,
					Workspace: &corev1alpha1.WorkspaceConfig{
						GitRepo: "https://github.com/example/repo.git",
					},
					SessionRef: &corev1alpha1.SessionReference{Name: "test-session"},
				},
			}

			job, err := builder.BuildWithOptions(context.Background(), task, nil, nil, JobBuildOptions{RepositoryMonitorValidation: true})
			if err != nil {
				t.Fatalf("BuildWithOptions() error = %v", err)
			}
			wantImages := map[string]string{
				"fetch-session":                                  builder.InitImage,
				workspacePreparationInitContainerName:            builder.GeneralWorkerImage,
				repositoryMonitorValidationCommandContainer:      builder.GeneralWorkerImage,
				repositoryMonitorValidationNetworkProbeContainer: builder.GeneralWorkerImage,
				repositoryMonitorValidationNetworkGateContainer:  builder.GeneralWorkerImage,
			}
			if got := len(job.Spec.Template.Spec.InitContainers); got != len(wantImages) {
				t.Fatalf("init container count = %d, want %d", got, len(wantImages))
			}
			for _, container := range job.Spec.Template.Spec.InitContainers {
				wantImage, ok := wantImages[container.Name]
				if !ok {
					t.Fatalf("unexpected init container %q", container.Name)
				}
				if container.Image != wantImage {
					t.Errorf("%s Image = %q, want %q", container.Name, container.Image, wantImage)
				}
				if container.ImagePullPolicy != tt.want {
					t.Errorf("%s ImagePullPolicy = %q, want %q", container.Name, container.ImagePullPolicy, tt.want)
				}
				delete(wantImages, container.Name)
			}
			if len(wantImages) != 0 {
				t.Errorf("missing init containers: %v", wantImages)
			}
			if got := job.Spec.Template.Spec.Containers[0].ImagePullPolicy; got != corev1.PullIfNotPresent {
				t.Errorf("user task ImagePullPolicy = %q, want %q", got, corev1.PullIfNotPresent)
			}
		})
	}
}

func TestJobBuilder_Build_UserTaskImagePullPolicyUnchanged(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	for _, image := range []string{
		"user/task:latest",
		"user/task:dev",
		"user/task:v1.2.3",
		"user/task",
		"user/task@" + digest,
		"user/task:latest@" + digest,
	} {
		t.Run(image, func(t *testing.T) {
			builder := setupJobBuilder()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS},
				Spec: corev1alpha1.TaskSpec{
					Type:  corev1alpha1.TaskTypeContainer,
					Image: image,
				},
			}

			job, err := builder.Build(context.Background(), task, nil, nil)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			container := job.Spec.Template.Spec.Containers[0]
			if container.Image != image {
				t.Errorf("Image = %q, want %q", container.Image, image)
			}
			if container.ImagePullPolicy != corev1.PullIfNotPresent {
				t.Errorf("ImagePullPolicy = %q, want %q", container.ImagePullPolicy, corev1.PullIfNotPresent)
			}
		})
	}
}
