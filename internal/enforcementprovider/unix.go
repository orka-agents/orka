package enforcementprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

const maxTransportFrame = 1 << 20

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
	defer raw.Close()
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		return errors.New("provider transport is not Unix")
	}
	sys, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var sockErr error
	if err = sys.Control(func(fd uintptr) {
		cred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return sockErr
	}
	if cred.Uid != p.allowedUID {
		return fmt.Errorf("provider peer uid %d is not trusted", cred.Uid)
	}
	deadline := time.Now().Add(p.timeout)
	_ = conn.SetDeadline(deadline)
	req := requestFrame{Version: 1, ID: "1", Operation: operation, Parameters: parameters}
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
	if err = json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Version != 1 || resp.ID != "1" {
		return errors.New("provider response binding mismatch")
	}
	if !resp.OK {
		if resp.Error == nil {
			return errors.New("provider error")
		}
		return fmt.Errorf("provider %s: %s", resp.Error.Code, resp.Error.Message)
	}
	return json.Unmarshal(resp.Result, result)
}

type descriptorWire struct {
	ProviderID, EndpointID                 string
	ProtocolVersions, Capabilities         []string
	Epoch                                  uint64
	IssuedAt, ExpiresAt                    string
	KeyID, Algorithm, PublicKey, Signature string
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
	IssuedAt        float64     `json:"issuedAt"`
	NotBefore       float64     `json:"notBefore"`
	ExpiresAt       float64     `json:"expiresAt"`
	Epoch           uint64      `json:"epoch"`
}

func wireAuthorization(a Authorization) authorizationWire {
	c := a.Context
	return authorizationWire{a.ID, contextWire{c.RequestID, c.AttemptID, c.WorkloadID, c.ProviderID, c.Audience, c.Challenge, c.IdempotencyKey, c.PolicyDigest, base64.StdEncoding.EncodeToString(c.PolicyBytes), c.RequiredCapabilities}, float64(a.IssuedAt.UnixNano()) / 1e9, float64(a.NotBefore.UnixNano()) / 1e9, float64(a.ExpiresAt.UnixNano()) / 1e9, a.Epoch}
}
func (p *unixProvider) Prepare(r PrepareRequest) (string, error) {
	var x struct {
		PreparationID string `json:"preparationId"`
	}
	err := p.call("Prepare", map[string]any{"authorization": wireAuthorization(r.Authorization)}, &x)
	return x.PreparationID, err
}
func (p *unixProvider) Activate(r ActivateRequest) (string, error) {
	var x struct {
		Proof struct {
			ActivationID string `json:"activationId"`
		} `json:"proof"`
	}
	err := p.call("Activate", map[string]any{"authorization": wireAuthorization(r.Authorization), "preparationId": r.PreparationID}, &x)
	return x.Proof.ActivationID, err
}
func (p *unixProvider) ReadBack(id string) (ReadBack, error) {
	var x struct {
		ActivationID        string `json:"activationId"`
		TargetID            string `json:"targetId"`
		PolicyDigest        string `json:"policyDigest"`
		ConfigurationDigest string `json:"configurationDigest"`
	}
	err := p.call("ReadBack", map[string]any{"activationId": id}, &x)
	return ReadBack{x.ActivationID, x.TargetID, x.PolicyDigest, x.ConfigurationDigest}, err
}
func (p *unixProvider) GetProof(id string) (Proof, error) {
	var w struct {
		ID                  string `json:"proofId"`
		AuthorizationID     string `json:"authorizationId"`
		ActivationID        string `json:"activationId"`
		WorkloadID          string `json:"workloadId"`
		ProviderID          string `json:"providerId"`
		Audience            string `json:"audience"`
		Challenge           string `json:"challenge"`
		PolicyDigest        string `json:"policyDigest"`
		ReadBackDigest      string `json:"readBackDigest"`
		KeyID               string `json:"keyId"`
		Epoch               uint64 `json:"epoch"`
		IssuedAt, ExpiresAt time.Time
		Signature           string `json:"signature"`
	}
	err := p.call("GetProof", map[string]any{"activationId": id}, &w)
	if err != nil {
		return Proof{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(w.Signature)
	return Proof{w.ID, w.AuthorizationID, w.ActivationID, w.WorkloadID, w.ProviderID, w.Audience, w.Challenge, w.PolicyDigest, w.ReadBackDigest, w.KeyID, w.Epoch, w.IssuedAt, w.ExpiresAt, sig}, err
}
func (p *unixProvider) Release(r ReleaseRequest) error {
	var x any
	return p.call("Release", map[string]any{"activationId": r.ActivationID, "authorizationId": r.AuthorizationID, "proofId": r.ProofID, "releaseId": r.ReleaseID}, &x)
}
func (p *unixProvider) GetStatus(id string) (Status, error) {
	var x struct {
		State string `json:"state"`
	}
	err := p.call("GetStatus", map[string]any{"activationId": id}, &x)
	return Status{State: x.State}, err
}
func (p *unixProvider) Reconcile(r ReconcileRequest) (Status, error) {
	var x struct {
		State string `json:"state"`
	}
	err := p.call("Reconcile", map[string]any{"activationId": r.AttemptID}, &x)
	return Status{State: x.State}, err
}
func (p *unixProvider) Cleanup(r CleanupRequest) error {
	var x any
	return p.call("Cleanup", map[string]any{"activationId": r.ActivationID}, &x)
}
