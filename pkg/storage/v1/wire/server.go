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

// Authenticator authenticates a call from its request headers before the
// server decodes the request. ctx carries the connection's peer, including
// its verified TLS state.
type Authenticator interface {
	Authenticate(ctx context.Context, fullMethod string, header metadata.MD) error
}

// Limits bounds the calls a Server runs at once and how long each may run,
// per call class: the class the contract's method catalog gives each method
// (storagev1.LookupMethod). A missing or zero entry means no limit.
type Limits struct {
	// MaxConcurrent caps the calls of each class the server runs at once. A
	// call over its class's limit is refused with OVERLOADED before the
	// provider runs, and before an upload's blob is read.
	MaxConcurrent map[storagev1.CallClass]int
	// MaxDuration caps how long a call of each class may run: describing the
	// providers for its dataset check, every message a streaming call
	// receives or sends, and the provider's work. The caller's deadline
	// still applies when it is shorter. An upload or download that does not finish in time is ended
	// as unavailable, except that a write whose response cannot be sent in
	// time after the provider ran is an unknown outcome.
	MaxDuration map[storagev1.CallClass]time.Duration
}

// CallInfo describes one finished call, without its content.
type CallInfo struct {
	// Method is the contract method name, such as "SaveArtifact".
	Method   string
	Kind     storagev1.MethodKind
	Class    storagev1.CallClass
	Code     codes.Code
	Reason   string
	Duration time.Duration
}

// CallObserver receives every storage call's outcome, for metrics.
type CallObserver interface {
	ObserveCall(CallInfo)
}

// ServerConfig configures a Server.
type ServerConfig struct {
	// State is the state.v1 provider the server serves, or nil when it
	// serves only artifacts.v1.
	State storagev1.StateProvider
	// Artifacts is the artifacts.v1 provider the server serves, or nil when
	// it serves only state.v1. At least one of State and Artifacts is
	// required. When both are set they must serve the same dataset, and
	// State answers Describe for both. A server answers every RPC of a
	// contract part it does not serve with UNIMPLEMENTED.
	Artifacts storagev1.ArtifactProvider
	// Info names the adapter. The server library sets ProtocolRevision and
	// LibraryVersion itself.
	Info ServerInfo
	// Authenticator, when set, authenticates every call before its request
	// is decoded, whatever method it names; a gRPC server built with
	// ServerOptions serves only the storage services. Without one the
	// server library authenticates nothing. It runs in the gRPC transport's
	// tap, so a call served through a transport that runs no tap, such as
	// grpc.Server.ServeHTTP, is refused as unauthenticated.
	Authenticator Authenticator
	// Logger receives one entry per call and the text of the provider
	// errors the server does not forward, which can quote stored values. It
	// never receives request or response messages. Nil means
	// slog.Default().
	Logger *slog.Logger
	// Limits bounds concurrency and call duration per class.
	Limits Limits
	// MaxBlobBytes caps the blob an upload may carry. Zero means
	// DefaultMaxBlobBytes. An artifact is also capped at the artifacts
	// provider's own MaxArtifactBytes.
	MaxBlobBytes int64
	// Observer, when set, receives every call's outcome.
	Observer CallObserver
}

// Server serves storage providers over gRPC.
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

	// datasetID is the serving dataset from the providers' latest
	// description.
	datasetID atomic.Pointer[string]
	// artifactLimit is the artifacts provider's MaxArtifactBytes from its
	// latest description.
	artifactLimit atomic.Int64
	// describeSeq numbers descriptions as they start. publishMu and
	// published let only a description newer than the last one published
	// change the serving dataset, so a slow description cannot restore a
	// dataset a newer one replaced or dropped.
	describeSeq atomic.Uint64
	publishMu   sync.Mutex
	published   uint64
}

// NewServer returns a Server for config.
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

// ServerOptions returns the options a gRPC server needs to serve this
// Server: authentication before decoding, the call interceptors, message size
// limits, keepalive enforcement that admits the client's pings, and a maximum
// connection age so long-lived connections spread across adapter replicas.
// Callers add TLS credentials. A gRPC server built with these options serves
// only the storage services: it authenticates every call and refuses any
// method the storage protocol does not define.
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

// Register registers the storage services on registrar: the Dataset service
// always, and the services of each contract part the server serves. A gRPC
// server built with ServerOptions answers any other RPC with UNIMPLEMENTED.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	s.registerServices(registrar)
}

// Refresh describes the served providers and records the serving dataset.
// A server describes its providers before the first call that needs the
// dataset and again for every read or administrative call that names one; an
// adapter calls Refresh at startup and periodically, so that a dataset
// change, such as a database replaced under the adapter, is noticed before
// the next write.
func (s *Server) Refresh(ctx context.Context) error {
	_, err := s.describe(ctx)
	return err
}

// DatasetID returns the serving dataset from the providers' last
// description, or "" before one succeeded or after one found no single
// dataset.
func (s *Server) DatasetID() string {
	if dataset := s.datasetID.Load(); dataset != nil {
		return *dataset
	}
	return ""
}

// describe describes the served providers and records the serving dataset.
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
	// Describe the server as a whole: the contract parts it registers that
	// its providers report, and the artifact limit it enforces, so a client
	// never accepts an endpoint whose methods are unimplemented.
	var contracts []string
	for _, contract := range reported {
		served := (contract == storagev1.StateContract && s.state != nil) ||
			(contract == storagev1.ArtifactsContract && s.artifacts != nil)
		if served && !slices.Contains(contracts, contract) {
			contracts = append(contracts, contract)
		}
	}
	info.Contracts = contracts
	if s.artifacts == nil {
		info.Limits.MaxArtifactBytes = 0
	} else {
		// The server refuses an artifact over its own blob limit too.
		info.Limits.MaxArtifactBytes = min(max(info.Limits.MaxArtifactBytes, storagev1.MaxArtifactBytes), s.maxBlobBytes)
	}
	s.publish(ctx, seq, info.DatasetID, info.Limits.MaxArtifactBytes, "")
	return info, nil
}

// publish records the outcome of description seq unless a newer description
// has already been published. An empty dataset drops the serving dataset
// after a description that names no single dataset, so no call is admitted
// until a consistent description succeeds; reason says why. A description
// that merely fails, such as one that times out, publishes nothing.
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

// datasetProvider answers Describe and HealthCheck for every contract part
// the server serves.
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

// tap authenticates every call before its request is read, and marks the
// call's context so the interceptors know it did. It does not depend on
// recognizing the method, so a path the method table does not name is never
// served unauthenticated.
func (s *Server) tap(ctx context.Context, info *tap.Info) (context.Context, error) {
	if s.authenticator == nil {
		return ctx, nil
	}
	if err := s.authenticator.Authenticate(ctx, info.FullMethodName, info.Header); err != nil {
		call := CallInfo{Method: unknownName, Code: codes.Unauthenticated, Reason: ReasonUnauthenticated}
		if m := methodByFullName(info.FullMethodName); m != nil {
			call.Method, call.Kind, call.Class = m.name, m.kind, m.class
		}
		s.logger.DebugContext(ctx, "storage authentication failed", "method", call.Method, "error", err)
		if s.observer != nil {
			s.observer.ObserveCall(call)
		}
		return ctx, unauthenticatedStatus()
	}
	return context.WithValue(ctx, tapAuthenticated{}, s), nil
}

// tapAuthenticated keys the Server whose tap authenticated a call.
type tapAuthenticated struct{}

// authenticatedByTap reports whether this server's tap authenticated the
// call. A transport that never runs the tap, as grpc.Server.ServeHTTP does
// not, leaves no mark.
func (s *Server) authenticatedByTap(ctx context.Context) bool {
	marked, _ := ctx.Value(tapAuthenticated{}).(*Server)
	return marked == s
}

func unauthenticatedStatus() error {
	return errorStatus(codes.Unauthenticated, ReasonUnauthenticated, "storage authentication failed", nil)
}

// unknownMethodStatus refuses a call the method table does not name. A
// server built with ServerOptions serves only the storage services, so such
// a call never reaches a handler without the protocol checks.
func unknownMethodStatus() error {
	return status.Error(codes.Unimplemented, "unknown storage method")
}

// intercept applies the protocol checks to every unary storage call before
// the provider sees it, so each refusal here means nothing was committed.
func (s *Server) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	m := methodByFullName(info.FullMethod)
	if m == nil {
		return nil, unknownMethodStatus()
	}
	if s.authenticator != nil && !s.authenticatedByTap(ctx) {
		// The call reached the server without passing its authenticator,
		// so it is refused before anything else runs.
		return nil, unauthenticatedStatus()
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = nil, s.panicked(ctx, m, recovered)
		}
		s.logCall(ctx, m, started, err)
	}()
	callCtx, release, err := s.admit(ctx, m)
	// Every authenticated call, including one refused, answers with the
	// contract, revision and, once known, the serving dataset.
	if headerErr := grpc.SetHeader(ctx, s.responseHeader()); headerErr != nil {
		if err == nil {
			release()
		}
		return nil, s.headerError(ctx, m, headerErr)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = callCtx
	if message, ok := req.(proto.Message); ok && hasUnknownFields(message.ProtoReflect()) {
		return nil, unsupportedFieldStatus()
	}
	resp, err = handler(ctx, req)
	if err != nil {
		return nil, s.statusError(ctx, m, err)
	}
	if message, ok := resp.(proto.Message); ok {
		if field, invalid := firstInvalidString(message.ProtoReflect()); invalid {
			return nil, s.encodeError(ctx, m, fmt.Errorf("response field %s is not valid UTF-8", field))
		}
		// gRPC refuses to send a response over the message limit with a bare
		// status the client cannot tell from a transport failure, so a read
		// would look safe to retry. Report it as the provider error it is. The
		// contract's message budgets keep a provider's batches under the limit.
		if size := proto.Size(message); size > MaxMessageBytes {
			return nil, s.encodeError(ctx, m, fmt.Errorf("response of %d bytes exceeds the %d byte message limit", size, MaxMessageBytes))
		}
	}
	return resp, nil
}

// interceptStream applies the same checks to every streaming storage call.
// The request messages are checked for unknown fields as they arrive.
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
	callCtx, release, err := s.admit(ctx, m)
	if headerErr := stream.SetHeader(s.responseHeader()); headerErr != nil {
		if err == nil {
			release()
		}
		return s.headerError(ctx, m, headerErr)
	}
	if err != nil {
		return err
	}
	defer release()
	ctx = callCtx
	err = handler(srv, &checkedStream{ServerStream: stream, ctx: ctx, kind: m.kind})
	if _, isStatus := status.FromError(err); err != nil && !isStatus {
		return s.providerError(ctx, m, err)
	}
	return err
}

// headerError reports response headers the server cannot set.
func (s *Server) headerError(ctx context.Context, m *method, err error) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage response headers cannot be set", "method", m.name, "correlation", correlation,
		"error", err)
	return providerErrorStatus(correlation)
}

// checkedStream gives a streaming handler the call's checked context,
// bounds every receive and send by it, including the receive of a download's
// request that gRPC makes before the handler runs, and refuses any request
// message with a field the server does not know.
type checkedStream struct {
	grpc.ServerStream
	ctx  context.Context
	kind storagev1.MethodKind
}

func (s *checkedStream) Context() context.Context { return s.ctx }

func (s *checkedStream) RecvMsg(message any) error {
	if err := withinCall(s.ctx, func() error { return s.ServerStream.RecvMsg(message) }); err != nil {
		return streamError(s.ctx, err)
	}
	if decoded, ok := message.(proto.Message); ok && hasUnknownFields(decoded.ProtoReflect()) {
		return unsupportedFieldStatus()
	}
	return nil
}

func (s *checkedStream) SendMsg(message any) error {
	err := withinCall(s.ctx, func() error { return s.ServerStream.SendMsg(message) })
	if err == nil {
		return nil
	}
	if s.kind != storagev1.MethodRead && s.ctx.Err() != nil && errors.Is(err, s.ctx.Err()) {
		// A write sends its response only after the provider ran, so a
		// response that could not be sent in time may follow a commit.
		return errorStatus(codes.Unknown, ReasonOutcomeUnknown,
			"the write's response could not be sent within the storage server's time limit", nil)
	}
	return streamError(s.ctx, err)
}

func unsupportedFieldStatus() error {
	return errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
		"the request carries a field this storage server does not know", nil)
}

// admit checks a call's contract header, admits it within its class's
// concurrency limit, caps its duration, checks its dataset header and decodes
// its write envelope and Task data fence. The dataset check runs within the
// admission and the cap, so a slow description of the providers holds a slot
// and ends at the cap rather than queueing calls without limit. release frees
// the call's admission when it ends. admit owns the admission until it
// returns successfully, so a refused call, or a provider that panics while it
// is described, has already released it.
func (s *Server) admit(ctx context.Context, m *method) (_ context.Context, release func(), err error) {
	header, _ := metadata.FromIncomingContext(ctx)
	if err := checkContract(header); err != nil {
		return nil, nil, err
	}
	release = func() {}
	if slots := s.admission[m.class]; slots != nil {
		select {
		case slots <- struct{}{}:
			release = func() { <-slots }
		default:
			return nil, nil, errorStatus(codes.Unavailable, ReasonOverloaded,
				"the storage server is at its "+m.class.String()+" call limit", nil)
		}
	}
	if limit := s.maxDuration[m.class]; limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		admitted := release
		release = func() { cancel(); admitted() }
	}
	// free is released on a refusal and while a panic unwinds. It is a copy
	// because returning a refusal clears the named release result before
	// deferred calls run.
	free, accepted := release, false
	defer func() {
		if !accepted {
			free()
		}
	}()
	served, describeErr := s.servingDataset(ctx, m, header)
	if err := s.checkDataset(ctx, m, header, served, describeErr); err != nil {
		return nil, nil, err
	}
	if ctx, err = withEnvelope(ctx, m, header); err != nil {
		return nil, nil, err
	}
	if ctx, err = withTaskDataFence(ctx, header); err != nil {
		return nil, nil, err
	}
	accepted = true
	return ctx, free, nil
}

// checkContract refuses a call without the orka.storage.v1 contract header
// and a protocol revision. The server serves every older revision.
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

// withEnvelope decodes a write's envelope header into ctx, where the provider
// reads it exactly as it does in process. Only a write carries an envelope,
// so the server refuses one on a read or an administrative call.
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
	return storagev1.WithWriteEnvelope(ctx, envelope), nil
}

// withTaskDataFence decodes a call's Task data fence header into ctx, where
// the provider reads it exactly as it does in process.
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

// decodeHeader decodes the binary header key into message. found is false
// when the call does not carry the header.
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

// checkDigest refuses a write whose envelope's request digest is not the
// digest of the request the server decoded, before the provider runs, so an
// operation ID can never be bound to a request other than the one its client
// digested. A write without an envelope has no digest to check.
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

// servingDataset returns the dataset the server checks a call against. A
// read or an administrative call that names a dataset describes the
// providers again before it runs, so a provider whose dataset changed while
// it serves never answers it for the dataset it served before. A write is
// checked against the dataset the providers last described, as its
// envelope's dataset is checked again by the provider itself, and a Describe
// or HealthCheck that names none needs no dataset. Describe and Refresh
// update the dataset too.
func (s *Server) servingDataset(ctx context.Context, m *method, header metadata.MD) (string, error) {
	known := s.DatasetID()
	omitted := m.mayOmitDataset() && len(header.Get(HeaderDataset)) == 0
	if known != "" && (omitted || m.kind == storagev1.MethodWrite) {
		return known, nil
	}
	if omitted {
		return "", nil
	}
	info, err := s.describe(ctx)
	if err != nil {
		return "", err
	}
	return info.DatasetID, nil
}

// checkDataset refuses, before the provider runs, a call that names a
// dataset other than the serving one, or that names none when it must.
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
		// Describe may fail only as unavailable, unauthenticated or
		// incompatible, each of which the client takes from a server that
		// has not checked its dataset. Any other error, such as a cold
		// replica's ErrNotReady or providers that disagree on their
		// dataset, would read as an answer for another dataset, so it is
		// reported as the server being unavailable.
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

// describeRefusal reports whether err is one of the errors Describe may
// return.
func describeRefusal(err error) bool {
	return errors.Is(err, storagev1.ErrUnavailable) || errors.Is(err, storagev1.ErrUnauthenticated) ||
		errors.Is(err, storagev1.ErrIncompatible)
}

// responseHeader names the contract, this library's revision and, once the
// server knows it, the serving dataset.
func (s *Server) responseHeader() metadata.MD {
	header := metadata.Pairs(HeaderContract, storagev1.ContractVersion, HeaderRevision, strconv.Itoa(ProtocolRevision))
	if dataset := s.DatasetID(); dataset != "" {
		header.Set(HeaderDataset, dataset)
	}
	return header
}

// libraryDescribe sets the server library's part of a Describe response. The
// description itself has already recorded the serving dataset.
func (s *Server) libraryDescribe(resp *storagepb.DescribeResponse, _ storagev1.ProviderInfo) {
	resp.Server = serverInfoToWire(s.info)
}

// blobLimit is the largest blob an upload of m may carry.
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

// receiveUpload reads a streaming write's request and reassembles its blob.
// The provider runs only after the client ends the stream and the blob
// matches its declared size and digest, so a refused or broken upload never
// reaches it.
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
	// stream is the call's checkedStream, which bounds each receive by the
	// call's duration limit.
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

// withinCall runs one blocking stream operation and returns ctx's error if
// ctx ends first. gRPC ends a stream only when its handler returns, so this
// is how a call capped by the server's duration limit stops waiting for a
// peer that stalls; the abandoned operation fails once the handler returns
// and the stream ends. The caller must not use the operation's message after
// abandoning it.
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

// streamError reports a stream that ended before the provider ran. A call
// that ran out of time is unavailable: nothing was committed.
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

// sendDownload sends a streaming read's response with its blob in parts.
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
	// stream is the call's checkedStream, which bounds each send by the
	// call's duration limit.
	return sendBlob(stream.SendMsg, response, path, data)
}

// requestStatus reports a request the server cannot decode into Go values.
func requestStatus(err error) error {
	if encoded, ok := contractStatus(err); ok {
		return encoded
	}
	return validationStatus("the request cannot be decoded")
}

// validationStatus refuses a request with a VALIDATION reason and a
// client-safe message.
func validationStatus(message string) error {
	return errorStatus(codes.InvalidArgument, ReasonValidation, message, map[string]string{messageKey: message})
}

// statusError passes a status the server library built and encodes any
// other error the handler returned.
func (s *Server) statusError(ctx context.Context, m *method, err error) error {
	if st, ok := status.FromError(err); ok && storageErrorInfo(st) != nil {
		return err
	}
	return s.providerError(ctx, m, err)
}

// providerError encodes a provider's error. A contract error keeps its
// reason; any other error is logged with a correlation ID and reported
// without its text.
func (s *Server) providerError(ctx context.Context, m *method, err error) error {
	if encoded, ok := contractStatus(err); ok {
		return encoded
	}
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage provider error", "method", m.name, "correlation", correlation, "error", err)
	return providerErrorStatus(correlation)
}

// encodeError reports a provider result the server cannot encode. The call
// may have committed, so the error never carries a contract reason that would
// tell the client nothing happened.
func (s *Server) encodeError(ctx context.Context, m *method, err error) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage response cannot be encoded", "method", m.name, "correlation", correlation, "error", err)
	return providerErrorStatus(correlation)
}

// panicked reports a provider panic as a provider error.
func (s *Server) panicked(ctx context.Context, m *method, recovered any) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage provider panicked", "method", m.name, "correlation", correlation,
		"panic", fmt.Sprint(recovered))
	return providerErrorStatus(correlation)
}

// logCall records one call without request or response content.
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
