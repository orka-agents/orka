package wire

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// DialOptions returns the options every storage client connection uses: the
// send-evidence handler, no configured retries (grpc-go keeps only its
// transparent retries of attempts the server never processed), no proxy,
// keepalive and the message size limits. Callers add transport credentials,
// which must be TLS. Prefer Dial, which also lets a client report a write
// that failed before any attempt as not sent; on a connection built with
// these options alone, such a write is an unknown outcome.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithStatsHandler(NewEvidenceHandler()),
		grpc.WithDisableRetry(),
		grpc.WithNoProxy(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(MaxMessageBytes),
			grpc.MaxCallSendMsgSize(MaxMessageBytes),
		),
	}
}

// ClientConfig configures a Client.
type ClientConfig struct {
	// Dataset is the dataset ID every call names in its request header. It
	// is empty only for a client that has not yet bound a dataset.
	Dataset string
	// ServerRevision is the server's protocol revision, from its Describe.
	// The client calls an RPC, and sets a request field, only when the
	// server's revision is at least the one that introduced it. Zero means
	// this library's ProtocolRevision.
	ServerRevision int
}

// Client implements the orka.storage.v1 methods that have wire RPCs over a
// gRPC connection. It returns the contract's typed errors.
type Client struct {
	stubs          clientStubs
	dataset        string
	serverRevision int
}

// Dial creates a storage client connection to target with the caller's
// options, which must include TLS transport credentials, followed by
// DialOptions. It installs the evidence interceptor last, so it runs
// innermost, directly before grpc-go starts the call on this connection: a
// call it marks has no other interceptor left that could send it elsewhere,
// and a write that fails before any attempt is reported as not sent.
func Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	var own atomic.Pointer[grpc.ClientConn]
	all := append(append(append([]grpc.DialOption(nil), opts...), DialOptions()...),
		grpc.WithChainUnaryInterceptor(coverEvidence(&own)))
	conn, err := grpc.NewClient(target, all...)
	if err != nil {
		return nil, err
	}
	own.Store(conn)
	return conn, nil
}

// NewClient returns a Client over conn. The connection should come from Dial,
// so the client can tell a request that was never sent from one that may
// have been.
func NewClient(conn grpc.ClientConnInterface, config ClientConfig) *Client {
	revision := config.ServerRevision
	if revision == 0 {
		revision = ProtocolRevision
	}
	return &Client{stubs: newClientStubs(conn), dataset: config.Dataset, serverRevision: revision}
}

// DescribeServer returns the provider's description together with the
// server library's.
func (c *Client) DescribeServer(ctx context.Context) (storagev1.ProviderInfo, ServerInfo, error) {
	req := &storagepb.DescribeRequest{}
	var resp *storagepb.DescribeResponse
	if err := c.invoke(ctx, &methodDescribe, req, func(ctx context.Context, opts ...grpc.CallOption) error {
		var err error
		resp, err = c.stubs.dataset.Describe(ctx, req, opts...)
		return err
	}); err != nil {
		return storagev1.ProviderInfo{}, ServerInfo{}, err
	}
	info, err := fromWireProviderInfo(resp.GetProvider())
	if err != nil {
		return storagev1.ProviderInfo{}, ServerInfo{}, c.responseError(&methodDescribe, err)
	}
	return info, serverInfoFromWire(resp.GetServer()), nil
}

// invoke checks and sends one call. Nothing is sent when the request is
// invalid, too large, or names an RPC the server's revision does not have.
func (c *Client) invoke(ctx context.Context, m *method, req proto.Message, call func(context.Context, ...grpc.CallOption) error) error {
	if err := ctx.Err(); err != nil {
		// gRPC starts no attempt for a call whose context is already done.
		return fmt.Errorf("%w: %s was not sent: %w", storagev1.ErrUnavailable, m.name, err)
	}
	if m.since > c.serverRevision {
		return markedError{
			err:    storagev1.ValidationErrorf("storage server protocol revision %d does not serve %s", c.serverRevision, m.name),
			marker: ErrIncompatible,
		}
	}
	if field, invalid := firstInvalidString(req.ProtoReflect()); invalid {
		return storagev1.ValidationErrorf("%s request field %s is not valid UTF-8", m.name, field)
	}
	if size := proto.Size(req); size > MaxMessageBytes {
		return storagev1.TooLargeError{What: m.name + " request", Size: int64(size), Limit: MaxMessageBytes}
	}
	pairs := []string{
		HeaderContract, storagev1.ContractVersion,
		HeaderRevision, strconv.Itoa(ProtocolRevision),
	}
	if c.dataset != "" {
		pairs = append(pairs, HeaderDataset, c.dataset)
	}
	if envelope, ok := storagev1.WriteEnvelopeFrom(ctx); ok && m.kind == KindWrite {
		encoded, err := toWireWriteEnvelope(envelope)
		if err != nil {
			return err
		}
		raw, err := proto.Marshal(encoded)
		if err != nil {
			return storagev1.ValidationErrorf("%s write envelope cannot be encoded", m.name)
		}
		pairs = append(pairs, HeaderEnvelope, string(raw))
	}
	ctx = metadata.AppendToOutgoingContext(ctx, pairs...)
	evidence := &sendEvidence{}
	if err := call(withSendEvidence(ctx, evidence)); err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	return nil
}

// responseError reports a successful call whose response this client cannot
// decode. A write may have committed, so its outcome is unknown. The decode
// error is text only: it must not make the call look like a refused request.
func (c *Client) responseError(m *method, err error) error {
	if m.kind == KindWrite {
		return markedError{
			err:    fmt.Errorf("%w: %s response cannot be decoded: %v", storagev1.ErrOutcomeUnknown, m.name, err),
			marker: ErrIncompatible,
		}
	}
	return markedError{err: fmt.Errorf("%s response cannot be decoded: %v", m.name, err), marker: ErrIncompatible}
}
