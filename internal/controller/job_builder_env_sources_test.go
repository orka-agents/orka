/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

func envSourceTestTask(taskType corev1alpha1.TaskType, image string, env ...corev1.EnvVar) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "env-source-task", Namespace: defaultNS, UID: types.UID("env-source-uid")},
		Spec: corev1alpha1.TaskSpec{
			Type:    taskType,
			Image:   image,
			Command: []string{"env"},
			Prompt:  "inspect env",
			Env:     env,
		},
	}
}

func secretKeyEnv(name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "other-team-secret"},
		Key:                  "token",
	}}}
}

func TestBuildRejectsContainerTaskEnvObjectSourcesWithoutDirectSecretMounts(t *testing.T) {
	tests := []struct {
		name   string
		env    corev1.EnvVar
		source string
	}{
		{name: "secretKeyRef", env: secretKeyEnv("API_TOKEN"), source: "valueFrom.secretKeyRef"},
		{
			name: "configMapKeyRef",
			env: corev1.EnvVar{Name: "SETTINGS", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "settings"},
				Key:                  "value",
			}}},
			source: "valueFrom.configMapKeyRef",
		},
		{
			name: "fileKeyRef",
			env: corev1.EnvVar{Name: "FROM_FILE", ValueFrom: &corev1.EnvVarSource{FileKeyRef: &corev1.FileKeySelector{
				VolumeName: "workspace", Path: "env", Key: "TOKEN",
			}}},
			source: "valueFrom.fileKeyRef",
		},
	}
	for _, tt := range tests {
		for worker, image := range map[string]string{"managed": "", "custom": testBusyboxImage} {
			t.Run(tt.name+"/"+worker, func(t *testing.T) {
				// The builder reads the opt-in from the process environment.
				t.Setenv(directSecretMountsEnvVar, "false")
				builder := setupJobBuilder()
				literal := corev1.EnvVar{Name: "MODE", Value: "smoke"}
				task := envSourceTestTask(corev1alpha1.TaskTypeContainer, image, literal, tt.env)

				job, err := builder.Build(context.Background(), task, nil, nil)
				if err == nil {
					t.Fatalf("Build() = %v, want an error for %s", job, tt.source)
				}
				for _, want := range []string{
					"spec.env[1] (" + tt.env.Name + ") uses " + tt.source,
					directSecretMountsEnvVar + "=true",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("Build() error = %q, want it to contain %q", err, want)
					}
				}
			})
		}
	}
}

func TestBuildAllowsContainerTaskPodMetadataEnvSources(t *testing.T) {
	builder := setupJobBuilder()
	podName := corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
	cpuLimit := corev1.EnvVar{Name: "CPU_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}}
	task := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, podName, cpuLimit)

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	for _, want := range []corev1.EnvVar{podName, cpuLimit} {
		got, ok := findEnvVar(env, want.Name)
		if !ok || got.ValueFrom == nil {
			t.Fatalf("env %s = %#v, found=%v; want the pod-metadata source passed through", want.Name, got, ok)
		}
	}
}

func TestBuildPassesContainerTaskEnvSecretSourceWithDirectSecretMounts(t *testing.T) {
	t.Setenv(directSecretMountsEnvVar, "true")
	builder := setupJobBuilder()
	task := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, secretKeyEnv("API_TOKEN"))

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	got, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "API_TOKEN")
	if !ok || got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil || got.ValueFrom.SecretKeyRef.Name != "other-team-secret" {
		t.Fatalf("API_TOKEN = %#v, found=%v; want the opted-in secretKeyRef passed through", got, ok)
	}
}

func TestBuildLeavesAITaskEnvSourcesToTheTrustedWorkerPolicy(t *testing.T) {
	builder := setupJobBuilder()
	task := envSourceTestTask(corev1alpha1.TaskTypeAI, "", secretKeyEnv("API_TOKEN"))

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if _, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "API_TOKEN"); !ok {
		t.Fatal("AI Task env secretKeyRef should keep its existing behavior")
	}
}

func TestBuildPassesContainerTaskRuntimeClassAllowlistOnlyToAIWorkers(t *testing.T) {
	builder := setupJobBuilder()
	builder.ContainerTaskRuntimeClasses = []string{"gvisor", "kata-qemu"}
	spoofed := corev1.EnvVar{Name: workerenv.ContainerTaskRuntimeClasses, Value: "runc-host"}

	aiTask := envSourceTestTask(corev1alpha1.TaskTypeAI, "", spoofed)
	job, err := builder.Build(context.Background(), aiTask, nil, nil)
	if err != nil {
		t.Fatalf("Build(ai) error = %v", err)
	}
	var values []string
	for _, envVar := range job.Spec.Template.Spec.Containers[0].Env {
		if envVar.Name == workerenv.ContainerTaskRuntimeClasses {
			values = append(values, envVar.Value)
		}
	}
	if len(values) != 1 || values[0] != "gvisor,kata-qemu" {
		t.Fatalf("AI worker %s = %v, want only the operator allowlist", workerenv.ContainerTaskRuntimeClasses, values)
	}

	containerTask := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, spoofed)
	job, err = builder.Build(context.Background(), containerTask, nil, nil)
	if err != nil {
		t.Fatalf("Build(container) error = %v", err)
	}
	if got, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, workerenv.ContainerTaskRuntimeClasses); ok {
		t.Fatalf("container Task env %s = %q, want it removed", workerenv.ContainerTaskRuntimeClasses, got.Value)
	}

	// An empty allowlist is pinned, not removed: env wins over envFrom, so a
	// key of this name in an Agent Secret cannot enable runtime classes.
	builder.ContainerTaskRuntimeClasses = nil
	job, err = builder.Build(context.Background(), envSourceTestTask(corev1alpha1.TaskTypeAI, "", spoofed), nil, nil)
	if err != nil {
		t.Fatalf("Build(ai, empty allowlist) error = %v", err)
	}
	if got, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, workerenv.ContainerTaskRuntimeClasses); !ok || got.Value != "" {
		t.Fatalf("AI worker %s with an empty allowlist = %#v, found=%v; want it pinned to an empty value", workerenv.ContainerTaskRuntimeClasses, got, ok)
	}
}

func TestBuildTellsContainerWorkersTheirTimeout(t *testing.T) {
	spoofed := corev1.EnvVar{Name: workerenv.TimeoutSeconds, Value: "99999"}
	spoofedDeadline := corev1.EnvVar{Name: workerenv.Deadline, Value: "2099-01-01T00:00:00Z"}
	for _, tt := range []struct {
		name    string
		task    *corev1alpha1.Task
		timeout *metav1.Duration
		want    string
	}{
		{name: "managed worker", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, "", spoofed, spoofedDeadline), timeout: &metav1.Duration{Duration: 90 * time.Second}, want: "90"},
		{name: "custom image", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, spoofed, spoofedDeadline), timeout: &metav1.Duration{Duration: 30 * time.Minute}, want: "1800"},
		{name: "no timeout", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, spoofed, spoofedDeadline)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.task.Spec.Timeout = tt.timeout
			before := time.Now()
			job, err := setupJobBuilder().Build(context.Background(), tt.task, nil, nil)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			var values, deadlines []string
			for _, envVar := range job.Spec.Template.Spec.Containers[0].Env {
				switch envVar.Name {
				case workerenv.TimeoutSeconds:
					values = append(values, envVar.Value)
				case workerenv.Deadline:
					deadlines = append(deadlines, envVar.Value)
				}
			}
			if tt.want == "" && len(values) != 0 || tt.want != "" && (len(values) != 1 || values[0] != tt.want) {
				t.Fatalf("%s = %v, want %q set only by the controller", workerenv.TimeoutSeconds, values, tt.want)
			}
			if tt.want == "" {
				if len(deadlines) != 0 {
					t.Fatalf("%s = %v, want it removed without a timeout", workerenv.Deadline, deadlines)
				}
				return
			}
			if len(deadlines) != 1 {
				t.Fatalf("%s = %v, want exactly one controller-owned value", workerenv.Deadline, deadlines)
			}
			deadline, err := time.Parse(time.RFC3339Nano, deadlines[0])
			if err != nil {
				t.Fatalf("%s = %q is not RFC 3339: %v", workerenv.Deadline, deadlines[0], err)
			}
			// The deadline is the build time plus the timeout, so startup time counts against it.
			if expected := before.Add(tt.timeout.Duration); deadline.Before(expected.Add(-time.Second)) || deadline.After(expected.Add(5*time.Second)) {
				t.Fatalf("%s = %v, want about %v", workerenv.Deadline, deadline, expected)
			}
		})
	}
}

func TestContainerWorkerDeadlineUsesTheJobsWholeSeconds(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 750_000_000, time.UTC)
	task := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage)
	task.Spec.Timeout = &metav1.Duration{Duration: 2900 * time.Millisecond}
	// activeDeadlineSeconds and ORKA_TIMEOUT_SECONDS see 2s, and a persisted
	// start time loses its 0.75s, so the deadline is 2s from the whole second.
	if got, want := containerWorkerDeadline(task, JobBuildOptions{}, now), "2026-10-28T12:00:02Z"; got != want {
		t.Fatalf("containerWorkerDeadline() = %q, want %q", got, want)
	}
	task.Spec.Timeout = &metav1.Duration{Duration: 500 * time.Millisecond}
	if got := containerWorkerDeadline(task, JobBuildOptions{}, now); got != "" {
		t.Fatalf("containerWorkerDeadline() = %q for a sub-second timeout, want none like ORKA_TIMEOUT_SECONDS", got)
	}
}

// The workspace preparation init container holds the clone credential; Git
// would run a caller-chosen command from GIT_CONFIG_PARAMETERS with it, so
// Task env stays out of it apart from the proxy variables.
func TestBuildKeepsTaskEnvOutOfTheWorkspaceInitContainer(t *testing.T) {
	t.Setenv(directSecretMountsEnvVar, "false")
	task := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage,
		corev1.EnvVar{Name: "GIT_CONFIG_PARAMETERS", Value: "'credential.helper=!cat /secrets/git/token'"},
		corev1.EnvVar{Name: "https_proxy", Value: "http://proxy:3128"},
		corev1.EnvVar{Name: "MODE", Value: "smoke"},
		corev1.EnvVar{Name: workerenv.PriorTaskDiffSHA256, Value: "abc123"},
		corev1.EnvVar{Name: "ORKA_SOMETHING_ELSE", Value: "x"},
	)
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		GitRepo:           "https://github.com/example/repo",
		ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "my-git-creds"},
	}
	job, err := setupJobBuilder().Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	var initEnv []corev1.EnvVar
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == workspacePreparationInitContainerName {
			initEnv = c.Env
		}
	}
	if initEnv == nil {
		t.Fatal("no workspace preparation init container")
	}
	for _, name := range []string{"GIT_CONFIG_PARAMETERS", "MODE", "ORKA_SOMETHING_ELSE"} {
		if got, ok := findEnvVar(initEnv, name); ok {
			t.Fatalf("init container has %s=%q; Task env must not reach the clone credential", name, got.Value)
		}
	}
	if got, ok := findEnvVar(initEnv, "https_proxy"); !ok || got.Value != "http://proxy:3128" {
		t.Fatalf("init container https_proxy = %#v, found=%v; the proxy variables pass through", got, ok)
	}
	// A repository mutation Task pins the approved patch for preparation to verify.
	if got, ok := findEnvVar(initEnv, workerenv.PriorTaskDiffSHA256); !ok || got.Value != "abc123" {
		t.Fatalf("init container %s = %#v, found=%v; the patch digest must pass through", workerenv.PriorTaskDiffSHA256, got, ok)
	}
	for _, name := range []string{"GIT_CONFIG_PARAMETERS", "https_proxy", "MODE"} {
		if _, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, name); !ok {
			t.Fatalf("worker container is missing %s; the Task's own command keeps its env", name)
		}
	}
}
