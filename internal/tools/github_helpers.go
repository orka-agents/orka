/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/orka-agents/orka/internal/workerenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const githubPRStateClosed = "closed"

type githubCredentialPolicy uint8

const (
	githubCredentialRead githubCredentialPolicy = iota
	githubCredentialForgeMutation
)

type githubRepoScope struct {
	source            string
	owner             string
	repo              string
	readCredentialRef *corev1alpha1.WorkspaceCredentialReference
}

type githubTaskContext struct {
	scopes             []githubRepoScope
	forgeCredentialRef *corev1alpha1.WorkspaceCredentialReference
}

// resolveReadRepoAndToken resolves GitHub owner/repo, read-only auth token, and API base URL.
// Repository resolution prefers an explicit repoURL, then task workspace config,
// then ORKA_GIT_REPO when no task context is available. If taskName, or an
// implicit ToolContext task, is available alongside repoURL, repoURL still
// selects the repository while the task workspace supplies repository scope
// and can supply credentials.
//
// For task-scoped calls, read credentials are selected by repository role:
// readCredentialRef for the source repository and publicationReadCredentialRef
// for the publication repository. Repository write and forge mutation credentials
// are never used for read-only calls.
func resolveReadRepoAndToken(ctx context.Context, k8sClient client.Client, toolName, taskName, repoURL, overrideBaseURL string) (owner, repo, token, baseURL string, err error) {
	return resolveRepoAndTokenWithPolicy(ctx, k8sClient, toolName, taskName, repoURL, overrideBaseURL, false, githubCredentialRead)
}

func resolveScopedReadRepoAndToken(ctx context.Context, k8sClient client.Client, toolName, taskName, repoURL, overrideBaseURL string) (owner, repo, token, baseURL string, err error) {
	return resolveRepoAndTokenWithPolicy(ctx, k8sClient, toolName, taskName, repoURL, overrideBaseURL, true, githubCredentialRead)
}

// resolveForgeRepoAndToken resolves GitHub owner/repo, forge-mutation auth token,
// and API base URL. Task-scoped mutations require forgeCredentialRef and never
// fall back to publication or read credentials.
func resolveForgeRepoAndToken(ctx context.Context, k8sClient client.Client, toolName, taskName, repoURL, overrideBaseURL string) (owner, repo, token, baseURL string, err error) {
	return resolveRepoAndTokenWithPolicy(ctx, k8sClient, toolName, taskName, repoURL, overrideBaseURL, false, githubCredentialForgeMutation)
}

func resolveScopedForgeRepoAndToken(ctx context.Context, k8sClient client.Client, toolName, taskName, repoURL, overrideBaseURL string) (owner, repo, token, baseURL string, err error) {
	return resolveRepoAndTokenWithPolicy(ctx, k8sClient, toolName, taskName, repoURL, overrideBaseURL, true, githubCredentialForgeMutation)
}

func resolveRepoAndTokenWithPolicy(
	ctx context.Context,
	k8sClient client.Client,
	toolName, taskName, repoURL, overrideBaseURL string,
	requireRepoURLScope bool,
	credentialPolicy githubCredentialPolicy,
) (owner, repo, token, baseURL string, err error) {
	baseURL = githubAPIBaseURL
	if overrideBaseURL != "" {
		baseURL = overrideBaseURL
	}

	// The requester's linked account comes first. A bound Connection that
	// cannot be used fails the call; only a call with no Connection bound
	// resolves the Task's own credential Secrets below.
	linked, err := linkedGitHubToken(ctx, toolName)
	if err != nil {
		return "", "", "", "", err
	}
	if linked != "" {
		token = linked
	}

	// Under a linked account the repository scope comes from the current
	// Task, or from a child it controls (a coordinator opening the pull
	// request for its coder's work): any other Task would lend its
	// workspace to point the person's token at a repository this Task was
	// never given. Those Tasks are read uncached, and a child is read once:
	// the workspace it was validated with is the one that scopes the call.
	scopeReader := client.Reader(k8sClient)
	var linkedChild *githubTaskContext
	if linked != "" {
		scopeReader = linkedScopeReader(ctx, k8sClient)
		var scopeNamespace string
		linkedChild, scopeNamespace, err = linkedTaskScopeAllowed(ctx, scopeReader, taskName)
		if err != nil {
			return "", "", "", "", err
		}
		if scopeNamespace != "" && scopeNamespace != githubTaskNamespace(ctx) {
			// The Task this turn created lives where its creation tool was
			// told; the scope lookup below reads it from there.
			ctx = withGitHubTaskNamespace(ctx, scopeNamespace)
		}
	}

	hasRepoURL := strings.TrimSpace(repoURL) != ""
	if hasRepoURL {
		owner, repo, err = parseGitHubRepo(repoURL)
		if err != nil {
			return "", "", "", "", fmt.Errorf("failed to parse GitHub repo from %s: %w", repoURL, err)
		}
	}

	scopeTaskName := githubTaskNameFromContext(ctx, taskName)
	requireTaskCredentials := false
	if tc := GetToolContext(ctx); tc != nil {
		requireTaskCredentials = tc.RequireGitHubTaskCredentials
	}
	if requireTaskCredentials && scopeTaskName == "" && token == "" {
		return "", "", "", "", fmt.Errorf("task_name or current Task context is required for external GitHub access")
	}
	if hasRepoURL && requireRepoURLScope && scopeTaskName == "" {
		return "", "", "", "", fmt.Errorf("repo_url repository %s/%s requires a permitted repository scope", owner, repo)
	}
	if scopeTaskName != "" {
		// A linked token is scoped by the Task's declared workspace alone;
		// a transaction's repository context never widens it.
		var taskContext githubTaskContext
		if linkedChild != nil {
			taskContext = *linkedChild
		} else if taskContext, err = loadGitHubTaskScopes(ctx, scopeReader, scopeTaskName, linked != ""); err != nil {
			return "", "", "", "", err
		}
		if owner == "" || repo == "" {
			if len(taskContext.scopes) == 0 {
				return "", "", "", "", fmt.Errorf("task %s workspace has no GitHub repository scope", scopeTaskName)
			}
			owner, repo = taskContext.scopes[0].owner, taskContext.scopes[0].repo
		} else if !githubRepoAllowed(owner, repo, taskContext.scopes) {
			return "", "", "", "", fmt.Errorf(
				"repo_url repository %s/%s does not match permitted repository scope %s",
				owner,
				repo,
				formatGitHubRepoScopes(taskContext.scopes),
			)
		}

		if token == "" {
			credentialRef, err := taskContext.credentialRef(scopeTaskName, owner, repo, credentialPolicy)
			if err != nil {
				return "", "", "", "", err
			}
			token, err = resolveGitSecretToken(ctx, k8sClient, credentialRef)
			if err != nil {
				return "", "", "", "", err
			}
		}
	}

	if owner == "" || repo == "" {
		if scopeTaskName != "" {
			return "", "", "", "", fmt.Errorf("task %s workspace has no GitHub repository scope", scopeTaskName)
		}
		envRepo := os.Getenv(workerenv.GitRepo)
		if envRepo == "" {
			return "", "", "", "", fmt.Errorf("no repo_url, task_name, or ORKA_GIT_REPO provided")
		}
		owner, repo, err = parseGitHubRepo(envRepo)
		if err != nil {
			return "", "", "", "", fmt.Errorf("failed to parse GitHub repo from ORKA_GIT_REPO: %w", err)
		}
	}

	if token == "" {
		if requireTaskCredentials {
			return "", "", "", "", fmt.Errorf("task %s requires an explicit GitHub credential reference", scopeTaskName)
		}
		token = resolveToken()
	}

	if token == "" {
		return "", "", "", "", fmt.Errorf("could not resolve GitHub token from task secret, /secrets/git/token, /secrets/git/password, or GITHUB_TOKEN")
	}

	return owner, repo, token, baseURL, nil
}

// linkedGitHubToken returns the requester's linked-account token for the
// named tool when the ToolContext binds one, "" when none is bound, and an
// error when a bound Connection cannot be used. The binding itself refuses
// a forge mutation under a linked account the person limited to reading.
func linkedGitHubToken(ctx context.Context, toolName string) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil || tc.LinkedAccounts == nil {
		return "", nil
	}
	if strings.TrimSpace(toolName) == "" {
		return "", fmt.Errorf("linked-account resolution requires the executing tool name")
	}
	credential, bound, err := tc.LinkedAccounts.BuiltinToolCredential(ctx, toolName)
	if err != nil {
		return "", fmt.Errorf("resolve the linked account for %s: %w", toolName, err)
	}
	if !bound {
		return "", nil
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return "", fmt.Errorf("the linked account bound for %s returned no credential", toolName)
	}
	return credential.AccessToken, nil
}

// githubResponseLimit bounds one GitHub API document.
const githubResponseLimit int64 = 1 << 20

// errGitHubResponseTooLarge reports a GitHub document past its read limit.
var errGitHubResponseTooLarge = errors.New("GitHub response is too large")

// readGitHubResponse reads at most limit bytes of a GitHub response and
// reports one that exceeds the limit, instead of handing a document cut
// mid-way to the JSON decoder.
func readGitHubResponse(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub response: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: it exceeds %d bytes; request fewer results (per_page) or a narrower filter", errGitHubResponseTooLarge, limit)
	}
	return data, nil
}

func githubRepoAllowed(owner, repo string, scopes []githubRepoScope) bool {
	for _, scope := range scopes {
		if githubRepoMatches(owner, repo, scope.owner, scope.repo) {
			return true
		}
	}
	return false
}

// linkedScopeReader is the reader a linked-account call resolves Task scope
// through: the tool context's uncached reader when there is one, so a Task
// deleted and recreated under the same name is never judged from a stale
// cache.
func linkedScopeReader(ctx context.Context, k8sClient client.Client) client.Reader {
	if tc := GetToolContext(ctx); tc != nil && tc.PolicyReader != nil {
		return tc.PolicyReader
	}
	if k8sClient == nil {
		return nil
	}
	return k8sClient
}

// linkedTaskScopeAllowed reports whether taskName may supply the repository
// scope for a call made through a linked account: it must be the current
// Task, a Task the current Task controller-owns, or (outside a Task) a Task
// this turn created for the same person. For a named Task it returns the
// scope read from the same object it validated, so the caller never reloads
// a name that may by then be another Task, and the namespace that Task lives
// in when it differs from the call's ("" means the call's own namespace).
func linkedTaskScopeAllowed(ctx context.Context, reader client.Reader, taskName string) (*githubTaskContext, string, error) {
	tc := GetToolContext(ctx)
	taskName = strings.TrimSpace(taskName)
	if taskName == "" {
		return nil, "", nil
	}
	// Outside a Task (chat and the compatibility proxies) there is no
	// current Task whose scope a named Task could be checked against, so
	// the only Tasks that may lend their workspace to the person's token
	// are the ones this turn's tools created for that same person.
	if tc == nil || strings.TrimSpace(tc.TaskID) == "" {
		return linkedCreatedTaskScopeAllowed(ctx, reader, tc, taskName)
	}
	if taskName == strings.TrimSpace(tc.TaskID) {
		return nil, "", nil
	}
	if strings.TrimSpace(tc.TaskUID) == "" || reader == nil {
		return nil, "", fmt.Errorf("task_name %q must name the current task when acting through a linked account", taskName)
	}
	var task corev1alpha1.Task
	if err := reader.Get(ctx, types.NamespacedName{Name: taskName, Namespace: githubTaskNamespace(ctx)}, &task); err != nil {
		return nil, "", fmt.Errorf("failed to get task %s: %w", taskName, err)
	}
	owner := metav1.GetControllerOf(&task)
	if owner == nil || string(owner.UID) != strings.TrimSpace(tc.TaskUID) || owner.Kind != taskKindString || owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return nil, "", fmt.Errorf("task_name %q must name the current task or one of its own child tasks when acting through a linked account", taskName)
	}
	// Ownership alone is not repository authority: a coordinator can create
	// a child naming any repository, so the child's workspace may only
	// point the linked token at repositories the current Task itself holds.
	parent, err := loadGitHubTaskScopes(ctx, reader, strings.TrimSpace(tc.TaskID), true)
	if err != nil {
		return nil, "", fmt.Errorf("resolve the current task's repository scope: %w", err)
	}
	child, err := githubTaskScopesOf(&task, taskName, true)
	if err != nil {
		return nil, "", err
	}
	for _, scope := range child.scopes {
		if !githubRepoAllowed(scope.owner, scope.repo, parent.scopes) {
			return nil, "", fmt.Errorf("child task %q names repository %s/%s, which is outside the current task's repository scope %s",
				taskName, scope.owner, scope.repo, formatGitHubRepoScopes(parent.scopes))
		}
	}
	return &child, "", nil
}

// linkedCreatedTaskScopeAllowed accepts taskName outside a Task only when
// this turn created it and it is stamped with the same requester the
// linked account belongs to: the person chose that repository themselves.
func linkedCreatedTaskScopeAllowed(ctx context.Context, reader client.Reader, tc *ToolContext, taskName string) (*githubTaskContext, string, error) {
	// The GitHub tools name a Task without a namespace, so the record this
	// turn kept (namespace, name, UID at creation) resolves it: the call's
	// namespace first, otherwise the one unambiguous entry with that name.
	namespace, uid := githubTaskNamespace(ctx), ""
	if tc != nil {
		if uid = tc.CreatedTaskUID(namespace, taskName); uid == "" {
			if ns, recorded, ok := tc.CreatedTaskByName(taskName); ok {
				namespace, uid = ns, recorded
			}
		}
	}
	if uid == "" {
		return nil, "", fmt.Errorf("task_name %q must name a task created in this conversation when acting through a linked account outside a task", taskName)
	}
	if reader == nil || tc.Requester == nil {
		return nil, "", fmt.Errorf("task_name %q cannot be verified for the linked account", taskName)
	}
	var task corev1alpha1.Task
	if err := reader.Get(ctx, types.NamespacedName{Name: taskName, Namespace: namespace}, &task); err != nil {
		return nil, "", fmt.Errorf("failed to get task %s: %w", taskName, err)
	}
	if string(task.UID) != uid {
		return nil, "", fmt.Errorf("task_name %q is not the task this conversation created (identity changed)", taskName)
	}
	// The repository scope is the one this turn chose at creation: a
	// workspace edited since then may not redirect the person's token.
	if WorkspaceDigest(&task) != tc.CreatedTaskWorkspaceDigest(namespace, taskName) {
		return nil, "", fmt.Errorf("task_name %q: its workspace changed after this conversation created it, so it cannot scope the linked account", taskName)
	}
	requested := task.Spec.RequestedBy
	if requested == nil || requested.Issuer != tc.Requester.Issuer || requested.Subject != tc.Requester.Subject {
		return nil, "", fmt.Errorf("task_name %q was not requested by the person whose linked account this call uses", taskName)
	}
	scoped, err := githubTaskScopesOf(&task, taskName, true)
	if err != nil {
		return nil, "", err
	}
	return &scoped, namespace, nil
}

// withGitHubTaskNamespace returns ctx with a ToolContext copy whose
// Namespace is namespace, so the Task-scope lookups read from there.
func withGitHubTaskNamespace(ctx context.Context, namespace string) context.Context {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ctx
	}
	scoped := *tc
	scoped.Namespace = namespace
	return WithToolContext(ctx, &scoped)
}

func loadGitHubTaskContext(ctx context.Context, k8sClient client.Client, taskName string) (githubTaskContext, error) {
	return loadGitHubTaskScopes(ctx, k8sClient, taskName, false)
}

// loadGitHubTaskScopes loads the Task's repository scopes. With
// workspaceOnly, only spec.workspace repositories count; the transaction's
// repository context is left out.
func loadGitHubTaskScopes(ctx context.Context, reader client.Reader, taskName string, workspaceOnly bool) (githubTaskContext, error) {
	taskName = githubTaskNameFromContext(ctx, taskName)
	if strings.TrimSpace(taskName) == "" {
		return githubTaskContext{}, nil
	}
	if reader == nil {
		return githubTaskContext{}, fmt.Errorf("task_name %q requires a Kubernetes client for repo_url scope validation", taskName)
	}

	ns := githubTaskNamespace(ctx)

	var task corev1alpha1.Task
	if err := reader.Get(ctx, types.NamespacedName{Name: taskName, Namespace: ns}, &task); err != nil {
		return githubTaskContext{}, fmt.Errorf("failed to get task %s: %w", taskName, err)
	}
	// The current Task is fenced by the UID the call was authenticated as:
	// a Task recreated under the same name since then is another Task, and
	// its workspace never scopes this call's credential.
	if tc := GetToolContext(ctx); tc != nil && strings.TrimSpace(tc.TaskUID) != "" &&
		taskName == strings.TrimSpace(tc.TaskID) && string(task.UID) != strings.TrimSpace(tc.TaskUID) {
		return githubTaskContext{}, fmt.Errorf("task %s was replaced since this call was authorized; its workspace does not scope the call", taskName)
	}
	return githubTaskScopesOf(&task, taskName, workspaceOnly)
}

// githubTaskScopesOf returns the repository scopes task declares. With
// workspaceOnly, only spec.workspace repositories count.
func githubTaskScopesOf(task *corev1alpha1.Task, taskName string, workspaceOnly bool) (githubTaskContext, error) {
	var result githubTaskContext
	var scopes []githubRepoScope
	ws := githubTaskWorkspace(task)
	if ws != nil {
		result.forgeCredentialRef = ws.ForgeCredentialRef
	}
	if ws != nil && strings.TrimSpace(ws.GitRepo) != "" {
		owner, repo, err := parseGitHubRepo(ws.GitRepo)
		if err != nil {
			return githubTaskContext{}, fmt.Errorf("failed to parse GitHub repo from %s: %w", ws.GitRepo, err)
		}
		scopes = append(scopes, githubRepoScope{
			source:            "task workspace",
			owner:             owner,
			repo:              repo,
			readCredentialRef: ws.ReadCredentialRef,
		})
	}
	if ws != nil && strings.TrimSpace(ws.PublicationGitRepo) != "" {
		owner, repo, err := parseGitHubRepo(ws.PublicationGitRepo)
		if err != nil {
			return githubTaskContext{}, fmt.Errorf("failed to parse GitHub publication repo from %s: %w", ws.PublicationGitRepo, err)
		}
		scopes = append(scopes, githubRepoScope{
			source:            "task publication workspace",
			owner:             owner,
			repo:              repo,
			readCredentialRef: ws.PublicationReadCredentialRef,
		})
	}
	if task.Spec.Transaction != nil && !workspaceOnly {
		if txRepo := strings.TrimSpace(task.Spec.Transaction.Context["repo"]); txRepo != "" {
			owner, repo, err := parseGitHubRepo(txRepo)
			if err != nil {
				return githubTaskContext{}, fmt.Errorf("failed to parse transaction repo context: %w", err)
			}
			scopes = append(scopes, githubRepoScope{source: "transaction repo context", owner: owner, repo: repo})
		}
	}
	if len(scopes) == 0 {
		if ws == nil {
			return githubTaskContext{}, fmt.Errorf("task %s does not have workspace configuration", taskName)
		}
		if strings.TrimSpace(ws.GitRepo) == "" {
			return githubTaskContext{}, fmt.Errorf("task %s workspace has no gitRepo configured", taskName)
		}
	}
	result.scopes = scopes
	return result, nil
}

func githubRepoMatches(owner, repo, wantOwner, wantRepo string) bool {
	return strings.EqualFold(owner, wantOwner) && strings.EqualFold(repo, wantRepo)
}

func githubTaskNameFromContext(ctx context.Context, taskName string) string {
	if taskName = strings.TrimSpace(taskName); taskName != "" {
		return taskName
	}
	if tc := GetToolContext(ctx); tc != nil {
		return strings.TrimSpace(tc.TaskID)
	}
	return ""
}

func githubTaskNamespace(ctx context.Context) string {
	if tc := GetToolContext(ctx); tc != nil {
		if ns := strings.TrimSpace(tc.Namespace); ns != "" {
			return ns
		}
	}
	if ns := os.Getenv(envOrkaTaskNamespace); ns != "" {
		return ns
	}
	return defaultNamespace
}

func formatGitHubRepoScopes(scopes []githubRepoScope) string {
	parts := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		parts = append(parts, fmt.Sprintf("%s=%s/%s", scope.source, scope.owner, scope.repo))
	}
	return strings.Join(parts, ", ")
}

func githubTaskWorkspace(task *corev1alpha1.Task) *corev1alpha1.WorkspaceConfig {
	if task == nil {
		return nil
	}
	return task.Spec.Workspace
}

func (c githubTaskContext) credentialRef(
	taskName, owner, repo string,
	policy githubCredentialPolicy,
) (*corev1alpha1.WorkspaceCredentialReference, error) {
	switch policy {
	case githubCredentialRead:
		for _, scope := range c.scopes {
			if githubRepoMatches(owner, repo, scope.owner, scope.repo) && scope.readCredentialRef != nil {
				return scope.readCredentialRef, nil
			}
		}
		return nil, nil
	case githubCredentialForgeMutation:
		if c.forgeCredentialRef == nil || strings.TrimSpace(c.forgeCredentialRef.Name) == "" {
			return nil, fmt.Errorf("task %s workspace has no forgeCredentialRef configured", taskName)
		}
		return c.forgeCredentialRef, nil
	default:
		return nil, fmt.Errorf("unsupported GitHub credential policy %d", policy)
	}
}

func resolveGitSecretToken(ctx context.Context, k8sClient client.Client, credentialRef *corev1alpha1.WorkspaceCredentialReference) (string, error) {
	if credentialRef == nil {
		return "", nil
	}
	credentialName := credentialRef.Name
	if strings.TrimSpace(credentialName) == "" {
		return "", fmt.Errorf("git credential reference name is required")
	}

	namespace := githubTaskNamespace(ctx)
	if tc := GetToolContext(ctx); tc != nil {
		if tc.AuthorizeSecretRead == nil {
			if tc.RequireSecretReadAuthorization {
				return "", fmt.Errorf("git secret %s/%s requires a secret credential authorizer", namespace, credentialName)
			}
		} else if authzErr := tc.AuthorizeSecretRead(ctx, namespace, credentialName); authzErr != nil {
			return "", fmt.Errorf("not authorized to read git secret %s/%s: %s", namespace, credentialName, authzErr.Message)
		}
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: credentialName, Namespace: namespace}, &secret); err != nil {
		return "", fmt.Errorf("failed to get git secret %s: %w", credentialName, err)
	}

	key := credentialRef.Key
	if key == "" {
		key = tokenKey
	}
	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("git secret %s does not contain configured key %q", credentialName, key)
	}
	token := strings.TrimSpace(string(value))
	if token == "" {
		return "", fmt.Errorf("git secret %s contains an empty configured key %q", credentialName, key)
	}

	return token, nil
}

// resolveToken attempts to read a GitHub token from well-known file paths
// and environment variables.
func resolveToken() string {
	// Try mounted secret files
	for _, path := range []string{"/secrets/git/token", "/secrets/git/password", "/secrets/git/" + workerenv.GitHubToken} {
		if data, err := os.ReadFile(path); err == nil {
			if t := strings.TrimSpace(string(data)); t != "" {
				return t
			}
		}
	}

	// Fall back to GITHUB_TOKEN env var
	return os.Getenv(workerenv.GitHubToken)
}
