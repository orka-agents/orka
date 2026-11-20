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

func (c *githubAppBrokerCase) call(t *testing.T, minter githubAppTokenMinter) (int, string) {
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
	return response.StatusCode, string(data)
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
			status, body := newGitHubAppBrokerCase(tc.operation).call(t, minter)
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
		name      string
		operation publisherservice.Operation
		mutate    func(*githubAppBrokerCase)
		mintErr   error
		status    int
		want      string // material for 200, error code otherwise
		minted    bool
	}{
		{name: "PKCS#8 key", mutate: func(c *githubAppBrokerCase) {
			c.secret.Data["privateKey"] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
		}, status: http.StatusOK, want: basic("ghs_minted"), minted: true},
		{name: "plain token path unchanged", mutate: func(c *githubAppBrokerCase) {
			c.task.Spec.Workspace.ReadCredentialRef.Type = ""
			c.secret.Data["privateKey"] = []byte("stored-token")
		}, status: http.StatusOK, want: basic("stored-token")},
		{name: "App not installed", mintErr: githubAppRefused("github_app_not_installed"), status: http.StatusForbidden, want: "github_app_not_installed", minted: true},
		{name: "GitHub unavailable is retryable", mintErr: githubAppUnavailable(), status: http.StatusServiceUnavailable, want: "github_app_unavailable", minted: true},
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
			status, body := c.call(t, minter)
			if status != tc.status || !strings.Contains(body, `"`+tc.want+`"`) || tc.minted != (len(minter.calls) == 1) {
				t.Fatalf("status = %d body = %s minter calls = %d, want %d %q minted %v", status, body, len(minter.calls), tc.status, tc.want, tc.minted)
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
		{name: "primary rate limit", installation: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "0"}, wantErr: "github_app_unavailable"},
		{name: "secondary rate limit", installation: http.StatusForbidden, header: map[string]string{"Retry-After": "60"}, wantErr: "github_app_unavailable"},
		{name: "secondary rate limit message", installation: http.StatusOK, tokenCode: http.StatusForbidden, tokenBody: `{"message": "You have exceeded a secondary rate limit."}`, wantErr: "github_app_unavailable"},
		{name: "server error", installation: http.StatusOK, tokenCode: http.StatusBadGateway, wantErr: "github_app_unavailable"},
		{name: "redirect is not followed", installation: http.StatusFound, header: map[string]string{"Location": "https://example.com/"}, wantErr: "github_app_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verifyGitHubAppTestJWT(t, r.Header.Get("Authorization"), &key.PublicKey, now)
				for name, value := range tc.header {
					w.Header().Set(name, value)
				}
				if r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/installation" {
					w.WriteHeader(tc.installation)
					_, _ = io.WriteString(w, `{"id": 42}`)
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
			if _, code := githubAppErrorResponse(err); err == nil || code != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if _, err := newGitHubAppHTTPMinter("http://127.0.0.1:1", nil).MintInstallationToken(context.Background(), request); err == nil {
		t.Fatal("transport failure: want error")
	} else if status, _ := githubAppErrorResponse(err); status != http.StatusServiceUnavailable {
		t.Fatalf("transport failure status = %d, want 503", status)
	}
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
