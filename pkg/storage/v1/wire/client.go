package wire

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// DialOptions returns the options every storage client connection uses: the
// send-evidence handler, no configured retries (grpc-go keeps only its
// transparent retries of attempts the server never processed), no proxy,
// keepalive and the message size limits. Dial uses them. A connection built
// with them directly may also carry caller interceptors, so on it every
// transport failure of a write is an unknown outcome.
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
	stubs clientStubs
	// conn is the connection when the client calls a *grpc.ClientConn
	// directly; send evidence is trusted only on such a connection from Dial.
	conn           *grpc.ClientConn
	dataset        string
	serverRevision int
}

// DialConfig configures Dial. It takes no other gRPC options, so no caller
// interceptor can sit between a Client and grpc-go and send a call where its
// send evidence cannot see it.
type DialConfig struct {
	// TransportCredentials secure the connection. They are required, and
	// storage traffic uses TLS 1.3.
	TransportCredentials credentials.TransportCredentials
	// PerRPCCredentials, when set, authenticate each call, such as a bearer
	// token that requires transport security.
	PerRPCCredentials credentials.PerRPCCredentials
	// ContextDialer, when set, opens the network connection, such as an
	// in-memory listener in tests.
	ContextDialer func(ctx context.Context, address string) (net.Conn, error)
}

// Dial creates a storage client connection to target with DialOptions and
// config. Its only interceptor marks each call on this connection as covered
// by send evidence, so a write that sent nothing, including one that failed
// before any attempt, is reported as not sent.
func Dial(target string, config DialConfig) (*grpc.ClientConn, error) {
	if config.TransportCredentials == nil {
		return nil, errors.New("storage connections require transport credentials")
	}
	var own atomic.Pointer[grpc.ClientConn]
	opts := append(DialOptions(), grpc.WithTransportCredentials(config.TransportCredentials),
		grpc.WithChainUnaryInterceptor(coverEvidence(&own)))
	if config.PerRPCCredentials != nil {
		opts = append(opts, grpc.WithPerRPCCredentials(config.PerRPCCredentials))
	}
	if config.ContextDialer != nil {
		opts = append(opts, grpc.WithContextDialer(config.ContextDialer))
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, err
	}
	own.Store(conn)
	return conn, nil
}

// NewClient returns a Client over conn. Only when conn is a connection from
// Dial, passed directly rather than wrapped, can the client tell a request
// that was never sent from one that may have been; over any other connection
// a write's transport failure is an unknown outcome.
func NewClient(conn grpc.ClientConnInterface, config ClientConfig) *Client {
	revision := config.ServerRevision
	if revision == 0 {
		revision = ProtocolRevision
	}
	direct, _ := conn.(*grpc.ClientConn)
	return &Client{stubs: newClientStubs(conn), conn: direct, dataset: config.Dataset, serverRevision: revision}
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
	evidence := &sendEvidence{conn: c.conn}
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
