package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
)

// TaskGrant is nonsecret authority frozen only from a UID-linked durable event.
// Tools and Deadline are narrowed by the controller before snapshot persistence.
type TaskGrant struct {
	GrantRef     string           `json:"grantRef"`
	NamespaceUID string           `json:"namespaceUID"`
	GatewayUID   string           `json:"gatewayUID"`
	Endpoint     string           `json:"endpoint"`
	ServiceRef   bool             `json:"serviceRef,omitempty"`
	Deadline     time.Time        `json:"deadline"`
	Tools        []string         `json:"tools"`
	Parent       *TaskGrantParent `json:"parent,omitempty"`
}

// TaskGrantParent pins controller-protected delegation to a frozen snapshot.
// It contains no credential or provider identity.
type TaskGrantParent struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	UID            string `json:"uid"`
	SnapshotDigest string `json:"snapshotDigest"`
}

// ResolveTaskGrant never trusts Task markers or a shared Session as authority.
func (s *Service) ResolveTaskGrant(ctx context.Context, task *corev1alpha1.Task) (*TaskGrant, error) {
	event, err := s.resolveReplyOrigin(ctx, task)
	if err != nil || event == nil {
		return nil, err
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent || event.EventType != protocol.EventTypeDelegatedText {
		return nil, nil
	}
	return s.taskGrantFromEvent(ctx, event)
}

// ValidateParentTaskGrantOrigin checks only retained root origin. The controller
// must verify that this is a live ancestor (Running or successfully parked for
// the exact child), with its original immutable binding, before calling it.
// This does not reopen root reply or terminal Task tool authority.
func (s *Service) ValidateParentTaskGrantOrigin(ctx context.Context, task *corev1alpha1.Task, frozen *TaskGrant) error {
	if s == nil || !s.Config.Enabled || s.EventStore == nil || frozen == nil || task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return errTaskPass
	}
	event, err := s.EventStore.GetGatewayEventForTask(ctx, task.Namespace, task.Name, string(task.UID))
	if err != nil || event == nil || event.TaskUID != string(task.UID) || !gatewayTaskCorrelatesWithEvent(task, event) || event.EventType != protocol.EventTypeDelegatedText || !event.ExpiresAt.After(time.Now()) ||
		(event.State != store.GatewayEventTaskCreated && event.State != store.GatewayEventCompleted) {
		return errTaskPass
	}
	origin, err := s.taskGrantFromEvent(ctx, event)
	if err != nil || !taskGrantMatchesOrigin(frozen, origin) {
		return errTaskPass
	}
	return nil
}

func taskGrantMatchesOrigin(frozen, origin *TaskGrant) bool {
	return frozen != nil && origin != nil && origin.GrantRef != "" && origin.GrantRef == frozen.GrantRef && origin.NamespaceUID == frozen.NamespaceUID && origin.GatewayUID == frozen.GatewayUID && origin.Endpoint == frozen.Endpoint && origin.ServiceRef == frozen.ServiceRef && !frozen.Deadline.After(origin.Deadline)
}

func (s *Service) taskGrantFromEvent(ctx context.Context, event *store.GatewayEvent) (*TaskGrant, error) {
	object, err := s.liveMessageGatewayIdentity(ctx, event)
	if err != nil {
		return nil, err
	}
	endpoint, _, err := (EndpointResolver{Client: s.freshReader(), AllowInsecureLoopback: s.Config.AllowInsecureLoopback}).Resolve(ctx, object)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	// Pass endpoints are fixed at the authenticated gateway's origin.
	return &TaskGrant{GrantRef: event.Metadata[protocol.GatewayGrantRefKey], NamespaceUID: event.NamespaceUID, GatewayUID: event.GatewayUID, Endpoint: parsed.Scheme + "://" + parsed.Host, ServiceRef: object.Spec.Adapter.ServiceRef != nil, Deadline: event.ExpiresAt}, nil
}

var errTaskPass = errors.New("gateway task pass is unavailable")

// TaskPassCredential pins the operator's credential to its trusted HTTPS origin.
// A namespace editor cannot retarget it by editing the same Gateway's spec.
type TaskPassCredential struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

// LoadTaskPassCredentials reads an operator-mounted Gateway UID to credential map.
// Values never enter Kubernetes objects or errors.
func LoadTaskPassCredentials(path string) (map[string]TaskPassCredential, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errTaskPass
	}
	var credentials map[string]TaskPassCredential
	if len(data) > 1<<20 || json.Unmarshal(data, &credentials) != nil || len(credentials) == 0 {
		return nil, errTaskPass
	}
	for uid, credential := range credentials {
		endpoint, err := validateAbsoluteEndpoint(credential.Endpoint)
		if strings.TrimSpace(uid) == "" || !validTaskPass(credential.Token) || err != nil || endpoint.Scheme != "https" || credential.Endpoint != endpoint.Scheme+"://"+endpoint.Host {
			return nil, errTaskPass
		}
	}
	return credentials, nil
}

func validTaskPass(token string) bool {
	if len(token) == 0 || len(token) > protocol.MaxTaskPassBytes {
		return false
	}
	for _, c := range token {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}

// Issued passes use a JSON-stable bearer alphabet so standard response encoding
// cannot hide the literal token from the executor's existing redaction.
func validIssuedTaskPass(token string) bool {
	if !validTaskPass(token) {
		return false
	}
	for _, c := range token {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("-._~+=", c) {
			return false
		}
	}
	return true
}

// ValidateTaskGrant rechecks Task liveness and durable origin immediately before
// both issuance and tool send. Children and unbound Tasks have no authority.
func (s *Service) ValidateTaskGrant(ctx context.Context, task *corev1alpha1.Task, frozen *TaskGrant, tool string) error {
	if s == nil || frozen == nil || task == nil || !frozen.Deadline.After(time.Now()) || !slices.Contains(frozen.Tools, tool) {
		return errTaskPass
	}
	live := &corev1alpha1.Task{}
	if s.freshReader().Get(ctx, client.ObjectKeyFromObject(task), live) != nil || live.UID != task.UID || live.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return errTaskPass
	}
	origin, err := s.ResolveTaskGrant(ctx, live)
	if err != nil || !taskGrantMatchesOrigin(frozen, origin) {
		return errTaskPass
	}
	return nil
}

func taskGrantParentUID(grant *TaskGrant) string {
	if grant.Parent == nil {
		return ""
	}
	return grant.Parent.UID
}

// TaskPass acquires one bounded pass, never retries issuance or persists tokens.
// The controller's required callback revalidates the complete live chain.
func (s *Service) TaskPass(ctx context.Context, task *corev1alpha1.Task, frozen *TaskGrant, validate func(context.Context) error) (string, error) {
	if validate == nil || validate(ctx) != nil {
		return "", errTaskPass
	}
	credential := s.TaskPassCredentials[frozen.GatewayUID]
	if credential.Endpoint != frozen.Endpoint || !validTaskPass(credential.Token) {
		return "", errTaskPass
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(struct {
		ProtocolVersion string    `json:"protocolVersion"`
		GrantRef        string    `json:"grantRef"`
		NamespaceUID    string    `json:"namespaceUID"`
		GatewayUID      string    `json:"gatewayUID"`
		TaskUID         string    `json:"taskUID"`
		ParentTaskUID   string    `json:"parentTaskUID,omitempty"`
		Tools           []string  `json:"tools"`
		Deadline        time.Time `json:"deadline"`
	}{ProtocolVersion: protocol.Version, GrantRef: frozen.GrantRef, NamespaceUID: frozen.NamespaceUID, GatewayUID: frozen.GatewayUID, TaskUID: string(task.UID), ParentTaskUID: taskGrantParentUID(frozen), Tools: frozen.Tools, Deadline: frozen.Deadline})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, frozen.Endpoint+protocol.TaskPassPath, bytes.NewReader(body))
	if err != nil {
		return "", errTaskPass
	}
	req.Header.Set("Authorization", "Bearer "+credential.Token)
	req.Header.Set("Content-Type", "application/json")
	httpClient, err := NewAdapterHTTPClient(s.HTTPClient, 30*time.Second, frozen.ServiceRef, s.Config.AllowInsecureLoopback)
	if err != nil {
		return "", errTaskPass
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", errTaskPass
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", errTaskPass
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxTaskPassResponseBytes+1))
	if err != nil || len(data) > protocol.MaxTaskPassResponseBytes {
		return "", errTaskPass
	}
	var reply struct {
		Pass      string    `json:"pass"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if json.Unmarshal(data, &reply) != nil || !validIssuedTaskPass(reply.Pass) || !reply.ExpiresAt.After(time.Now()) || reply.ExpiresAt.After(time.Now().Add(protocol.MaxTaskPassTTL)) || reply.ExpiresAt.After(frozen.Deadline) || validate(ctx) != nil {
		return "", errTaskPass
	}
	return reply.Pass, nil
}
