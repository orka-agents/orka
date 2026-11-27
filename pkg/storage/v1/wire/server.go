package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
	"google.golang.org/protobuf/proto"
)

type Authenticator interface {
	Authenticate(ctx context.Context, fullMethod string, header metadata.MD) error
}

type Limits struct {
	MaxConcurrent map[storagev1.CallClass]int
	MaxDuration   map[storagev1.CallClass]time.Duration
}

type CallInfo struct {
	Method   string
	Kind     storagev1.MethodKind
	Class    storagev1.CallClass
	Code     codes.Code
	Reason   string
	Duration time.Duration
}

type CallObserver interface {
	ObserveCall(CallInfo)
}

type ServerConfig struct {
	State         storagev1.StateProvider
	Artifacts     storagev1.ArtifactProvider
	Info          ServerInfo
	Authenticator Authenticator
	Logger        *slog.Logger
	Limits        Limits
	MaxBlobBytes  int64
	Observer      CallObserver
}

type Server struct {
	state         storagev1.StateProvider
	artifacts     storagev1.ArtifactProvider
	dataset       datasetProvider
	info          ServerInfo
	authenticator Authenticator
	logger        *slog.Logger
	observer      CallObserver
	maxDuration   map[storagev1.CallClass]time.Duration
	admission     map[storagev1.CallClass]chan struct{}
	maxBlobBytes  int64

	datasetID     atomic.Pointer[string]
	artifactLimit atomic.Int64
	describeSeq   atomic.Uint64
	publishMu     sync.Mutex
	published     uint64
}

func NewServer(config ServerConfig) (*Server, error) {
	if config.State == nil && config.Artifacts == nil {
		return nil, errors.New("storage server requires a state or artifacts provider")
	}
	info := config.Info
	info.ProtocolRevision = ProtocolRevision
	info.LibraryVersion = libraryVersion()
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBlob := config.MaxBlobBytes
	if maxBlob <= 0 {
		maxBlob = DefaultMaxBlobBytes
	}
	if config.State != nil && maxBlob < storagev1.MaxResultBytes {
		return nil, fmt.Errorf("storage server blob limit of %d bytes is below the state contract's %d byte result limit",
			maxBlob, storagev1.MaxResultBytes)
	}
	if config.Artifacts != nil && maxBlob < storagev1.MaxArtifactBytes {
		return nil, fmt.Errorf("storage server blob limit of %d bytes is below the artifacts contract's %d byte artifact limit",
			maxBlob, storagev1.MaxArtifactBytes)
	}
	s := &Server{
		state: config.State, artifacts: config.Artifacts, info: info, authenticator: config.Authenticator,
		logger: logger, observer: config.Observer, maxDuration: map[storagev1.CallClass]time.Duration{},
		admission: map[storagev1.CallClass]chan struct{}{}, maxBlobBytes: maxBlob,
	}
	s.dataset = datasetProvider{server: s}
	for class, limit := range config.Limits.MaxConcurrent {
		if limit > 0 {
			s.admission[class] = make(chan struct{}, limit)
		}
	}
	for class, duration := range config.Limits.MaxDuration {
		if duration > 0 {
			s.maxDuration[class] = duration
		}
	}
	return s, nil
}

func (s *Server) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.InTapHandle(s.tap),
		grpc.ChainUnaryInterceptor(s.intercept),
		grpc.ChainStreamInterceptor(s.interceptStream),
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.MaxSendMsgSize(MaxMessageBytes),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             20 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 5 * time.Minute,
		}),
	}
}

func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	s.registerServices(registrar)
}

func (s *Server) Refresh(ctx context.Context) error {
	_, err := s.describe(ctx)
	return err
}

func (s *Server) DatasetID() string {
	if dataset := s.datasetID.Load(); dataset != nil {
		return *dataset
	}
	return ""
}

func (s *Server) describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	seq := s.describeSeq.Add(1)
	var info storagev1.ProviderInfo
	var err error
	if s.state != nil {
		info, err = s.state.Describe(ctx)
	} else {
		info, err = s.artifacts.Describe(ctx)
	}
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	if info.DatasetID == "" {
		s.publish(ctx, seq, "", 0, "the storage provider reports no dataset ID")
		return storagev1.ProviderInfo{}, errors.New("the storage provider reports no dataset ID")
	}
	reported := info.Contracts
	if s.state != nil && s.artifacts != nil {
		artifacts, err := s.artifacts.Describe(ctx)
		if err != nil {
			return storagev1.ProviderInfo{}, err
		}
		if artifacts.DatasetID != info.DatasetID {
			s.publish(ctx, seq, "", 0, "the state and artifacts providers serve different datasets")
			return storagev1.ProviderInfo{}, fmt.Errorf("the state provider serves dataset %q and the artifacts provider %q",
				info.DatasetID, artifacts.DatasetID)
		}
		info.Limits.MaxArtifactBytes = artifacts.Limits.MaxArtifactBytes
		reported = append(slices.Clone(reported), artifacts.Contracts...)
	}
	var contracts []string
	for _, contract := range reported {
		served := (contract == storagev1.StateContract && s.state != nil) ||
			(contract == storagev1.ArtifactsContract && s.artifacts != nil)
		if served && !slices.Contains(contracts, contract) {
			contracts = append(contracts, contract)
		}
	}
	info.Contracts = contracts
	if s.state != nil {
		info.Limits.MaxResultBytes = min(info.Limits.MaxResultBytes, s.maxBlobBytes)
	}
	if s.artifacts == nil {
		info.Limits.MaxArtifactBytes = 0
	} else {
		info.Limits.MaxArtifactBytes = min(max(info.Limits.MaxArtifactBytes, storagev1.MaxArtifactBytes), s.maxBlobBytes)
	}
	s.publish(ctx, seq, info.DatasetID, info.Limits.MaxArtifactBytes, "")
	return info, nil
}

func (s *Server) publish(ctx context.Context, seq uint64, dataset string, artifactLimit int64, reason string) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if seq < s.published {
		return
	}
	s.published = seq
	if dataset == "" {
		if previous := s.datasetID.Swap(nil); previous != nil {
			s.logger.ErrorContext(ctx, "storage server no longer has a serving dataset", "previous", *previous, "reason", reason)
		}
		return
	}
	if s.artifacts != nil {
		s.artifactLimit.Store(artifactLimit)
	}
	if previous := s.datasetID.Swap(&dataset); previous != nil && *previous != dataset {
		s.logger.WarnContext(ctx, "storage provider now serves another dataset", "previous", *previous, "dataset", dataset)
	}
}

type datasetProvider struct {
	server *Server
}

func (p datasetProvider) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	return p.server.describe(ctx)
}

func (p datasetProvider) HealthCheck(ctx context.Context) error {
	if p.server.state != nil {
		if err := p.server.state.HealthCheck(ctx); err != nil {
			return err
		}
	}
	if p.server.artifacts != nil {
		return p.server.artifacts.HealthCheck(ctx)
	}
	return nil
}

type ownershipProvider interface {
	storagev1.OperationReceiptStore
	storagev1.ControllerEpochAdvancer
	storagev1.DatasetBindingStore
	storagev1.WriteWatermarkStore
}

func (p datasetProvider) owner() ownershipProvider {
	if p.server.state != nil {
		return p.server.state
	}
	return p.server.artifacts
}

func (p datasetProvider) ResolveOperation(ctx context.Context, request storagev1.ResolveOperationRequest) (storagev1.OperationOutcome, error) {
	return p.owner().ResolveOperation(ctx, request)
}

func (p datasetProvider) PruneOperationReceipts(ctx context.Context, limit int) (int, error) {
	return p.owner().PruneOperationReceipts(ctx, limit)
}

func (p datasetProvider) AdvanceControllerEpoch(
	ctx context.Context, request storagev1.AdvanceControllerEpochRequest,
) (storagev1.ControllerEpochMirrorState, error) {
	return p.owner().AdvanceControllerEpoch(ctx, request)
}

func (p datasetProvider) GetControllerEpochMirror(ctx context.Context, name string) (storagev1.ControllerEpochMirrorState, error) {
	return p.owner().GetControllerEpochMirror(ctx, name)
}

func (p datasetProvider) BindDataset(ctx context.Context, request storagev1.BindDatasetRequest) (storagev1.DatasetBinding, error) {
	return p.owner().BindDataset(ctx, request)
}

func (p datasetProvider) GetDatasetBinding(ctx context.Context, datasetID string) (storagev1.DatasetBinding, error) {
	return p.owner().GetDatasetBinding(ctx, datasetID)
}

func (p datasetProvider) DetachDatasetBinding(ctx context.Context, request storagev1.DetachDatasetRequest) (storagev1.DatasetBinding, error) {
	return p.owner().DetachDatasetBinding(ctx, request)
}

func (p datasetProvider) UpdateBindingKeyFingerprint(
	ctx context.Context, request storagev1.UpdateBindingKeyRequest,
) (storagev1.DatasetBinding, error) {
	return p.owner().UpdateBindingKeyFingerprint(ctx, request)
}

func (p datasetProvider) CompleteDatasetBootstrap(ctx context.Context, datasetID string) error {
	return p.owner().CompleteDatasetBootstrap(ctx, datasetID)
}

func (p datasetProvider) AdvanceWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	return p.owner().AdvanceWriteWatermark(ctx)
}

func (p datasetProvider) GetWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	return p.owner().GetWriteWatermark(ctx)
}

func (s *Server) tap(ctx context.Context, info *tap.Info) (context.Context, error) {
	if s.authenticator == nil {
		return ctx, nil
	}
	if err := s.authenticator.Authenticate(ctx, info.FullMethodName, info.Header); err != nil {
		call := CallInfo{Method: unknownName, Code: codes.Unauthenticated, Reason: ReasonUnauthenticated}
		if m := methodByFullName(info.FullMethodName); m != nil {
			call.Method, call.Kind, call.Class = m.name, m.kind, m.class
		}
		s.logger.DebugContext(ctx, "storage authentication failed", "method", call.Method)
		if s.observer != nil {
			s.observer.ObserveCall(call)
		}
		return ctx, unauthenticatedStatus()
	}
	return context.WithValue(ctx, tapAuthenticated{}, s), nil
}

type tapAuthenticated struct{}

func (s *Server) authenticatedByTap(ctx context.Context) bool {
	marked, _ := ctx.Value(tapAuthenticated{}).(*Server)
	return marked == s
}

func unauthenticatedStatus() error {
	return errorStatus(codes.Unauthenticated, ReasonUnauthenticated, "storage authentication failed", nil)
}

func unknownMethodStatus() error {
	return status.Error(codes.Unimplemented, "unknown storage method")
}

func (s *Server) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	m := methodByFullName(info.FullMethod)
	if m == nil {
		return nil, unknownMethodStatus()
	}
	if s.authenticator != nil && !s.authenticatedByTap(ctx) {
		return nil, unauthenticatedStatus()
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = nil, s.panicked(ctx, m, recovered)
		}
		s.logCall(ctx, m, started, err)
	}()
	callCtx, release, served, err := s.admit(ctx, m)
	isDescribe := m.name == methodDescribe.name
	if !isDescribe || err != nil {
		if headerErr := grpc.SetHeader(ctx, s.responseHeader(served)); headerErr != nil {
			if err == nil {
				release()
			}
			return nil, s.headerError(ctx, m, headerErr)
		}
	}
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = callCtx
	if message, ok := req.(proto.Message); ok && hasUnknownFields(message.ProtoReflect()) {
		if isDescribe {
			_ = grpc.SetHeader(ctx, s.responseHeader(served))
		}
		return nil, unsupportedFieldStatus()
	}
	ctx = context.WithValue(ctx, interceptedCall{}, s)
	resp, err = handler(ctx, req)
	if isDescribe {
		headerDataset := served
		if desc, ok := resp.(*storagepb.DescribeResponse); ok && desc.GetProvider().GetDatasetId() != "" {
			headerDataset = desc.GetProvider().GetDatasetId()
		}
		if hdrErr := grpc.SetHeader(ctx, s.responseHeader(headerDataset)); hdrErr != nil {
			return nil, s.headerError(ctx, m, hdrErr)
		}
	}
	if err != nil {
		return nil, s.statusError(ctx, m, err)
	}
	if message, ok := resp.(proto.Message); ok {
		if field, invalid := firstInvalidString(message.ProtoReflect()); invalid {
			return nil, s.encodeError(ctx, m, fmt.Errorf("response field %s is not valid UTF-8", field))
		}
		if size := proto.Size(message); size > MaxMessageBytes {
			return nil, s.encodeError(ctx, m, fmt.Errorf("response of %d bytes exceeds the %d byte message limit", size, MaxMessageBytes))
		}
	}
	return resp, nil
}

func (s *Server) interceptStream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	m := methodByFullName(info.FullMethod)
	if m == nil {
		return unknownMethodStatus()
	}
	ctx := stream.Context()
	if s.authenticator != nil && !s.authenticatedByTap(ctx) {
		return unauthenticatedStatus()
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = s.panicked(ctx, m, recovered)
		}
		s.logCall(ctx, m, started, err)
	}()
	callCtx, release, served, err := s.admit(ctx, m)
	if err != nil {
		if headerErr := stream.SetHeader(s.responseHeader(served)); headerErr != nil {
			return s.headerError(ctx, m, headerErr)
		}
		return err
	}
	defer release()
	ctx = context.WithValue(callCtx, interceptedCall{}, s)
	cs := &checkedStream{
		ServerStream: stream,
		server:       s,
		method:       m,
		ctx:          ctx,
		kind:         m.kind,
		served:       served,
	}
	err = handler(srv, cs)
	if !cs.headerSent {
		if headerErr := stream.SetHeader(s.responseHeader(cs.served)); headerErr != nil {
			return s.headerError(ctx, m, headerErr)
		}
	}
	if _, isStatus := status.FromError(err); err != nil && !isStatus {
		return s.providerError(ctx, m, err)
	}
	return err
}

func (s *Server) headerError(ctx context.Context, m *method, err error) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage response headers cannot be set", "method", m.name, "correlation", correlation,
		"error", err)
	return providerErrorStatus(correlation)
}

type checkedStream struct {
	grpc.ServerStream
	server     *Server
	method     *method
	ctx        context.Context
	kind       storagev1.MethodKind
	served     string
	headerSent bool
}

func (s *checkedStream) Context() context.Context { return s.ctx }

func (s *checkedStream) RecvMsg(message any) error {
	if err := withinCall(s.ctx, func() error { return s.ServerStream.RecvMsg(message) }); err != nil {
		return streamError(s.ctx, err)
	}
	if decoded, ok := message.(proto.Message); ok && hasUnknownFields(decoded.ProtoReflect()) {
		return unsupportedFieldStatus()
	}
	if !s.headerSent {
		header, _ := metadata.FromIncomingContext(s.ctx)
		served, describeErr := s.server.servingDataset(s.ctx, s.method, header)
		s.served = served
		if headerErr := s.SetHeader(s.server.responseHeader(served)); headerErr != nil {
			return s.server.headerError(s.ctx, s.method, headerErr)
		}
		s.headerSent = true
		if err := s.server.checkDataset(s.ctx, s.method, header, served, describeErr); err != nil {
			return err
		}
	}
	return nil
}

func (s *checkedStream) SendMsg(message any) error {
	err := withinCall(s.ctx, func() error { return s.ServerStream.SendMsg(message) })
	if err == nil {
		return nil
	}
	if s.kind != storagev1.MethodRead && s.ctx.Err() != nil && errors.Is(err, s.ctx.Err()) {
		return errorStatus(codes.Unknown, ReasonOutcomeUnknown,
			"the write's response could not be sent within the storage server's time limit", nil)
	}
	return streamError(s.ctx, err)
}

func unsupportedFieldStatus() error {
	return errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
		"the request carries a field this storage server does not know", nil)
}

func (s *Server) admit(ctx context.Context, m *method) (_ context.Context, release func(), served string, err error) {
	header, _ := metadata.FromIncomingContext(ctx)
	if err := checkContract(header); err != nil {
		return nil, nil, "", err
	}
	release = func() {}
	if slots := s.admission[m.class]; slots != nil {
		select {
		case slots <- struct{}{}:
			release = func() { <-slots }
		default:
			reason := ReasonOverloaded
			if revision, _ := strconv.Atoi(header.Get(HeaderRevision)[0]); revision < overloadedSince {
				reason = ReasonUnavailable
			}
			return nil, nil, "", errorStatus(codes.Unavailable, reason,
				"the storage server is at its "+m.class.String()+" call limit", nil)
		}
	}
	if limit := s.maxDuration[m.class]; limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		admitted := release
		release = func() { cancel(); admitted() }
	}
	free, accepted := release, false
	defer func() {
		if !accepted {
			free()
		}
	}()
	served, describeErr := s.servingDataset(ctx, m, header)
	if err := s.checkDataset(ctx, m, header, served, describeErr); err != nil {
		return nil, nil, "", err
	}
	if ctx, err = withEnvelope(ctx, m, header); err != nil {
		return nil, nil, "", err
	}
	if ctx, err = withTaskDataFence(ctx, header); err != nil {
		return nil, nil, "", err
	}
	if ctx.Err() != nil {
		return nil, nil, "", errorStatus(codes.Unavailable, ReasonUnavailable,
			"the storage call reached its time limit before it was admitted", nil)
	}
	accepted = true
	return ctx, free, served, nil
}

const overloadedSince = 2

type interceptedCall struct{}

func (s *Server) requireIntercepted(ctx context.Context) error {
	if marked, _ := ctx.Value(interceptedCall{}).(*Server); marked == s {
		return nil
	}
	return errorStatus(codes.FailedPrecondition, ReasonContractMismatch,
		"storage server was registered without ServerOptions", nil)
}

func checkContract(header metadata.MD) error {
	contracts := header.Get(HeaderContract)
	revisions := header.Get(HeaderRevision)
	if len(contracts) != 1 || contracts[0] != storagev1.ContractVersion || len(revisions) != 1 {
		return errorStatus(codes.FailedPrecondition, ReasonContractMismatch,
			"the call does not name contract "+storagev1.ContractVersion, nil)
	}
	if revision, err := strconv.Atoi(revisions[0]); err != nil || revision < 1 {
		return errorStatus(codes.FailedPrecondition, ReasonContractMismatch, "the call names an invalid protocol revision", nil)
	}
	return nil
}

func withEnvelope(ctx context.Context, m *method, header metadata.MD) (context.Context, error) {
	var encoded storagepb.WriteEnvelope
	found, err := decodeHeader(header, HeaderEnvelope, "write envelope", &encoded)
	if err != nil || !found {
		return ctx, err
	}
	if m.kind != storagev1.MethodWrite {
		return ctx, validationStatus("a " + m.kind.String() + " call carries no write envelope")
	}
	envelope, err := fromWireWriteEnvelope(&encoded, true)
	if err != nil {
		return ctx, requestStatus(err)
	}
	if named := header.Get(HeaderDataset); envelope.DatasetID != "" && (len(named) != 1 || envelope.DatasetID != named[0]) {
		return ctx, errorStatus(codes.FailedPrecondition, ReasonDatasetMismatch,
			"the write envelope names a dataset other than the call's", nil)
	}
	return storagev1.WithWriteEnvelope(ctx, envelope), nil
}

func withTaskDataFence(ctx context.Context, header metadata.MD) (context.Context, error) {
	var encoded storagepb.TaskDataFence
	found, err := decodeHeader(header, HeaderTaskDataFence, "Task data fence", &encoded)
	if err != nil || !found {
		return ctx, err
	}
	fence, err := fromWireTaskDataFence(&encoded, true)
	if err != nil {
		return ctx, requestStatus(err)
	}
	return storagev1.WithTaskDataFence(ctx, fence), nil
}

func decodeHeader(header metadata.MD, key, what string, message proto.Message) (found bool, err error) {
	values := header.Get(key)
	if len(values) == 0 {
		return false, nil
	}
	if len(values) > 1 {
		return false, validationStatus("the call carries more than one " + what)
	}
	if err := proto.Unmarshal([]byte(values[0]), message); err != nil {
		return false, validationStatus("the " + what + " cannot be decoded")
	}
	if hasUnknownFields(message.ProtoReflect()) {
		return false, errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
			"the "+what+" carries a field this storage server does not know", nil)
	}
	if field, invalid := firstInvalidString(message.ProtoReflect()); invalid {
		return false, validationStatus("the " + what + " field " + field + " is not valid UTF-8")
	}
	return true, nil
}

func (s *Server) checkDigest(ctx context.Context, m *method, args ...any) error {
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		return nil
	}
	digest, err := storagev1.RequestDigest(m.name, args...)
	if err != nil {
		return requestStatus(err)
	}
	if digest != envelope.RequestDigest {
		return validationStatus("the request does not match its write envelope's request digest")
	}
	return nil
}

func (s *Server) servingDataset(ctx context.Context, m *method, header metadata.MD) (string, error) {
	known := s.DatasetID()
	named := header.Get(HeaderDataset)
	omitted := m.mayOmitDataset() && len(named) == 0
	if known != "" && omitted {
		return known, nil
	}
	if known != "" && m.kind == storagev1.MethodWrite && (len(named) != 1 || named[0] == known) {
		return known, nil
	}
	if omitted {
		return "", nil
	}
	info, err := s.describe(ctx)
	if err != nil {
		return "", err
	}
	if current := s.DatasetID(); current != info.DatasetID {
		return "", errors.New("a newer description of the storage providers superseded this one")
	}
	return info.DatasetID, nil
}

func (s *Server) checkDataset(ctx context.Context, m *method, header metadata.MD, served string, describeErr error) error {
	named := header.Get(HeaderDataset)
	switch {
	case len(named) > 1:
		return validationStatus("the call names more than one dataset")
	case len(named) == 0 && m.mayOmitDataset():
		return nil
	case len(named) == 0:
		return validationStatus("a " + m.name + " call must name the dataset it expects")
	case describeErr != nil:
		if describeRefusal(describeErr) {
			if encoded, ok := contractStatus(describeErr); ok {
				return encoded
			}
		}
		if ctx.Err() != nil {
			return errorStatus(codes.Unavailable, ReasonUnavailable,
				"the storage server could not name its dataset within the call's time limit", nil)
		}
		s.logger.ErrorContext(ctx, "storage provider cannot describe its dataset", "method", m.name, "error", describeErr)
		return errorStatus(codes.Unavailable, ReasonUnavailable, "the storage server cannot name its dataset", nil)
	case named[0] != served:
		return errorStatus(codes.FailedPrecondition, ReasonDatasetMismatch,
			"the call names a dataset this storage server does not serve", nil)
	}
	return nil
}

func describeRefusal(err error) bool {
	return errors.Is(err, storagev1.ErrUnavailable) || errors.Is(err, storagev1.ErrUnauthenticated) ||
		errors.Is(err, storagev1.ErrIncompatible)
}

func (s *Server) responseHeader(admitted string) metadata.MD {
	header := metadata.Pairs(HeaderContract, storagev1.ContractVersion, HeaderRevision, strconv.Itoa(ProtocolRevision))
	dataset := admitted
	if dataset == "" {
		dataset = s.DatasetID()
	}
	if dataset != "" {
		header.Set(HeaderDataset, dataset)
	}
	return header
}

func (s *Server) libraryDescribe(resp *storagepb.DescribeResponse, _ storagev1.ProviderInfo) {
	resp.Server = serverInfoToWire(s.info)
}

func (s *Server) blobLimit(m *method) int64 {
	if m.contract == storagev1.ArtifactsContract {
		limit := s.artifactLimit.Load()
		if limit <= 0 {
			limit = storagev1.MaxArtifactBytes
		}
		return min(limit, s.maxBlobBytes)
	}
	return s.maxBlobBytes
}

func (s *Server) receiveUpload(stream grpc.ServerStream, m *method, request proto.Message) error {
	ctx := stream.Context()
	header, _ := metadata.FromIncomingContext(ctx)
	declared, err := parseBlobHeader(header)
	if err != nil {
		return blobValidationStatus(err)
	}
	if limit := s.blobLimit(m); declared.size > limit {
		encoded, _ := contractStatus(storagev1.TooLargeError{What: m.name + " blob", Size: declared.size, Limit: limit})
		return encoded
	}
	if err := stream.RecvMsg(request); err != nil {
		if errors.Is(err, io.EOF) {
			return blobValidationStatus(fmt.Errorf("%w: the stream carries no request", errBlobFraming))
		}
		return err
	}
	err = reassemble(request, func() (proto.Message, error) {
		part := request.ProtoReflect().New().Interface()
		return part, stream.RecvMsg(part)
	}, declared)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errBlobFraming):
		return blobValidationStatus(err)
	case errors.Is(err, errBlobShort), errors.Is(err, errBlobDigest):
		return errorStatus(codes.Unavailable, ReasonUnavailable, err.Error(), nil)
	default:
		return err
	}
}

func withinCall(ctx context.Context, operation func() error) error {
	done := make(chan error, 1)
	go func() { done <- operation() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func streamError(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return errorStatus(codes.Unavailable, ReasonUnavailable,
			"the call did not finish within the storage server's time limit", nil)
	}
	return err
}

func blobValidationStatus(err error) error {
	message := sanitizeText(err.Error(), messageLimit)
	return errorStatus(codes.InvalidArgument, ReasonValidation, message, map[string]string{messageKey: message})
}

func (s *Server) sendDownload(stream grpc.ServerStream, m *method, response proto.Message) error {
	ctx := stream.Context()
	path := blobPathOf(response.ProtoReflect().Descriptor())
	data := takeBlob(response.ProtoReflect(), path)
	if field, invalid := firstInvalidString(response.ProtoReflect()); invalid {
		return s.encodeError(ctx, m, fmt.Errorf("response field %s is not valid UTF-8", field))
	}
	if size := proto.Size(response); size > MaxMessageBytes-MaxChunkBytes {
		return s.encodeError(ctx, m, fmt.Errorf("the response is %d bytes without its blob", size))
	}
	if err := stream.SetHeader(metadata.Pairs(describeBlob(data).pairs()...)); err != nil {
		return status.Error(codes.Internal, "cannot set storage response headers")
	}
	return sendBlob(stream.SendMsg, response, path, data)
}

func requestStatus(err error) error {
	if encoded, ok := contractStatus(err); ok {
		return encoded
	}
	return validationStatus("the request cannot be decoded")
}

func validationStatus(message string) error {
	return errorStatus(codes.InvalidArgument, ReasonValidation, message, map[string]string{messageKey: message})
}

func (s *Server) statusError(ctx context.Context, m *method, err error) error {
	if st, ok := status.FromError(err); ok && storageErrorInfo(st) != nil {
		return err
	}
	return s.providerError(ctx, m, err)
}

func (s *Server) providerError(ctx context.Context, m *method, err error) error {
	if encoded, ok := contractStatus(err); ok {
		return encoded
	}
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage provider error", "method", m.name, "correlation", correlation, "error", err)
	return providerErrorStatus(correlation)
}

func (s *Server) encodeError(ctx context.Context, m *method, err error) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage response cannot be encoded", "method", m.name, "correlation", correlation, "error", err)
	return providerErrorStatus(correlation)
}

func (s *Server) panicked(ctx context.Context, m *method, _ any) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage provider panicked", "method", m.name, "correlation", correlation)
	return providerErrorStatus(correlation)
}

func (s *Server) logCall(ctx context.Context, m *method, started time.Time, err error) {
	code, reason := codes.OK, ""
	if err != nil {
		st := status.Convert(err)
		code = st.Code()
		if info := storageErrorInfo(st); info != nil {
			reason = info.GetReason()
		}
	}
	duration := time.Since(started)
	if s.observer != nil {
		s.observer.ObserveCall(CallInfo{Method: m.name, Kind: m.kind, Class: m.class, Code: code, Reason: reason, Duration: duration})
	}
	s.logger.DebugContext(ctx, "storage call", "method", m.name, "kind", m.kind.String(), "code", code.String(),
		"reason", reason, "duration", duration, "dataset", s.DatasetID())
}
