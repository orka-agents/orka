package workspaceprovider

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/pkg/workspaceagent"
)

const (
	// ConnectionDataVersion is the versioned adapter-to-core connection data contract.
	ConnectionDataVersion = workspaceagent.ProtocolVersion

	ConnectionDataVersionKey       = "protocolVersion"
	ConnectionDataEndpointKey      = "endpoint"
	ConnectionDataCAKey            = "ca.crt"
	ConnectionDataHostHeaderKey    = "hostHeader"
	ConnectionDataControlAuthKey   = "controlAuth"
	ConnectionDataAllowInsecureKey = "allowInsecure"
)

// ConnectionData is the public, credential-bearing data stored in the Secret
// referenced by WorkspaceObservation.ConnectionRef. It must never be
// copied into status, logs, or events.
type ConnectionData struct {
	Endpoint      string
	CAData        []byte
	HostHeader    string
	ControlAuth   string
	AllowInsecure bool
}

// ClientConfig converts connection data to the workspace-agent client contract.
// Callers may set Timeout or HTTPClient on the returned value before constructing
// the client.
func (d ConnectionData) ClientConfig() workspaceagent.ClientConfig {
	return workspaceagent.ClientConfig{
		Endpoint:      strings.TrimSpace(d.Endpoint),
		CAData:        append([]byte(nil), d.CAData...),
		AllowInsecure: d.AllowInsecure,
		HostHeader:    strings.TrimSpace(d.HostHeader),
		ControlAuth:   strings.TrimSpace(d.ControlAuth),
	}
}

// EncodeConnectionData serializes the versioned Secret data map after applying
// the same endpoint and TLS validation used by the public client.
func EncodeConnectionData(data ConnectionData) (map[string][]byte, error) {
	if strings.TrimSpace(data.ControlAuth) == "" {
		return nil, fmt.Errorf("workspace connection control auth is required")
	}
	if _, err := workspaceagent.NewClient(data.ClientConfig()); err != nil {
		return nil, fmt.Errorf("validate workspace connection data: %w", err)
	}
	values := map[string][]byte{
		ConnectionDataVersionKey:     []byte(ConnectionDataVersion),
		ConnectionDataEndpointKey:    []byte(strings.TrimSpace(data.Endpoint)),
		ConnectionDataControlAuthKey: []byte(strings.TrimSpace(data.ControlAuth)),
	}
	if len(data.CAData) > 0 {
		values[ConnectionDataCAKey] = append([]byte(nil), data.CAData...)
	}
	if host := strings.TrimSpace(data.HostHeader); host != "" {
		values[ConnectionDataHostHeaderKey] = []byte(host)
	}
	if data.AllowInsecure {
		values[ConnectionDataAllowInsecureKey] = []byte(strconv.FormatBool(true))
	}
	return values, nil
}

// ParseConnectionData validates and decodes an adapter-owned connection Secret.
func ParseConnectionData(values map[string][]byte) (ConnectionData, error) {
	if strings.TrimSpace(string(values[ConnectionDataVersionKey])) != ConnectionDataVersion {
		return ConnectionData{}, fmt.Errorf("workspace connection protocol version is incompatible")
	}
	allowInsecure := false
	if raw := strings.TrimSpace(string(values[ConnectionDataAllowInsecureKey])); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return ConnectionData{}, fmt.Errorf("workspace connection allowInsecure is invalid")
		}
		allowInsecure = parsed
	}
	data := ConnectionData{
		Endpoint:      strings.TrimSpace(string(values[ConnectionDataEndpointKey])),
		CAData:        append([]byte(nil), values[ConnectionDataCAKey]...),
		HostHeader:    strings.TrimSpace(string(values[ConnectionDataHostHeaderKey])),
		ControlAuth:   strings.TrimSpace(string(values[ConnectionDataControlAuthKey])),
		AllowInsecure: allowInsecure,
	}
	if strings.TrimSpace(data.ControlAuth) == "" {
		return ConnectionData{}, fmt.Errorf("workspace connection control auth is required")
	}
	if _, err := workspaceagent.NewClient(data.ClientConfig()); err != nil {
		return ConnectionData{}, fmt.Errorf("validate workspace connection data: %w", err)
	}
	return data, nil
}
