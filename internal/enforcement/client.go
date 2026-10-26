// Package enforcement implements the provider-neutral pre-execution boundary.
package enforcement

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

const (
	MaxPolicyBytes = 512 << 10
	MaxFactsBytes  = 64 << 10
	MaxRPCBytes    = 2 << 20
)

// ProviderConfig is operator configuration, never workload-supplied routing.
// A node-local provider has a separately trusted endpoint for each node.
type ProviderConfig struct {
	Name            string `json:"name"`
	Endpoint        string `json:"endpoint"`
	ServerName      string `json:"serverName"`
	CAFile          string `json:"caFile"`
	CertificateFile string `json:"certificateFile"`
	KeyFile         string `json:"keyFile"`
}

type Client struct {
	name     string
	caDigest string
	conn     *grpc.ClientConn
	rpc      enforcementv1.EnforcementProviderClient
}

func NewClient(config ProviderConfig) (*Client, error) {
	if config.Name == "" || config.Endpoint == "" || config.ServerName == "" ||
		config.CAFile == "" || config.CertificateFile == "" || config.KeyFile == "" {
		return nil, errors.New("enforcement provider requires name, endpoint, server identity, CA and client certificate/key")
	}
	ca, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read enforcement provider trust: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("enforcement provider CA contains no certificates")
	}
	if _, err := tls.LoadX509KeyPair(config.CertificateFile, config.KeyFile); err != nil {
		return nil, fmt.Errorf("load enforcement client identity: %w", err)
	}
	conn, err := grpc.NewClient(config.Endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13,
			ServerName: config.ServerName,
			RootCAs:    roots,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				current, err := tls.LoadX509KeyPair(config.CertificateFile, config.KeyFile)
				if err != nil {
					return nil, fmt.Errorf("reload enforcement client identity: %w", err)
				}
				return &current, nil
			},
		})),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(MaxRPCBytes),
			grpc.MaxCallSendMsgSize(MaxRPCBytes),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect enforcement provider: %w", err)
	}
	caSum := sha256.Sum256(ca)
	return &Client{
		name: config.Name, caDigest: hex.EncodeToString(caSum[:]),
		conn: conn, rpc: enforcementv1.NewEnforcementProviderClient(conn),
	}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func digest(domain string, message proto.Message) (string, error) {
	value, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return "", fmt.Errorf("encode enforcement identity: %w", err)
	}
	sum := sha256.Sum256(append([]byte(domain+"\x00"), value...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func RequirementDigest(requirement *enforcementv1.Requirement) (string, error) {
	return digest("orka.enforcement.requirement/v1", requirement)
}

func TargetFactsDigest(facts *enforcementv1.TargetFacts) (string, error) {
	return digest("orka.enforcement.target-facts/v1", facts)
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		!strings.ContainsRune(value, '\x00')
}

func ValidateRequest(request *enforcementv1.EnsureRequest) error {
	if request == nil || !validText(request.BindingId, 128) {
		return errors.New("enforcement requires a bounded immutable binding ID")
	}
	if err := ValidateRequirement(request.Requirement); err != nil {
		return err
	}
	facts := request.TargetFacts
	if facts == nil || !validText(facts.Format, 256) || len(facts.Value) == 0 || len(facts.Value) > MaxFactsBytes {
		return errors.New("enforcement requires bounded versioned target facts")
	}
	return nil
}

func ValidateRequirement(requirement *enforcementv1.Requirement) error {
	if requirement == nil || !validText(requirement.Format, 256) ||
		requirement.Mode != enforcementv1.Requirement_MODE_ENFORCE ||
		len(requirement.Policy) == 0 || len(requirement.Policy) > MaxPolicyBytes ||
		len(requirement.RequiredCapabilities) == 0 || len(requirement.RequiredCapabilities) > 32 {
		return errors.New("enforcement requires a complete bounded enforce-mode requirement")
	}
	seen := make(map[string]bool, len(requirement.RequiredCapabilities))
	for _, capability := range requirement.RequiredCapabilities {
		if !validText(capability, 256) || seen[capability] {
			return errors.New("enforcement capabilities must be bounded, nonempty and unique")
		}
		seen[capability] = true
	}
	return nil
}

// ValidateResponse checks request binding, not a provider-independent kernel
// challenge. The authenticated operator-trusted provider owns physical readback.
func ValidateResponse(request *enforcementv1.EnsureRequest, response *enforcementv1.EnsureResponse) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	if response == nil || proto.Size(response) > MaxRPCBytes {
		return errors.New("missing or oversized enforcement response")
	}
	switch result := response.Result.(type) {
	case *enforcementv1.EnsureResponse_Pending:
		if result.Pending != nil && validText(result.Pending.Reason, 4096) {
			return nil
		}
	case *enforcementv1.EnsureResponse_Refused:
		if result.Refused != nil && validText(result.Refused.Reason, 4096) {
			return nil
		}
	case *enforcementv1.EnsureResponse_Enforced:
		proof := result.Enforced
		requirementDigest, err := RequirementDigest(request.Requirement)
		if err != nil {
			return err
		}
		factsDigest, err := TargetFactsDigest(request.TargetFacts)
		if err != nil {
			return err
		}
		if proof == nil || proof.BindingId != request.BindingId ||
			proof.RequirementDigest != requirementDigest ||
			proof.TargetFactsDigest != factsDigest {
			return errors.New("enforcement evidence does not match the immutable request")
		}
		if proof.ResolvedTarget == nil || !validText(proof.ResolvedTarget.Format, 256) ||
			len(proof.ResolvedTarget.Value) == 0 || len(proof.ResolvedTarget.Value) > MaxFactsBytes ||
			len(proof.Evidence) == 0 || len(proof.EnforcedCapabilities) == 0 ||
			len(proof.EnforcedCapabilities) > len(request.Requirement.RequiredCapabilities) {
			return errors.New("enforcement evidence lacks bounded physical target, readback or capabilities")
		}
		seen := make(map[string]bool, len(proof.EnforcedCapabilities))
		for _, capability := range proof.EnforcedCapabilities {
			if seen[capability] || !slices.Contains(request.Requirement.RequiredCapabilities, capability) {
				return errors.New("enforcement evidence contains duplicate or unrequested capabilities")
			}
			seen[capability] = true
		}
		return nil
	}
	return errors.New("enforcement response has no valid explicit result")
}

func (c *Client) Ensure(ctx context.Context, request *enforcementv1.EnsureRequest) (*enforcementv1.EnsureResponse, error) {
	if err := ValidateRequest(request); err != nil {
		return nil, err
	}
	response, err := c.rpc.EnsureEnforced(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("enforcement provider %s uncertain: %w", c.name, err)
	}
	if err := ValidateResponse(request, response); err != nil {
		return nil, fmt.Errorf("enforcement provider %s: %w", c.name, err)
	}
	return response, nil
}

// EnsureAll returns evidence only when every mandatory provider is enforced and
// their union covers the complete requirement. Pending/refused/RPC failures
// never fall back to another provider or produce execution authority.
func EnsureAll(ctx context.Context, providers []*Client, request *enforcementv1.EnsureRequest) ([]*enforcementv1.Enforced, *enforcementv1.EnsureResponse, error) {
	if err := ValidateRequest(request); err != nil {
		return nil, nil, err
	}
	if len(providers) == 0 {
		return nil, nil, errors.New("no operator-trusted enforcement providers configured")
	}
	var proofs []*enforcementv1.Enforced
	covered := make(map[string]bool)
	for _, provider := range providers {
		if provider == nil {
			return nil, nil, errors.New("missing mandatory enforcement provider")
		}
		response, err := provider.Ensure(ctx, request)
		if err != nil {
			return nil, nil, err
		}
		proof := response.GetEnforced()
		if proof == nil {
			return nil, response, nil
		}
		proofs = append(proofs, proof)
		for _, capability := range proof.EnforcedCapabilities {
			covered[capability] = true
		}
	}
	for _, capability := range request.Requirement.RequiredCapabilities {
		if !covered[capability] {
			return nil, &enforcementv1.EnsureResponse{
				Result: &enforcementv1.EnsureResponse_Refused{Refused: &enforcementv1.Refused{
					Reason: "mandatory providers did not cover the complete requirement",
					Needs:  []string{"mandatory-capability-coverage"},
				}},
			}, nil
		}
	}
	return proofs, nil, nil
}

func (c *Client) Retire(ctx context.Context, bindingID string) (*enforcementv1.RetireResponse, error) {
	if !validText(bindingID, 128) {
		return nil, errors.New("retirement requires the immutable binding ID")
	}
	response, err := c.rpc.RetireEnforcement(ctx, &enforcementv1.RetireRequest{BindingId: bindingID})
	if err != nil {
		return nil, fmt.Errorf("retire enforcement provider %s uncertain: %w", c.name, err)
	}
	if response == nil || proto.Size(response) > MaxRPCBytes {
		return nil, errors.New("missing or oversized retirement response")
	}
	switch result := response.Result.(type) {
	case *enforcementv1.RetireResponse_Retired:
		if result.Retired != nil && result.Retired.BindingId == bindingID {
			return response, nil
		}
	case *enforcementv1.RetireResponse_Pending:
		if result.Pending != nil && validText(result.Pending.Reason, 4096) {
			return response, nil
		}
	case *enforcementv1.RetireResponse_Refused:
		if result.Refused != nil && validText(result.Refused.Reason, 4096) {
			return response, nil
		}
	}
	return nil, errors.New("retirement response has no valid matching result")
}
