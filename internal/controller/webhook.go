/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/taskwebhook"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	urlSchemeHTTP      = "http"
	urlSchemeHTTPS     = "https"
	k8sServiceDNSLabel = "svc"
	k8sClusterDNSLabel = "cluster"
	k8sLocalDNSLabel   = "local"
	localhostHost      = "localhost"
)

// webhookPolicyError is a destination that policy rejects outright. Such a
// notice is failed without any network attempt and never retried.
type webhookPolicyError struct{ message string }

func (e *webhookPolicyError) Error() string { return e.message }

func webhookPolicyErrorf(format string, args ...any) error {
	return &webhookPolicyError{message: fmt.Sprintf(format, args...)}
}

// webhookTarget is a validated webhook destination. A same-namespace Service
// destination is pinned to the Service's ClusterIPs; any other destination is
// resolved at dial time and must be public.
type webhookTarget struct {
	url    *url.URL
	pinned []netip.Addr
}

var blockedWebhookHosts = []string{
	localhostHost,
	"169.254.169.254",
	"metadata.google.internal",
	"kubernetes.default",
	"kubernetes.default.svc",
	"kubernetes.default.svc.cluster.local",
}

// validateWebhookURLPolicy applies every check that needs no network: scheme,
// host shape, well-known internal names, literal IP addresses, and the
// same-namespace rule for cluster Service names. It returns the parsed URL and
// whether the host names a same-namespace Service.
func validateWebhookURLPolicy(rawURL, namespace string, policy taskwebhook.AddressPolicy) (*url.URL, bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, false, webhookPolicyErrorf("invalid webhook URL")
	}

	// Only allow http and https schemes
	if u.Scheme != urlSchemeHTTP && u.Scheme != urlSchemeHTTPS {
		return nil, false, webhookPolicyErrorf("webhook URL scheme %q not allowed, must be http or https", u.Scheme)
	}

	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, false, webhookPolicyErrorf("webhook URL must have a host")
	}

	if slices.Contains(blockedWebhookHosts, host) || strings.HasSuffix(host, "."+localhostHost) {
		return nil, false, webhookPolicyErrorf("webhook URL host %q is not allowed", host)
	}

	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		if policy == nil {
			policy = taskwebhook.PublicAddressPolicy
		}
		if address.Zone() != "" || !policy(address.Unmap()) {
			return nil, false, webhookPolicyErrorf("webhook URL host %q is not a public address", host)
		}
		return u, false, nil
	}

	if _, serviceNamespace, ok := parseClusterServiceHost(host); ok {
		if serviceNamespace == "" || serviceNamespace != namespace {
			return nil, false, webhookPolicyErrorf("webhook URL host %q is outside the task namespace", host)
		}
		return u, true, nil
	}

	if isClusterServiceHost(host) {
		return nil, false, webhookPolicyErrorf("webhook URL host %q is outside the task namespace", host)
	}

	return u, false, nil
}

// resolveWebhookTarget validates rawURL for a Task in namespace. Public names
// are resolved and checked at dial time, on every connection, so a DNS
// failure or a rebinding answer can never reach a non-public address.
func resolveWebhookTarget(ctx context.Context, kubeClient ctrlclient.Reader, rawURL, namespace string, policy taskwebhook.AddressPolicy) (*webhookTarget, error) {
	u, clusterService, err := validateWebhookURLPolicy(rawURL, namespace, policy)
	if err != nil {
		return nil, err
	}
	if !clusterService {
		return &webhookTarget{url: u}, nil
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	serviceName, serviceNamespace, _ := parseClusterServiceHost(host)
	service, err := validateClusterServiceWebhook(ctx, kubeClient, serviceName, serviceNamespace, host)
	if err != nil {
		return nil, err
	}
	pinned := make([]netip.Addr, 0, len(service.Spec.ClusterIPs)+1)
	for _, raw := range append([]string{service.Spec.ClusterIP}, service.Spec.ClusterIPs...) {
		address, parseErr := netip.ParseAddr(raw)
		if parseErr != nil || slices.Contains(pinned, address.Unmap()) {
			continue
		}
		pinned = append(pinned, address.Unmap())
	}
	if len(pinned) == 0 {
		return nil, fmt.Errorf("webhook URL host %q must reference a ClusterIP service", host)
	}
	return &webhookTarget{url: u, pinned: pinned}, nil
}

func validateClusterServiceWebhook(ctx context.Context, kubeClient ctrlclient.Reader, serviceName, serviceNamespace, host string) (*corev1.Service, error) {
	if kubeClient == nil {
		return nil, fmt.Errorf("webhook URL host %q cannot be validated without a Kubernetes client", host)
	}

	service := &corev1.Service{}
	if err := kubeClient.Get(ctx, ctrlclient.ObjectKey{Name: serviceName, Namespace: serviceNamespace}, service); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("webhook URL host %q does not reference an existing service", host)
		}
		return nil, fmt.Errorf("validating webhook service %q: %s", host, apierrors.ReasonForError(err))
	}

	if service.Spec.Type == corev1.ServiceTypeExternalName {
		return nil, fmt.Errorf("webhook URL host %q must not reference an ExternalName service", host)
	}
	if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
		return nil, fmt.Errorf("webhook URL host %q must reference a ClusterIP service", host)
	}
	if len(service.Spec.Selector) == 0 {
		return nil, fmt.Errorf("webhook URL host %q must reference a service with a selector", host)
	}

	return service, nil
}

func parseClusterServiceHost(host string) (serviceName, namespace string, ok bool) {
	host = strings.TrimSuffix(host, ".")
	parts := strings.Split(host, ".")
	switch len(parts) {
	case 3:
		if parts[2] != k8sServiceDNSLabel {
			return "", "", false
		}
		return parts[0], parts[1], true
	case 5:
		if parts[2] != k8sServiceDNSLabel || parts[3] != k8sClusterDNSLabel || parts[4] != k8sLocalDNSLabel {
			return "", "", false
		}
		return parts[0], parts[1], true
	default:
		return "", "", false
	}
}

func isClusterServiceHost(host string) bool {
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
}

// WebhookPayload is the payload sent to webhook URLs. Every attempt of one
// delivery sends byte-identical JSON, so its digest identifies the notice.
type WebhookPayload struct {
	DeliveryID     string                 `json:"deliveryId"`
	EventType      string                 `json:"eventType"`
	OccurredAt     string                 `json:"occurredAt"`
	TaskUID        string                 `json:"taskUID,omitempty"`
	TaskName       string                 `json:"taskName"`
	TaskNamespace  string                 `json:"taskNamespace"`
	Phase          corev1alpha1.TaskPhase `json:"phase"`
	Message        string                 `json:"message,omitempty"`
	StartTime      *string                `json:"startTime,omitempty"`
	CompletionTime *string                `json:"completionTime,omitempty"`
	Attempts       int32                  `json:"attempts"`
	ResultRef      *ResultRefPayload      `json:"resultRef,omitempty"`
	RuntimeType    string                 `json:"runtimeType,omitempty"`
}

// ResultRefPayload is the result reference in the webhook payload
type ResultRefPayload struct {
	Available bool `json:"available"`
}

// webhookNotice identifies one delivery: a stable ID, the event, and the
// exact body every attempt sends.
type webhookNotice struct {
	DeliveryID string
	EventType  string
	OccurredAt time.Time
	Body       []byte
	Digest     string
}

// webhookEventType maps a terminal phase to its notice event type.
func webhookEventType(phase corev1alpha1.TaskPhase) string {
	return "task." + strings.ToLower(string(phase))
}

// buildWebhookNotice renders the notice body for the Task's current state.
func buildWebhookNotice(task *corev1alpha1.Task, deliveryID, eventType string, occurredAt time.Time) (webhookNotice, error) {
	payload := WebhookPayload{
		DeliveryID:    deliveryID,
		EventType:     eventType,
		OccurredAt:    occurredAt.UTC().Format(time.RFC3339),
		TaskUID:       string(task.UID),
		TaskName:      task.Name,
		TaskNamespace: task.Namespace,
		Phase:         task.Status.Phase,
		Message:       task.Status.Message,
		Attempts:      task.Status.Attempts,
	}

	// Set RuntimeType for agent tasks
	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		payload.RuntimeType = string(task.Spec.Type)
	}

	if task.Status.StartTime != nil {
		startTime := task.Status.StartTime.UTC().Format(time.RFC3339)
		payload.StartTime = &startTime
	}

	if task.Status.CompletionTime != nil {
		completionTime := task.Status.CompletionTime.UTC().Format(time.RFC3339)
		payload.CompletionTime = &completionTime
	}

	if task.Status.ResultRef != nil {
		payload.ResultRef = &ResultRefPayload{
			Available: task.Status.ResultRef.Available,
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return webhookNotice{}, fmt.Errorf("failed to marshal webhook payload: %w", err)
	}
	return webhookNotice{
		DeliveryID: deliveryID,
		EventType:  eventType,
		OccurredAt: occurredAt,
		Body:       body,
		Digest:     taskwebhook.PayloadDigest(body),
	}, nil
}

// WebhookNotifier sends signed webhook notifications for task completion.
type WebhookNotifier struct {
	client     *http.Client
	kubeClient ctrlclient.Reader
	keys       taskwebhook.KeySource
	// policy admits literal IP destinations; dial-time checks use the same
	// policy through the client's dialer.
	policy taskwebhook.AddressPolicy
	now    func() time.Time
	newID  func() string

	maxAttempts    int32
	initialBackoff time.Duration
	maxBackoff     time.Duration
	attemptLease   time.Duration
}

const (
	defaultWebhookMaxAttempts    = 8
	defaultWebhookInitialBackoff = 10 * time.Second
	defaultWebhookMaxBackoff     = 15 * time.Minute
	// defaultWebhookAttemptLease must exceed the request timeout so an
	// in-flight attempt is never duplicated by a restarted controller.
	defaultWebhookAttemptLease  = 2 * time.Minute
	webhookSigningRetryInterval = 30 * time.Second
	maxWebhookLastErrorLength   = 256
)

// NewWebhookNotifier creates a WebhookNotifier that only connects to public
// addresses or validated same-namespace Services. Configure a key source with
// SetKeySource; without one, deliveries fail closed and are never sent unsigned.
func NewWebhookNotifier() *WebhookNotifier {
	return newWebhookNotifier(&taskwebhook.Dialer{Policy: taskwebhook.PublicAddressPolicy})
}

func newWebhookNotifier(dialer *taskwebhook.Dialer) *WebhookNotifier {
	return &WebhookNotifier{
		client:         taskwebhook.NewHTTPClient(dialer, taskwebhook.DefaultRequestTimeout),
		policy:         dialer.Policy,
		now:            time.Now,
		newID:          uuid.NewString,
		maxAttempts:    defaultWebhookMaxAttempts,
		initialBackoff: defaultWebhookInitialBackoff,
		maxBackoff:     defaultWebhookMaxBackoff,
		attemptLease:   defaultWebhookAttemptLease,
	}
}

// SetKubeClient configures the Kubernetes reader used for same-namespace
// service validation.
func (w *WebhookNotifier) SetKubeClient(kubeClient ctrlclient.Reader) {
	w.kubeClient = kubeClient
}

// SetKeySource configures the Ed25519 signing keys.
func (w *WebhookNotifier) SetKeySource(keys taskwebhook.KeySource) {
	w.keys = keys
}

func (w *WebhookNotifier) backoff(attempts int32) time.Duration {
	delay := w.initialBackoff
	for i := int32(1); i < attempts && delay < w.maxBackoff; i++ {
		delay *= 2
	}
	return min(delay, w.maxBackoff)
}

// webhookAttemptResult is the sanitized outcome of one attempt.
type webhookAttemptResult struct {
	StatusCode int32
	Err        error
}

// deliver sends one signed attempt of notice. It never returns the URL,
// signature, or transport internals in its error, so the error is safe to
// record in Task status.
func (w *WebhookNotifier) deliver(ctx context.Context, task *corev1alpha1.Task, notice webhookNotice, attempt int32, key *taskwebhook.SigningKey) webhookAttemptResult {
	target, err := resolveWebhookTarget(ctx, w.kubeClient, task.Spec.WebhookURL, task.Namespace, w.policy)
	if err != nil {
		return webhookAttemptResult{Err: err}
	}
	requestCtx := ctx
	if target.pinned != nil {
		requestCtx = taskwebhook.WithPinnedAddresses(ctx, target.pinned)
	}

	claims := taskwebhook.NewClaims(
		task.Spec.WebhookURL,
		taskwebhook.TaskClaim{UID: string(task.UID), Namespace: task.Namespace, Name: task.Name},
		notice.DeliveryID, notice.EventType, attempt, notice.Body, w.now(),
	)
	token, err := taskwebhook.Sign(key, claims)
	if err != nil {
		return webhookAttemptResult{Err: err}
	}

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.url.String(), bytes.NewReader(notice.Body))
	if err != nil {
		return webhookAttemptResult{Err: errors.New("failed to create webhook request")}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Orka-Controller/1.0")
	req.Header.Set("X-Orka-Task", task.Name)
	req.Header.Set("X-Orka-Namespace", task.Namespace)
	req.Header.Set(taskwebhook.HeaderEvent, notice.EventType)
	req.Header.Set(taskwebhook.HeaderDeliveryID, notice.DeliveryID)
	req.Header.Set(taskwebhook.HeaderIdempotencyKey, notice.DeliveryID)
	req.Header.Set(taskwebhook.HeaderAttempt, strconv.FormatInt(int64(attempt), 10))
	req.Header.Set(taskwebhook.HeaderSignature, token)

	resp, err := w.client.Do(req)
	if err != nil {
		return webhookAttemptResult{Err: sanitizeWebhookTransportError(err)}
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	code := int32(resp.StatusCode) //nolint:gosec // HTTP status codes fit in int32.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return webhookAttemptResult{StatusCode: code, Err: fmt.Errorf("webhook returned non-2xx status: %d", resp.StatusCode)}
	}
	return webhookAttemptResult{StatusCode: code}
}

// sanitizeWebhookTransportError maps a client error to a fixed description.
// url.Error embeds the request URL, which may carry credentials.
func sanitizeWebhookTransportError(err error) error {
	switch {
	case errors.Is(err, taskwebhook.ErrRedirectRefused):
		return taskwebhook.ErrRedirectRefused
	case errors.Is(err, taskwebhook.ErrAddressNotAllowed):
		return taskwebhook.ErrAddressNotAllowed
	case errors.Is(err, taskwebhook.ErrResolution):
		return taskwebhook.ErrResolution
	case errors.Is(err, context.Canceled):
		return errors.New("webhook request was cancelled")
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return errors.New("webhook request timed out")
	default:
		return errors.New("webhook connection failed")
	}
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func boundedWebhookError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > maxWebhookLastErrorLength {
		message = strings.ToValidUTF8(message[:maxWebhookLastErrorLength], "")
	}
	return message
}
