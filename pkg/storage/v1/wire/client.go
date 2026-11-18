package wire

import (
	"context"
	"errors"
	"fmt"
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
	info, err := fromWireProviderInfo(resp.GetProvider(), false)
	if err != nil {
		return storagev1.ProviderInfo{}, ServerInfo{}, c.responseError(&methodDescribe, err)
	}
	return info, serverInfoFromWire(resp.GetServer()), nil
}

// invoke checks and sends one call. Nothing is sent when the request is
// invalid, too large, or names an RPC the server's revision does not have.
// A write carries the WriteEnvelope from ctx; a read or an administrative
// call never carries one. Any call carries the TaskDataFence from ctx.
func (c *Client) invoke(ctx context.Context, m *method, req proto.Message, call func(context.Context, ...grpc.CallOption) error) error {
	if err := ctx.Err(); err != nil {
		// gRPC starts no attempt for a call whose context is already done.
		return fmt.Errorf("%w: %s was not sent: %w", storagev1.ErrUnavailable, m.name, err)
	}
	if m.since > c.serverRevision {
		return fmt.Errorf("%w: storage server protocol revision %d does not serve %s",
			storagev1.ErrIncompatible, c.serverRevision, m.name)
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
	if c.dataset != "" && !m.mayOmitDataset() {
		pairs = append(pairs, HeaderDataset, c.dataset)
	}
	if envelope, ok := storagev1.WriteEnvelopeFrom(ctx); ok && m.kind == storagev1.MethodWrite {
		encoded, err := toWireWriteEnvelope(envelope)
		if err != nil {
			return err
		}
		raw, err := marshalHeader(m, "write envelope", encoded)
		if err != nil {
			return err
		}
		pairs = append(pairs, HeaderEnvelope, raw)
	}
	if fence, ok := storagev1.TaskDataFenceFrom(ctx); ok {
		encoded, err := toWireTaskDataFence(fence)
		if err != nil {
			return err
		}
		raw, err := marshalHeader(m, "Task data fence", encoded)
		if err != nil {
			return err
		}
		pairs = append(pairs, HeaderTaskDataFence, raw)
	}
	ctx = metadata.AppendToOutgoingContext(ctx, pairs...)
	evidence := &sendEvidence{conn: c.conn}
	var served metadata.MD
	if err := call(withSendEvidence(ctx, evidence), grpc.Header(&served)); err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	return c.checkServed(m, served)
}

// checkServed refuses a response a bound client's call received from a
// dataset other than its own, which a server that checks the request's
// dataset header never sends. A write or an administrative call reached a
// server serving another dataset, so its outcome is unknown.
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
