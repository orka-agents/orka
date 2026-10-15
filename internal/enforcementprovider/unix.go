package enforcementprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const (
	maxTransportFrame = 1 << 20
	activationIDField = "activationId"
)

var errTransportFrameTooLarge = errors.New("provider frame exceeds limit")

type unixProvider struct {
	path       string
	allowedUID uint32
	timeout    time.Duration
}

func NewUnixProvider(path string, allowedUID uint32, timeout time.Duration) Provider {
	return &unixProvider{path: path, allowedUID: allowedUID, timeout: timeout}
}

type requestFrame struct {
	Version    int    `json:"version"`
	ID         string `json:"id"`
	Operation  string `json:"operation"`
	Parameters any    `json:"parameters"`
}
type responseFrame struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readTransportFrame(reader io.Reader) ([]byte, error) {
	line, err := bufio.NewReaderSize(reader, maxTransportFrame+1).ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxTransportFrame {
		return nil, errTransportFrameTooLarge
	}
	if err != nil {
		return nil, err
	}
	return line, nil
}

type boundedFrameBuffer struct {
	bytes.Buffer
}

func (b *boundedFrameBuffer) Write(data []byte) (int, error) {
	if len(data) > maxTransportFrame-b.Len() {
		return 0, errTransportFrameTooLarge
	}
	return b.Buffer.Write(data)
}

func encodeTransportFrame(value any) ([]byte, error) {
	var buffer boundedFrameBuffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		if errors.Is(err, errTransportFrameTooLarge) {
			return nil, errTransportFrameTooLarge
		}
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (p *unixProvider) call(operation string, parameters, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	d := net.Dialer{}
	raw, err := d.DialContext(ctx, "unix", p.path)
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		return errors.New("provider transport is not Unix")
	}
	if err = authenticateUnixPeer(conn, p.allowedUID); err != nil {
		return err
	}
	deadline := time.Now().Add(p.timeout)
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	req := requestFrame{Version: transportFrameVersion, ID: "1", Operation: operation, Parameters: parameters}
	frame, err := encodeTransportFrame(req)
	if err != nil {
		return err
	}
	if _, err = io.Copy(conn, bytes.NewReader(frame)); err != nil {
		return err
	}
	line, err := readTransportFrame(conn)
	if err != nil {
		return err
	}
	var resp responseFrame
	if err = rejectDuplicateJSONKeys(json.NewDecoder(bytes.NewReader(line))); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&resp); err != nil {
		return err
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("provider response has trailing data")
	}
	if resp.Version != transportFrameVersion || resp.ID != "1" {
		return errors.New("provider response binding mismatch")
	}
	if !resp.OK {
		if resp.Error == nil {
			return errors.New("provider error")
		}
		return &ProviderError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if len(resp.Result) == 0 {
		return errors.New("provider response is missing result")
	}
	if err = strictDecodeJSON(resp.Result, result); err != nil {
		return errors.New("decode provider result: " + err.Error())
	}
	return nil
}

type descriptorWire struct {
	ProviderID       string   `json:"providerId"`
	EndpointID       string   `json:"endpointId"`
	ProtocolVersions []string `json:"protocolVersions"`
	Capabilities     []string `json:"capabilities"`
	Epoch            uint64   `json:"epoch"`
	IssuedAt         string   `json:"issuedAt"`
	ExpiresAt        string   `json:"expiresAt"`
	KeyID            string   `json:"keyId"`
	Algorithm        string   `json:"algorithm"`
	PublicKey        string   `json:"publicKey"`
	Signature        string   `json:"signature"`
}

func (p *unixProvider) DiscoverDescriptor() (Descriptor, error) {
	var w descriptorWire
	err := p.call("DiscoverDescriptor", map[string]any{}, &w)
	if err != nil {
		return Descriptor{}, err
	}
	pub, err := base64.StdEncoding.DecodeString(w.PublicKey)
	if err != nil {
		return Descriptor{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(w.Signature)
	if err != nil {
		return Descriptor{}, err
	}
	issued, err := time.Parse(time.RFC3339Nano, w.IssuedAt)
	if err != nil {
		return Descriptor{}, err
	}
	expires, err := time.Parse(time.RFC3339Nano, w.ExpiresAt)
	if err != nil {
		return Descriptor{}, err
	}
	return Descriptor{ProviderID: w.ProviderID, EndpointID: w.EndpointID, Algorithm: w.Algorithm, ProtocolVersions: w.ProtocolVersions, Capabilities: w.Capabilities, Epoch: w.Epoch, IssuedAt: issued, ExpiresAt: expires, KeyID: w.KeyID, PublicKey: pub, Signature: sig}, nil
}

type contextWire struct {
	RequestID            string   `json:"requestId"`
	AttemptID            string   `json:"attemptId"`
	WorkloadID           string   `json:"workloadId"`
	ProviderID           string   `json:"providerId"`
	Audience             string   `json:"audience"`
	Challenge            string   `json:"challenge"`
	IdempotencyKey       string   `json:"idempotencyKey"`
	PolicyDigest         string   `json:"policyDigest"`
	PolicyBytes          string   `json:"policyBytes"`
	RequiredCapabilities []string `json:"requiredCapabilities"`
}
type authorizationWire struct {
	AuthorizationID string      `json:"authorizationId"`
	Context         contextWire `json:"context"`
	IssuedAt        string      `json:"issuedAt"`
	NotBefore       string      `json:"notBefore"`
	ExpiresAt       string      `json:"expiresAt"`
	Epoch           uint64      `json:"epoch"`
}

func wireAuthorization(a Authorization) authorizationWire {
	c := a.Context
	return authorizationWire{a.ID, contextWire{c.RequestID, c.AttemptID, c.WorkloadID, c.ProviderID, c.Audience, c.Challenge, c.IdempotencyKey, c.PolicyDigest, base64.StdEncoding.EncodeToString(c.PolicyBytes), cloneStrings(c.RequiredCapabilities)}, a.IssuedAt.UTC().Format(time.RFC3339Nano), a.NotBefore.UTC().Format(time.RFC3339Nano), a.ExpiresAt.UTC().Format(time.RFC3339Nano), a.Epoch}
}

type preparationResultWire struct {
	PreparationID string `json:"preparationId"`
}

type activationResultWire struct {
	Proof struct {
		ActivationID string `json:"activationId"`
	} `json:"proof"`
}

type readBackResultWire struct {
	ActivationID        string `json:"activationId"`
	TargetID            string `json:"targetId"`
	PolicyDigest        string `json:"policyDigest"`
	ConfigurationDigest string `json:"configurationDigest"`
}

type proofResultWire struct {
	ID                  string    `json:"proofId"`
	AuthorizationID     string    `json:"authorizationId"`
	ActivationID        string    `json:"activationId"`
	WorkloadID          string    `json:"workloadId"`
	ProviderID          string    `json:"providerId"`
	Audience            string    `json:"audience"`
	Challenge           string    `json:"challenge"`
	PolicyDigest        string    `json:"policyDigest"`
	ReadBackDigest      string    `json:"readBackDigest"`
	AuthorizationDigest string    `json:"authorizationDigest"`
	KeyID               string    `json:"keyId"`
	Epoch               uint64    `json:"epoch"`
	IssuedAt            time.Time `json:"issuedAt"`
	ExpiresAt           time.Time `json:"expiresAt"`
	Signature           string    `json:"signature"`
}

type statusResultWire struct {
	State string `json:"state"`
}

type emptyResultWire struct{}

func (p *unixProvider) Prepare(r PrepareRequest) (string, error) {
	var x preparationResultWire
	err := p.call("Prepare", map[string]any{"authorization": wireAuthorization(r.Authorization)}, &x)
	return x.PreparationID, err
}
func (p *unixProvider) Activate(r ActivateRequest) (string, error) {
	var x activationResultWire
	err := p.call("Activate", map[string]any{"authorization": wireAuthorization(r.Authorization), "preparationId": r.PreparationID}, &x)
	return x.Proof.ActivationID, err
}
func (p *unixProvider) ReadBack(id string) (ReadBack, error) {
	var x readBackResultWire
	err := p.call("ReadBack", map[string]any{activationIDField: id}, &x)
	return ReadBack{x.ActivationID, x.TargetID, x.PolicyDigest, x.ConfigurationDigest}, err
}
func (p *unixProvider) GetProof(id string) (Proof, error) {
	var w proofResultWire
	err := p.call("GetProof", map[string]any{activationIDField: id}, &w)
	if err != nil {
		return Proof{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(w.Signature)
	return Proof{ID: w.ID, AuthorizationID: w.AuthorizationID, ActivationID: w.ActivationID, WorkloadID: w.WorkloadID, ProviderID: w.ProviderID, Audience: w.Audience, Challenge: w.Challenge, PolicyDigest: w.PolicyDigest, ReadBackDigest: w.ReadBackDigest, AuthorizationDigest: w.AuthorizationDigest, KeyID: w.KeyID, Epoch: w.Epoch, IssuedAt: w.IssuedAt, ExpiresAt: w.ExpiresAt, Signature: sig}, err
}
func (p *unixProvider) Release(r ReleaseRequest) error {
	var x emptyResultWire
	return p.call("Release", map[string]any{activationIDField: r.ActivationID, "authorizationId": r.AuthorizationID, "proofId": r.ProofID, "releaseId": r.ReleaseID}, &x)
}
func (p *unixProvider) GetStatus(id string) (Status, error) {
	var x statusResultWire
	err := p.call("GetStatus", map[string]any{activationIDField: id}, &x)
	return Status{State: x.State}, err
}
func (p *unixProvider) Reconcile(r ReconcileRequest) (Status, error) {
	var x statusResultWire
	err := p.call("Reconcile", map[string]any{activationIDField: r.AttemptID}, &x)
	return Status{State: x.State}, err
}
func (p *unixProvider) Cleanup(r CleanupRequest) error {
	var x emptyResultWire
	return p.call("Cleanup", map[string]any{"attemptId": r.AttemptID, activationIDField: r.ActivationID, "cleanupId": r.CleanupID}, &x)
}
