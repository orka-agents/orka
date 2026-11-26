package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

var githubAppTestKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

type fakeGitHubAppMinter struct {
	calls []githubAppMintRequest
	err   error
}

func (f *fakeGitHubAppMinter) MintInstallationToken(_ context.Context, request githubAppMintRequest) (string, error) {
	f.calls = append(f.calls, request)
	return "ghs_minted", f.err
}

type githubAppBrokerCase struct {
	task    *corev1alpha1.Task
	secret  *corev1.Secret
	objects []client.Object
	effect  *corev1alpha1.ExternalEffect
	request publisherservice.CredentialMaterialRequest
}

// newGitHubAppBrokerCase builds a Task whose credential for one publisher
// operation is a GitHub App Secret, with its frozen binding and parent effect.
func newGitHubAppBrokerCase(operation publisherservice.Operation) *githubAppBrokerCase {
	ref := &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey", Type: corev1alpha1.WorkspaceCredentialGitHubApp}
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/example/project.git",
		PublicationGitRepo: "git@github.com:example/project-fork.git",
	}
	execution := &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt", State: corev1alpha1.TaskExecutionStateSucceeded}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app-task", UID: "app-task-uid"},
		Spec:       corev1alpha1.TaskSpec{Workspace: workspace},
		Status:     corev1alpha1.TaskStatus{Execution: execution},
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(githubAppTestKey())})
	c := &githubAppBrokerCase{task: task, secret: &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-app", UID: "app-secret-uid", ResourceVersion: "21"},
		Type:       corev1.SecretType(corev1alpha1.GitHubAppSecretType),
		Data:       map[string][]byte{"appId": []byte("123456"), "privateKey": keyPEM},
	}}
	role, kind, state, effectKind := "", publisherservice.CredentialHTTPExtraHeader, "", ""
	switch operation {
	case publisherservice.OperationWorkspacePrepare:
		role, workspace.ReadCredentialRef, execution.ReadCredentialResourceVersion = "SourceRead", ref, "21"
		execution.State = corev1alpha1.TaskExecutionStatePlanned
	case publisherservice.OperationPublicationPublish:
		role, workspace.PublicationCredentialRef, execution.PublicationCredentialResourceVersion = "TargetWrite", ref, "21"
		state, effectKind = "Publishing", "publisher.publish"
	case publisherservice.OperationPublicationVerify:
		role, workspace.PublicationReadCredentialRef, execution.PublicationReadCredentialResourceVersion = "TargetRead", ref, "21"
		state, effectKind = "Verifying", "publisher.verify"
	case publisherservice.OperationPullRequestReconcile:
		role, workspace.ForgeCredentialRef, execution.ForgeCredentialResourceVersion = "Forge", ref, "21"
		state, effectKind, kind = "Verifying", "publisher.pull-request", publisherservice.CredentialForgeToken
	}
	c.request = publisherservice.CredentialMaterialRequest{
		ParentOperation: operation, Reference: publisherservice.CredentialReference{Name: "publisher-app", Kind: kind},
		Metadata: publisherservice.OperationMetadata{Namespace: "default", TaskID: string(task.UID), OperationID: "workspace-prepare-prompt"},
	}
	c.effect = publisherEffectForTest("app-effect", "workspace.prepare", string(task.UID), "workspace-prepare-prompt")
	if state != "" {
		publicationID := controller.ACPPublicationIDForTask(task)
		opKind, _, _ := publicationCredentialOperation(operation)
		operationID := controller.ACPPublicationOperationID(opKind, task)
		c.objects = append(c.objects, &corev1alpha1.Publication{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication"},
			Spec:       corev1alpha1.PublicationSpec{ID: publicationID, TaskUID: string(task.UID)},
			Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(state)},
		})
		c.effect = publisherEffectForTest("app-effect", effectKind, publicationID, operationID)
		c.request.Metadata = publisherservice.OperationMetadata{Namespace: "default", PublicationID: publicationID, OperationID: operationID}
	}
	c.objects = append(c.objects, &corev1alpha1.PromptAttempt{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "attempt"},
		Spec: corev1alpha1.PromptAttemptSpec{TaskUID: string(task.UID), Attempt: 1, PromptID: "prompt", CredentialBindings: []corev1alpha1.PromptCredentialBinding{{
			Role: role, Namespace: "default", SecretName: "publisher-app", SecretKey: "privateKey", SecretUID: "app-secret-uid", ResourceVersion: "21",
		}}},
	})
	return c
}

func (c *githubAppBrokerCase) call(t *testing.T, minter githubAppTokenMinter) (int, string, http.Header) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(c.objects, c.task, c.secret, c.effect)...).Build()
	publisherToken := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-token", []byte(publisherToken))
	app := fiber.New()
	server := &Server{app: app, client: kubeClient, githubAppMinter: minter, config: ServerConfig{
		ControllerEpochs: publisherEpochSourceForTest(), ExternalEffects: publisherEffectReaderForTest(c.effect),
	}}
	server.installACPArtifactAuthorizationBroker()
	body, _ := json.Marshal(c.request)
	request := httptest.NewRequest(http.MethodPost, publisherservice.CredentialBrokerPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+publisherToken)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data), response.Header
}

func TestPublisherCredentialBrokerMintsGitHubAppTokenPerRole(t *testing.T) {
	basic := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_minted"))
	read, write := map[string]string{"contents": "read"}, map[string]string{"contents": "write"}
	for _, tc := range []struct {
		operation   publisherservice.Operation
		repos       string
		permissions map[string]string
		material    string
	}{
		{publisherservice.OperationWorkspacePrepare, "project", read, basic},
		{publisherservice.OperationPublicationPublish, "project-fork", write, basic},
		{publisherservice.OperationPublicationVerify, "project-fork", read, basic},
		{publisherservice.OperationPullRequestReconcile, "project,project-fork", map[string]string{"pull_requests": "write", "contents": "read"}, "ghs_minted"},
	} {
		t.Run(string(tc.operation), func(t *testing.T) {
			minter := &fakeGitHubAppMinter{}
			status, body, _ := newGitHubAppBrokerCase(tc.operation).call(t, minter)
			var response publisherservice.CredentialMaterialResponse
			if status != http.StatusOK || json.Unmarshal([]byte(body), &response) != nil || response.Material != tc.material || response.ResourceVersion != "21" {
				t.Fatalf("status = %d body = %s", status, body)
			}
			if len(minter.calls) != 1 {
				t.Fatalf("minter calls = %d, want 1", len(minter.calls))
			}
			call := minter.calls[0]
			if call.AppID != "123456" || call.Owner != "example" || !call.PrivateKey.Equal(githubAppTestKey()) ||
				strings.Join(call.Repositories, ",") != tc.repos || !maps.Equal(call.Permissions, tc.permissions) {
				t.Fatalf("mint request = app %q owner %q repos %v perms %v", call.AppID, call.Owner, call.Repositories, call.Permissions)
			}
		})
	}
}

func TestPublisherCredentialBrokerGitHubAppEdgeCases(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(githubAppTestKey())
	if err != nil {
		t.Fatal(err)
	}
	basic := func(token string) string {
		return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	}
	for _, tc := range []struct {
		name           string
		operation      publisherservice.Operation
		mutate         func(*githubAppBrokerCase)
		mintErr        error
		status         int
		want           string // material for 200, error code otherwise
		wantRetryAfter string
		minted         bool
	}{
		{name: "PKCS#8 key", mutate: func(c *githubAppBrokerCase) {
			c.secret.Data["privateKey"] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
		}, status: http.StatusOK, want: basic("ghs_minted"), minted: true},
		{name: "plain token path unchanged", mutate: func(c *githubAppBrokerCase) {
			c.task.Spec.Workspace.ReadCredentialRef.Type = ""
			c.secret.Type = corev1.SecretTypeOpaque
			delete(c.secret.Data, "appId")
			c.secret.Data["privateKey"] = []byte("stored-token")
		}, status: http.StatusOK, want: basic("stored-token")},
		{name: "GitHubApp ref requires orka.ai/github-app Secret type", mutate: func(c *githubAppBrokerCase) {
			c.secret.Type = corev1.SecretTypeOpaque
		}, status: http.StatusForbidden, want: "github_app_secret_invalid"},
		{name: "App Secret refused as raw commit signing key", operation: publisherservice.OperationPublicationPrepare, mutate: func(c *githubAppBrokerCase) {
			c.task.Spec.Workspace.CommitSigningCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey"}
			c.task.Status.Execution.CommitSigningCredentialResourceVersion = "21"
			c.task.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
			publicationID := controller.ACPPublicationIDForTask(c.task)
			operationID := controller.ACPPublicationOperationID("prepare", c.task)
			c.objects = []client.Object{
				&corev1alpha1.Publication{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication"},
					Spec:       corev1alpha1.PublicationSpec{ID: publicationID, TaskUID: string(c.task.UID)},
					Status:     corev1alpha1.PublicationStatus{State: "Preparing"},
				},
				&corev1alpha1.PromptAttempt{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "attempt"},
					Spec: corev1alpha1.PromptAttemptSpec{TaskUID: string(c.task.UID), Attempt: 1, PromptID: "prompt", CredentialBindings: []corev1alpha1.PromptCredentialBinding{{
						Role: "CommitSign", Namespace: "default", SecretName: "publisher-app", SecretKey: "privateKey", SecretUID: "app-secret-uid", ResourceVersion: "21",
					}}},
				},
			}
			c.effect = publisherEffectForTest("app-effect", "publisher.prepare", publicationID, operationID)
			c.request = publisherservice.CredentialMaterialRequest{
				ParentOperation: publisherservice.OperationPublicationPrepare,
				Reference:       publisherservice.CredentialReference{Name: "publisher-app", Kind: publisherservice.CredentialSSHSigningKey, Role: publisherservice.CredentialRoleCommitSign},
				Metadata:        publisherservice.OperationMetadata{Namespace: "default", PublicationID: publicationID, OperationID: operationID},
			}
		}, status: http.StatusForbidden, want: apiErrorCredentialMaterialInvalid},
		{name: "App Secret PEM bundle refused as raw commit signing key", operation: publisherservice.OperationPublicationPrepare, mutate: func(c *githubAppBrokerCase) {
			certBlock := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("dummy-cert")})
			c.secret.Data["privateKey"] = append(certBlock, c.secret.Data["privateKey"]...)
			c.task.Spec.Workspace.CommitSigningCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey"}
			c.task.Status.Execution.CommitSigningCredentialResourceVersion = "21"
			c.task.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
			publicationID := controller.ACPPublicationIDForTask(c.task)
			operationID := controller.ACPPublicationOperationID("prepare", c.task)
			c.objects = []client.Object{
				&corev1alpha1.Publication{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication"},
					Spec:       corev1alpha1.PublicationSpec{ID: publicationID, TaskUID: string(c.task.UID)},
					Status:     corev1alpha1.PublicationStatus{State: "Preparing"},
				},
				&corev1alpha1.PromptAttempt{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "attempt"},
					Spec: corev1alpha1.PromptAttemptSpec{TaskUID: string(c.task.UID), Attempt: 1, PromptID: "prompt", CredentialBindings: []corev1alpha1.PromptCredentialBinding{{
						Role: "CommitSign", Namespace: "default", SecretName: "publisher-app", SecretKey: "privateKey", SecretUID: "app-secret-uid", ResourceVersion: "21",
					}}},
				},
			}
			c.effect = publisherEffectForTest("app-effect", "publisher.prepare", publicationID, operationID)
			c.request = publisherservice.CredentialMaterialRequest{
				ParentOperation: publisherservice.OperationPublicationPrepare,
				Reference:       publisherservice.CredentialReference{Name: "publisher-app", Kind: publisherservice.CredentialSSHSigningKey, Role: publisherservice.CredentialRoleCommitSign},
				Metadata:        publisherservice.OperationMetadata{Namespace: "default", PublicationID: publicationID, OperationID: operationID},
			}
		}, status: http.StatusForbidden, want: apiErrorCredentialMaterialInvalid},
		{name: "App Secret base64 PEM refused as raw token", mutate: func(c *githubAppBrokerCase) {
			c.task.Spec.Workspace.ReadCredentialRef.Type = ""
			c.secret.Data["privateKey"] = []byte(base64.StdEncoding.EncodeToString(c.secret.Data["privateKey"]))
		}, status: http.StatusForbidden, want: apiErrorCredentialMaterialInvalid},
		{name: "App Secret refused on branch-rebase path", mutate: func(c *githubAppBrokerCase) {
			c.secret.Data["token"] = []byte("ghp_should_never_be_released_raw")
			publicationID, operationID := "rm-rebase-test-monitor", "op-rebase"
			c.objects = []client.Object{
				&corev1alpha1.RepositoryMonitor{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-monitor"},
					Spec: corev1alpha1.RepositoryMonitorSpec{
						PublicationCredentialRef: &corev1.LocalObjectReference{Name: "publisher-app"},
						Repair: corev1alpha1.RepositoryMonitorRepairSpec{
							Enabled:              true,
							UpdateBranchStrategy: "rebase",
						},
					},
				},
			}
			c.effect = publisherEffectForTest("rebase-effect", "publisher.branch-rebase", publicationID, operationID)
			c.request = publisherservice.CredentialMaterialRequest{
				ParentOperation: publisherservice.OperationBranchRebase,
				Reference:       publisherservice.CredentialReference{Name: "publisher-app", Kind: publisherservice.CredentialHTTPExtraHeader, Role: publisherservice.CredentialRoleTargetWrite},
				Metadata:        publisherservice.OperationMetadata{Namespace: "default", PublicationID: publicationID, OperationID: operationID},
			}
		}, status: http.StatusForbidden, want: apiErrorCredentialMaterialInvalid},
		{name: "App not installed", mintErr: githubAppRefused("github_app_not_installed"), status: http.StatusForbidden, want: "github_app_not_installed", minted: true},
		{name: "GitHub unavailable is retryable", mintErr: githubAppUnavailable(), status: http.StatusServiceUnavailable, want: "github_app_unavailable", minted: true},
		{name: "GitHub rate limit preserves Retry-After", mintErr: githubAppRateLimitUnavailable(45 * time.Second), status: http.StatusTooManyRequests, want: "github_app_rate_limited", wantRetryAfter: "45", minted: true},
		{name: "missing App ID", mutate: func(c *githubAppBrokerCase) { delete(c.secret.Data, "appId") }, status: http.StatusForbidden, want: "github_app_secret_invalid"},
		{name: "bad private key", mutate: func(c *githubAppBrokerCase) { c.secret.Data["privateKey"] = []byte("not a key") }, status: http.StatusForbidden, want: "github_app_secret_invalid"},
		{name: "non-GitHub repository", mutate: func(c *githubAppBrokerCase) {
			c.task.Spec.Workspace.GitRepo = "https://gitlab.example.com/example/project.git"
		}, status: http.StatusForbidden, want: "github_app_repository_invalid"},
		{name: "forge across owners", operation: publisherservice.OperationPullRequestReconcile, mutate: func(c *githubAppBrokerCase) {
			c.task.Spec.Workspace.PublicationGitRepo = "https://github.com/someone/project"
		}, status: http.StatusForbidden, want: "github_app_repository_invalid"},
		{name: "App key rotated after reservation", mutate: func(c *githubAppBrokerCase) { c.secret.ResourceVersion = "22" }, status: http.StatusGone, want: "credential_version_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operation := tc.operation
			if operation == "" {
				operation = publisherservice.OperationWorkspacePrepare
			}
			c := newGitHubAppBrokerCase(operation)
			if tc.mutate != nil {
				tc.mutate(c)
			}
			minter := &fakeGitHubAppMinter{err: tc.mintErr}
			status, body, headers := c.call(t, minter)
			if status != tc.status || !strings.Contains(body, `"`+tc.want+`"`) || tc.minted != (len(minter.calls) == 1) {
				t.Fatalf("status = %d body = %s minter calls = %d, want %d %q minted %v", status, body, len(minter.calls), tc.status, tc.want, tc.minted)
			}
			if tc.wantRetryAfter != "" && headers.Get("Retry-After") != tc.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", headers.Get("Retry-After"), tc.wantRetryAfter)
			}
			if strings.Contains(body, "PRIVATE KEY") {
				t.Fatalf("response leaks the private key: %s", body)
			}
		})
	}
}

func TestGitHubAppHTTPMinter(t *testing.T) {
	key, now := githubAppTestKey(), time.Unix(1_800_000_000, 0)
	request := githubAppMintRequest{
		AppID: "123456", PrivateKey: key, Owner: "example", Repositories: []string{"project", "project-fork"},
		Permissions: map[string]string{"pull_requests": "write", "contents": "read"},
	}
	for _, tc := range []struct {
		name                    string
		installation, tokenCode int
		header                  map[string]string
		tokenBody               string
		wantErr                 string // empty for success; else the broker error code
	}{
		{name: "mints scoped token", installation: http.StatusOK, tokenCode: http.StatusCreated, tokenBody: `{"token": "ghs_scoped"}`},
		{name: "not installed", installation: http.StatusNotFound, wantErr: "github_app_not_installed"},
		{name: "bad JWT", installation: http.StatusUnauthorized, wantErr: "github_app_rejected"},
		{name: "permission not granted", installation: http.StatusOK, tokenCode: http.StatusUnprocessableEntity, wantErr: "github_app_rejected"},
		{name: "primary rate limit", installation: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800000120"}, wantErr: "github_app_rate_limited"},
		{name: "secondary rate limit", installation: http.StatusForbidden, header: map[string]string{"Retry-After": "60"}, wantErr: "github_app_rate_limited"},
		{name: "secondary rate limit message", installation: http.StatusOK, tokenCode: http.StatusForbidden, tokenBody: `{"message": "You have exceeded a secondary rate limit."}`, wantErr: "github_app_rate_limited"},
		{name: "login lockout message", installation: http.StatusOK, tokenCode: http.StatusForbidden, tokenBody: `{"message": "Maximum number of login attempts exceeded. Please try again later."}`, wantErr: "github_app_rate_limited"},
		{name: "request timeout", installation: http.StatusRequestTimeout, wantErr: "github_app_unavailable"},
		{name: "server error", installation: http.StatusOK, tokenCode: http.StatusBadGateway, wantErr: "github_app_unavailable"},
		{name: "same-origin temporary redirect loop is retryable", installation: http.StatusTemporaryRedirect, header: map[string]string{"Location": "/repos/example/project/installation"}, wantErr: "github_app_unavailable"},
		{name: "cross-origin redirect is not followed", installation: http.StatusFound, header: map[string]string{"Location": "https://example.com/"}, wantErr: "github_app_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenBody string
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				verifyGitHubAppTestJWT(t, r.Header.Get("Authorization"), &key.PublicKey, now)
				for name, value := range tc.header {
					w.Header().Set(name, value)
				}
				if r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation" {
					w.WriteHeader(tc.installation)
					_, _ = io.WriteString(w, `{"id": 42, "account": {"login": "example"}}`)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				data, _ := io.ReadAll(r.Body)
				tokenBody = string(data)
				w.WriteHeader(tc.tokenCode)
				_, _ = io.WriteString(w, tc.tokenBody)
			}))
			defer server.Close()
			minter := newGitHubAppHTTPMinter(server.URL, server.Client())
			minter.now = func() time.Time { return now }
			token, err := minter.MintInstallationToken(context.Background(), request)
			if tc.wantErr == "" {
				if err != nil || token != "ghs_scoped" ||
					tokenBody != `{"permissions":{"contents":"read","pull_requests":"write"},"repositories":["project","project-fork"]}` {
					t.Fatalf("token = %q err = %v body = %s", token, err, tokenBody)
				}
				return
			}
			status, code, retryAfter := githubAppErrorResponse(err)
			if err == nil || code != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if tc.wantErr == "github_app_rate_limited" {
				if status != http.StatusTooManyRequests || retryAfter < 1 {
					t.Fatalf("rate limit status=%d retryAfter=%d, want 429 and >0", status, retryAfter)
				}
				// A retry during cooldown must not hit GitHub again.
				before := requests
				if _, retryErr := minter.MintInstallationToken(context.Background(), request); retryErr == nil || requests != before {
					t.Fatalf("during cooldown: err=%v requests=%d (was %d)", retryErr, requests, before)
				}
			}
		})
	}
	if _, err := newGitHubAppHTTPMinter("http://127.0.0.1:1", nil).MintInstallationToken(context.Background(), request); err == nil {
		t.Fatal("transport failure: want error")
	} else if status, _, _ := githubAppErrorResponse(err); status != http.StatusServiceUnavailable {
		t.Fatalf("transport failure status = %d, want 503", status)
	}

	t.Run("consecutive secondary rate limits back off and reset after success", func(t *testing.T) {
		attempt := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation" {
				attempt++
				if attempt <= 2 || attempt == 4 {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{"message": "You have exceeded a secondary rate limit."}`)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id": 42, "account": {"login": "example"}}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token": "ghs_recovered"}`)
		}))
		defer server.Close()
		cur := now
		minter := newGitHubAppHTTPMinter(server.URL, server.Client())
		minter.now = func() time.Time { return cur }

		_, err := minter.MintInstallationToken(context.Background(), request)
		_, _, firstRetry := githubAppErrorResponse(err)
		if firstRetry != 60 {
			t.Fatalf("first secondary limit retryAfter = %d, want 60", firstRetry)
		}
		cur = cur.Add(61 * time.Second)
		_, err = minter.MintInstallationToken(context.Background(), request)
		_, _, secondRetry := githubAppErrorResponse(err)
		if secondRetry != 120 {
			t.Fatalf("second secondary limit retryAfter = %d, want 120", secondRetry)
		}
		cur = cur.Add(121 * time.Second)
		token, err := minter.MintInstallationToken(context.Background(), request)
		if err != nil || token != "ghs_recovered" {
			t.Fatalf("recovered mint = (%q, %v), want ghs_recovered", token, err)
		}
		_, err = minter.MintInstallationToken(context.Background(), request)
		_, _, resetRetry := githubAppErrorResponse(err)
		if resetRetry != 60 {
			t.Fatalf("post-recovery secondary limit retryAfter = %d, want 60 after reset", resetRetry)
		}
	})

	t.Run("unverified key lookup throttle stays isolated while verified App cooldown is shared", func(t *testing.T) {
		rogueKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate rogue key: %v", err)
		}
		goodKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate good key: %v", err)
		}
		secondGoodKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate second good key: %v", err)
		}
		stage := "rogue_throttled"
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation" {
				if stage == "rogue_throttled" {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id": 42, "account": {"login": "example"}}`)
				return
			}
			if stage == "token_throttled" {
				w.Header().Set("Retry-After", "90")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token": "ghs_good"}`)
		}))
		defer server.Close()

		minter := newGitHubAppHTTPMinter(server.URL, server.Client())
		minter.now = func() time.Time { return now }

		rogueReq := request
		rogueReq.PrivateKey = rogueKey
		if _, err := minter.MintInstallationToken(context.Background(), rogueReq); err == nil {
			t.Fatal("rogue throttled lookup: want error")
		} else if _, code, _ := githubAppErrorResponse(err); code != "github_app_rate_limited" {
			t.Fatalf("rogue throttled lookup code = %q, want github_app_rate_limited", code)
		}

		// A legitimate key for the same App ID must not be blocked by the unverified key's lookup throttle.
		stage = "good_succeeds"
		goodReq := request
		goodReq.PrivateKey = goodKey
		token, err := minter.MintInstallationToken(context.Background(), goodReq)
		if err != nil || token != "ghs_good" {
			t.Fatalf("good key after unverified throttle = (%q, %v), want ghs_good", token, err)
		}

		// Once authenticated, a cooldown recorded for the App must block another key for the same App without hitting GitHub.
		stage = "token_throttled"
		if _, err := minter.MintInstallationToken(context.Background(), goodReq); err == nil {
			t.Fatal("verified key token throttle: want error")
		} else if _, code, retryAfter := githubAppErrorResponse(err); code != "github_app_rate_limited" || retryAfter != 90 {
			t.Fatalf("verified key token throttle = (%q, %d), want (github_app_rate_limited, 90)", code, retryAfter)
		}
		before := requests
		secondReq := request
		secondReq.PrivateKey = secondGoodKey
		if _, err := minter.MintInstallationToken(context.Background(), secondReq); err == nil {
			t.Fatal("second key during verified App cooldown: want error")
		} else if _, code, retryAfter := githubAppErrorResponse(err); code != "github_app_rate_limited" || retryAfter != 90 {
			t.Fatalf("second key during verified App cooldown = (%q, %d), want (github_app_rate_limited, 90)", code, retryAfter)
		}
		if requests != before {
			t.Fatalf("second key hit GitHub during App-wide cooldown: requests=%d (was %d)", requests, before)
		}
	})

	t.Run("follows same-origin redirects during installation lookup and token minting", func(t *testing.T) {
		var redirectedTokenBody string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			verifyGitHubAppTestJWT(t, r.Header.Get("Authorization"), &key.PublicKey, now)
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation":
				w.Header().Set("Location", "/repositories/99/installation")
				w.WriteHeader(http.StatusMovedPermanently)
			case r.Method == http.MethodGet && r.URL.Path == "/repositories/99/installation":
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id": 42, "account": {"login": "example"}}`)
			case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
				w.Header().Set("Location", "/app/installations/42/access_tokens/v2")
				w.WriteHeader(http.StatusMovedPermanently)
			case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens/v2":
				data, _ := io.ReadAll(r.Body)
				redirectedTokenBody = string(data)
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"token": "ghs_redirected"}`)
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		minter := newGitHubAppHTTPMinter(server.URL, server.Client())
		minter.now = func() time.Time { return now }
		singleRepoReq := request
		singleRepoReq.Repositories = []string{"project"}
		token, err := minter.MintInstallationToken(context.Background(), singleRepoReq)
		if err != nil || token != "ghs_redirected" ||
			redirectedTokenBody != `{"permissions":{"contents":"read","pull_requests":"write"},"repository_ids":[99]}` {
			t.Fatalf("redirected mint = (%q, %v) body = %s, want ghs_redirected bound to repository_ids [99]", token, err, redirectedTokenBody)
		}
		// Multi-repository requests fail closed when the lookup redirects away
		// from the named repository path because the second repo's ID is unknown.
		if _, err := minter.MintInstallationToken(context.Background(), request); err == nil {
			t.Fatal("multi-repo redirected lookup: want error")
		} else if _, code, _ := githubAppErrorResponse(err); code != "github_app_repository_invalid" {
			t.Fatalf("multi-repo redirected lookup code = %q, want github_app_repository_invalid", code)
		}
	})

	t.Run("rejects installation owned by a different account after redirect", func(t *testing.T) {
		tokenRequested := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			verifyGitHubAppTestJWT(t, r.Header.Get("Authorization"), &key.PublicKey, now)
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation":
				w.Header().Set("Location", "/repositories/99/installation")
				w.WriteHeader(http.StatusMovedPermanently)
			case r.Method == http.MethodGet && r.URL.Path == "/repositories/99/installation":
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id": 99, "account": {"login": "transferred-org"}}`)
			case r.Method == http.MethodPost:
				tokenRequested = true
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"token": "ghs_wrong_owner"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		minter := newGitHubAppHTTPMinter(server.URL, server.Client())
		minter.now = func() time.Time { return now }
		if _, err := minter.MintInstallationToken(context.Background(), request); err == nil {
			t.Fatal("mismatched installation owner: want error")
		} else if _, code, _ := githubAppErrorResponse(err); code != "github_app_repository_invalid" {
			t.Fatalf("mismatched installation owner code = %q, want github_app_repository_invalid", code)
		}
		if tokenRequested {
			t.Fatal("broker minted a token for a mismatched installation owner")
		}
	})

	t.Run("revoked key 401 invalidates verification so later throttles do not block replacement key", func(t *testing.T) {
		oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate old key: %v", err)
		}
		newKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate new key: %v", err)
		}
		stage := "old_ok"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation" {
				switch stage {
				case "old_revoked":
					w.WriteHeader(http.StatusUnauthorized)
					return
				case "old_rate_limited":
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{"message": "You have exceeded a secondary rate limit."}`)
					return
				default:
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"id": 42, "account": {"login": "example"}}`)
					return
				}
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token": "ghs_replacement"}`)
		}))
		defer server.Close()

		minter := newGitHubAppHTTPMinter(server.URL, server.Client())
		minter.now = func() time.Time { return now }

		oldReq := request
		oldReq.PrivateKey = oldKey
		if token, err := minter.MintInstallationToken(context.Background(), oldReq); err != nil || token == "" {
			t.Fatalf("initial old key mint = (%q, %v), want success", token, err)
		}
		// Revoke oldKey -> 401 must drop its verified association.
		stage = "old_revoked"
		if _, err := minter.MintInstallationToken(context.Background(), oldReq); err == nil {
			t.Fatal("revoked key: want 401 error")
		}
		// Subsequent rate limit on oldKey must stay isolated to oldKey.
		stage = "old_rate_limited"
		if _, err := minter.MintInstallationToken(context.Background(), oldReq); err == nil {
			t.Fatal("rate-limited revoked key: want rate limit error")
		}
		// Replacement key for the same App must still mint without being blocked.
		stage = "new_ok"
		newReq := request
		newReq.PrivateKey = newKey
		if token, err := minter.MintInstallationToken(context.Background(), newReq); err != nil || token != "ghs_replacement" {
			t.Fatalf("replacement key after old key revocation = (%q, %v), want ghs_replacement", token, err)
		}
	})

	t.Run("in-flight lookup finishing after 401 does not re-verify revoked key", func(t *testing.T) {
		oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate old key: %v", err)
		}
		newKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate new key: %v", err)
		}
		inFlightStarted := make(chan struct{})
		releaseInFlight := make(chan struct{})
		var lookupCalls int
		var mu sync.Mutex
		stage := "race"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation" {
				mu.Lock()
				curStage := stage
				lookupCalls++
				callNum := lookupCalls
				mu.Unlock()
				switch curStage {
				case "race":
					if callNum == 1 {
						close(inFlightStarted)
						<-releaseInFlight
						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, `{"id": 42, "account": {"login": "example"}}`)
						return
					}
					w.WriteHeader(http.StatusUnauthorized)
					return
				case "old_rate_limited":
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{"message": "You have exceeded a secondary rate limit."}`)
					return
				default:
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"id": 42, "account": {"login": "example"}}`)
					return
				}
			}
			mu.Lock()
			curStage := stage
			mu.Unlock()
			if curStage == "race" {
				// Return a non-invalidating 503 (without Retry-After) so the
				// token POST itself cannot clear a mistakenly re-verified key.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token": "ghs_replacement"}`)
		}))
		defer server.Close()

		minter := newGitHubAppHTTPMinter(server.URL, server.Client())
		minter.now = func() time.Time { return now }

		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseInFlight) }) }
		defer release()

		oldReq := request
		oldReq.PrivateKey = oldKey
		done := make(chan error, 1)
		go func() {
			_, err := minter.MintInstallationToken(context.Background(), oldReq)
			done <- err
		}()
		select {
		case <-inFlightStarted:
		case err := <-done:
			t.Fatalf("in-flight lookup returned before reaching handler: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for in-flight lookup to reach handler")
		}

		// Concurrent request sees 401 and invalidates oldKey while call 1 is in flight.
		if _, err := minter.MintInstallationToken(context.Background(), oldReq); err == nil {
			t.Fatal("concurrent revoked lookup: want 401 error")
		}
		// Release the stale lookup; it must not re-verify oldKey.
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for released lookup to finish")
		}

		// Subsequent rate limit on oldKey must stay isolated and not block newKey.
		mu.Lock()
		stage = "old_rate_limited"
		mu.Unlock()
		if _, err := minter.MintInstallationToken(context.Background(), oldReq); err == nil {
			t.Fatal("rate-limited revoked key: want rate limit error")
		}
		mu.Lock()
		stage = "new_ok"
		mu.Unlock()
		newReq := request
		newReq.PrivateKey = newKey
		if token, err := minter.MintInstallationToken(context.Background(), newReq); err != nil || token != "ghs_replacement" {
			t.Fatalf("replacement key after stale lookup race = (%q, %v), want ghs_replacement", token, err)
		}
	})
}

func verifyGitHubAppTestJWT(t *testing.T, authorization string, publicKey *rsa.PublicKey, now time.Time) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(authorization, "Bearer "), ".")
	var claims map[string]any
	if len(parts) != 3 {
		t.Errorf("authorization is not a bearer JWT")
		return
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if string(header) != `{"alg":"RS256","typ":"JWT"}` || json.Unmarshal(payload, &claims) != nil ||
		rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature) != nil {
		t.Errorf("JWT header %s or signature is invalid", header)
		return
	}
	iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64))
	if claims["iss"] != "123456" || iat > now.Unix() || exp <= now.Unix() || exp-iat > 600 {
		t.Errorf("JWT claims %v", claims)
	}
}
