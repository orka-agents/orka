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
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

var (
	githubAppTestKeyOnce sync.Once
	githubAppTestKey     *rsa.PrivateKey
)

func githubAppTestPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	githubAppTestKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		githubAppTestKey = key
	})
	return githubAppTestKey
}

func githubAppTestPEM(t *testing.T) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(githubAppTestPrivateKey(t))})
}

type fakeGitHubAppMinter struct {
	calls []githubAppMintRequest
	token string
	err   error
}

func (f *fakeGitHubAppMinter) MintInstallationToken(_ context.Context, request githubAppMintRequest) (string, error) {
	f.calls = append(f.calls, request)
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

type githubAppBrokerCase struct {
	task        *corev1alpha1.Task
	publication *corev1alpha1.Publication
	secret      *corev1.Secret
	attempt     *corev1alpha1.PromptAttempt
	effect      *corev1alpha1.ExternalEffect
	request     publisherservice.CredentialMaterialRequest
}

func (c githubAppBrokerCase) objects() []client.Object {
	objects := []client.Object{c.task.DeepCopy(), c.secret.DeepCopy(), c.attempt.DeepCopy(), c.effect.DeepCopy()}
	if c.publication != nil {
		objects = append(objects, c.publication.DeepCopy())
	}
	return objects
}

const (
	githubAppTestSourceRepo = "https://github.com/example/project.git"
	githubAppTestTargetRepo = "git@github.com:example/project-fork.git"
)

func githubAppCredentialReference() *corev1alpha1.WorkspaceCredentialReference {
	return &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey", Type: corev1alpha1.WorkspaceCredentialGitHubApp}
}

// newGitHubAppBrokerCase builds a Task, frozen binding, parent effect and
// request for one publisher operation whose credential is a GitHub App Secret.
func newGitHubAppBrokerCase(t *testing.T, operation publisherservice.Operation) githubAppBrokerCase {
	t.Helper()
	taskUID := types.UID("app-task-uid")
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: githubAppTestSourceRepo, PublicationGitRepo: githubAppTestTargetRepo,
	}
	execution := &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt", State: corev1alpha1.TaskExecutionStateSucceeded}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app-task", UID: taskUID},
		Spec:       corev1alpha1.TaskSpec{Workspace: workspace},
		Status:     corev1alpha1.TaskStatus{Execution: execution},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-app", UID: "app-secret-uid", ResourceVersion: "21"},
		Data:       map[string][]byte{"appId": []byte("123456"), "privateKey": githubAppTestPEM(t)},
	}
	c := githubAppBrokerCase{task: task, secret: secret}
	var role string
	kind := publisherservice.CredentialHTTPExtraHeader
	publicationID := controller.ACPPublicationIDForTask(task)
	publication := func(state string) *corev1alpha1.Publication {
		return &corev1alpha1.Publication{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication"},
			Spec:       corev1alpha1.PublicationSpec{ID: publicationID, TaskUID: string(taskUID)},
			Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(state)},
		}
	}
	switch operation {
	case publisherservice.OperationWorkspacePrepare:
		role = "SourceRead"
		execution.State = corev1alpha1.TaskExecutionStatePlanned
		workspace.ReadCredentialRef = githubAppCredentialReference()
		execution.ReadCredentialResourceVersion = "21"
		c.effect = publisherEffectForTest("app-effect", "workspace.prepare", string(taskUID), "workspace-prepare-prompt")
		c.request = publisherservice.CredentialMaterialRequest{
			ParentOperation: operation,
			Metadata:        publisherservice.OperationMetadata{Namespace: "default", TaskID: string(taskUID), OperationID: "workspace-prepare-prompt"},
		}
	case publisherservice.OperationPublicationPublish:
		role = "TargetWrite"
		workspace.PublicationCredentialRef = githubAppCredentialReference()
		execution.PublicationCredentialResourceVersion = "21"
		c.publication = publication("Publishing")
	case publisherservice.OperationPublicationVerify:
		role = "TargetRead"
		workspace.PublicationReadCredentialRef = githubAppCredentialReference()
		execution.PublicationReadCredentialResourceVersion = "21"
		c.publication = publication("Verifying")
	case publisherservice.OperationPullRequestReconcile:
		role = "Forge"
		kind = publisherservice.CredentialForgeToken
		workspace.ForgeCredentialRef = githubAppCredentialReference()
		execution.ForgeCredentialResourceVersion = "21"
		c.publication = publication("Verifying")
	default:
		t.Fatalf("unsupported operation %q", operation)
	}
	if c.publication != nil {
		opKind, _, _ := publicationCredentialOperation(operation)
		operationID := controller.ACPPublicationOperationID(opKind, task)
		effectKind := map[publisherservice.Operation]string{
			publisherservice.OperationPublicationPublish:   "publisher.publish",
			publisherservice.OperationPublicationVerify:    "publisher.verify",
			publisherservice.OperationPullRequestReconcile: "publisher.pull-request",
		}[operation]
		c.effect = publisherEffectForTest("app-effect", effectKind, publicationID, operationID)
		c.request = publisherservice.CredentialMaterialRequest{
			ParentOperation: operation,
			Metadata:        publisherservice.OperationMetadata{Namespace: "default", PublicationID: publicationID, OperationID: operationID},
		}
	}
	c.request.Reference = publisherservice.CredentialReference{Name: "publisher-app", Kind: kind}
	c.attempt = &corev1alpha1.PromptAttempt{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "attempt"}, Spec: corev1alpha1.PromptAttemptSpec{
		TaskUID: string(taskUID), Attempt: 1, PromptID: "prompt",
		CredentialBindings: []corev1alpha1.PromptCredentialBinding{{
			Role: role, Namespace: "default", SecretName: "publisher-app", SecretKey: "privateKey", SecretUID: "app-secret-uid", ResourceVersion: "21",
		}},
	}}
	return c
}

func callGitHubAppBroker(t *testing.T, kubeClient client.Client, c githubAppBrokerCase, minter githubAppTokenMinter) (int, []byte) {
	t.Helper()
	publisherToken := strings.Repeat("p", 32)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher-token", []byte(publisherToken))
	app := fiber.New()
	server := &Server{
		app: app, client: kubeClient, githubAppMinter: minter,
		config: ServerConfig{ControllerEpochs: publisherEpochSourceForTest(), ExternalEffects: publisherEffectReaderForTest(c.effect)},
	}
	server.installACPArtifactAuthorizationBroker()
	body, err := json.Marshal(c.request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, publisherservice.CredentialBrokerPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+publisherToken)
	httpResponse, err := app.Test(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResponse.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResponse.StatusCode, data
}

func githubAppTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestPublisherCredentialBrokerMintsGitHubAppTokenPerRole(t *testing.T) {
	scheme := githubAppTestScheme(t)
	basic := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_minted"))
	for _, tc := range []struct {
		operation    publisherservice.Operation
		repositories []string
		permissions  map[string]string
		material     string
	}{
		{publisherservice.OperationWorkspacePrepare, []string{"project"}, map[string]string{"contents": "read"}, basic},
		{publisherservice.OperationPublicationPublish, []string{"project-fork"}, map[string]string{"contents": "write"}, basic},
		{publisherservice.OperationPublicationVerify, []string{"project-fork"}, map[string]string{"contents": "read"}, basic},
		{
			publisherservice.OperationPullRequestReconcile, []string{"project", "project-fork"},
			map[string]string{"pull_requests": "write", "contents": "read"}, "ghs_minted",
		},
	} {
		t.Run(string(tc.operation), func(t *testing.T) {
			c := newGitHubAppBrokerCase(t, tc.operation)
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objects()...).Build()
			minter := &fakeGitHubAppMinter{token: "ghs_minted"}
			status, body := callGitHubAppBroker(t, kubeClient, c, minter)
			if status != http.StatusOK {
				t.Fatalf("status = %d, body = %s", status, body)
			}
			var response publisherservice.CredentialMaterialResponse
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			if response.Material != tc.material || response.ResourceVersion != "21" {
				t.Fatalf("response = %#v", response)
			}
			if len(minter.calls) != 1 {
				t.Fatalf("minter calls = %d, want 1", len(minter.calls))
			}
			call := minter.calls[0]
			if call.AppID != "123456" || call.Owner != "example" || call.PrivateKey == nil ||
				!call.PrivateKey.Equal(githubAppTestPrivateKey(t)) ||
				strings.Join(call.Repositories, ",") != strings.Join(tc.repositories, ",") || !maps.Equal(call.Permissions, tc.permissions) {
				t.Fatalf("mint request = app %q owner %q repos %v perms %v", call.AppID, call.Owner, call.Repositories, call.Permissions)
			}
			if strings.Contains(string(body), "PRIVATE KEY") {
				t.Fatal("broker response contains the App private key")
			}
		})
	}
}

func TestPublisherCredentialBrokerGitHubAppFailures(t *testing.T) {
	scheme := githubAppTestScheme(t)
	for _, tc := range []struct {
		name       string
		mutate     func(*githubAppBrokerCase)
		mintErr    error
		wantStatus int
		wantCode   string
		wantMint   bool
	}{
		{name: "App not installed", mintErr: githubAppRefused("github_app_not_installed"), wantStatus: http.StatusForbidden, wantCode: "github_app_not_installed", wantMint: true},
		{name: "GitHub refuses", mintErr: githubAppRefused("github_app_rejected"), wantStatus: http.StatusForbidden, wantCode: "github_app_rejected", wantMint: true},
		{name: "GitHub unavailable is retryable", mintErr: githubAppUnavailable(), wantStatus: http.StatusServiceUnavailable, wantCode: "github_app_unavailable", wantMint: true},
		{
			name:       "missing App ID",
			mutate:     func(c *githubAppBrokerCase) { delete(c.secret.Data, "appId") },
			wantStatus: http.StatusForbidden, wantCode: "github_app_secret_invalid",
		},
		{
			name:       "bad private key",
			mutate:     func(c *githubAppBrokerCase) { c.secret.Data["privateKey"] = []byte("not a key") },
			wantStatus: http.StatusForbidden, wantCode: "github_app_secret_invalid",
		},
		{
			name: "non-GitHub repository",
			mutate: func(c *githubAppBrokerCase) {
				c.task.Spec.Workspace.GitRepo = "https://gitlab.example.com/example/project.git"
			},
			wantStatus: http.StatusForbidden, wantCode: "github_app_repository_invalid",
		},
		{
			name:       "App key rotated after reservation",
			mutate:     func(c *githubAppBrokerCase) { c.secret.ResourceVersion = "22" },
			wantStatus: http.StatusGone, wantCode: "credential_version_changed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGitHubAppBrokerCase(t, publisherservice.OperationWorkspacePrepare)
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objects()...).Build()
			minter := &fakeGitHubAppMinter{token: "ghs_minted", err: tc.mintErr}
			status, body := callGitHubAppBroker(t, kubeClient, c, minter)
			if status != tc.wantStatus || !strings.Contains(string(body), `"`+tc.wantCode+`"`) {
				t.Fatalf("status = %d body = %s, want %d %q", status, body, tc.wantStatus, tc.wantCode)
			}
			if tc.wantMint != (len(minter.calls) == 1) {
				t.Fatalf("minter calls = %d, want minted %v", len(minter.calls), tc.wantMint)
			}
			if strings.Contains(string(body), "PRIVATE KEY") || strings.Contains(string(body), "ghs_") {
				t.Fatalf("error body leaks credential material: %s", body)
			}
		})
	}
}

func TestPublisherCredentialBrokerPlainTokenIgnoresGitHubAppMinter(t *testing.T) {
	scheme := githubAppTestScheme(t)
	c := newGitHubAppBrokerCase(t, publisherservice.OperationWorkspacePrepare)
	c.task.Spec.Workspace.ReadCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey"}
	c.secret.Data["privateKey"] = []byte("stored-token")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objects()...).Build()
	minter := &fakeGitHubAppMinter{token: "ghs_minted"}
	status, body := callGitHubAppBroker(t, kubeClient, c, minter)
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:stored-token"))
	if status != http.StatusOK || !strings.Contains(string(body), want) || len(minter.calls) != 0 {
		t.Fatalf("status = %d body = %s minter calls = %d", status, body, len(minter.calls))
	}
	// An explicit Token type behaves exactly like an omitted type.
	c.task.Spec.Workspace.ReadCredentialRef.Type = corev1alpha1.WorkspaceCredentialToken
	kubeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objects()...).Build()
	status, body = callGitHubAppBroker(t, kubeClient, c, minter)
	if status != http.StatusOK || !strings.Contains(string(body), want) || len(minter.calls) != 0 {
		t.Fatalf("Token type: status = %d body = %s minter calls = %d", status, body, len(minter.calls))
	}
}

func TestGitHubAppRequestForRole(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace corev1alpha1.WorkspaceConfig
		role      publisherservice.CredentialRole
		repos     string
		wantErr   bool
	}{
		{name: "target falls back to source", workspace: corev1alpha1.WorkspaceConfig{GitRepo: githubAppTestSourceRepo}, role: publisherservice.CredentialRoleTargetWrite, repos: "project"},
		{name: "forge on one repository is deduplicated", workspace: corev1alpha1.WorkspaceConfig{GitRepo: githubAppTestSourceRepo, PublicationGitRepo: "https://github.com/Example/Project"}, role: publisherservice.CredentialRoleForge, repos: "project"},
		{name: "forge across owners is refused", workspace: corev1alpha1.WorkspaceConfig{GitRepo: githubAppTestSourceRepo, PublicationGitRepo: "https://github.com/someone/project"}, role: publisherservice.CredentialRoleForge, wantErr: true},
		{name: "source read ignores the publication repository", workspace: corev1alpha1.WorkspaceConfig{GitRepo: githubAppTestSourceRepo, PublicationGitRepo: "https://gitlab.example.com/x/y"}, role: publisherservice.CredentialRoleSourceRead, repos: "project"},
		{name: "missing repository", workspace: corev1alpha1.WorkspaceConfig{}, role: publisherservice.CredentialRoleSourceRead, wantErr: true},
		{name: "unknown role", workspace: corev1alpha1.WorkspaceConfig{GitRepo: githubAppTestSourceRepo}, role: "Admin", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := githubAppRequestForRole(&tc.workspace, tc.role)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("request = %#v, want error", request)
				}
				return
			}
			if err != nil || strings.Join(request.Repositories, ",") != tc.repos {
				t.Fatalf("request = %#v, err = %v, want repositories %q", request, err, tc.repos)
			}
		})
	}
}

func TestParseGitHubAppPrivateKeyFormats(t *testing.T) {
	key := githubAppTestPrivateKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"PKCS1": githubAppTestPEM(t),
		"PKCS8": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	} {
		parsed, err := parseGitHubAppPrivateKey(data)
		if err != nil || !parsed.Equal(key) {
			t.Fatalf("%s: parsed = %v, err = %v", name, parsed != nil, err)
		}
	}
	for name, data := range map[string][]byte{
		"public key":  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")}),
		"corrupt DER": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("x")}),
	} {
		if _, err := parseGitHubAppPrivateKey(data); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

func TestGitHubAppHTTPMinterMintsScopedInstallationToken(t *testing.T) {
	key := githubAppTestPrivateKey(t)
	now := time.Unix(1_800_000_000, 0)
	var tokenBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyGitHubAppTestJWT(t, r.Header.Get("Authorization"), &key.PublicKey, now)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation":
			_, _ = io.WriteString(w, `{"id": 42}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			if err := json.NewDecoder(r.Body).Decode(&tokenBody); err != nil {
				t.Errorf("decode token body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token": "ghs_scoped", "expires_at": "2027-01-15T08:00:00Z"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer server.Close()
	minter := newGitHubAppHTTPMinter(server.URL, server.Client())
	minter.now = func() time.Time { return now }
	token, err := minter.MintInstallationToken(context.Background(), githubAppMintRequest{
		AppID: "123456", PrivateKey: key, Owner: "example", Repositories: []string{"project", "project-fork"},
		Permissions: map[string]string{"pull_requests": "write", "contents": "read"},
	})
	if err != nil || token != "ghs_scoped" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
	encoded, _ := json.Marshal(tokenBody)
	if string(encoded) != `{"permissions":{"contents":"read","pull_requests":"write"},"repositories":["project","project-fork"]}` {
		t.Fatalf("token request body = %s", encoded)
	}
}

func TestGitHubAppHTTPMinterMapsGitHubFailures(t *testing.T) {
	key := githubAppTestPrivateKey(t)
	for _, tc := range []struct {
		name               string
		installationStatus int
		tokenStatus        int
		header             map[string]string
		tokenBody          string
		wantStatus         int
		wantCode           string
	}{
		{name: "not installed", installationStatus: http.StatusNotFound, wantStatus: http.StatusForbidden, wantCode: "github_app_not_installed"},
		{name: "bad JWT", installationStatus: http.StatusUnauthorized, wantStatus: http.StatusForbidden, wantCode: "github_app_rejected"},
		{name: "permission not granted", installationStatus: http.StatusOK, tokenStatus: http.StatusUnprocessableEntity, wantStatus: http.StatusForbidden, wantCode: "github_app_rejected"},
		{name: "repository outside installation", installationStatus: http.StatusOK, tokenStatus: http.StatusForbidden, wantStatus: http.StatusForbidden, wantCode: "github_app_rejected"},
		{name: "rate limited", installationStatus: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "0"}, wantStatus: http.StatusServiceUnavailable, wantCode: "github_app_unavailable"},
		{name: "secondary rate limit with Retry-After", installationStatus: http.StatusForbidden, header: map[string]string{"Retry-After": "60"}, wantStatus: http.StatusServiceUnavailable, wantCode: "github_app_unavailable"},
		{name: "secondary rate limit message", installationStatus: http.StatusOK, tokenStatus: http.StatusForbidden, tokenBody: `{"message": "You have exceeded a secondary rate limit."}`, wantStatus: http.StatusServiceUnavailable, wantCode: "github_app_unavailable"},
		{name: "too many requests", installationStatus: http.StatusTooManyRequests, wantStatus: http.StatusServiceUnavailable, wantCode: "github_app_unavailable"},
		{name: "server error", installationStatus: http.StatusOK, tokenStatus: http.StatusBadGateway, wantStatus: http.StatusServiceUnavailable, wantCode: "github_app_unavailable"},
		{name: "redirect is not followed", installationStatus: http.StatusFound, header: map[string]string{"Location": "https://example.com/"}, wantStatus: http.StatusForbidden, wantCode: "github_app_rejected"},
		{name: "empty token", installationStatus: http.StatusOK, tokenStatus: http.StatusCreated, tokenBody: `{"token": ""}`, wantStatus: http.StatusServiceUnavailable, wantCode: "github_app_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for name, value := range tc.header {
					w.Header().Set(name, value)
				}
				if r.Method == http.MethodGet {
					w.WriteHeader(tc.installationStatus)
					_, _ = io.WriteString(w, `{"id": 42}`)
					return
				}
				w.WriteHeader(tc.tokenStatus)
				body := tc.tokenBody
				if body == "" {
					body = `{"message": "refused"}`
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			_, err := newGitHubAppHTTPMinter(server.URL, server.Client()).MintInstallationToken(context.Background(), githubAppMintRequest{
				AppID: "123456", PrivateKey: key, Owner: "example", Repositories: []string{"project"}, Permissions: map[string]string{"contents": "read"},
			})
			status, code := githubAppErrorResponse(err)
			if err == nil || status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("err = %v, status = %d code = %q, want %d %q", err, status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
	if _, err := newGitHubAppHTTPMinter("http://127.0.0.1:1", nil).MintInstallationToken(context.Background(), githubAppMintRequest{
		AppID: "123456", PrivateKey: key, Owner: "example", Repositories: []string{"project"},
	}); err == nil {
		t.Fatal("transport failure: want error")
	} else if status, _ := githubAppErrorResponse(err); status != http.StatusServiceUnavailable {
		t.Fatalf("transport failure status = %d, want 503", status)
	}
}

func verifyGitHubAppTestJWT(t *testing.T, authorization string, publicKey *rsa.PublicKey, now time.Time) {
	t.Helper()
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	parts := strings.Split(token, ".")
	if !ok || len(parts) != 3 {
		t.Errorf("authorization is not a bearer JWT")
		return
	}
	var header map[string]string
	var claims map[string]any
	for i, out := range []any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(raw, out) != nil {
			t.Errorf("JWT part %d is invalid", i)
			return
		}
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature) != nil {
		t.Errorf("JWT signature does not verify")
	}
	iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64))
	if header["alg"] != "RS256" || claims["iss"] != "123456" || iat > now.Unix() || exp <= now.Unix() || exp-iat > 600 {
		t.Errorf("JWT header %v claims %v", header, claims)
	}
}
