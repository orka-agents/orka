package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/orka-agents/orka/internal/store"
)

type failCreateMonitorRunStore struct {
	store.RepositoryMonitorStore
	remainingFailures int
}

func (s *failCreateMonitorRunStore) CreateMonitorRun(ctx context.Context, run *store.MonitorRun) error {
	if s.remainingFailures > 0 {
		s.remainingFailures--
		return errors.New("injected monitor run create failure")
	}
	return s.RepositoryMonitorStore.CreateMonitorRun(ctx, run)
}

func TestGitHubWebhook_CommandLabelIsConsumedAfterDurableQueue(t *testing.T) {
	monitorStore := setupGitHubWebhookMonitorStore(t)
	pullRequestsEnabled := false
	monitor := githubWebhookRepositoryMonitor("consume-after-queue", false)
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels = true

	delivery := "delivery-consume-after-queue"
	target := githubLabelTarget{
		Kind:   repositoryMonitorTargetKindIssue,
		Number: 41,
		State:  "open",
		Title:  "Plan after queue",
		Body:   "Please plan only after durable intake.",
		Labels: []string{"orka:plan"},
	}
	dedupe := repositoryMonitorCommandDedupeKey(monitor, target, "orka:plan", delivery)
	command := &store.CommandEvent{ID: repositoryMonitorCommandID(dedupe)}
	runID := repositoryMonitorCommandRunID(command)
	actionID := store.RepositoryMonitorWorkActionID(command.ID, commandIntentPlan)

	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/octocat/permission"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"permission":"write"}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/41/labels/"):
			run, runErr := monitorStore.GetMonitorRun(r.Context(), monitor.Namespace, runID)
			action, actionErr := monitorStore.GetWorkAction(r.Context(), monitor.Namespace, actionID)
			if runErr != nil || actionErr != nil || run.Phase != repositoryMonitorRunPhaseQueued || action.Status != repositoryMonitorRunPhaseQueued || action.RunID != runID {
				http.Error(w, fmt.Sprintf("command label removed before durable queue: run=%#v runErr=%v action=%#v actionErr=%v", run, runErr, action, actionErr), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected GitHub API request", http.StatusNotFound)
		}
	}))
	t.Cleanup(githubServer.Close)
	secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: githubServer.URL})
	fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})
	body := []byte(`{
		"action":"labeled",
		"label":{"name":"orka:plan"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":41,"state":"open","title":"Plan after queue","body":"Please plan only after durable intake.","html_url":"https://github.com/sozercan/vekil/issues/41","labels":[{"name":"orka:plan"}]},
		"sender":{"login":"octocat"}
	}`)

	resp := performSignedGitHubWebhook(t, server, githubEventIssues, delivery, secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want created after durable queue; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
	mutations, _, err := monitorStore.ListGitHubMutationRecords(t.Context(), store.GitHubMutationRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Operation: "remove_label", Limit: 10})
	if err != nil {
		t.Fatalf("ListGitHubMutationRecords() error = %v", err)
	}
	if len(mutations) != 1 || mutations[0].Status != githubMutationStatusSucceeded {
		t.Fatalf("mutations = %#v, want one succeeded label removal", mutations)
	}
}

func TestGitHubWebhook_DuplicateCommandQueuesBeforeLabelCleanup(t *testing.T) {
	monitorStore := setupGitHubWebhookMonitorStore(t)
	pullRequestsEnabled := false
	monitor := githubWebhookRepositoryMonitor("duplicate-queue-before-consume", false)
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels = true

	delivery := "delivery-duplicate-queue-before-consume"
	target := githubLabelTarget{Kind: repositoryMonitorTargetKindIssue, Number: 45, State: "open", Title: "Repair handoff", Body: "Queue before cleanup.", Labels: []string{"orka:plan"}}
	dedupe := repositoryMonitorCommandDedupeKey(monitor, target, "orka:plan", delivery)
	processedAt := time.Now()
	command := &store.CommandEvent{
		ID:                  repositoryMonitorCommandID(dedupe),
		MonitorNamespace:    monitor.Namespace,
		MonitorName:         monitor.Name,
		Repo:                "sozercan/vekil",
		Kind:                target.Kind,
		Number:              int64(target.Number),
		Source:              githubCommandEventSourceLabel,
		DeliveryID:          delivery,
		Label:               "orka:plan",
		DedupeKey:           dedupe,
		IdempotencyKey:      dedupe,
		Intent:              commandIntentPlan,
		IssueSnapshotDigest: githubIssueSnapshotDigest(monitor, target),
		Status:              githubCommandStatusAccepted,
		CreatedAt:           processedAt,
		ProcessedAt:         &processedAt,
	}
	if err := monitorStore.CreateCommandEvent(t.Context(), command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	runID := repositoryMonitorCommandRunID(command)
	actionID := store.RepositoryMonitorWorkActionID(command.ID, commandIntentPlan)
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/issues/45/labels/") {
			http.Error(w, "unexpected GitHub API request", http.StatusNotFound)
			return
		}
		run, runErr := monitorStore.GetMonitorRun(r.Context(), monitor.Namespace, runID)
		action, actionErr := monitorStore.GetWorkAction(r.Context(), monitor.Namespace, actionID)
		if runErr != nil || actionErr != nil || run.Phase != repositoryMonitorRunPhaseQueued || action.Status != repositoryMonitorRunPhaseQueued || action.RunID != runID {
			http.Error(w, fmt.Sprintf("duplicate cleanup preceded durable repair: run=%#v runErr=%v action=%#v actionErr=%v", run, runErr, action, actionErr), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(githubServer.Close)
	secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: githubServer.URL})
	fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})
	body := []byte(`{
		"action":"labeled",
		"label":{"name":"orka:plan"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":45,"state":"open","title":"Repair handoff","body":"Queue before cleanup.","html_url":"https://github.com/sozercan/vekil/issues/45","labels":[{"name":"orka:plan"}]},
		"sender":{"login":"octocat"}
	}`)
	resp := performSignedGitHubWebhook(t, server, githubEventIssues, delivery, secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want created repaired handoff; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
}

func TestGitHubWebhook_ProcessedDuplicateRetriesLabelCleanupWithoutErasingReason(t *testing.T) {
	monitorStore := setupGitHubWebhookMonitorStore(t)
	pullRequestsEnabled := false
	monitor := githubWebhookRepositoryMonitor("processed-cleanup-retry", false)
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels = true
	fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())

	delivery := "delivery-processed-cleanup"
	target := githubLabelTarget{Kind: repositoryMonitorTargetKindIssue, Number: 46, State: "open", Title: "Processed", Body: "Retry cleanup.", Labels: []string{"orka:plan"}}
	dedupe := repositoryMonitorCommandDedupeKey(monitor, target, "orka:plan", delivery)
	processedAt := time.Now()
	const terminalReason = "workflow failed after retries"
	command := &store.CommandEvent{
		ID:                  repositoryMonitorCommandID(dedupe),
		MonitorNamespace:    monitor.Namespace,
		MonitorName:         monitor.Name,
		Repo:                "sozercan/vekil",
		Kind:                target.Kind,
		Number:              int64(target.Number),
		Source:              githubCommandEventSourceLabel,
		DeliveryID:          delivery,
		Label:               "orka:plan",
		DedupeKey:           dedupe,
		IdempotencyKey:      dedupe,
		Intent:              commandIntentPlan,
		IssueSnapshotDigest: githubIssueSnapshotDigest(monitor, target),
		Status:              githubCommandStatusProcessed,
		Error:               terminalReason,
		CreatedAt:           processedAt,
		ProcessedAt:         &processedAt,
	}
	if err := monitorStore.CreateCommandEvent(t.Context(), command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, commandIntentPlan)
	if err := monitorStore.CreateWorkAction(t.Context(), &store.WorkAction{
		ID:                   actionID,
		MonitorNamespace:     monitor.Namespace,
		MonitorName:          monitor.Name,
		CommandEventID:       command.ID,
		TargetKind:           target.Kind,
		TargetNumber:         int64(target.Number),
		TargetSnapshotDigest: command.IssueSnapshotDigest,
		DesiredAction:        commandIntentPlan,
		DedupeKey:            store.RepositoryMonitorWorkActionDedupeKey(monitor.Namespace, monitor.Name, monitor.Generation, target.Kind, int64(target.Number), "", command.IssueSnapshotDigest, commandIntentPlan),
		Status:               repositoryMonitorRunPhaseFailed,
		Phase:                repositoryMonitorRunPhaseFailed,
		Error:                terminalReason,
		CreatedAt:            processedAt,
		CompletedAt:          &processedAt,
	}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	mutationID := "ghmut-" + githubReplayKeySuffix(githubWebhookReplayKey([]byte(command.ID+"|remove_label")))
	if err := monitorStore.CreateGitHubMutationRecord(t.Context(), &store.GitHubMutationRecord{
		ID:               mutationID,
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		CommandEventID:   command.ID,
		WorkActionID:     actionID,
		Operation:        "remove_label",
		TargetKind:       target.Kind,
		TargetNumber:     int64(target.Number),
		Reason:           "consume_command_label",
		Status:           repositoryMonitorRunPhaseFailed,
		Error:            "temporary GitHub failure",
		CreatedAt:        processedAt,
	}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}

	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/issues/46/labels/") {
			http.Error(w, "unexpected GitHub API request", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(githubServer.Close)
	secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: githubServer.URL})
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})
	body := []byte(`{
		"action":"labeled",
		"label":{"name":"orka:plan"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":46,"state":"open","title":"Processed","body":"Retry cleanup.","html_url":"https://github.com/sozercan/vekil/issues/46","labels":[{"name":"orka:plan"}]},
		"sender":{"login":"octocat"}
	}`)
	resp := performSignedGitHubWebhook(t, server, githubEventIssues, delivery, secret, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want accepted processed cleanup retry; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
	persisted, err := monitorStore.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
	if err != nil {
		t.Fatalf("GetCommandEvent() error = %v", err)
	}
	if persisted.Error != terminalReason {
		t.Fatalf("command error = %q, want preserved terminal reason %q", persisted.Error, terminalReason)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(t.Context(), monitor.Namespace, mutationID)
	if err != nil {
		t.Fatalf("GetGitHubMutationRecord() error = %v", err)
	}
	if mutation.Status != githubMutationStatusSucceeded || mutation.Error != "" {
		t.Fatalf("mutation = %#v, want succeeded cleanup retry", mutation)
	}
}

func TestGitHubWebhook_CompletedDuplicateStillConsumesCommandLabel(t *testing.T) {
	monitorStore := setupGitHubWebhookMonitorStore(t)
	pullRequestsEnabled := false
	monitor := githubWebhookRepositoryMonitor("consume-completed-duplicate", false)
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels = true
	fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())

	delivery := "delivery-consume-completed"
	target := githubLabelTarget{Kind: repositoryMonitorTargetKindIssue, Number: 44, State: "open", Title: "Completed", Body: "Recover cleanup.", Labels: []string{"orka:plan"}}
	dedupe := repositoryMonitorCommandDedupeKey(monitor, target, "orka:plan", delivery)
	processedAt := time.Now()
	command := &store.CommandEvent{
		ID:                  repositoryMonitorCommandID(dedupe),
		MonitorNamespace:    monitor.Namespace,
		MonitorName:         monitor.Name,
		Repo:                "sozercan/vekil",
		Kind:                target.Kind,
		Number:              int64(target.Number),
		Source:              githubCommandEventSourceLabel,
		DeliveryID:          delivery,
		Label:               "orka:plan",
		DedupeKey:           dedupe,
		IdempotencyKey:      dedupe,
		Intent:              commandIntentPlan,
		IssueSnapshotDigest: githubIssueSnapshotDigest(monitor, target),
		Status:              githubCommandStatusCompleted,
		CreatedAt:           processedAt,
		ProcessedAt:         &processedAt,
	}
	if err := monitorStore.CreateCommandEvent(t.Context(), command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, commandIntentPlan)
	if err := monitorStore.CreateWorkAction(t.Context(), &store.WorkAction{
		ID:                   actionID,
		MonitorNamespace:     monitor.Namespace,
		MonitorName:          monitor.Name,
		CommandEventID:       command.ID,
		TargetKind:           target.Kind,
		TargetNumber:         int64(target.Number),
		TargetSnapshotDigest: command.IssueSnapshotDigest,
		DesiredAction:        commandIntentPlan,
		DependsOnActionID:    "wa-active",
		DedupeKey:            store.RepositoryMonitorWorkActionDedupeKey(monitor.Namespace, monitor.Name, monitor.Generation, target.Kind, int64(target.Number), "", command.IssueSnapshotDigest, commandIntentPlan),
		Status:               githubCommandStatusCompleted,
		Phase:                "coalesced",
		CreatedAt:            processedAt,
		CompletedAt:          &processedAt,
	}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}

	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/issues/44/labels/") {
			http.Error(w, "unexpected GitHub API request", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(githubServer.Close)
	secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: githubServer.URL})
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})
	body := []byte(`{
		"action":"labeled",
		"label":{"name":"orka:plan"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":44,"state":"open","title":"Completed","body":"Recover cleanup.","html_url":"https://github.com/sozercan/vekil/issues/44","labels":[{"name":"orka:plan"}]},
		"sender":{"login":"octocat"}
	}`)
	resp := performSignedGitHubWebhook(t, server, githubEventIssues, delivery, secret, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want accepted duplicate cleanup; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
	mutations, _, err := monitorStore.ListGitHubMutationRecords(t.Context(), store.GitHubMutationRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Operation: "remove_label", Limit: 10})
	if err != nil {
		t.Fatalf("ListGitHubMutationRecords() error = %v", err)
	}
	if len(mutations) != 1 || mutations[0].Status != githubMutationStatusSucceeded {
		t.Fatalf("mutations = %#v, want recovered label cleanup", mutations)
	}
	persisted, err := monitorStore.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
	if err != nil {
		t.Fatalf("GetCommandEvent() error = %v", err)
	}
	if !strings.Contains(persisted.Error, "coalesced with active workflow action") {
		t.Fatalf("command error = %q, want preserved coalescing reason", persisted.Error)
	}
}

func TestGitHubWebhook_RunCreateFailureLeavesRecoverableWorkAction(t *testing.T) {
	baseStore := setupGitHubWebhookMonitorStore(t)
	monitorStore := &failCreateMonitorRunStore{RepositoryMonitorStore: baseStore, remainingFailures: 1}
	permissionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"permission":"write"}`))
	}))
	t.Cleanup(permissionServer.Close)
	secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: permissionServer.URL})
	pullRequestsEnabled := false
	monitor := githubWebhookRepositoryMonitor("run-create-failure", false)
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})
	body := []byte(`{
		"action":"labeled",
		"label":{"name":"orka:plan"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":42,"state":"open","title":"Retry durable intake","body":"Do not coalesce against phantom work.","html_url":"https://github.com/sozercan/vekil/issues/42","labels":[{"name":"orka:plan"}]},
		"sender":{"login":"octocat"}
	}`)
	target := githubLabelTarget{Kind: repositoryMonitorTargetKindIssue, Number: 42, State: "open", Title: "Retry durable intake", Body: "Do not coalesce against phantom work.", Labels: []string{"orka:plan"}}

	firstDelivery := "delivery-run-create-fails"
	firstResp := performSignedGitHubWebhook(t, server, githubEventIssues, firstDelivery, secret, body)
	if firstResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want internal server error; body: %s", firstResp.StatusCode, readRespBody(t, firstResp))
	}
	firstDedupe := repositoryMonitorCommandDedupeKey(monitor, target, "orka:plan", firstDelivery)
	firstCommandID := repositoryMonitorCommandID(firstDedupe)
	firstAction, err := baseStore.GetWorkAction(t.Context(), monitor.Namespace, store.RepositoryMonitorWorkActionID(firstCommandID, commandIntentPlan))
	if err != nil {
		t.Fatalf("GetWorkAction(first) error = %v", err)
	}
	if firstAction.Status != store.RepositoryMonitorWorkActionStatusRetryPending || firstAction.BlockedReason != store.RepositoryMonitorWorkActionBlockedReasonRunSignalFailed || firstAction.CompletedAt != nil {
		t.Fatalf("first action = %#v, want nonterminal retryable run creation failure", firstAction)
	}

	secondDelivery := "delivery-run-create-retry"
	secondResp := performSignedGitHubWebhook(t, server, githubEventIssues, secondDelivery, secret, body)
	if secondResp.StatusCode != http.StatusAccepted {
		t.Fatalf("second status = %d, want accepted coalesced retry; body: %s", secondResp.StatusCode, readRespBody(t, secondResp))
	}
	secondDedupe := repositoryMonitorCommandDedupeKey(monitor, target, "orka:plan", secondDelivery)
	secondCommand := &store.CommandEvent{ID: repositoryMonitorCommandID(secondDedupe)}
	secondRunID := repositoryMonitorCommandRunID(secondCommand)
	if _, err := baseStore.GetMonitorRun(t.Context(), monitor.Namespace, secondRunID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetMonitorRun(second) error = %v, want no duplicate run", err)
	}
	secondAction, err := baseStore.GetWorkAction(t.Context(), monitor.Namespace, store.RepositoryMonitorWorkActionID(secondCommand.ID, commandIntentPlan))
	if err != nil {
		t.Fatalf("GetWorkAction(second) error = %v", err)
	}
	if secondAction.Status != githubCommandStatusCompleted || secondAction.Phase != "coalesced" || secondAction.DependsOnActionID != firstAction.ID {
		t.Fatalf("second action = %#v, want coalescing onto retry-pending handoff", secondAction)
	}
}

func TestCreateRepositoryMonitorCommandEventRunCreateFailureMarksWorkActionRetryPending(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	createBody := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true}},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	if err != nil || createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor status=%v err=%v", createResp.StatusCode, err)
	}
	baseStore := handlers.repositoryMonitorStore
	if err := baseStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		Kind:             repositoryMonitorTargetKindIssue,
		ItemKey:          "43",
		Number:           43,
		State:            "open",
		SnapshotDigest:   "sha256:issue43",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	handlers.repositoryMonitorStore = &failCreateMonitorRunStore{RepositoryMonitorStore: baseStore, remainingFailures: 1}

	commandReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"issue","number":43,"intent":"plan"}`))
	commandReq.Header.Set("Content-Type", "application/json")
	commandResp, err := app.Test(commandReq)
	if err != nil {
		t.Fatalf("command request error = %v", err)
	}
	if commandResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("command status = %d, want internal server error; body: %s", commandResp.StatusCode, readRespBody(t, commandResp))
	}
	commands, _, err := baseStore.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: "demo", MonitorName: "repo-monitor", Kind: repositoryMonitorTargetKindIssue, Number: 43, Limit: 10})
	if err != nil {
		t.Fatalf("ListCommandEvents() error = %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("commands = %#v, want one durable command", commands)
	}
	action, err := baseStore.GetWorkAction(t.Context(), "demo", store.RepositoryMonitorWorkActionID(commands[0].ID, commandIntentPlan))
	if err != nil {
		t.Fatalf("GetWorkAction() error = %v", err)
	}
	if action.Status != store.RepositoryMonitorWorkActionStatusRetryPending || action.BlockedReason != store.RepositoryMonitorWorkActionBlockedReasonRunSignalFailed || action.CompletedAt != nil {
		t.Fatalf("action = %#v, want nonterminal retryable run creation failure", action)
	}
}
