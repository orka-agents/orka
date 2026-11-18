//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/test/utils"
)

const e2eCleanupFinalizer = "e2e.orka.ai/cleanup-observer"

// These records deliberately exclude Task specs, transcripts, credentials,
// response bodies and unrestricted command errors. A failed run keeps the
// last completed boundary instead of relying on Ginkgo's primary assertion.
type e2eTaskCleanupEvidence struct {
	Name                        string    `json:"name"`
	Namespace                   string    `json:"namespace"`
	UID                         types.UID `json:"uid"`
	SessionName                 string    `json:"sessionName,omitempty"`
	RuntimeSessionControlName   string    `json:"runtimeSessionControlName,omitempty"`
	RuntimeSessionControlUID    types.UID `json:"runtimeSessionControlUID,omitempty"`
	RuntimeSessionControlAbsent bool      `json:"runtimeSessionControlAbsent,omitempty"`
	RuntimeSessionUID           string    `json:"runtimeSessionUID,omitempty"`
	RuntimeSessionGeneration    int64     `json:"runtimeSessionGeneration,omitempty"`
	RuntimeInstanceDigest       string    `json:"runtimeInstanceDigest,omitempty"`
	Attempt                     int32     `json:"attempt,omitempty"`
	RuntimeCleanupDigest        string    `json:"runtimeSessionCleanupDigest,omitempty"`
	ReceiptKind                 string    `json:"receiptKind,omitempty"`
	ReceiptRequired             bool      `json:"receiptRequired"`
	ReceiptVerified             bool      `json:"receiptVerified"`
	ObserverInstalled           bool      `json:"observerInstalled"`
	DeleteRequested             bool      `json:"deleteRequested"`
	ProductFinalizersReleased   bool      `json:"productFinalizersReleased"`
	ObserverReleased            bool      `json:"observerReleased"`
	Absent                      bool      `json:"absent"`
}

type e2eSessionCleanupEvidence struct {
	Name              string    `json:"name"`
	Namespace         string    `json:"namespace"`
	CreatedAt         string    `json:"createdAt,omitempty"`
	RuntimeSessionUID string    `json:"runtimeSessionUID,omitempty"`
	ControlName       string    `json:"controlName,omitempty"`
	ControlUID        types.UID `json:"controlUID,omitempty"`
	DeleteStatuses    []int     `json:"deleteStatuses,omitempty"`
	Absent            bool      `json:"absent"`
}

type e2eCleanupReport struct {
	SchemaVersion int                          `json:"schemaVersion"`
	StartedAt     time.Time                    `json:"startedAt"`
	FinishedAt    time.Time                    `json:"finishedAt,omitempty"`
	Tasks         []*e2eTaskCleanupEvidence    `json:"tasks"`
	Sessions      []*e2eSessionCleanupEvidence `json:"sessions"`
	Stage         string                       `json:"stage"`
	Passed        bool                         `json:"passed"`
	Failure       string                       `json:"failure,omitempty"`
}

type e2eSessionMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	CreatedAt string `json:"createdAt"`
}

type e2eCleanup struct {
	kube       client.Client
	apiBaseURL string
	token      string
	http       *http.Client
	pollEvery  time.Duration
	report     e2eCleanupReport
	save       func(e2eCleanupReport) error
}

func (c *e2eCleanup) interval() time.Duration {
	if c.pollEvery > 0 {
		return c.pollEvery
	}
	return time.Second
}

func newE2ECleanup(apiBaseURL, token string) (*e2eCleanup, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.New("read E2E Kubernetes configuration")
	}
	cfg.Timeout = 10 * time.Second
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		return nil, errors.New("register E2E cleanup resource types")
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, errors.New("initialize E2E cleanup client")
	}
	dir := os.Getenv("E2E_CLEANUP_REPORT_DIR")
	dir, err = e2eCleanupReportDir(dir)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "cleanup-*.json")
	if err != nil {
		return nil, errors.New("create E2E cleanup report")
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return nil, errors.New("close E2E cleanup report")
	}
	return &e2eCleanup{
		kube: kube, apiBaseURL: apiBaseURL, token: token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		report: e2eCleanupReport{SchemaVersion: 1, StartedAt: time.Now().UTC()},
		save: func(report e2eCleanupReport) error {
			data, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return errors.New("encode E2E cleanup evidence")
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				return errors.New("save E2E cleanup evidence")
			}
			return nil
		},
	}, nil
}

func e2eCleanupReportDir(dir string) (string, error) {
	if dir == "" {
		root, err := utils.GetProjectDir()
		if err != nil {
			return "", errors.New("locate E2E cleanup report directory")
		}
		dir = filepath.Join(root, "bin", "e2e-cleanup")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.New("create E2E cleanup report directory")
	}
	return dir, nil
}

type e2eSuiteCleanupEvidence struct {
	SchemaVersion int       `json:"schemaVersion"`
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt,omitempty"`
	Stage         string    `json:"stage"`
	Completed     []string  `json:"completed"`
	Passed        bool      `json:"passed"`
}

func saveE2ESuiteCleanup(report e2eSuiteCleanupEvidence) error {
	dir, err := e2eCleanupReportDir(os.Getenv("E2E_CLEANUP_REPORT_DIR"))
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.New("encode E2E suite cleanup evidence")
	}
	if err := os.WriteFile(filepath.Join(dir, "suite-cleanup.json"), append(data, '\n'), 0o600); err != nil {
		return errors.New("save E2E suite cleanup evidence")
	}
	return nil
}

func cleanupRemainingE2ETasks() error {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Minute)
	defer stop()
	apiBaseURL, cancel, command, err := startControllerAPIPortForward(18119)
	if err != nil {
		return errors.New("start authenticated suite cleanup API connection")
	}
	defer stopPortForward(cancel, command)
	token, err := cleanupE2EToken(ctx)
	if err != nil || token == "" {
		return errors.New("authenticate suite cleanup")
	}
	cleanup, err := newE2ECleanup(apiBaseURL, token)
	if err != nil {
		return err
	}
	var tasks corev1alpha1.TaskList
	if err := cleanup.kube.List(ctx, &tasks, client.InNamespace(namespace)); err != nil {
		return safeCleanupError("inventory remaining E2E Tasks", err)
	}
	names := make([]string, 0, len(tasks.Items))
	for _, task := range tasks.Items {
		names = append(names, task.Name)
	}
	if err := cleanup.tasks(ctx, names, true); err != nil {
		return err
	}
	if err := cleanup.kube.List(ctx, &tasks, client.InNamespace(namespace)); err != nil {
		return safeCleanupError("verify no remaining E2E Tasks", err)
	}
	if len(tasks.Items) != 0 {
		return errors.New("Tasks appeared or remained after the final cleanup sweep")
	}
	sessions, err := cleanup.listSessions(ctx)
	if err != nil {
		return err
	}
	if len(sessions) != 0 {
		return errors.New("Sessions appeared or remained after the final cleanup sweep")
	}
	return nil
}

func cleanupE2EToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "create", "token", serviceAccountName,
		"-n", namespace, "--duration=10m", "--request-timeout=10s")
	cmd.WaitDelay = time.Second
	// Capture the token only in memory, without the logging command runner.
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", safeCleanupError("authenticate suite cleanup", ctx.Err())
		}
		return "", errors.New("authenticate suite cleanup failed; response details omitted")
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("suite cleanup authentication returned an empty token")
	}
	return token, nil
}

func (c *e2eCleanup) checkpoint(stage string) error {
	c.report.Stage = stage
	return c.save(c.report)
}

// tasks requests normal deletion under exact UID preconditions. Its finalizer
// only observes the controller's existing cleanup barriers; it never supplies
// a receipt or removes a product finalizer. allSessions is used only by the
// final sweep of this suite's isolated test namespace.
func (c *e2eCleanup) tasks(ctx context.Context, taskNames []string, allSessions bool) (resultErr error) {
	defer func() {
		c.report.FinishedAt = time.Now().UTC()
		c.report.Passed = resultErr == nil
		if resultErr != nil {
			c.report.Failure = cleanupFailureClass(resultErr)
		}
		resultErr = errors.Join(resultErr, c.save(c.report))
	}()
	if err := c.checkpoint("capture_tasks"); err != nil {
		return err
	}
	sessionNames := map[string]string{}
	for _, name := range taskNames {
		task := &corev1alpha1.Task{}
		if err := c.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, task); err != nil {
			return safeCleanupError("read Task", err)
		}
		record := &e2eTaskCleanupEvidence{Name: name, Namespace: namespace, UID: task.UID}
		if task.Status.Execution != nil && task.Status.Execution.RuntimeSessionUID != "" {
			// An admitted Task must prove runtime cleanup even if it later
			// loses its execution status; never downgrade it to non-ACP.
			record.ReceiptRequired = true
		}
		c.report.Tasks = append(c.report.Tasks, record)
		if err := c.observeTask(ctx, task, record); err != nil {
			return err
		}
		if task.Spec.SessionRef != nil {
			runtimeUID := ""
			if task.Status.Execution != nil {
				runtimeUID = task.Status.Execution.RuntimeSessionUID
			}
			sessionName := task.Spec.SessionRef.Name
			if prior, exists := sessionNames[sessionName]; exists && prior != "" && runtimeUID != "" && prior != runtimeUID {
				return errors.New("Tasks disagree on immutable Session identity")
			}
			if runtimeUID != "" || sessionNames[sessionName] == "" {
				sessionNames[sessionName] = runtimeUID
			}
		}
	}
	if err := c.checkpoint("capture_sessions"); err != nil {
		return err
	}
	if err := c.captureSessions(ctx, sessionNames, allSessions); err != nil {
		return err
	}
	if err := c.checkpoint("request_task_deletion"); err != nil {
		return err
	}
	for _, record := range c.report.Tasks {
		task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
			Namespace: record.Namespace, Name: record.Name, UID: record.UID,
		}}
		options := &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &record.UID}}
		if err := c.kube.Delete(ctx, task, options); err != nil {
			return safeCleanupError("delete exact Task", err)
		}
		record.DeleteRequested = true
	}
	if err := c.checkpoint("archive_sessions"); err != nil {
		return err
	}
	for _, session := range c.report.Sessions {
		if err := c.archiveSession(ctx, session); err != nil {
			return err
		}
	}
	if err := c.checkpoint("observe_task_receipts"); err != nil {
		return err
	}
	for _, record := range c.report.Tasks {
		if err := c.finalizeTask(ctx, record); err != nil {
			return err
		}
	}
	return c.checkpoint("complete")
}

func (c *e2eCleanup) observeTask(ctx context.Context, task *corev1alpha1.Task, record *e2eTaskCleanupEvidence) error {
	return wait.PollUntilContextCancel(ctx, c.interval(), true, func(ctx context.Context) (bool, error) {
		if task.UID == "" || task.UID != record.UID || !task.DeletionTimestamp.IsZero() {
			return false, errors.New("Task identity changed or deletion started before cleanup observation")
		}
		if !slices.Contains(task.Finalizers, e2eCleanupFinalizer) {
			patch, err := cleanupFinalizerPatch(task, append(slices.Clone(task.Finalizers), e2eCleanupFinalizer))
			if err != nil {
				return false, err
			}
			if err := c.kube.Patch(ctx, task, patch); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
					if err := c.kube.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
						return false, safeCleanupError("refresh cleanup observer", err)
					}
					return false, nil
				}
				return false, safeCleanupError("install cleanup observer", err)
			}
		}
		record.ObserverInstalled = true
		return true, c.checkpoint("capture_tasks")
	})
}

func cleanupFinalizerPatch(task *corev1alpha1.Task, finalizers []string) (client.Patch, error) {
	data, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": task.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": task.ResourceVersion},
		{"op": "add", "path": "/metadata/finalizers", "value": finalizers},
	})
	if err != nil {
		return nil, errors.New("encode exact cleanup observer patch")
	}
	return client.RawPatch(types.JSONPatchType, data), nil
}

func (c *e2eCleanup) finalizeTask(ctx context.Context, record *e2eTaskCleanupEvidence) error {
	return wait.PollUntilContextCancel(ctx, c.interval(), true, func(ctx context.Context) (bool, error) {
		task := &corev1alpha1.Task{}
		if err := c.kube.Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: record.Name}, task); err != nil {
			if apierrors.IsNotFound(err) && record.ObserverReleased {
				record.Absent = true
				return true, c.checkpoint("observe_task_receipts")
			}
			return false, safeCleanupError("observe exact Task finalization", err)
		}
		if task.UID != record.UID {
			return false, errors.New("Task UID changed during cleanup")
		}
		if record.ObserverReleased {
			return false, nil
		}
		if !slices.Contains(task.Finalizers, e2eCleanupFinalizer) {
			return false, errors.New("Task cleanup observer disappeared before evidence was captured")
		}
		ready, err := captureTaskCleanupReceipt(task, record)
		if err != nil || !ready {
			return false, err
		}
		if len(task.Finalizers) != 1 {
			return false, nil
		}
		record.ProductFinalizersReleased = true
		if err := c.checkpoint("release_task_observer"); err != nil {
			return false, err
		}
		patch, err := cleanupFinalizerPatch(task, []string{})
		if err != nil {
			return false, err
		}
		if err := c.kube.Patch(ctx, task, patch); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
				return false, nil
			}
			return false, safeCleanupError("release Task cleanup observer", err)
		}
		record.ObserverReleased = true
		return false, c.checkpoint("observe_task_absence")
	})
}

func captureTaskCleanupReceipt(task *corev1alpha1.Task, record *e2eTaskCleanupEvidence) (bool, error) {
	if task.DeletionTimestamp.IsZero() {
		return false, nil
	}
	e := task.Status.Execution
	if e == nil || e.RuntimeSessionUID == "" {
		if record.ReceiptRequired {
			return false, errors.New("Task lost a required runtime cleanup identity")
		}
		// A never-admitted or non-ACP Task can finalize without a terminal
		// status update. Its product finalizers remain the cleanup barrier.
		return true, nil
	}
	terminal := []corev1alpha1.TaskPhase{
		corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled,
	}
	if task.Status.CompletionTime == nil || !slices.Contains(terminal, task.Status.Phase) {
		return false, nil
	}
	if record.RuntimeSessionUID != "" && (record.RuntimeSessionUID != e.RuntimeSessionUID ||
		record.RuntimeSessionGeneration != e.RuntimeSessionGeneration || record.Attempt != e.Attempt) {
		return false, errors.New("Task runtime cleanup identity changed")
	}
	if record.RuntimeInstanceDigest != "" {
		idHash := sha256.Sum256([]byte(e.RuntimeInstanceID))
		if record.RuntimeInstanceDigest != "sha256:"+hex.EncodeToString(idHash[:]) {
			return false, errors.New("Task runtime instance changed during cleanup")
		}
	}
	record.ReceiptRequired = true
	record.RuntimeSessionUID = e.RuntimeSessionUID
	record.RuntimeSessionGeneration, record.Attempt = e.RuntimeSessionGeneration, e.Attempt
	if e.RuntimeInstanceID != "" {
		idHash := sha256.Sum256([]byte(e.RuntimeInstanceID))
		record.RuntimeInstanceDigest = "sha256:" + hex.EncodeToString(idHash[:])
	}
	// An authenticated registration drain can prove cleanup even when Task
	// admission did not retain the complete task-scoped runtime identity.
	drainDigest, drainErr := e2eRuntimeDrainCleanupDigest(task)
	if drainErr == nil && drainDigest != "" && e.RuntimeSessionCleanupDigest == drainDigest {
		record.RuntimeCleanupDigest, record.ReceiptVerified = drainDigest, true
		record.ReceiptKind = "runtime_drain"
		return true, nil
	}
	if e.Attempt < 1 || e.RuntimeInstanceID == "" || e.RuntimeSessionGeneration < 1 {
		return false, errors.New("incomplete Task runtime cleanup identity")
	}
	expected, err := e2eCleanupDigest("task-runtime-session-cleanup", map[string]any{
		"taskUID": string(task.UID), "attempt": e.Attempt, "runtimeInstanceID": e.RuntimeInstanceID,
		"runtimeSessionUID": e.RuntimeSessionUID, "runtimeSessionGeneration": e.RuntimeSessionGeneration,
	})
	if err != nil {
		return false, err
	}
	if e.RuntimeSessionCleanupDigest != expected {
		return false, drainErr
	}
	record.ReceiptKind = "runtime_session"
	record.RuntimeCleanupDigest, record.ReceiptVerified = expected, true
	return true, nil
}

func e2eCleanupDigest(domain string, value any) (string, error) {
	body, err := harnessv2.CanonicalValue(value)
	if err != nil {
		return "", errors.New("encode cleanup receipt identity")
	}
	sum := sha256.Sum256(append([]byte("orka.acp."+domain+"\x00"), body...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func e2eRuntimeDrainCleanupDigest(task *corev1alpha1.Task) (string, error) {
	binding, execution := task.Status.AgentExecutionBinding, task.Status.Execution
	if binding == nil || binding.RuntimeRef == nil || execution == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent ||
		binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 ||
		binding.Backend != corev1alpha1.AgentExecutionBackendExternalEndpoint {
		return "", nil
	}
	ref := binding.RuntimeRef
	if binding.Task.UID != task.UID || ref.Name == "" || ref.UID == "" || ref.Generation < 1 ||
		execution.AgentRuntimeName != ref.Name || execution.AgentRuntimeUID != string(ref.UID) ||
		execution.RuntimePoolName != "" || execution.RuntimePoolUID != "" {
		return "", errors.New("runtime drain receipt has a mismatched Task or runtime identity")
	}
	normalized := binding.DeepCopy()
	normalized.BindingDigest, normalized.BoundAt = "", metav1.Time{}
	digest, err := e2eCleanupDigest("agent-execution-binding", normalized)
	if err != nil || digest != binding.BindingDigest {
		return "", errors.New("runtime drain receipt has an invalid frozen binding digest")
	}
	return e2eCleanupDigest("agent-runtime-drain-cleanup", map[string]any{
		"taskUID": string(task.UID), "bindingDigest": binding.BindingDigest,
		"agentRuntimeName": ref.Name, "agentRuntimeUID": string(ref.UID), "agentRuntimeGeneration": ref.Generation,
	})
}

func cleanupFailureClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "cleanup_unproved"
}

func safeCleanupError(operation string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, context.Canceled)
	}
	return fmt.Errorf("%s failed; response details omitted", operation)
}

func (c *e2eCleanup) sessionRequest(ctx context.Context, method, path string, target any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.apiBaseURL, "/")+path, nil)
	if err != nil {
		return 0, errors.New("construct Session cleanup request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, safeCleanupError("Session cleanup request", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if target != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
			return resp.StatusCode, errors.New("decode bounded Session metadata response")
		}
	}
	return resp.StatusCode, nil
}

func (c *e2eCleanup) listSessions(ctx context.Context) (map[string]e2eSessionMetadata, error) {
	items := map[string]e2eSessionMetadata{}
	cursor := ""
	for range 100 {
		var page struct {
			Items    []e2eSessionMetadata `json:"items"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
		}
		path := "/api/v1/sessions?namespace=" + url.QueryEscape(namespace) + "&limit=100&continue=" + url.QueryEscape(cursor)
		status, err := c.sessionRequest(ctx, http.MethodGet, path, &page)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("Session metadata listing returned HTTP %d", status)
		}
		for _, item := range page.Items {
			if item.Name == "" || item.Namespace != namespace || item.CreatedAt == "" {
				return nil, errors.New("Session metadata identity is incomplete")
			}
			if _, exists := items[item.Name]; exists {
				return nil, errors.New("Session metadata pagination repeated an identity")
			}
			items[item.Name] = item
		}
		if page.Metadata.Continue == "" {
			return items, nil
		}
		if page.Metadata.Continue == cursor || len(page.Items) == 0 {
			return nil, errors.New("Session metadata pagination did not advance")
		}
		cursor = page.Metadata.Continue
	}
	return nil, errors.New("Session metadata pagination exceeded its bound")
}

func (c *e2eCleanup) sessionControl(ctx context.Context, name string) (*corev1alpha1.RuntimeSessionControl, error) {
	control := &corev1alpha1.RuntimeSessionControl{}
	key := client.ObjectKey{Namespace: namespace, Name: storekube.RuntimeSessionControlObjectName(name)}
	if err := c.kube.Get(ctx, key, control); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, safeCleanupError("read exact RuntimeSessionControl", err)
	}
	if control.Spec.SessionName != name || control.Spec.SessionUID == "" || control.UID == "" {
		return nil, errors.New("RuntimeSessionControl identity does not match Session")
	}
	return control, nil
}

func (c *e2eCleanup) captureSessions(ctx context.Context, names map[string]string, all bool) error {
	if len(names) == 0 && !all {
		return nil
	}
	metadata, err := c.listSessions(ctx)
	if err != nil {
		return err
	}
	if all {
		for name := range metadata {
			if _, exists := names[name]; !exists {
				names[name] = ""
			}
		}
	}
	for name, runtimeUID := range names {
		item, exists := metadata[name]
		control, err := c.sessionControl(ctx, name)
		if err != nil {
			return err
		}
		record := &e2eSessionCleanupEvidence{
			Name: name, Namespace: namespace, CreatedAt: item.CreatedAt, RuntimeSessionUID: runtimeUID,
		}
		c.report.Sessions = append(c.report.Sessions, record)
		if exists && runtimeUID != "" && control == nil {
			return errors.New("existing bound Session lacks its original RuntimeSessionControl")
		}
		if control != nil {
			if !exists || runtimeUID != "" && control.Spec.SessionUID != runtimeUID {
				return errors.New("Session metadata and runtime identity disagree")
			}
			record.ControlName, record.ControlUID, record.RuntimeSessionUID = control.Name, control.UID, control.Spec.SessionUID
		}
		record.Absent = !exists && control == nil
	}
	return c.checkpoint("capture_sessions")
}

func (c *e2eCleanup) archiveSession(ctx context.Context, record *e2eSessionCleanupEvidence) error {
	attempted := false
	return wait.PollUntilContextCancel(ctx, c.interval(), true, func(ctx context.Context) (bool, error) {
		metadata, err := c.listSessions(ctx)
		if err != nil {
			return false, err
		}
		item, exists := metadata[record.Name]
		control, err := c.sessionControl(ctx, record.Name)
		if err != nil {
			return false, err
		}
		if exists && item.CreatedAt != record.CreatedAt {
			return false, errors.New("Session creation identity changed during cleanup")
		}
		if control != nil && (control.UID != record.ControlUID || control.Spec.SessionUID != record.RuntimeSessionUID) {
			return false, errors.New("RuntimeSessionControl identity changed during cleanup")
		}
		if !exists && control == nil {
			record.Absent = true
			return true, c.checkpoint("archive_sessions")
		}
		if !attempted && record.ControlUID != "" && control == nil {
			return false, errors.New("RuntimeSessionControl disappeared before Session cleanup")
		}
		path := "/api/v1/sessions/" + url.PathEscape(record.Name) + "?namespace=" + url.QueryEscape(namespace)
		status, err := c.sessionRequest(ctx, http.MethodDelete, path, nil)
		if err != nil {
			return false, err
		}
		attempted = true
		// Bound evidence size for a slow but healthy 409 retry loop.
		if len(record.DeleteStatuses) == 0 || record.DeleteStatuses[len(record.DeleteStatuses)-1] != status {
			record.DeleteStatuses = append(record.DeleteStatuses, status)
		}
		if status != http.StatusNoContent && status != http.StatusNotFound && status != http.StatusConflict {
			return false, fmt.Errorf("Session DELETE returned HTTP %d", status)
		}
		return false, c.checkpoint("archive_sessions")
	})
}
