package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	DefaultServiceAccountBearerPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultEventRecorderTimeout     = 2 * time.Second
)

// HTTPEventRecorderConfig configures the worker HTTP event recorder.
type HTTPEventRecorderConfig struct {
	ControllerURL string
	Namespace     string
	TaskName      string
	SessionName   string
	BearerPath    string
	Client        *http.Client
	Timeout       time.Duration
	Now           func() time.Time
}

// HTTPEventRecorder posts worker execution events to the controller internal API.
type HTTPEventRecorder struct {
	endpoint    string
	namespace   string
	taskName    string
	sessionName string
	bearerPath  string
	client      *http.Client
	timeout     time.Duration
	now         func() time.Time
}

var _ EventRecorder = (*HTTPEventRecorder)(nil)

// NewHTTPEventRecorderFromEnv builds a controller-backed event recorder from worker env.
// If required env is missing or invalid, it returns a no-op recorder so event recording
// never blocks task execution.
func NewHTTPEventRecorderFromEnv() EventRecorder {
	return NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: os.Getenv(workerenv.ControllerURL),
		Namespace:     os.Getenv(workerenv.TaskNamespace),
		TaskName:      os.Getenv(workerenv.TaskName),
		SessionName:   os.Getenv(workerenv.SessionName),
		BearerPath:    firstNonEmpty(os.Getenv(workerenv.ServiceAccountTokenPath), DefaultServiceAccountBearerPath),
	})
}

// NewHTTPEventRecorder creates a controller-backed event recorder.
// Missing required controller/task configuration returns NoopEventRecorder.
func NewHTTPEventRecorder(cfg HTTPEventRecorderConfig) EventRecorder {
	controllerURL := strings.TrimSpace(cfg.ControllerURL)
	namespace := strings.TrimSpace(cfg.Namespace)
	taskName := strings.TrimSpace(cfg.TaskName)
	sessionName := strings.TrimSpace(cfg.SessionName)
	if controllerURL == "" || namespace == "" || taskName == "" {
		return NoopEventRecorder{}
	}
	endpoint, err := executionEventEndpoint(controllerURL, namespace, taskName)
	if err != nil {
		return NoopEventRecorder{}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultEventRecorderTimeout
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	bearerPath := strings.TrimSpace(cfg.BearerPath)
	if bearerPath == "" {
		bearerPath = DefaultServiceAccountBearerPath
	}
	return &HTTPEventRecorder{
		endpoint:    endpoint,
		namespace:   namespace,
		taskName:    taskName,
		sessionName: sessionName,
		bearerPath:  bearerPath,
		client:      client,
		timeout:     timeout,
		now:         now,
	}
}

// Record implements EventRecorder. Failures are warning-only and never returned to callers.
func (r *HTTPEventRecorder) Record(ctx context.Context, typ string, opts ...EventOption) {
	if err := r.RecordStrict(ctx, typ, opts...); err != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		log.FromContext(ctx).Info("warning: failed to record execution event", "error", err)
	}
}

// RecordStrict implements StrictEventRecorder by posting the event and returning
// transport or non-2xx response errors.
func (r *HTTPEventRecorder) RecordStrict(ctx context.Context, typ string, opts ...EventOption) error {
	if r == nil {
		return fmt.Errorf("http execution event recorder is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	event := RecordedEvent{
		Type:        events.NormalizeExecutionEventType(typ),
		Severity:    events.ExecutionEventSeverityInfo,
		TaskName:    r.taskName,
		SessionName: r.sessionName,
		CreatedAt:   r.now().UTC(),
	}
	if event.Type == "" {
		event.Type = typ
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&event)
		}
	}
	payload, err := events.SanitizeExecutionEventPayload(event.Summary, event.Content, event.ContentText)
	if err == nil {
		event.Summary = payload.Summary
		event.Content = payload.Content
		event.ContentText = payload.ContentText
		event.Truncation = payload.Truncation
	} else {
		event.Summary, event.ContentText, event.Truncation = sanitizeRecordedEventTextFields(event.Summary, event.ContentText)
		event.Content = nil
	}

	body, err := json.Marshal(submitRecordedEventRequest{
		Type:        event.Type,
		Severity:    event.Severity,
		TaskName:    event.TaskName,
		SessionName: event.SessionName,
		AgentName:   event.AgentName,
		ToolName:    event.ToolName,
		ToolCallID:  event.ToolCallID,
		Summary:     event.Summary,
		Content:     event.Content,
		ContentText: event.ContentText,
		Truncation:  event.Truncation,
	})
	if err != nil {
		return fmt.Errorf("marshal execution event: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create execution event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := readServiceAccountToken(r.bearerPath); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("record execution event: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	bodyPreview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"controller rejected execution event: HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(bodyPreview)),
		)
	}
	return nil
}

type submitRecordedEventRequest struct {
	Type        string                           `json:"type"`
	Severity    string                           `json:"severity,omitempty"`
	TaskName    string                           `json:"taskName,omitempty"`
	SessionName string                           `json:"sessionName,omitempty"`
	AgentName   string                           `json:"agentName,omitempty"`
	ToolName    string                           `json:"toolName,omitempty"`
	ToolCallID  string                           `json:"toolCallID,omitempty"`
	Summary     string                           `json:"summary,omitempty"`
	Content     json.RawMessage                  `json:"content,omitempty"`
	ContentText string                           `json:"contentText,omitempty"`
	Truncation  *events.ExecutionEventTruncation `json:"truncation,omitempty"`
}

func executionEventEndpoint(controllerURL, namespace, taskName string) (string, error) {
	base, err := url.Parse(controllerURL)
	if err != nil {
		return "", err
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("controller URL must be absolute")
	}
	return url.JoinPath(
		base.String(),
		"internal", "v1", "events", namespace, events.ExecutionEventStreamTypeTask, taskName,
	)
}

func readServiceAccountToken(path string) string {
	if token := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken)); token != "" {
		return token
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the Kubernetes service-account token mount or a test override.
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
