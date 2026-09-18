// Package runtimefeedback is the controller-only client for Gatekeeper Runtime's
// bounded, container-scoped diagnostic service. It does not change enforcement.
package runtimefeedback

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	MaxResponseBytes = 128 << 10
	MaxEvents        = 128
	CaptureSeconds   = 600
	APIVersion       = "runtime.gatekeeper.sh/v1alpha1"
	ReportKind       = "RuntimeFeedbackReport"
	apiPath          = "/v1alpha1/runtime-feedback/"
)

const (
	Collecting  = "Collecting"
	Finalized   = "Finalized"
	Expired     = "Expired"
	Stale       = "Stale"
	Unavailable = "Unavailable"
	Completed   = "Completed"
	Cancelled   = "Cancelled"
)

// Workload is obtained from the trusted runtime fence and live Kubernetes Pod,
// never from an agent's tool arguments. ContainerID includes the CRI scheme.
type Workload struct {
	Namespace     string `json:"namespace"`
	PodName       string `json:"podName"`
	PodUID        string `json:"podUID"`
	ContainerName string `json:"containerName"`
	ContainerID   string `json:"containerID"`
	RestartCount  int32  `json:"restartCount"`
	Node          string `json:"node"`
}

type Query struct {
	RunID    string   `json:"runID"`
	Workload Workload `json:"workload"`
}

type Capture struct {
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	ExpiresAt time.Time  `json:"expiresAt"`
}

type Source struct {
	Name         string `json:"name"`
	AgentVersion string `json:"agentVersion"`
	Gadget       string `json:"gadget"`
	InstanceID   string `json:"instanceID"`
}

type Event struct {
	Timestamp          time.Time `json:"timestamp"`
	Decision           string    `json:"decision"`
	DecisionReason     string    `json:"decisionReason"`
	KernelEnforced     bool      `json:"kernelEnforced"`
	DestinationAddress string    `json:"destinationAddress"`
	DestinationPort    uint16    `json:"destinationPort"`
	Protocol           string    `json:"protocol"`
	PolicyUID          string    `json:"policyUID,omitempty"`
	PolicyGeneration   int64     `json:"policyGeneration,omitempty"`
	ActiveGeneration   uint64    `json:"activeGeneration"`
}

type Report struct {
	APIVersion       string            `json:"apiVersion"`
	Kind             string            `json:"kind"`
	RunID            string            `json:"runID"`
	Workload         Workload          `json:"workload"`
	Status           string            `json:"status"`
	SampledAt        time.Time         `json:"sampledAt"`
	Capture          *Capture          `json:"capture,omitempty"`
	Source           Source            `json:"source"`
	AttributionScope string            `json:"attributionScope"`
	Completeness     string            `json:"completeness"`
	Events           []Event           `json:"events"`
	DroppedEvents    uint64            `json:"droppedEvents"`
	Losses           map[string]uint64 `json:"losses"`
	Limitations      []string          `json:"limitations"`
	Explanation      string            `json:"explanation"`
}

// Service is deliberately separate from the agent-facing read-only tool:
// registration/completion belong to the trusted prompt lifecycle.
type Service interface {
	Register(context.Context, Query) error
	Report(context.Context, Query) (Report, error)
	Complete(context.Context, Query, string) error
}

type Config struct {
	URL          string
	NodeURLsFile string
	CAFile       string
	CertFile     string
	KeyFile      string
}

type Client struct {
	endpoint      string
	nodeEndpoints map[string]string
	http          *http.Client
}

func NewClient(config Config) (*Client, error) {
	endpoint, nodeEndpoints, err := config.endpoints()
	if err != nil {
		return nil, err
	}
	if config.CAFile == "" || config.CertFile == "" || config.KeyFile == "" {
		return nil, errors.New("runtime feedback requires CA, client certificate, and client key files")
	}
	ca, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, errors.New("read runtime feedback CA file")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("runtime feedback CA file is invalid")
	}
	certificate, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, errors.New("load runtime feedback client certificate")
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}},
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10, IdleConnTimeout: 30 * time.Second,
	}
	return &Client{endpoint: endpoint, nodeEndpoints: nodeEndpoints, http: &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Register(ctx context.Context, query Query) error {
	_, err := c.call(ctx, "runs", struct {
		Query
		DurationSeconds int `json:"durationSeconds"`
	}{query, CaptureSeconds}, query)
	return err
}

func (c *Client) Report(ctx context.Context, query Query) (Report, error) {
	return c.call(ctx, "report", query, query)
}

func (c *Client) Complete(ctx context.Context, query Query, reason string) error {
	if reason != Completed && reason != Cancelled {
		return errors.New("invalid runtime feedback completion reason")
	}
	_, err := c.call(ctx, "complete", struct {
		Query
		Reason string `json:"reason"`
	}{query, reason}, query)
	return err
}

func (c *Client) call(ctx context.Context, method string, input any, query Query) (Report, error) {
	endpoint := c.endpoint
	if c.nodeEndpoints != nil {
		var ok bool
		endpoint, ok = c.nodeEndpoints[query.Workload.Node]
		if !ok {
			return Report{}, errors.New("runtime feedback is unavailable for the execution node")
		}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return Report{}, errors.New("encode runtime feedback request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+apiPath+method, bytes.NewReader(body))
	if err != nil {
		return Report{}, errors.New("construct runtime feedback request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Report{}, errors.New("runtime feedback service is unavailable")
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return Report{}, errors.New("runtime feedback service rejected the execution binding")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(data) > MaxResponseBytes {
		return Report{}, errors.New("runtime feedback response exceeds bounds")
	}
	// Reject duplicates and aliases before decoding can collapse conflicting
	// bindings or evidence. Decode the original bytes to retain numeric checks.
	if !validReportJSON(data) {
		return Report{}, errors.New("runtime feedback response is invalid")
	}
	var report Report
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return Report{}, errors.New("runtime feedback response is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Report{}, errors.New("runtime feedback response has trailing data")
	}
	if err := report.Validate(query, time.Now().UTC()); err != nil {
		return Report{}, err
	}
	return report, nil
}

// Validate prevents stale/cross-execution reports from entering an ACP child.
// GKR owns evidence interpretation: absence never establishes permission or
// success, and the consumer never upgrades completeness or causal attribution.
func (r Report) Validate(query Query, now time.Time) error {
	if r.APIVersion != APIVersion || r.Kind != ReportKind || r.RunID != query.RunID || r.Workload != query.Workload ||
		r.AttributionScope != "Container" || len(r.Events) > MaxEvents ||
		(r.Completeness != "Unknown" && r.Completeness != "Partial") {
		return errors.New("runtime feedback response binding or schema is invalid")
	}
	switch r.Status {
	case Collecting, Finalized, Expired, Stale, Unavailable:
	default:
		return errors.New("runtime feedback status is invalid")
	}
	maxSampleAge := 2 * CaptureSeconds * time.Second
	if r.Status == Collecting {
		maxSampleAge = 30 * time.Second
	}
	if r.SampledAt.IsZero() || r.SampledAt.After(now.Add(5*time.Second)) || now.Sub(r.SampledAt) > maxSampleAge {
		return errors.New("runtime feedback response is stale")
	}
	if r.Status == Unavailable || r.Status == Stale {
		if len(r.Events) != 0 {
			return errors.New("runtime feedback unavailable evidence is not empty")
		}
		return nil
	}
	return r.validateCapture()
}

func (r Report) validateCapture() error {
	if r.Source.Name != "gkr-runtime-observer" || r.Source.InstanceID == "" || r.Capture == nil || r.Capture.StartedAt.IsZero() ||
		r.Capture.ExpiresAt.Before(r.Capture.StartedAt) || r.Capture.ExpiresAt.Sub(r.Capture.StartedAt) > CaptureSeconds*time.Second ||
		r.Capture.StartedAt.After(r.SampledAt) {
		return errors.New("runtime feedback capture metadata is invalid")
	}
	if r.Status == Collecting && (r.Capture.EndedAt != nil || r.Capture.ExpiresAt.Before(r.SampledAt)) {
		return errors.New("runtime feedback collecting window is invalid")
	}
	if r.Status != Collecting && (r.Capture.EndedAt == nil || r.Capture.EndedAt.Before(r.Capture.StartedAt) ||
		r.Capture.EndedAt.After(r.Capture.ExpiresAt) || r.Capture.EndedAt.After(r.SampledAt)) {
		return errors.New("runtime feedback ended window is invalid")
	}
	for _, event := range r.Events {
		if event.Timestamp.Before(r.Capture.StartedAt) || event.Timestamp.After(r.SampledAt) || event.Timestamp.After(r.Capture.ExpiresAt) ||
			(r.Capture.EndedAt != nil && event.Timestamp.After(*r.Capture.EndedAt)) || net.ParseIP(event.DestinationAddress) == nil {
			return fmt.Errorf("runtime feedback event is outside the capture boundary")
		}
	}
	return nil
}
