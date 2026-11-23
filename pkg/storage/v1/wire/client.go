package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
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
	// Dataset is the dataset ID every call except Describe and HealthCheck
	// names in its request header. A client without one can call only those
	// two, which it uses to learn the dataset. A bound client refuses any
	// other call's response that does not come from its dataset; Describe
	// reports whichever dataset the server serves, as it does in process,
	// and the caller compares its DatasetID.
	Dataset string
	// ServerRevision is the server's protocol revision, from its Describe.
	// The client calls an RPC, and sets a request field, only when the
	// server's revision is at least the one that introduced it. Zero means
	// this library's ProtocolRevision.
	ServerRevision int
	// Bulk, when set, carries the streaming RPCs that move blobs, so a large
	// transfer has its own HTTP/2 flow-control window and cannot hold up
	// ordinary calls or health checks. Nil sends them over the main
	// connection.
	Bulk grpc.ClientConnInterface
	// MaxBlobBytes caps the blob the client sends or accepts in one call.
	// Zero means DefaultMaxBlobBytes.
	MaxBlobBytes int64
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
	conn           *grpc.ClientConn
	bulkConn       *grpc.ClientConn
	dataset        string
	serverRevision int
	maxBlobBytes   int64
	limits         storagev1.ProviderLimits
}

// DialConfig configures Dial. It takes no other gRPC options, so no caller
// interceptor can sit between a Client and grpc-go and send a call where its
// send evidence cannot see it.
type DialConfig struct {
	// TransportCredentials secure the connection. They are required; Dial
	// does not inspect them, so the caller configures TLS 1.3.
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
	bulk := config.Bulk
	if bulk == nil {
		bulk = conn
	}
	direct, _ := conn.(*grpc.ClientConn)
	bulkDirect, _ := bulk.(*grpc.ClientConn)
	return &Client{
		stubs: newClientStubs(conn), bulk: newClientStubs(bulk), conn: direct, bulkConn: bulkDirect,
		dataset: config.Dataset, serverRevision: revision, maxBlobBytes: maxBlob, limits: config.Limits,
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
	info, err := fromWireProviderInfo(resp.GetProvider(), false)
	if err != nil {
		return storagev1.ProviderInfo{}, ServerInfo{}, c.responseError(&methodDescribe, err)
	}
	return info, serverInfoFromWire(resp.GetServer()), nil
}

// prepare checks one call and returns the context to send it with: the
// request headers, extra headers and the call's send evidence for conn.
// Nothing is sent when the request is invalid, larger than limit, or names an
// RPC the server's revision does not have. A write carries the WriteEnvelope
// from ctx; a read or an administrative call never carries one. Any call
// carries the TaskDataFence from ctx.
func (c *Client) prepare(ctx context.Context, m *method, req proto.Message, conn *grpc.ClientConn, limit int,
	extra ...string) (context.Context, *sendEvidence, error) {
	if err := ctx.Err(); err != nil {
		// gRPC starts no attempt for a call whose context is already done.
		return nil, nil, fmt.Errorf("%w: %s was not sent: %w", storagev1.ErrUnavailable, m.name, err)
	}
	if m.since > c.serverRevision {
		return nil, nil, fmt.Errorf("%w: storage server protocol revision %d does not serve %s",
			storagev1.ErrIncompatible, c.serverRevision, m.name)
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
	if c.dataset != "" && !m.mayOmitDataset() {
		pairs = append(pairs, HeaderDataset, c.dataset)
	}
	if envelope, ok := storagev1.WriteEnvelopeFrom(ctx); ok && m.kind == storagev1.MethodWrite {
		// The server checks a write against the dataset the call names, so
		// an envelope for another dataset could commit there while the
		// response names the bound one.
		if c.dataset != "" && envelope.DatasetID != "" && envelope.DatasetID != c.dataset {
			return nil, nil, fmt.Errorf("%w: %s write envelope names a dataset other than the client's", storagev1.ErrDatasetMismatch, m.name)
		}
		encoded, err := toWireWriteEnvelope(envelope)
		if err != nil {
			return nil, nil, err
		}
		raw, err := marshalHeader(m, "write envelope", encoded)
		if err != nil {
			return nil, nil, err
		}
		pairs = append(pairs, HeaderEnvelope, raw)
	}
	if fence, ok := storagev1.TaskDataFenceFrom(ctx); ok {
		encoded, err := toWireTaskDataFence(fence)
		if err != nil {
			return nil, nil, err
		}
		raw, err := marshalHeader(m, "Task data fence", encoded)
		if err != nil {
			return nil, nil, err
		}
		pairs = append(pairs, HeaderTaskDataFence, raw)
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
	var served metadata.MD
	if err := call(callCtx, grpc.Header(&served)); err != nil {
		return c.callError(ctx, m, err, evidence, served)
	}
	return c.checkServed(m, served)
}

// callError decodes a failed call. A storage error that answers for the
// server's dataset is first checked against the client's dataset.
func (c *Client) callError(ctx context.Context, m *method, err error, evidence *sendEvidence, served metadata.MD) error {
	if answersForDataset(err) {
		if mismatch := c.checkServed(m, served); mismatch != nil {
			return mismatch
		}
	}
	return decodeError(ctx, m, err, evidence)
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
		served, _ := stream.Header()
		return c.callError(ctx, m, err, evidence, served)
	}
	served, err := stream.Header()
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	return c.checkServed(m, served)
}

// checkServed refuses a response, or a storage error that answers for the
// server's dataset, that a bound client's call received from a dataset other
// than its own, which a server that checks the request's dataset header never
// sends. A write or an administrative call reached a server serving another
// dataset, so its outcome is unknown.
func (c *Client) checkServed(m *method, served metadata.MD) error {
	if c.dataset == "" || m.mayOmitDataset() {
		return nil
	}
	if datasets := served.Get(HeaderDataset); len(datasets) == 1 && datasets[0] == c.dataset {
		return nil
	}
	if m.mayChange() {
		return fmt.Errorf("%w: %s was answered for a dataset other than %q", storagev1.ErrOutcomeUnknown, m.name, c.dataset)
	}
	return fmt.Errorf("%w: %s was answered for a dataset other than %q", storagev1.ErrDatasetMismatch, m.name, c.dataset)
}

// marshalHeader encodes a binary request header, checking it as the client
// checks a request.
func marshalHeader(m *method, what string, message proto.Message) (string, error) {
	if field, invalid := firstInvalidString(message.ProtoReflect()); invalid {
		return "", storagev1.ValidationErrorf("%s %s field %s is not valid UTF-8", m.name, what, field)
	}
	raw, err := proto.Marshal(message)
	if err != nil {
		return "", storagev1.ValidationErrorf("%s %s cannot be encoded", m.name, what)
	}
	return string(raw), nil
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
		return c.callError(ctx, m, err, evidence, header)
	}
	// The response came from the server's dataset only if its headers say
	// so; a refused call answers above instead.
	if err := c.checkServed(m, header); err != nil {
		return err
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
// decode. A write or an administrative call may have committed, so its
// outcome is unknown; a read's response came from a server this client does
// not understand. The decode error is text only: it must not make the call
// look like a refused request.
func (c *Client) responseError(m *method, err error) error {
	if m.mayChange() {
		return fmt.Errorf("%w: %s response cannot be decoded: %v", storagev1.ErrOutcomeUnknown, m.name, err)
	}
	return fmt.Errorf("%w: %s response cannot be decoded: %v", storagev1.ErrIncompatible, m.name, err)
}

// missingWriteBack reports a successful write whose response does not carry
// back the record the provider wrote into.
func missingWriteBack(param string) error {
	return fmt.Errorf("the response does not carry back %s", param)
}

// checkNewerParameter refuses, before anything is sent, a call that sets a
// parameter newer than the server's protocol revision, which that server
// would ignore. A zero value, or an empty list or map, is not sent anyway.
func (c *Client) checkNewerParameter(m *method, param string, since int, value any) error {
	if c.serverRevision >= since {
		return nil
	}
	v := reflect.ValueOf(value)
	switch {
	case !v.IsValid() || v.IsZero():
		return nil
	case (v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.Len() == 0:
		return nil
	}
	return fmt.Errorf("%w: storage server protocol revision %d does not take the %s parameter of %s",
		storagev1.ErrIncompatible, c.serverRevision, param, m.name)
}
