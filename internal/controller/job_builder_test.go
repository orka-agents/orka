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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/contexttoken"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	testControllerURL      = "http://orka-controller.orka-system.svc:8080"
	testGitCredentials     = "git-credentials"
	testOpenAIAPIKey       = "OPENAI_API_KEY"
	testBusyboxImage       = "busybox:latest"
	testAgentSecretName    = "agent-secret"
	testProviderSecretName = "provider-secret"
	testProviderBaseURL    = "https://api.example.test/v1"
)

const (
	testTask               = "test-task"
	defaultNS              = "default"
	envAIProviderKey       = "ORKA_AI_PROVIDER"
	testCodexSandboxMode   = "danger-full-access"
	testTransactionID      = "txn-123"
	testNodeLabelKey       = "sandbox-runtime"
	testNodeValueKata      = "kata"
	testNodeValueGVisor    = "gvisor"
	testRuntimeClassKata   = "kata-qemu"
	testRuntimeClassGVisor = "gvisor"
	testTTSAudience        = "tts-audience"
)

func TestEnvFlagEnabledAliases(t *testing.T) {
	const name = "ORKA_TEST_FLAG"

	for _, value := range []string{"1", "true", "TRUE", "t", "yes", "y", "on"} {
		t.Run("true_"+value, func(t *testing.T) {
			t.Setenv(name, value)
			if !envFlagEnabled(name) {
				t.Fatalf("envFlagEnabled(%q) = false, want true", value)
			}
		})
	}

	for _, value := range []string{"", "0", "false", "FALSE", "f", "no", "n", "off", "invalid"} {
		t.Run("false_"+value, func(t *testing.T) {
			t.Setenv(name, value)
			if envFlagEnabled(name) {
				t.Fatalf("envFlagEnabled(%q) = true, want false", value)
			}
		})
	}
}

func TestNewJobBuilderSnapshotsDirectRuntimeSecretPolicy(t *testing.T) {
	t.Setenv(directProviderSecretsEnvVar, "true")
	t.Setenv(directSecretMountsEnvVar, "true")
	t.Setenv(directGitCredentialsEnvVar, "true")

	builder := setupJobBuilder()
	t.Setenv(directProviderSecretsEnvVar, "false")
	t.Setenv(directSecretMountsEnvVar, "false")
	t.Setenv(directGitCredentialsEnvVar, "false")

	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Type:  corev1alpha1.TaskTypeContainer,
		Image: testBusyboxImage,
	}}
	if !builder.directProviderSecretsAllowed(task) {
		t.Fatal("provider secret policy should be captured when JobBuilder is constructed")
	}
	if !builder.directSecretMountsAllowed(task) {
		t.Fatal("secret mount policy should be captured when JobBuilder is constructed")
	}
	if !builder.directGitCredentialsAllowed(task) {
		t.Fatal("git credential policy should be captured when JobBuilder is constructed")
	}
}

func setupJobBuilder() *JobBuilder {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	b := NewJobBuilder(fakeClient)
	b.ControllerURL = testControllerURL
	return b
}

func assertServiceAccountName(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("ServiceAccountName = %s, want %s", got, want)
	}
}

func assertAutomountServiceAccountToken(t *testing.T, got *bool, want bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("AutomountServiceAccountToken is nil, want %v", want)
	}
	if *got != want {
		t.Errorf("AutomountServiceAccountToken = %v, want %v", *got, want)
	}
}

func TestNewJobBuilder(t *testing.T) {
	builder := setupJobBuilder()
	if builder == nil {
		t.Fatal("NewJobBuilder returned nil")
	}
	if builder.AIWorkerImage != DefaultAIWorkerImage {
		t.Errorf("AIWorkerImage = %s, want %s", builder.AIWorkerImage, DefaultAIWorkerImage)
	}
	if builder.GeneralWorkerImage != DefaultGeneralWorkerImage {
		t.Errorf("GeneralWorkerImage = %s, want %s", builder.GeneralWorkerImage, DefaultGeneralWorkerImage)
	}
	if builder.CodexWorkerImage != DefaultCodexWorkerImage {
		t.Errorf("CodexWorkerImage = %s, want %s", builder.CodexWorkerImage, DefaultCodexWorkerImage)
	}
	if builder.CodexSandboxMode != "" {
		t.Errorf("CodexSandboxMode = %s, want empty", builder.CodexSandboxMode)
	}
}

func TestJobBuilder_Build_ContainerTask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   testBusyboxImage,
			Command: []string{"echo"},
			Args:    []string{"hello"},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if job == nil {
		t.Fatal("Build() returned nil job")
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, ContainerWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, false)

	// Verify container settings
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != testBusyboxImage {
		t.Errorf("Image = %s, want %s", container.Image, testBusyboxImage)
	}
	if len(container.Command) != 1 || container.Command[0] != "echo" {
		t.Errorf("Command = %v, want [echo]", container.Command)
	}
	if len(container.Args) != 1 || container.Args[0] != "hello" {
		t.Errorf("Args = %v, want [hello]", container.Args)
	}
}

func TestJobBuilder_Build_GeneralContainerWorkerAutomountsTokenForCallback(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "general-container-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Command: []string{"echo"},
			Args:    []string{"hello"},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, ContainerWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)
}

func TestJobBuilder_Build_PropagatesTransactionMetadata(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: "busybox:latest",
			Env: []corev1.EnvVar{
				{Name: workerenv.TransactionScope, Value: "spoofed"},
				{Name: workerenv.TransactionScopes, Value: "spoofed"},
			},
			Transaction: &corev1alpha1.TaskTransaction{
				Profile:                "kontxt",
				ID:                     testTransactionID,
				Issuer:                 "https://issuer.example.test",
				Subject:                "spiffe://example.test/ns/default/sa/client",
				RequestingWorkload:     "spiffe://example.test/ns/default/sa/client",
				Scope:                  "read write",
				Scopes:                 []string{"read", "write"},
				ContextDigest:          "sha256:context",
				RequesterContextDigest: "sha256:requester",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	for name, meta := range map[string]metav1.ObjectMeta{
		"job":          job.ObjectMeta,
		"pod template": job.Spec.Template.ObjectMeta,
	} {
		if meta.Labels[labels.LabelTransactionID] != labels.SelectorValue(testTransactionID) {
			t.Fatalf("%s transaction label = %q, want txn-123", name, meta.Labels[labels.LabelTransactionID])
		}
		if meta.Labels[labels.LabelAuthProfile] != "kontxt" {
			t.Fatalf("%s auth profile label = %q, want kontxt", name, meta.Labels[labels.LabelAuthProfile])
		}
		if meta.Annotations[labels.AnnotationTransactionID] != testTransactionID {
			t.Fatalf("%s transaction annotation = %q, want txn-123", name, meta.Annotations[labels.AnnotationTransactionID])
		}
		if meta.Annotations[labels.AnnotationTransactionContextDigest] != "sha256:context" {
			t.Fatalf("%s context digest annotation = %q", name, meta.Annotations[labels.AnnotationTransactionContextDigest])
		}
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	wantEnv := map[string]string{
		workerenv.TransactionID:                     testTransactionID,
		workerenv.TransactionProfile:                "kontxt",
		workerenv.TransactionIssuer:                 "https://issuer.example.test",
		workerenv.TransactionSubject:                "spiffe://example.test/ns/default/sa/client",
		workerenv.TransactionRequestingWorkload:     "spiffe://example.test/ns/default/sa/client",
		workerenv.TransactionScope:                  "read write",
		workerenv.TransactionScopes:                 "read,write",
		workerenv.TransactionContextDigest:          "sha256:context",
		workerenv.TransactionRequesterContextDigest: "sha256:requester",
	}
	for name, want := range wantEnv {
		env, ok := findEnvVar(envVars, name)
		if !ok {
			t.Fatalf("missing env var %s", name)
		}
		if env.Value != want {
			t.Fatalf("%s = %q, want %q", name, env.Value, want)
		}
		if count := countEnvVars(envVars, name); count != 1 {
			t.Fatalf("%s appeared %d times, want exactly once", name, count)
		}
	}
}

func TestJobBuilder_Build_MountsTransactionTokenSecret(t *testing.T) {
	builder := setupJobBuilder()
	builder.ContextTokenTTSURL = "https://tts.example.test"
	builder.ContextTokenTTSTokenSource = contexttoken.TTSTokenSourceIncoming
	builder.ContextTokenTTSAudience = testTTSAudience
	builder.ContextTokenTTSTimeout = "7s"
	builder.ContextTokenSubjectTokenType = "urn:ietf:params:oauth:token-type:txn_token"
	builder.ContextTokenChildScope = "orka:agents:run"
	builder.ContextTokenOutboundScope = "orka:tools:use"
	builder.ContextTokenChildTokenTTL = "3m"
	builder.ContextTokenToolTokenTTL = "30s"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenSecret: "child-tx-token",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: "busybox:latest",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var foundVolume bool
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == "transaction-token" && volume.Secret != nil && volume.Secret.SecretName == "child-tx-token" {
			if volume.Secret.DefaultMode == nil {
				t.Fatal("transaction-token secret volume DefaultMode is nil, want 0400")
			}
			if *volume.Secret.DefaultMode != int32(0400) {
				t.Fatalf("transaction-token secret volume DefaultMode = %#o, want 0400", *volume.Secret.DefaultMode)
			}
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Fatalf("expected transaction-token secret volume, got %#v", job.Spec.Template.Spec.Volumes)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if _, ok := findEnvVar(container.Env, workerenv.TransactionTokenFile); !ok {
		t.Fatalf("missing %s env var", workerenv.TransactionTokenFile)
	}
	if _, ok := findEnvVar(container.Env, workerenv.ContextTokenSubjectTokenFile); !ok {
		t.Fatalf("missing %s env var", workerenv.ContextTokenSubjectTokenFile)
	}
	for name, want := range map[string]string{
		workerenv.ContextTokenTTSURL:           "https://tts.example.test",
		workerenv.ContextTokenTTSTokenSource:   contexttoken.TTSTokenSourceIncoming,
		workerenv.ContextTokenTTSAudience:      testTTSAudience,
		workerenv.ContextTokenTTSTimeout:       "7s",
		workerenv.ContextTokenSubjectTokenType: "urn:ietf:params:oauth:token-type:txn_token",
		workerenv.ContextTokenChildScope:       "orka:agents:run",
		workerenv.ContextTokenOutboundScope:    "orka:tools:use",
		workerenv.ContextTokenChildTokenTTL:    "3m",
		workerenv.ContextTokenToolTokenTTL:     "30s",
	} {
		got, ok := findEnvVar(container.Env, name)
		if !ok || got.Value != want {
			t.Fatalf("env %s = %#v, want %q", name, got, want)
		}
	}
}

func TestJobBuilder_AddTransactionTokenSecretExposesToAllContainers(t *testing.T) {
	builder := setupJobBuilder()
	builder.ContextTokenTTSTokenSource = contexttoken.TTSTokenSourceIncoming
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenSecret: "child-tx-token",
			},
		},
	}
	job := &batchv1.Job{
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker"},
						{Name: "sidecar"},
					},
				},
			},
		},
	}

	builder.addTransactionTokenSecret(job, task)

	for _, container := range job.Spec.Template.Spec.Containers {
		if !hasVolumeMount(container.VolumeMounts, "transaction-token") {
			t.Fatalf("%s container missing transaction-token mount: %#v", container.Name, container.VolumeMounts)
		}
		for _, name := range []string{workerenv.TransactionTokenFile, workerenv.ContextTokenSubjectTokenFile} {
			got, ok := findEnvVar(container.Env, name)
			if !ok {
				t.Fatalf("%s container missing %s env var", container.Name, name)
			}
			if got.Value != "/var/run/orka/transaction-token/token" {
				t.Fatalf("%s container %s = %q, want token file path", container.Name, name, got.Value)
			}
		}
	}
}

func TestJobBuilder_Build_InjectsContextTokenTTSConfigWithoutTransactionTokenSecret(t *testing.T) {
	builder := setupJobBuilder()
	builder.ContextTokenTTSURL = "https://tts.example.test"
	builder.ContextTokenTTSTokenSource = contexttoken.TTSTokenSourceServiceAccount
	builder.ContextTokenTTSAudience = testTTSAudience
	builder.ContextTokenTTSTimeout = "7s"
	builder.ContextTokenSubjectTokenType = "urn:ietf:params:oauth:token-type:txn_token"
	builder.ContextTokenChildScope = "orka:agents:run"
	builder.ContextTokenOutboundScope = "orka:tools:use"
	builder.ContextTokenChildTokenTTL = "3m"
	builder.ContextTokenToolTokenTTL = "30s"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: "busybox:latest",
			Transaction: &corev1alpha1.TaskTransaction{
				ID:     "txn-123",
				Scope:  "orka:tools:use",
				Scopes: []string{"orka:tools:use"},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	for name, want := range map[string]string{
		workerenv.ContextTokenTTSURL:           "https://tts.example.test",
		workerenv.ContextTokenTTSTokenSource:   contexttoken.TTSTokenSourceServiceAccount,
		workerenv.ContextTokenTTSAudience:      testTTSAudience,
		workerenv.ContextTokenTTSTimeout:       "7s",
		workerenv.ContextTokenSubjectTokenType: "urn:ietf:params:oauth:token-type:txn_token",
		workerenv.ContextTokenChildScope:       "orka:agents:run",
		workerenv.ContextTokenOutboundScope:    "orka:tools:use",
		workerenv.ContextTokenChildTokenTTL:    "3m",
		workerenv.ContextTokenToolTokenTTL:     "30s",
	} {
		got, ok := findEnvVar(container.Env, name)
		if !ok || got.Value != want {
			t.Fatalf("env %s = %#v, want %q", name, got, want)
		}
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "transaction-token") {
		t.Fatalf("unexpected transaction-token volume without transaction token secret annotation: %#v", job.Spec.Template.Spec.Volumes)
	}
	for _, name := range []string{workerenv.TransactionTokenFile, workerenv.ContextTokenSubjectTokenFile} {
		if got, ok := findEnvVar(container.Env, name); ok {
			t.Fatalf("unexpected env %s without transaction token secret annotation: %#v", name, got)
		}
	}
}

func TestJobBuilder_Build_AITask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider: "anthropic",
				Model:    "claude-3-5-sonnet",
				Prompt:   "Hello",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, AIWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultAIWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultAIWorkerImage)
	}
}

func TestJobBuilder_Build_WorkerServiceAccountForTrustLevel(t *testing.T) {
	trueValue := true
	falseValue := false
	tests := []struct {
		name          string
		taskType      corev1alpha1.TaskType
		image         string
		wantSA        string
		wantAutomount *bool
	}{
		{
			name:          "ai task keeps service account token",
			taskType:      corev1alpha1.TaskTypeAI,
			wantSA:        AIWorkerServiceAccount,
			wantAutomount: &trueValue,
		},
		{
			name:          "agent task keeps service account token",
			taskType:      corev1alpha1.TaskTypeAgent,
			wantSA:        VendorWorkerServiceAccount,
			wantAutomount: &trueValue,
		},
		{
			name:          "container task with general worker keeps service account token",
			taskType:      corev1alpha1.TaskTypeContainer,
			wantSA:        ContainerWorkerServiceAccount,
			wantAutomount: &trueValue,
		},
		{
			name:          "container task with custom image uses container service account without token",
			taskType:      corev1alpha1.TaskTypeContainer,
			image:         "busybox:latest",
			wantSA:        ContainerWorkerServiceAccount,
			wantAutomount: &falseValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := setupJobBuilder()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      strings.ReplaceAll(tt.name, " ", "-"),
					Namespace: defaultNS,
					UID:       types.UID("12345678-1234-1234-1234-123456789012"),
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   tt.taskType,
					Image:  tt.image,
					Prompt: "test prompt",
					AI: &corev1alpha1.AISpec{
						Prompt: "test prompt",
					},
				},
			}

			job, err := builder.Build(context.Background(), task, nil, nil)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}

			if got := job.Spec.Template.Spec.ServiceAccountName; got != tt.wantSA {
				t.Errorf("ServiceAccountName = %s, want %s", got, tt.wantSA)
			}
			gotAutomount := job.Spec.Template.Spec.AutomountServiceAccountToken
			if tt.wantAutomount == nil {
				if gotAutomount != nil {
					t.Errorf("AutomountServiceAccountToken = %v, want nil", *gotAutomount)
				}
				return
			}
			if gotAutomount == nil {
				t.Fatalf("AutomountServiceAccountToken = nil, want %v", *tt.wantAutomount)
			}
			if *gotAutomount != *tt.wantAutomount {
				t.Errorf("AutomountServiceAccountToken = %v, want %v", *gotAutomount, *tt.wantAutomount)
			}
		})
	}
}

func TestJobBuilder_Build_WithTimeout(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Timeout: &metav1.Duration{Duration: 5 * time.Minute},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Error("ActiveDeadlineSeconds should be set")
	}
	if *job.Spec.ActiveDeadlineSeconds != 300 {
		t.Errorf("ActiveDeadlineSeconds = %d, want 300", *job.Spec.ActiveDeadlineSeconds)
	}
}

func TestJobBuilder_Build_WithSession(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)

	// Verify session-data emptyDir volume
	hasSessionVolume := false
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Name == "session-data" {
			hasSessionVolume = true
			if vol.EmptyDir == nil {
				t.Error("session-data volume should be emptyDir")
			}
			break
		}
	}
	if !hasSessionVolume {
		t.Error("Job should have session-data volume")
	}

	// Verify init container exists
	if len(job.Spec.Template.Spec.InitContainers) == 0 {
		t.Fatal("Job should have init container for session fetch")
	}
	initContainer := job.Spec.Template.Spec.InitContainers[0]
	if initContainer.Name != "fetch-session" {
		t.Errorf("Init container name = %s, want fetch-session", initContainer.Name)
	}
}

func TestJobBuilder_Build_AgentTask_WithSessionAutomountsToken(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Continue this session",
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, VendorWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)
}

func TestJobBuilder_Build_CustomContainerWithSessionKeepsTokenOutOfMainContainer(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container-session-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   testBusyboxImage,
			Command: []string{"echo"},
			Args:    []string{"hello"},
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, ContainerWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, false)
	if !hasVolume(job.Spec.Template.Spec.Volumes, "session-token") {
		t.Fatal("session-token projected volume should be present for the fetch init container")
	}
	if hasVolumeMount(job.Spec.Template.Spec.Containers[0].VolumeMounts, "session-token") {
		t.Fatal("main custom container should not mount the session token")
	}
	if len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("init container count = %d, want 1", len(job.Spec.Template.Spec.InitContainers))
	}
	if !hasVolumeMount(job.Spec.Template.Spec.InitContainers[0].VolumeMounts, "session-token") {
		t.Fatal("fetch-session init container should mount the session token")
	}
}

func TestJobBuilder_Build_AppliesAgentExecutionDefaults(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Execution: &corev1alpha1.ExecutionSpec{
				RuntimeClassName: testRuntimeClassKata,
				NodeSelector: map[string]string{
					testNodeLabelKey: testNodeValueKata,
				},
				Tolerations: []corev1.Toleration{
					{
						Key:      testNodeLabelKey,
						Operator: corev1.TolerationOpEqual,
						Value:    testNodeValueKata,
						Effect:   corev1.TaintEffectNoSchedule,
					},
				},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      testNodeLabelKey,
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{testNodeValueKata},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	podSpec := job.Spec.Template.Spec
	if podSpec.RuntimeClassName == nil || *podSpec.RuntimeClassName != testRuntimeClassKata {
		t.Fatalf("RuntimeClassName = %v, want %s", podSpec.RuntimeClassName, testRuntimeClassKata)
	}
	if got := podSpec.NodeSelector[testNodeLabelKey]; got != testNodeValueKata {
		t.Errorf("NodeSelector[%s] = %q, want %q", testNodeLabelKey, got, testNodeValueKata)
	}
	if len(podSpec.Tolerations) != 1 {
		t.Fatalf("Tolerations len = %d, want 1", len(podSpec.Tolerations))
	}
	if podSpec.Tolerations[0].Value != testNodeValueKata {
		t.Errorf("Tolerations[0].Value = %q, want %q", podSpec.Tolerations[0].Value, testNodeValueKata)
	}
	if podSpec.Affinity == nil || podSpec.Affinity.NodeAffinity == nil {
		t.Fatal("Affinity.NodeAffinity should not be nil")
	}

	podSpec.NodeSelector[testNodeLabelKey] = "modified"
	if agent.Spec.Execution.NodeSelector[testNodeLabelKey] != testNodeValueKata {
		t.Error("Build should copy agent execution nodeSelector instead of aliasing it")
	}
}

func TestJobBuilder_Build_TaskExecutionOverridesAgentExecution(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
			Execution: &corev1alpha1.ExecutionSpec{
				RuntimeClassName: testRuntimeClassGVisor,
				NodeSelector: map[string]string{
					testNodeLabelKey: testNodeValueGVisor,
				},
				Tolerations: []corev1.Toleration{
					{
						Key:      testNodeLabelKey,
						Operator: corev1.TolerationOpEqual,
						Value:    testNodeValueGVisor,
						Effect:   corev1.TaintEffectNoSchedule,
					},
				},
				Affinity: &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
							{
								Weight: 1,
								PodAffinityTerm: corev1.PodAffinityTerm{
									TopologyKey: "kubernetes.io/hostname",
								},
							},
						},
					},
				},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Execution: &corev1alpha1.ExecutionSpec{
				RuntimeClassName: testRuntimeClassKata,
				NodeSelector: map[string]string{
					testNodeLabelKey: "kata",
					"dedicated":      "true",
				},
				Tolerations: []corev1.Toleration{
					{
						Key:      testNodeLabelKey,
						Operator: corev1.TolerationOpEqual,
						Value:    testNodeValueKata,
						Effect:   corev1.TaintEffectNoSchedule,
					},
					{
						Key:      "dedicated",
						Operator: corev1.TolerationOpExists,
						Effect:   corev1.TaintEffectNoSchedule,
					},
				},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{},
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	podSpec := job.Spec.Template.Spec
	if podSpec.RuntimeClassName == nil || *podSpec.RuntimeClassName != testRuntimeClassGVisor {
		t.Fatalf("RuntimeClassName = %v, want %s", podSpec.RuntimeClassName, testRuntimeClassGVisor)
	}
	if len(podSpec.NodeSelector) != 1 {
		t.Fatalf("NodeSelector len = %d, want 1", len(podSpec.NodeSelector))
	}
	if got := podSpec.NodeSelector[testNodeLabelKey]; got != testNodeValueGVisor {
		t.Errorf("NodeSelector[%s] = %q, want %q", testNodeLabelKey, got, testNodeValueGVisor)
	}
	if len(podSpec.Tolerations) != 1 {
		t.Fatalf("Tolerations len = %d, want 1", len(podSpec.Tolerations))
	}
	if podSpec.Tolerations[0].Value != testNodeValueGVisor {
		t.Errorf("Tolerations[0].Value = %q, want %q", podSpec.Tolerations[0].Value, testNodeValueGVisor)
	}
	if podSpec.Affinity == nil || podSpec.Affinity.PodAntiAffinity == nil {
		t.Fatal("Affinity.PodAntiAffinity should not be nil")
	}
	if podSpec.Affinity.NodeAffinity != nil {
		t.Error("Task affinity should replace agent affinity instead of merging with it")
	}
}

func TestResolveExecution_IgnoresWorkspace(t *testing.T) {
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			Execution: &corev1alpha1.ExecutionSpec{
				Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
					Enabled:     true,
					TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "default"},
				},
			},
		},
	}

	if execution := resolveExecution(task, nil); execution != nil {
		t.Fatalf("resolveExecution() = %#v, want nil when only workspace is set", execution)
	}

	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Execution: &corev1alpha1.ExecutionSpec{
				RuntimeClassName: testRuntimeClassKata,
			},
		},
	}

	execution := resolveExecution(task, agent)
	if execution == nil {
		t.Fatal("resolveExecution() returned nil, want agent execution")
	}
	if execution.RuntimeClassName != testRuntimeClassKata {
		t.Fatalf("RuntimeClassName = %q, want %q", execution.RuntimeClassName, testRuntimeClassKata)
	}
	if execution.Workspace != nil {
		t.Fatalf("Workspace = %#v, want nil", execution.Workspace)
	}
}

func TestJobBuilder_buildPodSecurityContext(t *testing.T) {
	builder := setupJobBuilder()
	psc := builder.buildPodSecurityContext()

	if psc == nil {
		t.Fatal("buildPodSecurityContext returned nil")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("RunAsNonRoot should be true")
	}
	if psc.RunAsUser == nil || *psc.RunAsUser != 1000 {
		t.Errorf("RunAsUser = %v, want 1000", psc.RunAsUser)
	}
}

func TestJobBuilder_buildContainerSecurityContext(t *testing.T) {
	builder := setupJobBuilder()
	csc := builder.buildContainerSecurityContext()

	if csc == nil {
		t.Fatal("buildContainerSecurityContext returned nil")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false")
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem should be true")
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) == 0 {
		t.Error("Should drop all capabilities")
	}
}

func TestJobBuilder_buildResources_TaskResources(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		},
	}

	resources := builder.buildResources(task, nil)
	cpuReq := resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "200m" {
		t.Errorf("CPU request = %s, want 200m", cpuReq.String())
	}
}

func TestJobBuilder_buildResources_AgentResources(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{}, // No resources
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("300m"),
				},
			},
		},
	}

	resources := builder.buildResources(task, agent)
	cpuReq := resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "300m" {
		t.Errorf("CPU request = %s, want 300m", cpuReq.String())
	}
}

func TestJobBuilder_buildResources_Defaults(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{}, // No resources
	}

	resources := builder.buildResources(task, nil)
	cpuReq := resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "100m" {
		t.Errorf("CPU request = %s, want 100m (default)", cpuReq.String())
	}
}

func TestJobBuilder_buildEnvVars(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
			Env: []corev1.EnvVar{
				{Name: "CUSTOM_VAR", Value: "custom-value"},
			},
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, nil, nil)

	// Check required env vars
	hasTaskName := false
	hasTaskNamespace := false
	hasResultEndpoint := false
	hasControllerURL := false
	hasCustomVar := false

	for _, env := range envVars {
		switch env.Name {
		case TaskNameEnvVar:
			hasTaskName = true
			if env.Value != testTask {
				t.Errorf("ORKA_TASK_NAME = %s, want test-task", env.Value)
			}
		case TaskNamespaceEnvVar:
			hasTaskNamespace = true
			if env.Value != defaultNS {
				t.Errorf("ORKA_TASK_NAMESPACE = %s, want default", env.Value)
			}
		case ResultEndpointEnvVar:
			hasResultEndpoint = true
		case ControllerURLEnvVar:
			hasControllerURL = true
		case "CUSTOM_VAR":
			hasCustomVar = true
			if env.Value != "custom-value" {
				t.Errorf("CUSTOM_VAR = %s, want custom-value", env.Value)
			}
		}
	}

	if !hasTaskName {
		t.Error("Missing ORKA_TASK_NAME")
	}
	if !hasTaskNamespace {
		t.Error("Missing ORKA_TASK_NAMESPACE")
	}
	if !hasResultEndpoint {
		t.Error("Missing ORKA_RESULT_ENDPOINT")
	}
	if !hasControllerURL {
		t.Error("Missing ORKA_CONTROLLER_URL")
	}
	if !hasCustomVar {
		t.Error("Missing CUSTOM_VAR")
	}
}

func TestJobBuilder_buildEnvVars_AITask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider:     "anthropic",
				Model:        "claude-3-5-sonnet",
				Prompt:       "Hello",
				SystemPrompt: "You are helpful",
				Tools:        []string{"web_search"},
			},
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, nil, nil)

	// Check AI-specific env vars
	hasProvider := false
	hasModel := false
	hasPrompt := false

	for _, env := range envVars {
		switch env.Name {
		case envAIProviderKey:
			hasProvider = true
			if env.Value != "anthropic" {
				t.Errorf("ORKA_AI_PROVIDER = %s, want anthropic", env.Value)
			}
		case "ORKA_AI_MODEL":
			hasModel = true
			if env.Value != "claude-3-5-sonnet" {
				t.Errorf("ORKA_AI_MODEL = %s, want claude-3-5-sonnet", env.Value)
			}
		case "ORKA_AI_PROMPT":
			hasPrompt = true
			if env.Value != "Hello" {
				t.Errorf("ORKA_AI_PROMPT = %s, want Hello", env.Value)
			}
		}
	}

	if !hasProvider {
		t.Error("Missing ORKA_AI_PROVIDER")
	}
	if !hasModel {
		t.Error("Missing ORKA_AI_MODEL")
	}
	if !hasPrompt {
		t.Error("Missing ORKA_AI_PROMPT")
	}
}

func TestJobBuilder_buildEnvVars_WithAgent(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Hello from task",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4",
			},
			SystemPrompt: &corev1alpha1.PromptSource{
				Inline: "You are an assistant",
			},
			Tools: []corev1alpha1.ToolReference{
				{Name: "code_exec"},
			},
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, agent, nil)

	// Agent values should be used when task doesn't specify them
	hasProvider := false
	hasModel := false

	for _, env := range envVars {
		switch env.Name {
		case envAIProviderKey:
			hasProvider = true
			if env.Value != "openai" {
				t.Errorf("ORKA_AI_PROVIDER = %s, want openai", env.Value)
			}
		case "ORKA_AI_MODEL":
			hasModel = true
			if env.Value != "gpt-4" {
				t.Errorf("ORKA_AI_MODEL = %s, want gpt-4", env.Value)
			}
		}
	}

	if !hasProvider {
		t.Error("Missing ORKA_AI_PROVIDER")
	}
	if !hasModel {
		t.Error("Missing ORKA_AI_MODEL")
	}
}

func TestJobBuilder_buildEnvVars_WithProvider(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Hello",
		},
	}
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-3-opus",
			BaseURL:      "https://custom.api.com",
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, nil, provider)

	hasBaseURL := false
	for _, env := range envVars {
		if env.Name == "ORKA_AI_BASE_URL" {
			hasBaseURL = true
			if env.Value != "https://custom.api.com" {
				t.Errorf("ORKA_AI_BASE_URL = %s, want https://custom.api.com", env.Value)
			}
		}
	}

	if !hasBaseURL {
		t.Error("Missing ORKA_AI_BASE_URL")
	}
}

func TestJobBuilder_buildEnvVars_ProviderCRDTypeOverridesModelProvider(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Hello",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4",
			},
		},
	}
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type:    corev1alpha1.ProviderTypeAzureOpenAI,
			BaseURL: "https://my-azure.openai.azure.com",
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, agent, provider)

	for _, env := range envVars {
		if env.Name == envAIProviderKey {
			if env.Value != string(corev1alpha1.ProviderTypeAzureOpenAI) {
				t.Errorf("ORKA_AI_PROVIDER = %s, want %s (Provider CRD type should override model.provider)",
					env.Value, corev1alpha1.ProviderTypeAzureOpenAI)
			}
			return
		}
	}
	t.Error("Missing ORKA_AI_PROVIDER")
}

func TestJobBuilder_buildContainer_ContainerWithoutImage(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: "", // No image specified
		},
	}

	container := builder.buildContainer(context.Background(), task, nil, nil)
	if container.Image != DefaultGeneralWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultGeneralWorkerImage)
	}
}

func TestConstants(t *testing.T) {
	if DefaultAIWorkerImage != "ghcr.io/sozercan/orka/ai-worker:latest" {
		t.Errorf("DefaultAIWorkerImage = %s", DefaultAIWorkerImage)
	}
	if DefaultGeneralWorkerImage != "ghcr.io/sozercan/orka/general-worker:latest" {
		t.Errorf("DefaultGeneralWorkerImage = %s", DefaultGeneralWorkerImage)
	}
	if ResultEndpointEnvVar != "ORKA_RESULT_ENDPOINT" {
		t.Errorf("ResultEndpointEnvVar = %s", ResultEndpointEnvVar)
	}
	if ControllerURLEnvVar != "ORKA_CONTROLLER_URL" {
		t.Errorf("ControllerURLEnvVar = %s", ControllerURLEnvVar)
	}
	if TaskNameEnvVar != "ORKA_TASK_NAME" {
		t.Errorf("TaskNameEnvVar = %s", TaskNameEnvVar)
	}
	if TaskNamespaceEnvVar != "ORKA_TASK_NAMESPACE" {
		t.Errorf("TaskNamespaceEnvVar = %s", TaskNamespaceEnvVar)
	}
}

func TestJobBuilder_buildEnvVars_WithCoordination(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Coordinate work",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:               true,
				MaxDepth:              3,
				MaxConcurrentChildren: 5,
				AllowedAgents: []corev1alpha1.AllowedAgent{
					{Name: "backend-dev"},
					{Name: "frontend-dev"},
				},
			},
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, agent, nil)

	tests := []struct {
		name  string
		value string
	}{
		{"ORKA_COORDINATION_ENABLED", "true"},
		{"ORKA_COORDINATION_MAX_DEPTH", "3"},
		{"ORKA_COORDINATION_MAX_CHILDREN", "5"},
		{"ORKA_COORDINATION_ALLOWED_AGENTS", "backend-dev,frontend-dev"},
		{"ORKA_COORDINATION_DEPTH", "0"},
	}
	for _, tt := range tests {
		env, found := findEnvVar(envVars, tt.name)
		if !found {
			t.Errorf("Missing %s", tt.name)
		} else if env.Value != tt.value {
			t.Errorf("%s = %s, want %s", tt.name, env.Value, tt.value)
		}
	}

	toolsEnv, found := findEnvVar(envVars, "ORKA_AI_TOOLS")
	if !found {
		t.Fatal("Missing ORKA_AI_TOOLS")
	}
	for _, tool := range []string{"delegate_task", "wait_for_tasks", "create_container_task", "list_pull_requests", "check_pr_review_marker", "check_pull_request_ci"} {
		if !strings.Contains(toolsEnv.Value, tool) {
			t.Errorf("ORKA_AI_TOOLS = %s, want to contain %s", toolsEnv.Value, tool)
		}
	}
}

func TestJobBuilder_buildEnvVars_WithCoordination_IncludesMemoryTools(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Coordinate work and remember important findings",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled: true,
			},
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, agent, nil)

	toolsEnv, found := findEnvVar(envVars, "ORKA_AI_TOOLS")
	if !found {
		t.Fatal("Missing ORKA_AI_TOOLS")
	}

	tools := map[string]bool{}
	for tool := range strings.SplitSeq(toolsEnv.Value, ",") {
		tools[strings.TrimSpace(tool)] = true
	}

	for _, want := range []string{"recall_memory", "remember", "propose_memory", "search_transcript"} {
		if !tools[want] {
			t.Errorf("ORKA_AI_TOOLS = %s, want to contain %s", toolsEnv.Value, want)
		}
	}
}

func TestJobBuilder_buildEnvVars_WithCoordination_ChildTask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task",
			Namespace: defaultNS,
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: "2",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Sub-task work",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:               true,
				MaxDepth:              3,
				MaxConcurrentChildren: 5,
				AllowedAgents: []corev1alpha1.AllowedAgent{
					{Name: "backend-dev"},
				},
			},
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, agent, nil)

	env, found := findEnvVar(envVars, "ORKA_COORDINATION_DEPTH")
	if !found {
		t.Fatal("Missing ORKA_COORDINATION_DEPTH")
	}
	if env.Value != "2" {
		t.Errorf("ORKA_COORDINATION_DEPTH = %s, want 2", env.Value)
	}
}

func TestJobBuilder_buildEnvVars_NoCoordination(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Simple task",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
		},
	}

	envVars := builder.buildEnvVars(context.Background(), task, agent, nil)

	coordinationVars := []string{
		"ORKA_COORDINATION_ENABLED",
		"ORKA_COORDINATION_MAX_DEPTH",
		"ORKA_COORDINATION_MAX_CHILDREN",
		"ORKA_COORDINATION_ALLOWED_AGENTS",
		"ORKA_COORDINATION_DEPTH",
	}
	for _, name := range coordinationVars {
		if _, found := findEnvVar(envVars, name); found {
			t.Errorf("Unexpected env var %s present without coordination", name)
		}
	}
}

// --- Agent task path tests ---

// helper to find env var by name
func findEnvVar(envVars []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range envVars {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func countEnvVars(envVars []corev1.EnvVar, name string) int {
	count := 0
	for _, e := range envVars {
		if e.Name == name {
			count++
		}
	}
	return count
}

// helper to check volume exists by name
func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// helper to find a volume mount by name
func findVolumeMount(mounts []corev1.VolumeMount, name string) (corev1.VolumeMount, bool) {
	for _, m := range mounts {
		if m.Name == name {
			return m, true
		}
	}
	return corev1.VolumeMount{}, false
}

// helper to check volume mount exists by name
func hasVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	_, ok := findVolumeMount(mounts, name)
	return ok
}

// helper to check EnvFrom includes the standard agent secret.
func hasAgentEnvFromSecret(envFrom []corev1.EnvFromSource) bool {
	for _, ef := range envFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == testAgentSecretName {
			return true
		}
	}
	return false
}

func agentSandboxEnvNames() []string {
	return []string{
		workerenv.AgentSandboxEnabled,
		workerenv.AgentSandboxRouterURL,
		workerenv.AgentSandboxTemplateName,
		workerenv.AgentSandboxTemplateNamespace,
		workerenv.AgentSandboxClaimNamespace,
		workerenv.AgentSandboxReusePolicy,
		workerenv.AgentSandboxReuseKey,
		workerenv.AgentSandboxCleanupPolicy,
		workerenv.AgentSandboxWarmPoolPolicy,
		workerenv.AgentSandboxNamespaceStrategy,
		workerenv.AgentSandboxClaimTimeoutSeconds,
		workerenv.AgentSandboxCommandTimeoutSeconds,
		workerenv.AgentSandboxDepth,
	}
}

func TestJobBuilder_BuildWithOptions_AgentTask_NoSandboxWorkspaceEnvWithoutRequest(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task-no-sandbox",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}

	job, err := builder.BuildWithOptions(context.Background(), task, nil, nil, JobBuildOptions{})
	if err != nil {
		t.Fatalf("BuildWithOptions() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	for _, name := range agentSandboxEnvNames() {
		if _, found := findEnvVar(envVars, name); found {
			t.Errorf("unexpected sandbox env var %s without workspace request", name)
		}
	}
}

func TestJobBuilder_BuildWithOptions_AgentTask_AddsSandboxWorkspaceEnv(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task-sandbox",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
			Execution: &corev1alpha1.ExecutionSpec{
				RuntimeClassName: testRuntimeClassKata,
				NodeSelector: map[string]string{
					testNodeLabelKey: testNodeValueKata,
				},
				Tolerations: []corev1.Toleration{
					{
						Key:      testNodeLabelKey,
						Operator: corev1.TolerationOpEqual,
						Value:    testNodeValueKata,
						Effect:   corev1.TaintEffectNoSchedule,
					},
				},
			},
		},
	}
	request := &AgentSandboxWorkspaceRequest{
		RouterURL:         "http://agent-sandbox-router.default.svc",
		TemplateName:      "workspace-template",
		TemplateNamespace: "sandbox-system",
		ClaimNamespace:    "sandbox-system",
		ReusePolicy:       corev1alpha1.WorkspaceReusePolicySession,
		ReuseKey:          "session-123",
		CleanupPolicy:     corev1alpha1.WorkspaceCleanupPolicyRetain,
		WarmPoolPolicy:    AgentSandboxWarmPoolPolicyTemplate,
		NamespaceStrategy: AgentSandboxNamespaceStrategyController,
		ClaimTimeout:      2 * time.Minute,
		CommandTimeout:    30 * time.Minute,
	}

	job, err := builder.BuildWithOptions(context.Background(), task, nil, nil, JobBuildOptions{
		AgentSandboxWorkspace: request,
	})
	if err != nil {
		t.Fatalf("BuildWithOptions() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	expected := map[string]string{
		workerenv.AgentSandboxEnabled:               "true",
		workerenv.AgentSandboxRouterURL:             "http://agent-sandbox-router.default.svc",
		workerenv.AgentSandboxTemplateName:          "workspace-template",
		workerenv.AgentSandboxTemplateNamespace:     "sandbox-system",
		workerenv.AgentSandboxClaimNamespace:        "sandbox-system",
		workerenv.AgentSandboxReusePolicy:           "session",
		workerenv.AgentSandboxReuseKey:              "session-123",
		workerenv.AgentSandboxCleanupPolicy:         "retain",
		workerenv.AgentSandboxWarmPoolPolicy:        "template",
		workerenv.AgentSandboxNamespaceStrategy:     "controller",
		workerenv.AgentSandboxClaimTimeoutSeconds:   "120",
		workerenv.AgentSandboxCommandTimeoutSeconds: "1800",
		workerenv.AgentSandboxDepth:                 "0",
	}
	for name, want := range expected {
		ev, ok := findEnvVar(envVars, name)
		if !ok {
			t.Errorf("missing sandbox env var %s", name)
			continue
		}
		if ev.Value != want {
			t.Errorf("%s = %q, want %q", name, ev.Value, want)
		}
	}

	podSpec := job.Spec.Template.Spec
	if podSpec.RuntimeClassName == nil || *podSpec.RuntimeClassName != testRuntimeClassKata {
		t.Fatalf("RuntimeClassName = %v, want %s", podSpec.RuntimeClassName, testRuntimeClassKata)
	}
	if got := podSpec.NodeSelector[testNodeLabelKey]; got != testNodeValueKata {
		t.Errorf("NodeSelector[%s] = %q, want %q", testNodeLabelKey, got, testNodeValueKata)
	}
	if len(podSpec.Tolerations) != 1 {
		t.Fatalf("Tolerations len = %d, want 1", len(podSpec.Tolerations))
	}
	if podSpec.Tolerations[0].Value != testNodeValueKata {
		t.Errorf("Tolerations[0].Value = %q, want %q", podSpec.Tolerations[0].Value, testNodeValueKata)
	}
}

func TestJobBuilder_BuildWithOptions_NonAgentTask_IgnoresSandboxWorkspaceEnv(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container-task-sandbox",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"sh", "-c"},
			Args:    []string{"echo hello"},
		},
	}
	request := &AgentSandboxWorkspaceRequest{
		RouterURL:         "http://agent-sandbox-router.default.svc",
		TemplateName:      "workspace-template",
		TemplateNamespace: "sandbox-system",
		ClaimNamespace:    "sandbox-system",
		ReusePolicy:       corev1alpha1.WorkspaceReusePolicySession,
		ReuseKey:          "session-123",
		CleanupPolicy:     corev1alpha1.WorkspaceCleanupPolicyRetain,
		WarmPoolPolicy:    AgentSandboxWarmPoolPolicyTemplate,
		NamespaceStrategy: AgentSandboxNamespaceStrategyController,
		ClaimTimeout:      2 * time.Minute,
		CommandTimeout:    30 * time.Minute,
	}

	job, err := builder.BuildWithOptions(context.Background(), task, nil, nil, JobBuildOptions{
		AgentSandboxWorkspace: request,
	})
	if err != nil {
		t.Fatalf("BuildWithOptions() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	for _, name := range agentSandboxEnvNames() {
		if _, found := findEnvVar(envVars, name); found {
			t.Errorf("unexpected sandbox env var %s for non-agent task", name)
		}
	}
}

func TestJobBuilder_BuildWithOptions_AgentTask_AddsSubstrateWorkspaceEnv(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task-substrate",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	request := &ExecutionWorkspaceRequest{
		Provider:                           corev1alpha1.WorkspaceProviderSubstrate,
		TemplateName:                       "orka-codex",
		TemplateNamespace:                  "ate-demo",
		ClaimNamespace:                     "ate-demo",
		ClaimName:                          "orka-t-abc-1",
		ReusePolicy:                        corev1alpha1.WorkspaceReusePolicyNone,
		CleanupPolicy:                      corev1alpha1.WorkspaceCleanupPolicyDelete,
		Boot:                               true,
		PoolName:                           "codex-pool",
		PoolNamespace:                      "ate-demo",
		SnapshotRestoreURI:                 "gs://ate-snapshots/restore/",
		SnapshotCheckpointURI:              "gs://ate-snapshots/checkpoint/",
		SnapshotOnRelease:                  true,
		ProcessMode:                        corev1alpha1.ExecutionWorkspaceProcessModeResident,
		ResidentKey:                        "resident-session",
		ClaimTimeout:                       2 * time.Minute,
		CommandTimeout:                     30 * time.Minute,
		SubstrateAPIEndpoint:               "api.ate-system.svc:443",
		SubstrateAPICAFile:                 "/var/run/orka/substrate/ca.crt",
		SubstrateRouterURL:                 "http://atenet-router.ate-system.svc",
		SubstrateActorDNSSuffix:            "actors.resources.substrate.ate.dev",
		SubstrateBootstrapSecretName:       "orka-substrate-bootstrap",
		SubstrateBootstrapSecretKey:        "token",
		SubstrateSessionIdentitySecretName: "orka-substrate-session-identity",
		SubstrateSessionIdentitySecretKey:  "session-token",
		SubstrateSessionIdentityRequired:   true,
		SubstrateSessionIdentityAudience:   "orka-workspace-daemon,custom-audience",
		SubstrateSessionIdentityAppID:      "orka",
		SubstrateSessionIdentityUserID:     "orka-worker",
	}

	job, err := builder.BuildWithOptions(context.Background(), task, nil, nil, JobBuildOptions{
		ExecutionWorkspace: request,
	})
	if err != nil {
		t.Fatalf("BuildWithOptions() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	expected := map[string]string{
		workerenv.ExecutionWorkspaceProvider:              "substrate",
		workerenv.ExecutionWorkspaceTemplateName:          "orka-codex",
		workerenv.ExecutionWorkspaceClaimName:             "orka-t-abc-1",
		workerenv.SubstrateAPIEndpoint:                    "api.ate-system.svc:443",
		workerenv.SubstrateAPICAFile:                      "/var/run/orka/substrate/ca.crt",
		workerenv.SubstrateRouterURL:                      "http://atenet-router.ate-system.svc",
		workerenv.SubstrateActorDNSSuffix:                 "actors.resources.substrate.ate.dev",
		workerenv.ExecutionWorkspaceCleanupPolicy:         "delete",
		workerenv.ExecutionWorkspaceClaimNamespace:        "ate-demo",
		workerenv.ExecutionWorkspaceBoot:                  "true",
		workerenv.ExecutionWorkspacePoolName:              "codex-pool",
		workerenv.ExecutionWorkspacePoolNamespace:         "ate-demo",
		workerenv.ExecutionWorkspaceSnapshotRestoreURI:    "gs://ate-snapshots/restore/",
		workerenv.ExecutionWorkspaceSnapshotCheckpointURI: "gs://ate-snapshots/checkpoint/",
		workerenv.ExecutionWorkspaceSnapshotOnRelease:     "true",
		workerenv.ExecutionWorkspaceProcessMode:           "resident",
		workerenv.ExecutionWorkspaceResidentKey:           "resident-session",
		workerenv.SubstrateSessionIdentityRequired:        "true",
		workerenv.SubstrateSessionIdentityAudience:        "orka-workspace-daemon,custom-audience",
		workerenv.SubstrateSessionIdentityAppID:           "orka",
		workerenv.SubstrateSessionIdentityUserID:          "orka-worker",
	}
	for name, want := range expected {
		ev, ok := findEnvVar(envVars, name)
		if !ok {
			t.Errorf("missing substrate env var %s", name)
			continue
		}
		if ev.Value != want {
			t.Errorf("%s = %q, want %q", name, ev.Value, want)
		}
	}
	bootstrapEnv, ok := findEnvVar(envVars, workerenv.WorkspaceBootstrapToken)
	if !ok {
		t.Fatalf("missing %s env var", workerenv.WorkspaceBootstrapToken)
	}
	if bootstrapEnv.ValueFrom == nil || bootstrapEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s ValueFrom = %#v, want SecretKeyRef", workerenv.WorkspaceBootstrapToken, bootstrapEnv.ValueFrom)
	}
	if got := bootstrapEnv.ValueFrom.SecretKeyRef.Name; got != "orka-substrate-bootstrap" {
		t.Fatalf("bootstrap secret name = %q, want orka-substrate-bootstrap", got)
	}
	if got := bootstrapEnv.ValueFrom.SecretKeyRef.Key; got != "token" {
		t.Fatalf("bootstrap secret key = %q, want token", got)
	}
	sessionIdentityEnv, ok := findEnvVar(envVars, workerenv.SubstrateSessionIdentityToken)
	if !ok {
		t.Fatalf("missing %s env var", workerenv.SubstrateSessionIdentityToken)
	}
	if sessionIdentityEnv.ValueFrom == nil || sessionIdentityEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s ValueFrom = %#v, want SecretKeyRef", workerenv.SubstrateSessionIdentityToken, sessionIdentityEnv.ValueFrom)
	}
	if got := sessionIdentityEnv.ValueFrom.SecretKeyRef.Name; got != "orka-substrate-session-identity" {
		t.Fatalf("SessionIdentity secret name = %q, want orka-substrate-session-identity", got)
	}
	if got := sessionIdentityEnv.ValueFrom.SecretKeyRef.Key; got != "session-token" {
		t.Fatalf("SessionIdentity secret key = %q, want session-token", got)
	}
}

func TestJobBuilder_Build_AgentTask_CopilotRuntime(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCopilot,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, VendorWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultCopilotWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultCopilotWorkerImage)
	}
	if len(container.Command) != 1 || container.Command[0] != "/worker" {
		t.Errorf("Command = %v, want [/worker]", container.Command)
	}
	if len(container.Args) != 1 || container.Args[0] != "--mode=agent" {
		t.Errorf("Args = %v, want [--mode=agent]", container.Args)
	}
}

func TestJobBuilder_Build_AgentTask_ClaudeRuntime(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeClaude,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, VendorWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultClaudeWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultClaudeWorkerImage)
	}
}

func TestJobBuilder_Build_AgentTask_CodexRuntime(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, VendorWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultCodexWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultCodexWorkerImage)
	}
}

func TestJobBuilder_Build_AgentTask_CodexRuntime_ConfiguredSandboxMode(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodexSandboxMode = testCodexSandboxMode
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_CODEX_SANDBOX_MODE")
	if !ok {
		t.Fatal("Missing ORKA_CODEX_SANDBOX_MODE")
	}
	if ev.Value != testCodexSandboxMode {
		t.Errorf("ORKA_CODEX_SANDBOX_MODE = %q, want %q", ev.Value, testCodexSandboxMode)
	}
}

func TestJobBuilder_Build_AgentTask_CodexRuntime_TaskEnvOverridesConfiguredSandboxMode(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodexSandboxMode = testCodexSandboxMode
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
			Env: []corev1.EnvVar{
				{Name: "ORKA_CODEX_SANDBOX_MODE", Value: "workspace-write"},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	ev, ok := findEnvVar(envVars, "ORKA_CODEX_SANDBOX_MODE")
	if !ok {
		t.Fatal("Missing ORKA_CODEX_SANDBOX_MODE")
	}
	if ev.Value != "workspace-write" {
		t.Errorf("ORKA_CODEX_SANDBOX_MODE = %q, want task env value %q", ev.Value, "workspace-write")
	}

	count := 0
	for _, envVar := range envVars {
		if envVar.Name == "ORKA_CODEX_SANDBOX_MODE" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ORKA_CODEX_SANDBOX_MODE count = %d, want 1", count)
	}
}

func TestJobBuilder_Build_AgentTask_ReadOnlyAnnotationRejectsCodexRuntimeBeforeRuntimeSecretLookup(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodexSandboxMode = testCodexSandboxMode
	allowBash := true
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
			Annotations: map[string]string{
				labels.AnnotationAgentReadOnly: "true",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:         corev1alpha1.TaskTypeAgent,
			Prompt:       "Review only",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowBash: &allowBash},
			Env: []corev1.EnvVar{
				{Name: workerenv.CodexSandboxMode, Value: "workspace-write"},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:             corev1alpha1.AgentRuntimeCodex,
				DefaultAllowBash: &allowBash,
			},
			SecretRef: &corev1.LocalObjectReference{Name: "missing-runtime-secret"},
		},
	}

	_, err := builder.Build(context.Background(), task, agent, nil)
	if err == nil {
		t.Fatal("Build() error = nil, want read-only codex rejection")
	}
	if !strings.Contains(err.Error(), "do not support codex runtime") {
		t.Fatalf("Build() error = %q, want codex rejection", err.Error())
	}
	if strings.Contains(err.Error(), "missing-runtime-secret") {
		t.Fatalf("Build() error = %q, want codex rejection before runtime secret lookup", err.Error())
	}
}

func TestJobBuilder_Build_AgentTask_ReadOnlyAnnotationInjectsClaudeRuntimeCredentialKeysWithoutRuntimeSecretMounts(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	runtimeSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentSecretName, Namespace: defaultNS},
		Data: map[string][]byte{
			workerenv.AnthropicAPIKey:  []byte("anthropic-key"),
			workerenv.AnthropicBaseURL: []byte(testProviderBaseURL),
			workerenv.GitHubToken:      []byte("github-token"),
		},
	}
	builder := NewJobBuilder(fake.NewClientBuilder().WithScheme(scheme).WithObjects(runtimeSecret).Build())
	builder.ControllerURL = testControllerURL
	allowBash := true
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
			Annotations: map[string]string{
				labels.AnnotationAgentReadOnly:          scheduledRunLabelValue,
				labels.AnnotationWorkspaceInitContainer: scheduledRunLabelValue,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Review only",
			Env: []corev1.EnvVar{
				{Name: workerenv.AgentReadOnly, Value: "false"},
				{Name: workerenv.ResultStdout, Value: "false"},
				{Name: workerenv.AllowBash, Value: scheduledRunLabelValue},
				{Name: workerenv.AllowedTools, Value: "Bash,Write"},
				{Name: workerenv.DisallowedTools, Value: ""},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/example/repo.git",
					Ref:          "abc123",
					GitSecretRef: &corev1.LocalObjectReference{Name: "git-token"},
				},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:             corev1alpha1.AgentRuntimeClaude,
				DefaultAllowBash: &allowBash,
			},
			SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, false)

	container := job.Spec.Template.Spec.Containers[0]
	assertReadOnlyClaudeRuntimeCredentialEnv(t, container.Env)
	assertReadOnlyClaudeRuntimeSecretIsolation(t, job, container)
}

func TestJobBuilder_Build_AgentTask_ReadOnlyAnnotationRejectsClaudeSecretWithoutAuthKey(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	runtimeSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentSecretName, Namespace: defaultNS},
		Data: map[string][]byte{
			workerenv.AnthropicBaseURL: []byte(testProviderBaseURL),
		},
	}
	builder := NewJobBuilder(fake.NewClientBuilder().WithScheme(scheme).WithObjects(runtimeSecret).Build())
	builder.ControllerURL = testControllerURL
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
			Annotations: map[string]string{
				labels.AnnotationAgentReadOnly: scheduledRunLabelValue,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Review only",
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
			SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
		},
	}

	_, err := builder.Build(context.Background(), task, agent, nil)
	if err == nil || !strings.Contains(err.Error(), "contains no supported auth credential keys") {
		t.Fatalf("Build() error = %v, want missing auth key error", err)
	}
}

func TestJobBuilder_Build_AgentTask_ReadOnlyAnnotationInjectsClaudeCredentialsForNilRuntimeFallback(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	runtimeSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentSecretName, Namespace: defaultNS},
		Data: map[string][]byte{
			workerenv.AnthropicAPIKey:  []byte("anthropic-key"),
			workerenv.AnthropicBaseURL: []byte(testProviderBaseURL),
			workerenv.GitHubToken:      []byte("github-token"),
		},
	}
	builder := NewJobBuilder(fake.NewClientBuilder().WithScheme(scheme).WithObjects(runtimeSecret).Build())
	builder.ControllerURL = testControllerURL
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
			Annotations: map[string]string{
				labels.AnnotationAgentReadOnly: scheduledRunLabelValue,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Review only",
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultClaudeWorkerImage {
		t.Fatalf("container image = %q, want Claude fallback image %q", container.Image, DefaultClaudeWorkerImage)
	}
	assertReadOnlyClaudeRuntimeCredentialEnv(t, container.Env)
	if hasAgentEnvFromSecret(container.EnvFrom) {
		t.Fatal("agent secret EnvFrom should not be present for read-only reviewer agents")
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "agent-secrets") {
		t.Fatal("agent-secrets volume should not be present for read-only reviewer agents")
	}
	if hasVolumeMount(container.VolumeMounts, "agent-secrets") {
		t.Fatal("agent-secrets volume mount should not be present for read-only reviewer agents")
	}
}

func assertReadOnlyClaudeRuntimeCredentialEnv(t *testing.T, env []corev1.EnvVar) {
	t.Helper()

	assertSecretKeyEnvVar(t, env, workerenv.AnthropicAPIKey, testAgentSecretName, workerenv.AnthropicAPIKey)
	assertSecretKeyEnvVar(t, env, workerenv.AnthropicBaseURL, testAgentSecretName, workerenv.AnthropicBaseURL)
	assertEnvVarAbsent(t, env, workerenv.GitHubToken, "GITHUB_TOKEN should not be injected into read-only reviewer agents")
	assertEnvVarAbsent(t, env, workerenv.AllowBash, "ORKA_ALLOW_BASH should not be enabled for read-only reviewer agents")
	assertEnvVarValue(t, env, workerenv.AgentReadOnly, scheduledRunLabelValue)
	assertEnvVarValue(t, env, workerenv.ResultStdout, scheduledRunLabelValue)
	assertEnvVarValue(t, env, workerenv.ClaudeBare, scheduledRunLabelValue)
	assertEnvVarValue(t, env, workerenv.ClaudeDisableSettingSources, scheduledRunLabelValue)
	assertEnvVarValue(t, env, workerenv.ClaudePermissionMode, "dontAsk")
	assertReadOnlyClaudeToolEnv(t, env)
}

func assertSecretKeyEnvVar(t *testing.T, env []corev1.EnvVar, name, secretName, key string) {
	t.Helper()

	envVar, ok := findEnvVar(env, name)
	if !ok {
		t.Fatalf("%s should be present for read-only reviewer agents", name)
	}
	if envVar.ValueFrom == nil ||
		envVar.ValueFrom.SecretKeyRef == nil ||
		envVar.ValueFrom.SecretKeyRef.Name != secretName ||
		envVar.ValueFrom.SecretKeyRef.Key != key {
		t.Fatalf("%s secret ref = %#v, want %s/%s", name, envVar.ValueFrom, secretName, key)
	}
}

func assertEnvVarAbsent(t *testing.T, env []corev1.EnvVar, name, message string) {
	t.Helper()

	if _, ok := findEnvVar(env, name); ok {
		t.Fatal(message)
	}
}

func assertEnvVarValue(t *testing.T, env []corev1.EnvVar, name, want string) {
	t.Helper()

	envVar, ok := findEnvVar(env, name)
	if !ok || envVar.Value != want {
		t.Fatalf("%s = %q, present %v; want %q", name, envVar.Value, ok, want)
	}
}

func assertReadOnlyClaudeToolEnv(t *testing.T, env []corev1.EnvVar) {
	t.Helper()

	allowedTools, ok := findEnvVar(env, workerenv.AllowedTools)
	if !ok {
		t.Fatal("ORKA_ALLOWED_TOOLS should be present for read-only reviewer agents")
	}
	if allowedTools.Value != joinStrings(readOnlyAgentAllowedTools()) {
		t.Fatalf("ORKA_ALLOWED_TOOLS = %q, want %q", allowedTools.Value, joinStrings(readOnlyAgentAllowedTools()))
	}
	if strings.Contains(allowedTools.Value, "Read,") || strings.Contains(allowedTools.Value, ",Read,") {
		t.Fatalf("ORKA_ALLOWED_TOOLS = %q, should not include unrestricted Read", allowedTools.Value)
	}

	disallowedTools, ok := findEnvVar(env, workerenv.DisallowedTools)
	if !ok {
		t.Fatal("ORKA_DISALLOWED_TOOLS should be present for read-only reviewer agents")
	}
	for _, denied := range []string{
		"Bash",
		"Read(/proc/**)",
		"Read(/var/run/secrets/**)",
		"Read(/secrets/**)",
		"Read(/home/worker/**)",
	} {
		if !strings.Contains(disallowedTools.Value, denied) {
			t.Fatalf("ORKA_DISALLOWED_TOOLS = %q, want %q", disallowedTools.Value, denied)
		}
	}
}

func assertReadOnlyClaudeRuntimeSecretIsolation(t *testing.T, job *batchv1.Job, container corev1.Container) {
	t.Helper()

	if hasAgentEnvFromSecret(container.EnvFrom) {
		t.Fatal("agent secret EnvFrom should not be present for read-only reviewer agents")
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "agent-secrets") {
		t.Fatal("agent-secrets volume should not be present for read-only reviewer agents")
	}
	if hasVolumeMount(container.VolumeMounts, "agent-secrets") {
		t.Fatal("agent-secrets volume mount should not be present for read-only reviewer agents")
	}
	if !hasVolume(job.Spec.Template.Spec.Volumes, testGitCredentials) {
		t.Fatal("git-credentials volume should exist for the workspace init container")
	}
	if hasVolumeMount(container.VolumeMounts, testGitCredentials) {
		t.Fatal("git-credentials volume mount should not be present on the read-only reviewer main container")
	}
	if len(job.Spec.Template.Spec.InitContainers) != 1 ||
		!hasVolumeMount(job.Spec.Template.Spec.InitContainers[0].VolumeMounts, testGitCredentials) {
		t.Fatalf("init containers = %#v, want workspace init container with git credentials mount", job.Spec.Template.Spec.InitContainers)
	}
}

func TestJobBuilder_Build_AgentTask_ReadOnlyAnnotationRejectsCopilotRuntime(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
			Annotations: map[string]string{
				labels.AnnotationAgentReadOnly: scheduledRunLabelValue,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Review only",
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot},
			SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
		},
	}

	_, err := builder.Build(context.Background(), task, agent, nil)
	if err == nil {
		t.Fatal("Build() error = nil, want read-only copilot rejection")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN can mutate GitHub") {
		t.Fatalf("Build() error = %q, want GITHUB_TOKEN rejection", err.Error())
	}
}

func TestJobBuilder_BuildWithOptions_ReadOnlyAgentWorkspaceWrapperKeepsOuterServiceAccountToken(t *testing.T) {
	builder := setupJobBuilder()
	for name, opts := range map[string]JobBuildOptions{
		"legacy_agent_sandbox": {
			AgentSandboxWorkspace: &AgentSandboxWorkspaceRequest{
				RouterURL:         "http://agent-sandbox-router.default.svc",
				TemplateName:      "workspace-template",
				TemplateNamespace: "sandbox-system",
				ClaimNamespace:    "sandbox-system",
				ReusePolicy:       corev1alpha1.WorkspaceReusePolicySession,
				ReuseKey:          "session-123",
				CleanupPolicy:     corev1alpha1.WorkspaceCleanupPolicyDelete,
				WarmPoolPolicy:    AgentSandboxWarmPoolPolicyTemplate,
				NamespaceStrategy: AgentSandboxNamespaceStrategyController,
				ClaimTimeout:      2 * time.Minute,
				CommandTimeout:    30 * time.Minute,
			},
		},
		"execution_workspace": {
			ExecutionWorkspace: &ExecutionWorkspaceRequest{
				Provider:          corev1alpha1.WorkspaceProviderAgentSandbox,
				TemplateName:      "workspace-template",
				TemplateNamespace: "sandbox-system",
				ClaimNamespace:    "sandbox-system",
				ReusePolicy:       corev1alpha1.WorkspaceReusePolicyNone,
				CleanupPolicy:     corev1alpha1.WorkspaceCleanupPolicyDelete,
				ClaimTimeout:      2 * time.Minute,
				CommandTimeout:    30 * time.Minute,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "agent-task",
					Namespace: defaultNS,
					UID:       types.UID("12345678-1234-1234-1234-123456789012"),
					Annotations: map[string]string{
						labels.AnnotationAgentReadOnly: scheduledRunLabelValue,
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAgent,
					Prompt: "Review only",
				},
			}

			job, err := builder.BuildWithOptions(context.Background(), task, nil, nil, opts)
			if err != nil {
				t.Fatalf("BuildWithOptions() error = %v", err)
			}

			assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)
			envVars := job.Spec.Template.Spec.Containers[0].Env
			assertEnvVarValue(t, envVars, workerenv.AgentReadOnly, scheduledRunLabelValue)
			assertEnvVarValue(t, envVars, workerenv.ResultStdout, scheduledRunLabelValue)
		})
	}
}

func TestJobBuilder_Build_AgentTask_NonCodexRuntime_NoConfiguredSandboxMode(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodexSandboxMode = testCodexSandboxMode
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeClaude,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if _, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_CODEX_SANDBOX_MODE"); ok {
		t.Fatal("Unexpected ORKA_CODEX_SANDBOX_MODE for non-Codex runtime")
	}
}

func TestJobBuilder_Build_AgentTask_NilAgent_FallbackToClaude(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultClaudeWorkerImage {
		t.Errorf("Image = %s, want %s (fallback)", container.Image, DefaultClaudeWorkerImage)
	}
}

func TestJobBuilder_Build_AgentTask_NilRuntime_FallbackToClaude(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			// Runtime is nil
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultClaudeWorkerImage {
		t.Errorf("Image = %s, want %s (fallback)", container.Image, DefaultClaudeWorkerImage)
	}
}

func TestJobBuilder_Build_AgentTask_EnvVars(t *testing.T) {
	builder := setupJobBuilder()
	maxTurns := int32(20)
	allowBash := true
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "test-ns",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Refactor the code",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				MaxTurns:        &maxTurns,
				AllowedTools:    []string{"Read", "Write", "Bash"},
				DisallowedTools: []string{"WebFetch"},
				AllowBash:       &allowBash,
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:    "https://github.com/example/repo",
					Branch:     "main",
					Ref:        "abc123",
					SubPath:    "src",
					PushBranch: "feature/my-change",
				},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Name: "claude-sonnet-4-20250514",
			},
			SystemPrompt: &corev1alpha1.PromptSource{
				Inline: "You are a coding assistant",
			},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeClaude,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env

	tests := []struct {
		name  string
		value string
	}{
		{TaskNameEnvVar, "agent-task"},
		{TaskNamespaceEnvVar, "test-ns"},
		{ResultEndpointEnvVar, "http://orka-controller.orka-system.svc:8080/internal/v1/results/test-ns/agent-task"},
		{ControllerURLEnvVar, testControllerURL},
		{"ORKA_PROMPT", "Refactor the code"},
		{"ORKA_MODEL", "claude-sonnet-4-20250514"},
		{"ORKA_SYSTEM_PROMPT", "You are a coding assistant"},
		{"ORKA_MAX_TURNS", "20"},
		{"ORKA_ALLOWED_TOOLS", "Read,Write,Bash"},
		{"ORKA_DISALLOWED_TOOLS", "WebFetch"},
		{"ORKA_ALLOW_BASH", "true"},
		{"ORKA_GIT_REPO", "https://github.com/example/repo"},
		{"ORKA_GIT_BRANCH", "main"},
		{"ORKA_GIT_REF", "abc123"},
		{"ORKA_WORKSPACE_SUBPATH", "src"},
		{"ORKA_PUSH_BRANCH", "feature/my-change"},
		{"ORKA_REQUIRE_PUSH_BRANCH", "true"},
	}

	for _, tt := range tests {
		ev, ok := findEnvVar(envVars, tt.name)
		if !ok {
			t.Errorf("Missing env var %s", tt.name)
			continue
		}
		if ev.Value != tt.value {
			t.Errorf("%s = %q, want %q", tt.name, ev.Value, tt.value)
		}
	}
}

func TestJobBuilder_Build_AgentTask_MaxTurns_Default(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_MAX_TURNS")
	if !ok {
		t.Fatal("Missing ORKA_MAX_TURNS")
	}
	if ev.Value != "50" {
		t.Errorf("ORKA_MAX_TURNS = %s, want 50 (default)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_MaxTurns_AgentDefault(t *testing.T) {
	builder := setupJobBuilder()
	agentMaxTurns := int32(100)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeClaude,
				DefaultMaxTurns: &agentMaxTurns,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_MAX_TURNS")
	if !ok {
		t.Fatal("Missing ORKA_MAX_TURNS")
	}
	if ev.Value != "100" {
		t.Errorf("ORKA_MAX_TURNS = %s, want 100 (from agent)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_MaxTurns_TaskOverridesAgent(t *testing.T) {
	builder := setupJobBuilder()
	agentMaxTurns := int32(100)
	taskMaxTurns := int32(30)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				MaxTurns: &taskMaxTurns,
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeClaude,
				DefaultMaxTurns: &agentMaxTurns,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_MAX_TURNS")
	if !ok {
		t.Fatal("Missing ORKA_MAX_TURNS")
	}
	if ev.Value != "30" {
		t.Errorf("ORKA_MAX_TURNS = %s, want 30 (task override)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowedTools_AgentDefault(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                corev1alpha1.AgentRuntimeClaude,
				DefaultAllowedTools: []string{"Read", "Write"},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_ALLOWED_TOOLS")
	if !ok {
		t.Fatal("Missing ORKA_ALLOWED_TOOLS")
	}
	if ev.Value != "Read,Write" {
		t.Errorf("ORKA_ALLOWED_TOOLS = %s, want Read,Write", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowedTools_TaskOverridesAgent(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				AllowedTools: []string{"Bash"},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                corev1alpha1.AgentRuntimeClaude,
				DefaultAllowedTools: []string{"Read", "Write"},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_ALLOWED_TOOLS")
	if !ok {
		t.Fatal("Missing ORKA_ALLOWED_TOOLS")
	}
	if ev.Value != "Bash" {
		t.Errorf("ORKA_ALLOWED_TOOLS = %s, want Bash (task override)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowBash_AgentDefault(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:             corev1alpha1.AgentRuntimeClaude,
				DefaultAllowBash: new(true),
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_ALLOW_BASH")
	if !ok {
		t.Fatal("Missing ORKA_ALLOW_BASH")
	}
	if ev.Value != scheduledRunLabelValue {
		t.Errorf("ORKA_ALLOW_BASH = %s, want true", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowBash_NotSetWhenFalse(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:             corev1alpha1.AgentRuntimeClaude,
				DefaultAllowBash: new(false),
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	_, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_ALLOW_BASH")
	if ok {
		t.Error("ORKA_ALLOW_BASH should not be set when allowBash is false")
	}
}

func TestJobBuilder_Build_AgentTask_PromptFromAISpec(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AI: &corev1alpha1.AISpec{
				Prompt: "Prompt from AI spec",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_PROMPT")
	if !ok {
		t.Fatal("Missing ORKA_PROMPT")
	}
	if ev.Value != "Prompt from AI spec" {
		t.Errorf("ORKA_PROMPT = %q, want %q", ev.Value, "Prompt from AI spec")
	}
}

func TestJobBuilder_Build_AgentTask_Volumes(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	volumes := job.Spec.Template.Spec.Volumes
	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts

	// tmp volume (always present)
	if !hasVolume(volumes, "tmp") {
		t.Error("Missing tmp volume")
	}
	if !hasVolumeMount(mounts, "tmp") {
		t.Error("Missing tmp volume mount")
	}

	// workspace emptyDir
	if !hasVolume(volumes, "workspace") {
		t.Error("Missing workspace volume")
	}
	if !hasVolumeMount(mounts, "workspace") {
		t.Error("Missing workspace volume mount")
	}
	// Verify workspace is emptyDir
	for _, v := range volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Error("workspace volume should be emptyDir")
			}
		}
	}

	// home emptyDir
	if !hasVolume(volumes, "home") {
		t.Error("Missing home volume")
	}
	if !hasVolumeMount(mounts, "home") {
		t.Error("Missing home volume mount")
	}
	// Verify home is emptyDir
	for _, v := range volumes {
		if v.Name == "home" {
			if v.EmptyDir == nil {
				t.Error("home volume should be emptyDir")
			}
		}
	}

	// Verify mount paths
	for _, m := range mounts {
		switch m.Name {
		case "workspace":
			if m.MountPath != "/workspace" {
				t.Errorf("workspace mountPath = %s, want /workspace", m.MountPath)
			}
		case "home":
			if m.MountPath != "/home/worker" {
				t.Errorf("home mountPath = %s, want /home/worker", m.MountPath)
			}
		case "tmp":
			if m.MountPath != "/tmp" {
				t.Errorf("tmp mountPath = %s, want /tmp", m.MountPath)
			}
		}
	}
}

func TestJobBuilder_Build_AgentTask_GitSecretVolume(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Clone and fix",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo",
					GitSecretRef: &corev1.LocalObjectReference{
						Name: "my-git-creds",
					},
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	volumes := job.Spec.Template.Spec.Volumes
	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts

	// git-credentials volume
	if !hasVolume(volumes, testGitCredentials) {
		t.Fatal("Missing git-credentials volume")
	}
	for _, v := range volumes {
		if v.Name == testGitCredentials {
			if v.Secret == nil {
				t.Error("git-credentials should be a Secret volume")
			} else if v.Secret.SecretName != "my-git-creds" {
				t.Errorf("git-credentials secretName = %s, want my-git-creds", v.Secret.SecretName)
			}
		}
	}

	mount, ok := findVolumeMount(mounts, testGitCredentials)
	if !ok {
		t.Fatal("agent main container should mount git credentials because it performs the clone")
	}
	if mount.MountPath != "/secrets/git" {
		t.Errorf("git-credentials mountPath = %s, want /secrets/git", mount.MountPath)
	}
	if !mount.ReadOnly {
		t.Error("git-credentials mount should be read-only")
	}
}

func TestJobBuilder_Build_ContainerTask_GitSecretVolume_DirectMountOptIn(t *testing.T) {
	t.Setenv(directGitCredentialsEnvVar, "true")

	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: testBusyboxImage,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo: "https://github.com/example/repo",
				GitSecretRef: &corev1.LocalObjectReference{
					Name: "my-git-creds",
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	mount, ok := findVolumeMount(job.Spec.Template.Spec.Containers[0].VolumeMounts, testGitCredentials)
	if !ok {
		t.Fatal("Missing git-credentials volume mount")
	}
	if mount.MountPath != "/secrets/git" {
		t.Errorf("git-credentials mountPath = %s, want /secrets/git", mount.MountPath)
	}
	if !mount.ReadOnly {
		t.Error("git-credentials mount should be read-only")
	}
}

func TestJobBuilder_Build_AgentTask_NoGitSecretVolume_WhenNotSpecified(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo",
					// No GitSecretRef
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if hasVolume(job.Spec.Template.Spec.Volumes, testGitCredentials) {
		t.Error("git-credentials volume should not exist when GitSecretRef is not specified")
	}
}

func TestJobBuilder_Build_UntrustedContainerTask_DirectSecretsDisabledByDefault(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeContainer,
			Image:     testBusyboxImage,
			Command:   []string{"echo"},
			Args:      []string{"hello"},
			SecretRef: &corev1alpha1.SecretReference{Name: "task-secret"},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-sec", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
		},
	}
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type: corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: testProviderSecretName,
				Key:  defaultSecretKey,
			},
			BaseURL: testProviderBaseURL,
		},
	}

	job, err := builder.Build(context.Background(), task, agent, provider)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if _, ok := findEnvVar(container.Env, testOpenAIAPIKey); ok {
		t.Fatal("OPENAI_API_KEY should not be present on untrusted container task by default")
	}
	if _, ok := findEnvVar(container.Env, "OPENAI_BASE_URL"); ok {
		t.Fatal("OPENAI_BASE_URL should not be present on untrusted container task by default")
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "task-secrets") {
		t.Fatal("task-secrets volume should not be present on untrusted container task by default")
	}
	if hasVolumeMount(container.VolumeMounts, "task-secrets") {
		t.Fatal("task-secrets volume mount should not be present on untrusted container task by default")
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "agent-secrets") {
		t.Fatal("agent-secrets volume should not be present on untrusted container task by default")
	}
	if hasVolumeMount(container.VolumeMounts, "agent-secrets") {
		t.Fatal("agent-secrets volume mount should not be present on untrusted container task by default")
	}
	if hasAgentEnvFromSecret(container.EnvFrom) {
		t.Fatal("agent secret EnvFrom should not be present on untrusted container task by default")
	}
}

func TestJobBuilder_Build_VendorAgentTask_DirectCredentialsPreservedByDefault(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeAgent,
			Prompt:    "Do something",
			SecretRef: &corev1alpha1.SecretReference{Name: "task-secret"},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-sec", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
			SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
		},
	}
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type: corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: testProviderSecretName,
				Key:  defaultSecretKey,
			},
			BaseURL: testProviderBaseURL,
		},
	}

	job, err := builder.Build(context.Background(), task, agent, provider)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	assertServiceAccountName(t, job.Spec.Template.Spec.ServiceAccountName, VendorWorkerServiceAccount)
	assertAutomountServiceAccountToken(t, job.Spec.Template.Spec.AutomountServiceAccountToken, true)

	container := job.Spec.Template.Spec.Containers[0]
	apiKey, ok := findEnvVar(container.Env, testOpenAIAPIKey)
	if !ok {
		t.Fatal("OPENAI_API_KEY should be present for built-in vendor agents by default")
	}
	if apiKey.ValueFrom == nil || apiKey.ValueFrom.SecretKeyRef == nil || apiKey.ValueFrom.SecretKeyRef.Name != testProviderSecretName || apiKey.ValueFrom.SecretKeyRef.Key != defaultSecretKey {
		t.Errorf("OPENAI_API_KEY secret ref = %#v, want %s/api-key", apiKey.ValueFrom, testProviderSecretName)
	}
	baseURL, ok := findEnvVar(container.Env, "OPENAI_BASE_URL")
	if !ok || baseURL.Value != testProviderBaseURL {
		t.Fatalf("OPENAI_BASE_URL = %q, present %v; want %s", baseURL.Value, ok, testProviderBaseURL)
	}
	if !hasVolume(job.Spec.Template.Spec.Volumes, "task-secrets") {
		t.Fatal("task-secrets volume should be present for built-in vendor agents")
	}
	if !hasVolumeMount(container.VolumeMounts, "task-secrets") {
		t.Fatal("task-secrets volume mount should be present for built-in vendor agents")
	}
	if !hasVolume(job.Spec.Template.Spec.Volumes, "agent-secrets") {
		t.Fatal("agent-secrets volume should be present for built-in vendor agents")
	}
	if !hasVolumeMount(container.VolumeMounts, "agent-secrets") {
		t.Fatal("agent-secrets volume mount should be present for built-in vendor agents")
	}
	if !hasAgentEnvFromSecret(container.EnvFrom) {
		t.Fatal("agent secret EnvFrom should be present for built-in vendor agents")
	}
}

func TestJobBuilder_Build_UntrustedTask_DirectSecretsOptIn(t *testing.T) {
	t.Setenv(directSecretMountsEnvVar, "true")
	t.Setenv(directProviderSecretsEnvVar, "true")

	tests := []struct {
		name     string
		taskType corev1alpha1.TaskType
	}{
		{name: "agent", taskType: corev1alpha1.TaskTypeAgent},
		{name: "container", taskType: corev1alpha1.TaskTypeContainer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := setupJobBuilder()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.name + "-task",
					Namespace: defaultNS,
					UID:       types.UID("12345678-1234-1234-1234-123456789012"),
				},
				Spec: corev1alpha1.TaskSpec{
					Type:      tt.taskType,
					Prompt:    "Do something",
					SecretRef: &corev1alpha1.SecretReference{Name: "task-secret"},
				},
			}
			if tt.taskType == corev1alpha1.TaskTypeContainer {
				task.Spec.Image = testBusyboxImage
				task.Spec.Command = []string{"echo"}
				task.Spec.Args = []string{"hello"}
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "agent-sec", Namespace: defaultNS},
				Spec: corev1alpha1.AgentSpec{
					SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
				},
			}
			provider := &corev1alpha1.Provider{
				Spec: corev1alpha1.ProviderSpec{
					Type: corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{
						Name: testProviderSecretName,
						Key:  defaultSecretKey,
					},
					BaseURL: testProviderBaseURL,
				},
			}

			job, err := builder.Build(context.Background(), task, agent, provider)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}

			container := job.Spec.Template.Spec.Containers[0]
			apiKey, ok := findEnvVar(container.Env, testOpenAIAPIKey)
			if !ok {
				t.Fatal("OPENAI_API_KEY should be present when direct provider secrets are enabled")
			}
			if apiKey.ValueFrom == nil || apiKey.ValueFrom.SecretKeyRef == nil || apiKey.ValueFrom.SecretKeyRef.Name != testProviderSecretName || apiKey.ValueFrom.SecretKeyRef.Key != defaultSecretKey {
				t.Errorf("OPENAI_API_KEY secret ref = %#v, want %s/api-key", apiKey.ValueFrom, testProviderSecretName)
			}
			baseURL, ok := findEnvVar(container.Env, "OPENAI_BASE_URL")
			if !ok || baseURL.Value != testProviderBaseURL {
				t.Fatalf("OPENAI_BASE_URL = %q, present %v; want %s", baseURL.Value, ok, testProviderBaseURL)
			}
			if !hasVolume(job.Spec.Template.Spec.Volumes, "task-secrets") {
				t.Fatal("task-secrets volume should be present when direct secret mounts are enabled")
			}
			taskMount, ok := findVolumeMount(container.VolumeMounts, "task-secrets")
			if !ok {
				t.Fatal("task-secrets volume mount should be present when direct secret mounts are enabled")
			}
			if taskMount.MountPath != "/secrets/task" || !taskMount.ReadOnly {
				t.Errorf("task-secrets mount = %#v, want /secrets/task read-only", taskMount)
			}
			if !hasVolume(job.Spec.Template.Spec.Volumes, "agent-secrets") {
				t.Fatal("agent-secrets volume should be present when direct secret mounts are enabled")
			}
			agentMount, ok := findVolumeMount(container.VolumeMounts, "agent-secrets")
			if !ok {
				t.Fatal("agent-secrets volume mount should be present when direct secret mounts are enabled")
			}
			if agentMount.MountPath != "/secrets/agent" || !agentMount.ReadOnly {
				t.Errorf("agent-secrets mount = %#v, want /secrets/agent read-only", agentMount)
			}
			if !hasAgentEnvFromSecret(container.EnvFrom) {
				t.Fatal("agent secret EnvFrom should be present when direct secret mounts are enabled")
			}
		})
	}
}

func TestJobBuilder_Build_AgentTask_SecurityContext(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Pod security context
	psc := job.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("Pod security context is nil")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("RunAsNonRoot should be true")
	}
	if psc.RunAsUser == nil || *psc.RunAsUser != 1000 {
		t.Errorf("RunAsUser = %v, want 1000", psc.RunAsUser)
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("SeccompProfile should be RuntimeDefault")
	}

	// Container security context
	csc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if csc == nil {
		t.Fatal("Container security context is nil")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false")
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem should be true")
	}
	if csc.RunAsNonRoot == nil || !*csc.RunAsNonRoot {
		t.Error("Container RunAsNonRoot should be true")
	}
	if csc.RunAsUser == nil || *csc.RunAsUser != 1000 {
		t.Errorf("Container RunAsUser = %v, want 1000", csc.RunAsUser)
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) == 0 {
		t.Error("Should drop capabilities")
	}
	if csc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("Capabilities.Drop = %v, want [ALL]", csc.Capabilities.Drop)
	}
}

func TestJobBuilder_Build_AgentTask_NoAgentEnvVars_WhenNoWorkspace(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Simple task",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env

	// Workspace env vars should not be present
	for _, name := range []string{"ORKA_GIT_REPO", "ORKA_GIT_BRANCH", "ORKA_GIT_REF", "ORKA_WORKSPACE_SUBPATH"} {
		if _, ok := findEnvVar(envVars, name); ok {
			t.Errorf("%s should not be set when no workspace is configured", name)
		}
	}

	// But core env vars and agent env vars should still exist
	if _, ok := findEnvVar(envVars, "ORKA_PROMPT"); !ok {
		t.Error("Missing ORKA_PROMPT")
	}
	if _, ok := findEnvVar(envVars, "ORKA_MAX_TURNS"); !ok {
		t.Error("Missing ORKA_MAX_TURNS")
	}
}

func TestJobBuilder_Build_AgentTask_ContainerTaskNoAgentVolumes(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox",
			Command: []string{"echo", "hi"},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Container tasks should NOT have workspace or home volumes
	if hasVolume(job.Spec.Template.Spec.Volumes, "workspace") {
		t.Error("Container task should not have workspace volume")
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "home") {
		t.Error("Container task should not have home volume")
	}
}

func TestJobBuilder_Build_ContainerTask_Workspace(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "golang:1.26",
			Command: []string{"sh", "-lc"},
			Args:    []string{"go test ./..."},
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:    "https://github.com/example/repo.git",
				Branch:     "feature",
				Ref:        "abc123",
				SubPath:    "src",
				PushBranch: "demo/fixup",
				GitSecretRef: &corev1.LocalObjectReference{
					Name: "git-credentials",
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if !hasVolume(job.Spec.Template.Spec.Volumes, "workspace") {
		t.Fatal("missing workspace volume")
	}
	if !hasVolume(job.Spec.Template.Spec.Volumes, "home") {
		t.Fatal("missing home volume")
	}
	if !hasVolume(job.Spec.Template.Spec.Volumes, testGitCredentials) {
		t.Fatal("missing git credentials volume")
	}
	if len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("init container count = %d, want 1", len(job.Spec.Template.Spec.InitContainers))
	}
	init := job.Spec.Template.Spec.InitContainers[0]
	if init.Name != "prepare-workspace" {
		t.Errorf("init name = %q", init.Name)
	}
	if init.Image != builder.GeneralWorkerImage {
		t.Errorf("init image = %q, want %q", init.Image, builder.GeneralWorkerImage)
	}
	if !hasVolumeMount(init.VolumeMounts, testGitCredentials) {
		t.Fatal("prepare-workspace init container missing git-credentials mount")
	}
	if _, ok := findEnvVar(init.Env, "ORKA_GIT_REPO"); !ok {
		t.Fatal("init missing ORKA_GIT_REPO")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if hasVolumeMount(container.VolumeMounts, testGitCredentials) {
		t.Fatal("main container should not mount git credentials by default")
	}
	if container.WorkingDir != "/workspace/src" {
		t.Errorf("workingDir = %q, want /workspace/src", container.WorkingDir)
	}
	if _, ok := findEnvVar(container.Env, "ORKA_GIT_REF"); !ok {
		t.Fatal("container missing ORKA_GIT_REF")
	}
	if _, ok := findEnvVar(container.Env, "ORKA_PUSH_BRANCH"); !ok {
		t.Fatal("container missing ORKA_PUSH_BRANCH")
	}
}

func TestJobBuilder_Build_AgentTask_WorkspaceInitMarksPreparedWorkspace(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
			Annotations: map[string]string{
				labels.AnnotationWorkspaceInitContainer: scheduledRunLabelValue,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Review this workspace",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo.git",
					Ref:     "abc123",
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("init container count = %d, want 1", len(job.Spec.Template.Spec.InitContainers))
	}
	if _, ok := findEnvVar(job.Spec.Template.Spec.InitContainers[0].Env, workerenv.WorkspacePrepared); ok {
		t.Fatalf("init container should not receive %s", workerenv.WorkspacePrepared)
	}

	containerEnv := job.Spec.Template.Spec.Containers[0].Env
	env, ok := findEnvVar(containerEnv, workerenv.WorkspacePrepared)
	if !ok {
		t.Fatalf("main container missing %s", workerenv.WorkspacePrepared)
	}
	if env.Value != scheduledRunLabelValue {
		t.Fatalf("%s = %q, want %q", workerenv.WorkspacePrepared, env.Value, scheduledRunLabelValue)
	}
	if count := countEnvVars(containerEnv, workerenv.WorkspacePrepared); count != 1 {
		t.Fatalf("%s count = %d, want 1", workerenv.WorkspacePrepared, count)
	}
}

func TestJobBuilder_Build_AgentTask_Labels(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if job.Labels[labels.LabelTaskType] != "agent" {
		t.Errorf("Job label orka.ai/task-type = %s, want agent", job.Labels[labels.LabelTaskType])
	}
	if job.Spec.Template.Labels[labels.LabelTaskType] != "agent" {
		t.Errorf("Pod label orka.ai/task-type = %s, want agent", job.Spec.Template.Labels[labels.LabelTaskType])
	}
}

func TestJobBuilder_Build_TruncatesLongJobNamesToKubernetesLimit(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-manual-discovery-auth-secrets-privilege-1775868783-1",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if len(job.Name) > maxJobNameLength {
		t.Fatalf("len(job.Name) = %d, want <= %d (%q)", len(job.Name), maxJobNameLength, job.Name)
	}
	if !strings.HasSuffix(job.Name, "-job-12345678-0") {
		t.Fatalf("job.Name = %q, want suffix %q", job.Name, "-job-12345678-0")
	}
	wantTaskLabel := labels.SelectorValue(task.Name)
	if job.Labels[labels.LabelTask] != wantTaskLabel {
		t.Fatalf("job.Labels[%q] = %q, want selector-safe task label %q", labels.LabelTask, job.Labels[labels.LabelTask], wantTaskLabel)
	}
	if job.Spec.Template.Labels[labels.LabelTask] != wantTaskLabel {
		t.Fatalf("pod label %q = %q, want selector-safe task label %q", labels.LabelTask, job.Spec.Template.Labels[labels.LabelTask], wantTaskLabel)
	}
	if len(job.Labels[labels.LabelTask]) > 63 {
		t.Fatalf("job label %q length = %d, want <= 63", labels.LabelTask, len(job.Labels[labels.LabelTask]))
	}
}

func TestJobBuilder_Build_AgentTask_WithTimeout(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeAgent,
			Prompt:  "Do something",
			Timeout: &metav1.Duration{Duration: 10 * time.Minute},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Verify timeout env var
	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_TIMEOUT_SECONDS")
	if !ok {
		t.Fatal("Missing ORKA_TIMEOUT_SECONDS")
	}
	if ev.Value != "600" {
		t.Errorf("ORKA_TIMEOUT_SECONDS = %s, want 600", ev.Value)
	}

	// Verify job active deadline
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("ActiveDeadlineSeconds should be set")
	}
	if *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("ActiveDeadlineSeconds = %d, want 600", *job.Spec.ActiveDeadlineSeconds)
	}
}

func TestJobBuilder_Build_UsesParentTaskAnnotationForEnvVar(t *testing.T) {
	builder := setupJobBuilder()
	parentName := "very-long-parent-task-name-that-exceeds-kubernetes-label-limits-1234567890"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(parentName),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: parentName,
			},
			UID: types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "ORKA_PARENT_TASK")
	if !ok {
		t.Fatal("Missing ORKA_PARENT_TASK")
	}
	if ev.Value != parentName {
		t.Fatalf("ORKA_PARENT_TASK = %q, want %q", ev.Value, parentName)
	}
}

func TestJobBuilder_Build_PriorTaskRef(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			UID:       "uid-1234-5678",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "fix the issue",
			PriorTaskRef: &corev1alpha1.PriorTaskReference{
				Name:      "prior-task-abc",
				Namespace: "staging",
			},
		},
	}

	job, err := jb.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	var foundPriorTask, foundPriorNS bool
	for _, env := range envVars {
		if env.Name == "ORKA_PRIOR_TASK" && env.Value == "prior-task-abc" {
			foundPriorTask = true
		}
		if env.Name == "ORKA_PRIOR_TASK_NAMESPACE" && env.Value == "staging" {
			foundPriorNS = true
		}
	}
	if !foundPriorTask {
		t.Error("expected ORKA_PRIOR_TASK env var")
	}
	if !foundPriorNS {
		t.Error("expected ORKA_PRIOR_TASK_NAMESPACE env var")
	}
}

func TestJobBuilder_Build_PriorTaskRef_DefaultNamespace(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: "my-ns",
			UID:       "uid-4567-8901",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "fix it",
			PriorTaskRef: &corev1alpha1.PriorTaskReference{
				Name: "prior-task-def",
				// No namespace — should default to task namespace
			},
		},
	}

	job, err := jb.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	for _, env := range envVars {
		if env.Name == "ORKA_PRIOR_TASK_NAMESPACE" && env.Value == "my-ns" {
			return // success
		}
	}
	t.Error("expected ORKA_PRIOR_TASK_NAMESPACE to default to task namespace 'my-ns'")
}

// ---------------------------------------------------------------------------
// addSecretVolumes
// ---------------------------------------------------------------------------

func TestAddSecretVolumes_ProviderOpenAI(t *testing.T) {
	jb := setupJobBuilder()
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type: corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "openai-secret",
				Key:  "my-key",
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	job, _ := jb.Build(context.Background(), task, nil, nil)
	if err := jb.addSecretVolumes(context.Background(), job, task, nil, provider); err != nil {
		t.Fatalf("addSecretVolumes() error = %v", err)
	}
	found := false
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == testOpenAIAPIKey && env.ValueFrom != nil &&
			env.ValueFrom.SecretKeyRef.Key == "my-key" {
			found = true
		}
	}
	if !found {
		t.Error("expected OPENAI_API_KEY env var from secret")
	}
}

func TestAddSecretVolumes_ProviderAnthropic(t *testing.T) {
	jb := setupJobBuilder()
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type: corev1alpha1.ProviderTypeAnthropic,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "anthropic-secret",
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	job, _ := jb.Build(context.Background(), task, nil, nil)
	if err := jb.addSecretVolumes(context.Background(), job, task, nil, provider); err != nil {
		t.Fatalf("addSecretVolumes() error = %v", err)
	}
	found := false
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "ANTHROPIC_API_KEY" && env.ValueFrom != nil &&
			env.ValueFrom.SecretKeyRef.Key == defaultSecretKey {
			found = true
		}
	}
	if !found {
		t.Error("expected ANTHROPIC_API_KEY env var with default key")
	}
}

func TestAddSecretVolumes_TaskSecret(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeAI,
			SecretRef: &corev1alpha1.SecretReference{Name: "task-secret"},
		},
	}
	job, _ := jb.Build(context.Background(), task, nil, nil)
	if err := jb.addSecretVolumes(context.Background(), job, task, nil, nil); err != nil {
		t.Fatalf("addSecretVolumes() error = %v", err)
	}
	found := false
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "task-secrets" && v.Secret != nil && v.Secret.SecretName == "task-secret" {
			found = true
		}
	}
	if !found {
		t.Error("expected task-secrets volume")
	}
}

func TestAddSecretVolumes_AgentSecret(t *testing.T) {
	jb := setupJobBuilder()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-sec", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName},
			Model:     &corev1alpha1.ModelConfig{Provider: "openai"},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	job, _ := jb.Build(context.Background(), task, nil, nil)
	if err := jb.addSecretVolumes(context.Background(), job, task, agent, nil); err != nil {
		t.Fatalf("addSecretVolumes() error = %v", err)
	}
	foundVol := false
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "agent-secrets" && v.Secret != nil && v.Secret.SecretName == testAgentSecretName {
			foundVol = true
		}
	}
	if !foundVol {
		t.Error("expected agent-secrets volume")
	}
	foundEnvFrom := false
	for _, ef := range job.Spec.Template.Spec.Containers[0].EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == testAgentSecretName {
			foundEnvFrom = true
		}
	}
	if !foundEnvFrom {
		t.Error("expected agent-secret envFrom")
	}
}

func TestAddSecretVolumes_FallbackProvider(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	fbProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-prov", Namespace: defaultNS},
		Spec: corev1alpha1.ProviderSpec{
			Type: corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "fb-secret",
				Key:  defaultSecretKey,
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fbProvider).Build()
	jb := NewJobBuilder(fc)
	jb.ControllerURL = testControllerURL

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Fallbacks: []corev1alpha1.ModelFallback{
					{ProviderRef: "fb-prov", Model: "gpt-3.5"},
				},
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	job, _ := jb.Build(context.Background(), task, nil, nil)
	if err := jb.addSecretVolumes(context.Background(), job, task, agent, nil); err != nil {
		t.Fatalf("addSecretVolumes() error = %v", err)
	}
	found := false
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "ORKA_AI_FALLBACK_0_API_KEY" && env.ValueFrom != nil {
			found = true
		}
	}
	if !found {
		t.Error("expected fallback API key env var")
	}
}

func TestJobBuilder_Build_UntrustedFallbackProvidersDoNotReadProviderSecretsByDefault(t *testing.T) {
	t.Setenv(directProviderSecretsEnvVar, "false")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	var providerGets int
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1alpha1.Provider); ok {
					providerGets++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	jb := NewJobBuilder(fc)
	jb.ControllerURL = testControllerURL

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Fallbacks: []corev1alpha1.ModelFallback{
					{ProviderRef: "fb-prov", Model: "gpt-3.5"},
				},
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   testBusyboxImage,
			Command: []string{"echo"},
			Args:    []string{"hello"},
		},
	}

	job, err := jb.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if providerGets != 0 {
		t.Fatalf("Provider Get count = %d, want 0", providerGets)
	}
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if strings.HasPrefix(env.Name, "ORKA_AI_FALLBACK_") && strings.HasSuffix(env.Name, "_API_KEY") {
			t.Fatalf("unexpected fallback API key env var %s", env.Name)
		}
	}
}

func TestAddSecretVolumes_ProviderAzureOpenAI(t *testing.T) {
	jb := setupJobBuilder()
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type: corev1alpha1.ProviderTypeAzureOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "azure-secret",
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	job, _ := jb.Build(context.Background(), task, nil, nil)
	if err := jb.addSecretVolumes(context.Background(), job, task, nil, provider); err != nil {
		t.Fatalf("addSecretVolumes() error = %v", err)
	}
	found := false
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == testOpenAIAPIKey {
			found = true
		}
	}
	if !found {
		t.Error("expected OPENAI_API_KEY for Azure OpenAI provider")
	}
}

// ---------------------------------------------------------------------------
// addAIEnvVars — fallback providers
// ---------------------------------------------------------------------------

func TestAddAIEnvVars_FallbackProviders(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	fbProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-prov2", Namespace: defaultNS},
		Spec: corev1alpha1.ProviderSpec{
			Type:    corev1alpha1.ProviderTypeOpenAI,
			BaseURL: "https://custom.api.com",
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "fb-secret2",
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fbProvider).Build()
	jb := NewJobBuilder(fc)
	jb.ControllerURL = testControllerURL

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-agent2", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Fallbacks: []corev1alpha1.ModelFallback{
					{ProviderRef: "fb-prov2", Model: "gpt-3.5"},
				},
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{Prompt: "test"},
		},
	}
	envVars := jb.addAIEnvVars(context.Background(), nil, task, agent, nil)
	envMap := make(map[string]string)
	for _, e := range envVars {
		envMap[e.Name] = e.Value
	}
	if envMap["ORKA_AI_FALLBACK_COUNT"] != "1" {
		t.Errorf("expected fallback count 1, got %s", envMap["ORKA_AI_FALLBACK_COUNT"])
	}
	if envMap["ORKA_AI_FALLBACK_0_PROVIDER"] != string(corev1alpha1.ProviderTypeOpenAI) {
		t.Errorf("expected fallback provider openai, got %s", envMap["ORKA_AI_FALLBACK_0_PROVIDER"])
	}
	if envMap["ORKA_AI_FALLBACK_0_BASE_URL"] != "https://custom.api.com" {
		t.Errorf("expected fallback base URL, got %s", envMap["ORKA_AI_FALLBACK_0_BASE_URL"])
	}
}

// ---------------------------------------------------------------------------
// getAgentWorkerImage
// ---------------------------------------------------------------------------

func TestGetAgentWorkerImage(t *testing.T) {
	jb := setupJobBuilder()

	tests := []struct {
		name     string
		agent    *corev1alpha1.Agent
		expected string
	}{
		{
			name:     "nil agent",
			agent:    nil,
			expected: DefaultClaudeWorkerImage,
		},
		{
			name:     "nil runtime",
			agent:    &corev1alpha1.Agent{},
			expected: DefaultClaudeWorkerImage,
		},
		{
			name: "copilot runtime",
			agent: &corev1alpha1.Agent{
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot},
				},
			},
			expected: DefaultCopilotWorkerImage,
		},
		{
			name: "claude runtime",
			agent: &corev1alpha1.Agent{
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
				},
			},
			expected: DefaultClaudeWorkerImage,
		},
		{
			name: "codex runtime",
			agent: &corev1alpha1.Agent{
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
				},
			},
			expected: DefaultCodexWorkerImage,
		},
		{
			name: "unknown runtime type",
			agent: &corev1alpha1.Agent{
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: "unknown"},
				},
			},
			expected: DefaultClaudeWorkerImage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jb.getAgentWorkerImage(tc.agent)
			if got != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// addAgentWorkspaceEnvVars
// ---------------------------------------------------------------------------

func TestAddAgentWorkspaceEnvVars_AllFields(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/org/repo",
					Branch:       "main",
					Ref:          "abc123",
					SubPath:      "src/",
					ForkRepo:     "https://github.com/fork/repo",
					PRBaseBranch: "develop",
					PushBranch:   "feature-branch",
				},
			},
		},
	}
	envVars := jb.addAgentWorkspaceEnvVars(nil, task)
	expectedVars := map[string]string{
		"ORKA_GIT_REPO":            "https://github.com/org/repo",
		"ORKA_GIT_BRANCH":          "main",
		"ORKA_GIT_REF":             "abc123",
		"ORKA_WORKSPACE_SUBPATH":   "src/",
		"ORKA_FORK_REPO":           "https://github.com/fork/repo",
		"ORKA_PR_BASE_BRANCH":      "develop",
		"ORKA_PUSH_BRANCH":         "feature-branch",
		"ORKA_REQUIRE_PUSH_BRANCH": "true",
	}
	envMap := make(map[string]string)
	for _, e := range envVars {
		envMap[e.Name] = e.Value
	}
	for k, v := range expectedVars {
		if envMap[k] != v {
			t.Errorf("expected %s=%s, got %s", k, v, envMap[k])
		}
	}
}

// ---------------------------------------------------------------------------
// addAIEnvVars — fallback providers and child task coordination
// ---------------------------------------------------------------------------

func TestAddAIEnvVars_ChildTaskMessaging(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTask,
			Namespace: defaultNS,
			Labels:    map[string]string{labels.LabelParentTask: "parent"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{Prompt: "test"},
		},
	}
	envVars := jb.addAIEnvVars(context.Background(), nil, task, nil, nil)
	envMap := make(map[string]string)
	for _, e := range envVars {
		envMap[e.Name] = e.Value
	}
	// Child task should get messaging tools auto-injected
	tools := envMap["ORKA_AI_TOOLS"]
	if !strings.Contains(tools, "send_message") || !strings.Contains(tools, "check_messages") {
		t.Errorf("expected messaging tools for child task, got %s", tools)
	}
	// Also ORKA_COORDINATION_ENABLED should be set
	if envMap["ORKA_COORDINATION_ENABLED"] != scheduledRunLabelValue {
		t.Error("expected ORKA_COORDINATION_ENABLED=true for child task without coordination agent")
	}
}

func TestAddAIEnvVars_CoordinationEnabled(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{Prompt: "test"},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coord-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled: true,
			},
		},
	}
	envVars := jb.addAIEnvVars(context.Background(), nil, task, agent, nil)
	envMap := make(map[string]string)
	for _, e := range envVars {
		envMap[e.Name] = e.Value
	}
	tools := envMap["ORKA_AI_TOOLS"]
	for _, tool := range []string{"delegate_task", "list_pull_requests", "check_pr_review_marker"} {
		if !strings.Contains(tools, tool) {
			t.Errorf("expected coordination tool %s, got %s", tool, tools)
		}
	}
}

func TestAddAIEnvVars_CoordinationEnabledWithExplicitToolsOnly(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testTask,
			Namespace:   defaultNS,
			Labels:      map[string]string{labels.LabelParentTask: labels.SelectorValue("scheduled-parent")},
			Annotations: map[string]string{labels.AnnotationDisableCoordinationToolInject: scheduledRunLabelValue},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Prompt: "test",
				Tools:  []string{"list_pull_requests", "check_pr_review_marker"},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coord-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled: true,
			},
		},
	}
	envVars := jb.addAIEnvVars(context.Background(), nil, task, agent, nil)
	envMap := make(map[string]string)
	for _, e := range envVars {
		envMap[e.Name] = e.Value
	}
	if envMap[workerenv.CoordinationEnabled] != scheduledRunLabelValue {
		t.Error("expected ORKA_COORDINATION_ENABLED=true")
	}
	tools := envMap[workerenv.AITools]
	for _, tool := range []string{"list_pull_requests", "check_pr_review_marker"} {
		if !strings.Contains(tools, tool) {
			t.Errorf("expected explicit tool %s, got %s", tool, tools)
		}
	}
	for _, tool := range []string{"delegate_task", "send_message", "check_messages", "merge_pull_request", "auto_merge_pull_request"} {
		if strings.Contains(tools, tool) {
			t.Errorf("unexpected auto-injected coordination tool %s in %s", tool, tools)
		}
	}
}

func TestJobBuilderBuildAddsSkillVolumeAndConfigMap(t *testing.T) {
	const skillVolumeName = "skills"

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)

	skill := &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "research-skill", Namespace: defaultNS},
		Spec: corev1alpha1.SkillSpec{
			Description: "Research guidance",
			Content: corev1alpha1.SkillContent{
				Inline: "Use reliable sources.",
				Files: map[string]string{
					"templates/checklist.md": "- [ ] verify source",
				},
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(skill).Build()
	jb := NewJobBuilder(fc)
	jb.ControllerURL = testControllerURL

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-task", Namespace: defaultNS, UID: "uid-1234-5678"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider: "openai",
				Model:    "gpt-4o-mini",
				Prompt:   "hello",
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Skills: []corev1alpha1.SkillReference{{Name: "research-skill"}},
		},
	}

	job, err := jb.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if !hasVolume(job.Spec.Template.Spec.Volumes, skillVolumeName) {
		t.Fatal("expected skills volume to be mounted")
	}
	if !hasVolumeMount(job.Spec.Template.Spec.Containers[0].VolumeMounts, skillVolumeName) {
		t.Fatal("expected skills volume mount")
	}

	var skillsVolume *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == skillVolumeName {
			skillsVolume = &job.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	if skillsVolume == nil || skillsVolume.ConfigMap == nil {
		t.Fatal("skills volume should reference a ConfigMap")
	}

	cm := &corev1.ConfigMap{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      skillsVolume.ConfigMap.Name,
		Namespace: defaultNS,
	}, cm); err != nil {
		t.Fatalf("expected skill ConfigMap to exist: %v", err)
	}
	if got := strings.TrimSpace(cm.Data["system-prompt"]); got != "Use reliable sources." {
		t.Fatalf("system-prompt = %q, want %q", got, "Use reliable sources.")
	}
	if len(skillsVolume.ConfigMap.Items) == 0 {
		t.Fatal("expected skills ConfigMap volume to include key-to-path mappings")
	}
}

func TestJobBuilderBuildFailsWhenSkillMissing(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-skill-task",
			Namespace: defaultNS,
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider: "openai",
				Model:    "gpt-4o-mini",
				Prompt:   "hello",
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-skill-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Skills: []corev1alpha1.SkillReference{{Name: "does-not-exist"}},
		},
	}

	_, err := builder.Build(context.Background(), task, agent, nil)
	if err == nil {
		t.Fatal("expected Build() to fail when referenced skill does not exist")
	}
	if !strings.Contains(err.Error(), "failed to get Skill") {
		t.Fatalf("error = %v, expected missing skill message", err)
	}
}

func TestJobBuilderBuildDeduplicatesSkills(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)

	skill := &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-skill", Namespace: defaultNS},
		Spec: corev1alpha1.SkillSpec{
			Description: "Shared skill",
			Content: corev1alpha1.SkillContent{
				Inline: "Shared skill content.",
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(skill).Build()
	jb := NewJobBuilder(fc)
	jb.ControllerURL = testControllerURL

	// Same skill referenced by both agent and task
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dedup-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Skills: []corev1alpha1.SkillReference{{Name: "shared-skill"}},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "dedup-task", Namespace: defaultNS, UID: "uid-dedup"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider: "openai",
				Model:    "gpt-4o-mini",
				Prompt:   "hello",
				Skills:   []corev1alpha1.SkillReference{{Name: "shared-skill"}},
			},
		},
	}

	job, err := jb.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Find the skills ConfigMap and verify content appears only once
	var skillsVolume *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "skills" {
			skillsVolume = &job.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	if skillsVolume == nil || skillsVolume.ConfigMap == nil {
		t.Fatal("skills volume should reference a ConfigMap")
	}

	cm := &corev1.ConfigMap{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      skillsVolume.ConfigMap.Name,
		Namespace: defaultNS,
	}, cm); err != nil {
		t.Fatalf("expected skill ConfigMap to exist: %v", err)
	}

	// Should have exactly 1 inline entry (skill-0-inline), not 2
	inlineCount := 0
	for key := range cm.Data {
		if strings.HasPrefix(key, "skill-") && strings.HasSuffix(key, "-inline") {
			inlineCount++
		}
	}
	if inlineCount != 1 {
		t.Fatalf("expected 1 inline skill entry (deduplicated), got %d", inlineCount)
	}
}

func TestJobBuilderBuildLoadsConfigMapSkills(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)

	skillCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-cm", Namespace: defaultNS},
		Data:       map[string]string{"skill.txt": "You are a careful reviewer."},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(skillCM).Build()
	jb := NewJobBuilder(fc)
	jb.ControllerURL = testControllerURL

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "configmap-skill-task", Namespace: defaultNS, UID: "uid-configmap-skill"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider: "openai",
				Model:    "gpt-4o-mini",
				Prompt:   "hello",
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "configmap-skill-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Skills: []corev1alpha1.SkillReference{{
				ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "skill-cm", Key: "skill.txt"},
			}},
		},
	}

	job, err := jb.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var skillsVolume *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "skills" {
			skillsVolume = &job.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	if skillsVolume == nil || skillsVolume.ConfigMap == nil {
		t.Fatal("skills volume should reference a ConfigMap")
	}

	cm := &corev1.ConfigMap{}
	if err := fc.Get(context.Background(), types.NamespacedName{
		Name:      skillsVolume.ConfigMap.Name,
		Namespace: defaultNS,
	}, cm); err != nil {
		t.Fatalf("expected generated skill ConfigMap to exist: %v", err)
	}
	if got := strings.TrimSpace(cm.Data["system-prompt"]); got != "You are a careful reviewer." {
		t.Fatalf("system-prompt = %q, want %q", got, "You are a careful reviewer.")
	}
}
