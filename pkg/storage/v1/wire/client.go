package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// is empty only for a client that has not yet bound a dataset; such a
	// client can call only Describe and HealthCheck.
	Dataset string
	// ServerRevision is the server's protocol revision, from its Describe.
	// The client calls an RPC, and sets a request field, only when the
	// server's revision is at least the one that introduced it. Zero means
	// this library's ProtocolRevision.
	ServerRevision int
	// Bulk, when set, carries the streaming RPCs that move blobs and batched
	// reads, so a large transfer has its own HTTP/2 flow-control window and
	// cannot hold up ordinary calls or health checks. Nil sends them over the
	// main connection.
	Bulk grpc.ClientConnInterface
	// MaxBlobBytes caps the blob the client sends or accepts in one call.
	// Zero means DefaultMaxBlobBytes.
	MaxBlobBytes int64
	// MaxSnapshotBytes caps the list the client accepts from one batched
	// read, such as a Session's transcript. Zero means
	// DefaultMaxSnapshotBytes.
	MaxSnapshotBytes int64
	// Limits are the provider's limits from its Describe. The client refuses
	// an artifact larger than Limits.MaxArtifactBytes without sending it.
	// Zero leaves that check to the server.
	Limits storagev1.ProviderLimits
}

// Client implements the orka.storage.v1 methods that have wire RPCs over a
// gRPC connection. It returns the contract's typed errors.
type Client struct {
	stubs clientStubs
	// bulk holds the stubs for streaming RPCs.
	bulk clientStubs
	// conn and bulkConn are the connections when the client calls a
	// *grpc.ClientConn directly; send evidence is trusted only on such a
	// connection from Dial.
	conn             *grpc.ClientConn
	bulkConn         *grpc.ClientConn
	dataset          string
	serverRevision   int
	maxBlobBytes     int64
	maxSnapshotBytes int64
	limits           storagev1.ProviderLimits
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
// config. Its only interceptors mark each call on this connection as covered
// by send evidence, so a write that sent nothing, including one that failed
// before any attempt, is reported as not sent.
func Dial(target string, config DialConfig) (*grpc.ClientConn, error) {
	if config.TransportCredentials == nil {
		return nil, errors.New("storage connections require transport credentials")
	}
	var own atomic.Pointer[grpc.ClientConn]
	opts := append(DialOptions(), grpc.WithTransportCredentials(config.TransportCredentials),
		grpc.WithChainUnaryInterceptor(coverEvidence(&own)), grpc.WithChainStreamInterceptor(coverStreamEvidence(&own)))
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

// NewClient returns a Client over conn. Only when conn (and config.Bulk, for
// streaming RPCs) is a connection from Dial, passed directly rather than
// wrapped, can the client tell a request that was never sent from one that
// may have been; over any other connection a write's transport failure is an
// unknown outcome.
func NewClient(conn grpc.ClientConnInterface, config ClientConfig) *Client {
	revision := config.ServerRevision
	if revision == 0 {
		revision = ProtocolRevision
	}
	maxBlob := config.MaxBlobBytes
	if maxBlob <= 0 {
		maxBlob = DefaultMaxBlobBytes
	}
	maxSnapshot := config.MaxSnapshotBytes
	if maxSnapshot <= 0 {
		maxSnapshot = DefaultMaxSnapshotBytes
	}
	bulk := config.Bulk
	if bulk == nil {
		bulk = conn
	}
	direct, _ := conn.(*grpc.ClientConn)
	bulkDirect, _ := bulk.(*grpc.ClientConn)
	return &Client{
		stubs: newClientStubs(conn), bulk: newClientStubs(bulk), conn: direct, bulkConn: bulkDirect,
		dataset: config.Dataset, serverRevision: revision, maxBlobBytes: maxBlob, maxSnapshotBytes: maxSnapshot,
		limits: config.Limits,
	}
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

// prepare checks one call and returns the context to send it with: the
// request headers, extra headers and the call's send evidence for conn.
// Nothing is sent when the request is invalid, larger than limit, or names an
// RPC the server's revision does not have.
func (c *Client) prepare(ctx context.Context, m *method, req proto.Message, conn *grpc.ClientConn, limit int,
	extra ...string) (context.Context, *sendEvidence, error) {
	if err := ctx.Err(); err != nil {
		// gRPC starts no attempt for a call whose context is already done.
		return nil, nil, fmt.Errorf("%w: %s was not sent: %w", storagev1.ErrUnavailable, m.name, err)
	}
	if m.since > c.serverRevision {
		return nil, nil, markedError{
			err:    storagev1.ValidationErrorf("storage server protocol revision %d does not serve %s", c.serverRevision, m.name),
			marker: ErrIncompatible,
		}
	}
	if field, invalid := firstInvalidString(req.ProtoReflect()); invalid {
		return nil, nil, storagev1.ValidationErrorf("%s request field %s is not valid UTF-8", m.name, field)
	}
	if size := proto.Size(req); size > limit {
		return nil, nil, storagev1.TooLargeError{What: m.name + " request", Size: int64(size), Limit: int64(limit)}
	}
	pairs := []string{
		HeaderContract, storagev1.ContractVersion,
		HeaderRevision, strconv.Itoa(ProtocolRevision),
	}
	if c.dataset != "" {
		pairs = append(pairs, HeaderDataset, c.dataset)
	}
	if envelope, ok := storagev1.WriteEnvelopeFrom(ctx); ok && m.takesEnvelope() {
		encoded, err := toWireWriteEnvelope(envelope)
		if err != nil {
			return nil, nil, err
		}
		raw, err := proto.Marshal(encoded)
		if err != nil {
			return nil, nil, storagev1.ValidationErrorf("%s write envelope cannot be encoded", m.name)
		}
		pairs = append(pairs, HeaderEnvelope, string(raw))
	}
	evidence := &sendEvidence{conn: conn}
	ctx = metadata.AppendToOutgoingContext(ctx, append(pairs, extra...)...)
	return withSendEvidence(ctx, evidence), evidence, nil
}

// invoke checks and sends one unary call.
func (c *Client) invoke(ctx context.Context, m *method, req proto.Message, call func(context.Context, ...grpc.CallOption) error) error {
	callCtx, evidence, err := c.prepare(ctx, m, req, c.conn, MaxMessageBytes)
	if err != nil {
		return err
	}
	if err := call(callCtx); err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	return nil
}

// upload sends a streaming write: request with its blob in parts on the
// bulk connection, then reads response. Nothing is sent when the blob is
// larger than the client accepts.
func (c *Client) upload(ctx context.Context, m *method, request, response proto.Message,
	open func(context.Context, ...grpc.CallOption) (grpc.ClientStream, error)) error {
	path := blobPathOf(request.ProtoReflect().Descriptor())
	data := takeBlob(request.ProtoReflect(), path)
	what, limit := m.name+" blob", c.maxBlobBytes
	if m.contract == storagev1.ArtifactsContract && c.limits.MaxArtifactBytes > 0 && c.limits.MaxArtifactBytes < limit {
		what, limit = "artifact", c.limits.MaxArtifactBytes
	}
	if int64(len(data)) > limit {
		return storagev1.TooLargeError{What: what, Size: int64(len(data)), Limit: limit}
	}
	callCtx, evidence, err := c.prepare(ctx, m, request, c.bulkConn, MaxMessageBytes-MaxChunkBytes, describeBlob(data).pairs()...)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(callCtx)
	defer cancel()
	stream, err := open(callCtx)
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	// io.EOF from a send means the server ended the call; its status comes
	// from the receive below.
	if err := sendBlob(stream.SendMsg, request, path, data); err != nil && !errors.Is(err, io.EOF) {
		return decodeError(ctx, m, err, evidence)
	}
	if err := stream.CloseSend(); err != nil && !errors.Is(err, io.EOF) {
		return decodeError(ctx, m, err, evidence)
	}
	if err := stream.RecvMsg(response); err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	return nil
}

// download sends a streaming read on the bulk connection and reassembles
// response with its blob. A blob larger than the client accepts is refused
// before it is read.
func (c *Client) download(ctx context.Context, m *method, request, response proto.Message,
	open func(context.Context, ...grpc.CallOption) (grpc.ClientStream, error)) error {
	callCtx, evidence, err := c.prepare(ctx, m, request, c.bulkConn, MaxMessageBytes)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(callCtx)
	defer cancel()
	stream, err := open(callCtx)
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	// The blob's size arrives in the response headers, before any of its
	// bytes, so an oversized blob is refused without reading it.
	header, err := stream.Header()
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	declared, headerErr := parseBlobHeader(header)
	if headerErr == nil && declared.size > c.maxBlobBytes {
		return storagev1.TooLargeError{What: m.name + " response blob", Size: declared.size, Limit: c.maxBlobBytes}
	}
	if err := stream.RecvMsg(response); err != nil {
		// A refused call carries its status instead of a response.
		if errors.Is(err, io.EOF) {
			return c.responseError(m, errors.New("the response stream is empty"))
		}
		return decodeError(ctx, m, err, evidence)
	}
	if headerErr != nil {
		return c.responseError(m, headerErr)
	}
	err = reassemble(response, func() (proto.Message, error) {
		part := response.ProtoReflect().New().Interface()
		return part, stream.RecvMsg(part)
	}, declared)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errBlobFraming), errors.Is(err, errBlobShort), errors.Is(err, errBlobDigest):
		return c.responseError(m, err)
	default:
		return decodeError(ctx, m, err, evidence)
	}
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
