/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// maxStatusBytes bounds an idle status document.
const maxStatusBytes = 64 << 10

// IdleStatus is the part of a host's idle status file Orka reads. copilotd's
// --idle-status-file format satisfies it; unknown fields are ignored.
type IdleStatus struct {
	Idle             bool       `json:"idle"`
	IdleSince        *time.Time `json:"idleSince,omitempty"`
	ActiveSessions   *int       `json:"activeSessions,omitempty"`
	BusySessions     *int       `json:"busySessions,omitempty"`
	BackgroundWork   *int       `json:"backgroundWork,omitempty"`
	ConnectedClients *int       `json:"connectedClients,omitempty"`
	UpdatedAt        *time.Time `json:"updatedAt,omitempty"`
	// Stale is set by the host agent when the host has not refreshed the file recently.
	Stale bool `json:"stale,omitempty"`
}

// ParseIdleStatus decodes an idle status document.
func ParseIdleStatus(raw []byte) (IdleStatus, error) {
	if len(raw) > maxStatusBytes {
		return IdleStatus{}, fmt.Errorf("idle status is larger than %d bytes", maxStatusBytes)
	}
	var status IdleStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return IdleStatus{}, fmt.Errorf("decode idle status: %w", err)
	}
	return status, nil
}

// StatusReader reads a running host's idle status from its host agent.
type StatusReader interface {
	ReadIdleStatus(ctx context.Context, podIP string) (IdleStatus, error)
}

// HTTPStatusReader reads the host agent's /status endpoint.
type HTTPStatusReader struct {
	Client *http.Client
}

// ReadIdleStatus implements StatusReader.
func (r HTTPStatusReader) ReadIdleStatus(ctx context.Context, podIP string) (IdleStatus, error) {
	if net.ParseIP(podIP) == nil {
		return IdleStatus{}, fmt.Errorf("pod IP %q is not an IP address", podIP)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	url := "http://" + net.JoinHostPort(podIP, strconv.Itoa(int(corev1alpha1.AgentHostStatusPort))) + "/status"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return IdleStatus{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return IdleStatus{}, fmt.Errorf("read host status: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return IdleStatus{}, fmt.Errorf("read host status: HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxStatusBytes+1))
	if err != nil {
		return IdleStatus{}, fmt.Errorf("read host status: %w", err)
	}
	return ParseIdleStatus(raw)
}
