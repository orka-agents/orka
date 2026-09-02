package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/publisher"
)

type Server struct {
	config      Config
	handler     http.Handler
	journal     *journalStore
	artifacts   *artifactClient
	credentials credentialManager
	gitBinary   string
	gitVersion  string
	semaphore   chan struct{}
	locks       [64]sync.Mutex

	publicationMu sync.Mutex
}

func New(config Config) (*Server, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	gitBinary, err := ensureGitBinary(normalized.GitBinary)
	if err != nil {
		return nil, err
	}
	gitVersion, err := detectGitVersion(gitBinary)
	if err != nil {
		return nil, err
	}
	if normalized.RequiredGitVersion != "" && gitVersion != normalized.RequiredGitVersion {
		return nil, fmt.Errorf("git version %q does not match required %q", gitVersion, normalized.RequiredGitVersion)
	}
	if err := ensurePrivateDirectory(normalized.ArtifactRoot); err != nil {
		return nil, fmt.Errorf("prepare publication artifact root: %w", err)
	}
	if err := ensurePrivateDirectory(normalized.TempRoot); err != nil {
		return nil, fmt.Errorf("prepare temporary root: %w", err)
	}
	if normalized.CredentialRoot != "" {
		info, statErr := osLstat(normalized.CredentialRoot)
		if statErr != nil || info.modeSymlink || !info.directory {
			return nil, fmt.Errorf("credential root is missing or unsafe")
		}
	}
	journal, err := newJournalStore(normalized.JournalRoot, normalized.MaxJournalBytes, normalized.Now)
	if err != nil {
		return nil, err
	}
	var artifactAuthorizer artifactAuthorizer
	if normalized.ArtifactAuthorizationBrokerURL != "" {
		artifactAuthorizer, err = newBrokerArtifactAuthorizer(
			normalized.ArtifactAuthorizationBrokerURL, normalized.HTTPClient,
			normalized.ControllerBearerToken, normalized.ArtifactTimeout,
		)
	} else {
		artifactAuthorizer = &localArtifactAuthorizer{
			secret: append([]byte(nil), normalized.ArtifactCapabilitySecret...),
			ttl:    normalized.CapabilityTTL, now: normalized.Now,
		}
	}
	if err != nil {
		return nil, err
	}
	artifacts, err := newArtifactClient(
		normalized.ArtifactAPIURL, normalized.HTTPClient, artifactAuthorizer, normalized.ArtifactTimeout,
	)
	if err != nil {
		return nil, err
	}
	var credentialProvider credentialProvider
	if normalized.CredentialBrokerURL != "" {
		credentialProvider, err = newBrokerCredentialProvider(
			normalized.CredentialBrokerURL, normalized.HTTPClient,
			normalized.ControllerBearerToken, normalized.ArtifactTimeout,
		)
	} else if normalized.CredentialRoot != "" {
		credentialProvider = &fileCredentialProvider{root: normalized.CredentialRoot}
	}
	if err != nil {
		return nil, err
	}
	server := &Server{
		config: normalized, journal: journal, artifacts: artifacts, gitBinary: gitBinary, gitVersion: gitVersion,
		semaphore: make(chan struct{}, normalized.MaxConcurrentOperations),
		credentials: credentialManager{
			provider: credentialProvider, tempRoot: normalized.TempRoot, realGit: gitBinary,
			defaultGit: normalized.DefaultGitCredential,
		},
	}
	server.handler = server.routes()
	return server, nil
}

// Handler returns the bounded HTTP API. The handler never logs request bodies,
// headers, repository URLs, credentials, or upstream error strings.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+HealthPath, s.handleHealth)
	mux.HandleFunc("GET "+CapabilitiesPath, s.handleCapabilities)
	mux.HandleFunc("POST "+WorkspaceResolvePath, s.handleWorkspaceResolve)
	mux.HandleFunc("POST "+WorkspacePreparePath, s.handleWorkspacePrepare)
	mux.HandleFunc("POST "+PublicationPreflightPath, s.handlePublicationPreflight)
	mux.HandleFunc("POST "+PublicationPreparePath, s.handlePublicationPrepare)
	mux.HandleFunc("POST "+PublicationPublishPath, s.handlePublicationPublish)
	mux.HandleFunc("POST "+PublicationVerifyPath, s.handlePublicationVerify)
	mux.HandleFunc("POST "+PublicationReclaimPath, s.handlePublicationReclaim)
	mux.HandleFunc("POST "+PullRequestReconcilePath, s.handlePullRequestReconcile)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	s.writePublicJSON(writer, http.StatusOK, HealthResponse{Status: "ok", Ready: true})
}

func (s *Server) handleCapabilities(writer http.ResponseWriter, _ *http.Request) {
	operations := []Operation{
		OperationWorkspaceResolve, OperationWorkspacePrepare, OperationPublicationPreflight, OperationPublicationPrepare,
		OperationPublicationPublish, OperationPublicationVerify, OperationPublicationReclaim,
	}
	credentialKinds := []CredentialKind{CredentialHTTPExtraHeader}
	if s.config.PRFactory != nil {
		operations = append(operations, OperationPullRequestReconcile)
		credentialKinds = append(credentialKinds, CredentialForgeToken)
	}
	response := CapabilitiesResponse{
		Protocol: ProtocolVersion, NetworkIdentity: "workspace-publisher", Operations: operations,
		Authentication:  []string{"controller-bearer", "operation-capability"},
		CredentialKinds: append([]CredentialKind(nil), credentialKinds...),
		SCMSchemes:      []string{schemeHTTPS}, GitVersion: s.gitVersion, RedirectsAllowed: false,
		ProviderOrMCPAccess: false, PullRequestReconciliation: s.config.PRFactory != nil,
		Limits: CapabilityLimits{
			MaxConcurrentOperations: s.config.MaxConcurrentOperations,
			MaxRequestBytes:         s.config.MaxRequestBytes, MaxResponseBytes: s.config.MaxResponseBytes,
			MaxWorkspaceEntries:       s.config.WorkspaceLimits.MaxEntries,
			MaxWorkspaceFileBytes:     s.config.WorkspaceLimits.MaxFileBytes,
			MaxWorkspaceBytes:         s.config.WorkspaceLimits.MaxExpandedBytes,
			MaxWorkspaceArtifactBytes: s.config.WorkspaceLimits.MaxArtifactBytes,
			MaxDeltaBytes:             s.config.MaxDeltaBytes, MaxBundleBytes: s.config.MaxBundleBytes,
			MaxJournalBytes: s.config.MaxJournalBytes,
		},
	}
	if s.config.AllowFileRepositories {
		response.SCMSchemes = append(response.SCMSchemes, "file")
	}
	s.writePublicJSON(writer, http.StatusOK, response)
}

type operationDecoder func([]byte) (OperationMetadata, any, error)
type operationExecutor func(context.Context, OperationMetadata, string, journalRecord, any) (int, any, error)

func (s *Server) serveOperation(writer http.ResponseWriter, request *http.Request, operation Operation, decode operationDecoder, execute operationExecutor) {
	if !s.authenticateBearer(request) {
		s.writePublicJSON(writer, http.StatusUnauthorized, ErrorResponse{Code: "unauthorized", Message: "authorization failed", Retryable: false})
		return
	}
	body, err := s.readBody(writer, request)
	if err != nil {
		s.writeOperationError(writer, OperationMetadata{}, "", err)
		return
	}
	canonical, err := canonicalRequestBody(body)
	if err != nil {
		s.writeOperationError(writer, OperationMetadata{}, "", err)
		return
	}
	requestDigest, err := RequestDigest(http.MethodPost, operation.Path(), canonical)
	if err != nil {
		s.writeOperationError(writer, OperationMetadata{}, "", err)
		return
	}
	metadata, value, err := decode(canonical)
	if err != nil {
		s.writeOperationError(writer, metadata, requestDigest, err)
		return
	}
	if err := metadata.validateFor(operation); err != nil {
		s.writeOperationError(writer, metadata, requestDigest, err)
		return
	}
	expected := NewClaims(operation, metadata, requestDigest, s.config.Now(), s.config.CapabilityTTL)
	if err := VerifyCapability(s.config.OperationCapabilitySecret, request.Header.Get(OperationCapabilityHeader), expected, s.config.Now()); err != nil ||
		!constantEqual(request.Header.Get(OperationRequestDigestHeader), requestDigest) {
		s.writeOperationError(writer, metadata, requestDigest, apiError(ErrUnauthorized, "unauthorized", "authorization failed", http.StatusForbidden, false, nil))
		return
	}
	// Acquire the global operation slot only after the operation capability is
	// verified: an unauthenticated peer must not be able to hold slots (and
	// starve legitimate operations with 429s) by drip-feeding request bodies.
	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	default:
		s.writePublicJSON(writer, http.StatusTooManyRequests, ErrorResponse{Code: "busy", Message: "publisher concurrency limit reached", Retryable: true})
		return
	}
	lock := s.operationLock(metadata.OperationID)
	lock.Lock()
	defer lock.Unlock()
	record, _, err := s.journal.begin(request.Context(), operation, metadata, requestDigest)
	if err != nil {
		s.writeOperationError(writer, metadata, requestDigest, err)
		return
	}
	if record.State == journalCompleted || record.State == journalFailed {
		s.writeBytes(writer, record.StatusCode, record.Response)
		return
	}
	status, response, err := execute(request.Context(), metadata, requestDigest, record, value)
	if err != nil {
		s.writeAndJournalError(writer, metadata, requestDigest, err)
		return
	}
	data, err := encodeJSON(response)
	if err != nil || int64(len(data)) > s.config.MaxResponseBytes {
		s.writeAndJournalError(writer, metadata, requestDigest, apiError(nil, "response_invalid", "publisher response could not be encoded", 500, true, err))
		return
	}
	if err := s.journal.complete(context.WithoutCancel(request.Context()), metadata.OperationID, requestDigest, status, data); err != nil {
		s.writeOperationError(writer, metadata, requestDigest, apiError(ErrJournalFull, "journal_unavailable", "publisher result could not be durably recorded", 507, true, err))
		return
	}
	s.writeBytes(writer, status, data)
}

func (s *Server) authenticateBearer(request *http.Request) bool {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	return constantEqual(strings.TrimPrefix(value, "Bearer "), string(s.config.ControllerBearerToken))
}

func (s *Server) readBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	mediaType := request.Header.Get("Content-Type")
	if mediaType != "application/json" {
		return nil, invalidRequest("content type must be application/json", nil)
	}
	reader := http.MaxBytesReader(writer, request.Body, s.config.MaxRequestBytes)
	defer reader.Close() //nolint:errcheck
	body, err := io.ReadAll(reader)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, apiError(ErrInvalidRequest, "request_too_large", "request body exceeds the configured limit", http.StatusRequestEntityTooLarge, false, nil)
		}
		return nil, invalidRequest("request body could not be read", err)
	}
	if len(body) == 0 {
		return nil, invalidRequest("request body is empty", nil)
	}
	return body, nil
}

func canonicalRequestBody(body []byte) ([]byte, error) {
	canonical, err := harnessv2.CanonicalJSON(body)
	if err != nil {
		return nil, invalidRequest("request body contains invalid or ambiguous JSON", err)
	}
	return canonical, nil
}

func (s *Server) operationLock(operationID string) *sync.Mutex {
	sum := sha256.Sum256([]byte(operationID))
	index := int(sum[0]) % len(s.locks)
	return &s.locks[index]
}

func (s *Server) writeAndJournalError(writer http.ResponseWriter, metadata OperationMetadata, requestDigest string, err error) {
	status, response, terminal := errorResponse(classifyPublisherError(err), metadata, requestDigest)
	data, encodeErr := encodeJSON(response)
	if encodeErr != nil {
		data = []byte("{\"code\":\"internal\",\"message\":\"publisher error response failed\",\"retryable\":true}\n")
		status, terminal = http.StatusInternalServerError, false
	}
	if terminal && metadata.OperationID != "" && requestDigest != "" {
		_ = s.journal.fail(context.Background(), metadata.OperationID, requestDigest, status, data)
	}
	s.writeBytes(writer, status, data)
}

func (s *Server) writeOperationError(writer http.ResponseWriter, metadata OperationMetadata, requestDigest string, err error) {
	status, response, _ := errorResponse(classifyPublisherError(err), metadata, requestDigest)
	s.writePublicJSON(writer, status, response)
}

func (s *Server) writePublicJSON(writer http.ResponseWriter, status int, value any) {
	data, err := encodeJSON(value)
	if err != nil || int64(len(data)) > s.config.MaxResponseBytes {
		status = http.StatusInternalServerError
		data = []byte("{\"code\":\"internal\",\"message\":\"response encoding failed\",\"retryable\":true}\n")
	}
	s.writeBytes(writer, status, data)
}

func (s *Server) writeBytes(writer http.ResponseWriter, status int, data []byte) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	writer.WriteHeader(status)
	_, _ = writer.Write(data)
}

func (s *Server) newPublisher(gitBinary string) (*publisher.Publisher, error) {
	return publisher.New(publisher.Options{
		ArtifactRoot: s.config.ArtifactRoot, TempRoot: s.config.TempRoot, GitBinary: gitBinary,
		MaxDeltaBytes: s.config.MaxDeltaBytes, MaxBundleBytes: s.config.MaxBundleBytes,
		MaxCommandOutput: s.config.MaxCommandOutput, PublishTimeout: s.config.PublishTimeout,
		VerifyAttempts: s.config.VerifyAttempts, VerifyBackoff: s.config.VerifyBackoff,
		ProxyEnvironment: s.config.ProxyEnvironment,
	})
}

func detectGitVersion(binary string) (string, error) {
	command := exec.CommandContext(context.Background(), binary, "version")
	configureCommand(command)
	command.Env = []string{"HOME=/dev/null", gitConfigGlobalNull, gitConfigNoSystem, lcAllC, langC, "PATH=/usr/local/bin:/usr/bin:/bin"}
	output := &limitedBuffer{limit: 256}
	command.Stdout = output
	command.Stderr = &limitedBuffer{limit: 256}
	if err := command.Run(); err != nil || output.truncated {
		return "", fmt.Errorf("inspect Git version")
	}
	value := strings.TrimSpace(output.String())
	const prefix = "git version "
	if !strings.HasPrefix(value, prefix) || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("git returned a malformed version")
	}
	return strings.TrimPrefix(value, prefix), nil
}

func classifyPublisherError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*operationError](err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return apiError(nil, "operation_interrupted", "operation was interrupted before settlement", 503, true, err)
	case errors.Is(err, publisher.ErrInvalidRequest), errors.Is(err, publisher.ErrInvalidRepository),
		errors.Is(err, publisher.ErrInvalidRef), errors.Is(err, publisher.ErrInvalidObjectID):
		return apiError(ErrInvalidRequest, "invalid_request", "publisher request is invalid", 400, false, err)
	case errors.Is(err, publisher.ErrUnsafeDelta), errors.Is(err, publisher.ErrUnsupportedDelta):
		return apiError(ErrInvalidRequest, "unsafe_delta", "workspace delta is unsafe or unsupported", 422, false, err)
	case errors.Is(err, publisher.ErrSourceMoved):
		return apiError(nil, "source_moved", "source ref moved from the persisted baseline", 409, false, err)
	case errors.Is(err, publisher.ErrBranchMoved), errors.Is(err, publisher.ErrBranchClaimMismatch):
		return apiError(nil, "branch_moved", "publication branch no longer matches its durable claim", 409, false, err)
	case errors.Is(err, publisher.ErrIdempotencyConflict):
		return apiError(ErrOperationConflict, "operation_conflict", "publisher operation conflicts with durable state", 409, false, err)
	case errors.Is(err, publisher.ErrPreparedArtifactMissing):
		return apiError(nil, "prepared_artifact_missing", "prepared publication artifact is missing", 409, false, err)
	case errors.Is(err, publisher.ErrPreparedArtifactCorrupt):
		return apiError(nil, "prepared_artifact_corrupt", "prepared publication artifact is corrupt", 500, false, err)
	case errors.Is(err, ErrUnauthorized):
		return apiError(ErrUnauthorized, "unauthorized", "authorization failed", 403, false, nil)
	default:
		return apiError(nil, "upstream_failure", "clean-room SCM operation failed", 502, true, err)
	}
}

func (s *Server) LogStartup(logger *slog.Logger) {
	logger.Info("workspace publisher configured", "listenAddress", s.config.ListenAddress, "protocol", ProtocolVersion, "gitVersion", s.gitVersion)
}

type safeFileInfo struct {
	modeSymlink bool
	directory   bool
}

func osLstat(path string) (safeFileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return safeFileInfo{}, err
	}
	return safeFileInfo{modeSymlink: info.Mode()&os.ModeSymlink != 0, directory: info.IsDir()}, nil
}
