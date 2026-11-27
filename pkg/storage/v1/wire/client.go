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

type ClientConfig struct {
	Dataset        string
	ServerRevision int
	Bulk           grpc.ClientConnInterface
	MaxBlobBytes   int64
	Limits         storagev1.ProviderLimits
}

type Client struct {
	stubs          clientStubs
	bulk           clientStubs
	conn           *grpc.ClientConn
	bulkConn       *grpc.ClientConn
	dataset        string
	serverRevision int
	maxBlobBytes   int64
	limits         storagev1.ProviderLimits
}

type DialConfig struct {
	TransportCredentials credentials.TransportCredentials
	PerRPCCredentials    credentials.PerRPCCredentials
	ContextDialer        func(ctx context.Context, address string) (net.Conn, error)
}

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

func (c *Client) prepare(ctx context.Context, m *method, req proto.Message, conn *grpc.ClientConn, limit int,
	extra ...string) (context.Context, *sendEvidence, error) {
	if err := ctx.Err(); err != nil {
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

func (c *Client) callError(ctx context.Context, m *method, err error, evidence *sendEvidence, served metadata.MD) error {
	if answersForDataset(err) {
		if mismatch := c.checkServed(m, served); mismatch != nil {
			return mismatch
		}
	}
	return decodeError(ctx, m, err, evidence)
}

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
	header, err := stream.Header()
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	declared, headerErr := parseBlobHeader(header)
	if headerErr == nil && declared.size > c.maxBlobBytes {
		return storagev1.TooLargeError{What: m.name + " response blob", Size: declared.size, Limit: c.maxBlobBytes}
	}
	if err := stream.RecvMsg(response); err != nil {
		if errors.Is(err, io.EOF) {
			return c.responseError(m, errors.New("the response stream is empty"))
		}
		return c.callError(ctx, m, err, evidence, header)
	}
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

func (c *Client) responseError(m *method, err error) error {
	if m.mayChange() {
		return fmt.Errorf("%w: %s response cannot be decoded: %v", storagev1.ErrOutcomeUnknown, m.name, err)
	}
	return fmt.Errorf("%w: %s response cannot be decoded: %v", storagev1.ErrIncompatible, m.name, err)
}

func missingWriteBack(param string) error {
	return fmt.Errorf("the response does not carry back %s", param)
}

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
