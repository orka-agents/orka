/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	ateapipb "github.com/sozercan/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	substrateReadyInitialPollInterval = 100 * time.Millisecond
	substrateReadyMaxPollInterval     = 2 * time.Second
	substratePlacementLookupTimeout   = 100 * time.Millisecond
	substrateExecInitialPollInterval  = 250 * time.Millisecond
	substrateExecMaxPollInterval      = 2 * time.Second
	substrateDefaultHandoffTokenEnv   = "ORKA_WORKSPACE_HANDOFF_TOKEN"
	substrateDefaultBootstrapTokenEnv = "ORKA_WORKSPACE_BOOTSTRAP_TOKEN"
	substrateHandoffTokenUploadPath   = "orka-workspace-handoff-token"
	substrateSessionCertUploadPath    = "orka-workspace-session.crt"
	substrateSessionKeyUploadPath     = "orka-workspace-session.key"
	substrateDefaultIdentityAudience  = "orka-workspace-daemon"
	substrateDefaultIdentityAppID     = "orka"
	substrateDefaultIdentityUserID    = "orka-worker"

	substrateStatusResuming   = "STATUS_RESUMING"
	substrateStatusRunning    = "STATUS_RUNNING"
	substrateStatusSuspending = "STATUS_SUSPENDING"
	substrateStatusSuspended  = "STATUS_SUSPENDED"
)

// SubstrateConfig configures a Substrate-backed WorkspaceExecutor.
type SubstrateConfig struct {
	APIEndpoint             string
	APICAFile               string
	APIInsecureSkipVerify   bool
	RouterURL               string
	ActorDNSSuffix          string
	HandoffToken            string
	BootstrapToken          string
	HTTPClient              *http.Client
	ControlClient           substrateControlClient
	SessionIdentityToken    string
	SessionIdentityAudience []string
	SessionIdentityAppID    string
	SessionIdentityUserID   string
	SessionIdentityRequired bool
	SessionIdentityMintCert bool
	SessionIdentityClient   substrateSessionIdentityClient
}

// SubstrateOption configures a SubstrateWorkspaceExecutor.
type SubstrateOption func(*SubstrateConfig)

func WithSubstrateControlClient(client substrateControlClient) SubstrateOption {
	return func(c *SubstrateConfig) {
		c.ControlClient = client
	}
}

func WithSubstrateHTTPClient(client *http.Client) SubstrateOption {
	return func(c *SubstrateConfig) {
		c.HTTPClient = client
	}
}

func WithSubstrateHandoffToken(token string) SubstrateOption {
	return func(c *SubstrateConfig) {
		c.HandoffToken = token
	}
}

func WithSubstrateBootstrapToken(token string) SubstrateOption {
	return func(c *SubstrateConfig) {
		c.BootstrapToken = token
	}
}

func WithSubstrateSessionIdentityClient(client substrateSessionIdentityClient) SubstrateOption {
	return func(c *SubstrateConfig) {
		c.SessionIdentityClient = client
	}
}

func normalizeSubstrateIdentityAudience(audience []string) []string {
	normalized := make([]string, 0, len(audience))
	for _, item := range audience {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	if len(normalized) == 0 {
		return []string{substrateDefaultIdentityAudience}
	}
	return normalized
}

// NewSubstrateExecutor returns a WorkspaceExecutor backed by Agent Substrate.
func NewSubstrateExecutor(cfg SubstrateConfig, opts ...SubstrateOption) (*SubstrateWorkspaceExecutor, error) {
	for _, opt := range opts {
		opt(&cfg)
	}
	if strings.TrimSpace(cfg.RouterURL) == "" {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "router URL is required", false, nil)
	}
	if strings.TrimSpace(cfg.ActorDNSSuffix) == "" {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "actor DNS suffix is required", false, nil)
	}
	if cfg.SessionIdentityMintCert {
		return nil, NewError(
			"configure substrate",
			ErrorKindFailedPrecondition,
			"Substrate SessionIdentity certificate minting is not supported yet",
			false,
			nil,
		)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.HandoffToken == "" {
		cfg.HandoffToken = strings.TrimSpace(os.Getenv(substrateDefaultHandoffTokenEnv))
	}
	if cfg.BootstrapToken == "" {
		cfg.BootstrapToken = strings.TrimSpace(os.Getenv(substrateDefaultBootstrapTokenEnv))
	}
	cfg.SessionIdentityAudience = normalizeSubstrateIdentityAudience(cfg.SessionIdentityAudience)
	if strings.TrimSpace(cfg.SessionIdentityAppID) == "" {
		cfg.SessionIdentityAppID = substrateDefaultIdentityAppID
	}
	if strings.TrimSpace(cfg.SessionIdentityUserID) == "" {
		cfg.SessionIdentityUserID = substrateDefaultIdentityUserID
	}
	if cfg.ControlClient == nil {
		client, err := newGRPCSubstrateControlClient(cfg)
		if err != nil {
			return nil, err
		}
		cfg.ControlClient = client
	}
	if cfg.SessionIdentityClient == nil && strings.TrimSpace(cfg.SessionIdentityToken) != "" && strings.TrimSpace(cfg.APIEndpoint) != "" {
		client, err := newGRPCSubstrateSessionIdentityClient(cfg)
		if err != nil {
			return nil, err
		}
		cfg.SessionIdentityClient = client
	}

	return &SubstrateWorkspaceExecutor{
		control:                 cfg.ControlClient,
		sessionIdentity:         cfg.SessionIdentityClient,
		httpClient:              cfg.HTTPClient,
		routerURL:               strings.TrimRight(cfg.RouterURL, "/"),
		actorDNSSuffix:          strings.Trim(strings.TrimSpace(cfg.ActorDNSSuffix), "."),
		handoffToken:            cfg.HandoffToken,
		bootstrapToken:          cfg.BootstrapToken,
		sessionIdentityToken:    strings.TrimSpace(cfg.SessionIdentityToken),
		sessionIdentityAudience: cfg.SessionIdentityAudience,
		sessionIdentityAppID:    strings.TrimSpace(cfg.SessionIdentityAppID),
		sessionIdentityUserID:   strings.TrimSpace(cfg.SessionIdentityUserID),
		sessionIdentityRequired: cfg.SessionIdentityRequired,
		now:                     time.Now,
	}, nil
}

type SubstrateWorkspaceExecutor struct {
	control                 substrateControlClient
	sessionIdentity         substrateSessionIdentityClient
	httpClient              *http.Client
	routerURL               string
	actorDNSSuffix          string
	handoffToken            string
	bootstrapToken          string
	sessionIdentityToken    string
	sessionIdentityAudience []string
	sessionIdentityAppID    string
	sessionIdentityUserID   string
	sessionIdentityRequired bool
	now                     func() time.Time
}

var _ WorkspaceExecutor = (*SubstrateWorkspaceExecutor)(nil)

type substrateControlClient interface {
	GetActor(ctx context.Context, actorID string) (*substrateActor, error)
	CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*substrateActor, error)
	ResumeActor(ctx context.Context, actorID string, boot bool) (*substrateActor, error)
	SuspendActor(ctx context.Context, actorID string) (*substrateActor, error)
	DeleteActor(ctx context.Context, actorID string) error
	ListWorkers(ctx context.Context) ([]substrateWorker, error)
	ListActors(ctx context.Context) ([]substrateActor, error)
}

type substrateSessionIdentityClient interface {
	MintJWT(ctx context.Context, req substrateMintJWTRequest, bearerToken string) (string, error)
}

type substrateActor struct {
	ActorID            string
	TemplateNamespace  string
	TemplateName       string
	Status             string
	PodNamespace       string
	PodName            string
	PodIP              string
	LastSnapshot       string
	InProgressSnapshot string
}

type substrateMintJWTRequest struct {
	Audience  []string
	AppID     string
	UserID    string
	SessionID string
}

type substrateWorker struct {
	WorkerNamespace string
	WorkerPool      string
	WorkerPod       string
	ActorID         string
	IP              string
}

func (e *SubstrateWorkspaceExecutor) Claim(ctx context.Context, req ClaimRequest) (*ClaimResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("claim", err)
	}
	actorID := strings.TrimSpace(req.ClaimName)
	if actorID == "" {
		return nil, NewError("claim", ErrorKindInvalidArgument, "claim name must contain the Substrate actor id", false, nil)
	}
	if strings.TrimSpace(req.Template.Namespace) == "" || strings.TrimSpace(req.Template.Name) == "" {
		return nil, NewError("claim", ErrorKindInvalidArgument, "template namespace and name are required", false, nil)
	}

	actor, err := e.control.GetActor(ctx, actorID)
	if err == nil {
		return e.reattachedSubstrateClaimResult(req, actor)
	}
	if !IsKind(err, ErrorKindNotFound) {
		return nil, err
	}
	if !req.CreateIfMissing {
		return nil, err
	}

	actor, err = e.control.CreateActor(ctx, actorID, req.Template.Namespace, req.Template.Name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, contextError("claim", ctxErr)
		}
		if IsKind(err, ErrorKindAlreadyExists) {
			actor, err = e.control.GetActor(ctx, actorID)
			if err == nil {
				return e.reattachedSubstrateClaimResult(req, actor)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, contextError("claim", ctxErr)
			}
		}
		return nil, err
	}
	now := e.now()
	return &ClaimResult{
		Ref:       substrateRef(req.Template.Namespace, actor),
		Template:  req.Template,
		ReuseKey:  req.ReuseKey,
		Created:   true,
		Phase:     PhasePending,
		Message:   "workspace actor created",
		ClaimedAt: now,
		Placement: substratePlacement(actor, nil),
	}, nil
}

func (e *SubstrateWorkspaceExecutor) reattachedSubstrateClaimResult(
	req ClaimRequest,
	actor *substrateActor,
) (*ClaimResult, error) {
	if err := validateSubstrateActorTemplate(actor, req.Template); err != nil {
		return nil, err
	}
	return &ClaimResult{
		Ref:       substrateRef(req.Template.Namespace, actor),
		Template:  req.Template,
		ReuseKey:  req.ReuseKey,
		Reused:    true,
		Phase:     substratePhase(actor),
		Message:   "workspace actor reattached",
		Placement: substratePlacement(actor, nil),
	}, nil
}

func validateSubstrateActorTemplate(actor *substrateActor, template TemplateRef) error {
	return validateSubstrateActorTemplateForOp("claim", actor, template)
}

func validateSubstrateActorTemplateForOp(op string, actor *substrateActor, template TemplateRef) error {
	if actor == nil {
		return NewError(op, ErrorKindFailedPrecondition, "Substrate actor lookup returned no actor", false, nil)
	}
	actualNamespace := strings.TrimSpace(actor.TemplateNamespace)
	actualName := strings.TrimSpace(actor.TemplateName)
	wantNamespace := strings.TrimSpace(template.Namespace)
	wantName := strings.TrimSpace(template.Name)
	if actualNamespace == wantNamespace && actualName == wantName {
		return nil
	}
	return NewError(
		op,
		ErrorKindFailedPrecondition,
		fmt.Sprintf(
			"existing Substrate actor uses template %s/%s, want %s/%s",
			actualNamespace,
			actualName,
			wantNamespace,
			wantName,
		),
		false,
		nil,
	)
}

// Close releases network resources owned by this executor.
func (e *SubstrateWorkspaceExecutor) Close() error {
	var closeErr error
	if closer, ok := e.control.(interface{ Close() error }); ok {
		closeErr = errors.Join(closeErr, closer.Close())
	}
	if closer, ok := e.sessionIdentity.(interface{ Close() error }); ok {
		closeErr = errors.Join(closeErr, closer.Close())
	}
	return closeErr
}

func (e *SubstrateWorkspaceExecutor) WaitReady(ctx context.Context, req WaitReadyRequest) (*ReadyResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("wait ready", err)
	}
	actorID := substrateActorID(req.Ref)
	if actorID == "" {
		return nil, NewError("wait ready", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}
	if strings.TrimSpace(req.SnapshotRestoreURI) != "" {
		return nil, NewError(
			"wait ready",
			ErrorKindFailedPrecondition,
			"explicit Substrate snapshot restore is not available through the public control API yet",
			false,
			nil,
		)
	}
	resumeStartedAt := e.now()
	if _, err := e.control.ResumeActor(ctx, actorID, req.Boot); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, contextError("wait ready", ctxErr)
		}
		return nil, err
	}

	backoff := substrateReadyInitialPollInterval
	for {
		actor, err := e.control.GetActor(ctx, actorID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, contextError("wait ready", ctxErr)
			}
			if !retryableWorkspaceError(err) {
				return nil, err
			}
		}
		if err == nil && actor.Status == substrateStatusRunning && strings.TrimSpace(actor.PodIP) != "" {
			if err := e.daemonRequest(ctx, actorID, http.MethodGet, "/healthz", nil, nil); err == nil {
				readyAt := e.now()
				resumeLatency := max(readyAt.Sub(resumeStartedAt), 0)
				placement, density := e.substrateTelemetry(ctx, actor)
				return &ReadyResult{
					Ref:           substrateRef(req.Ref.Namespace, actor),
					Phase:         PhaseReady,
					Message:       "workspace ready",
					ReadyAt:       readyAt,
					Placement:     placement,
					Density:       density,
					ResumeLatency: resumeLatency,
				}, nil
			} else {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, contextError("wait ready", ctxErr)
				}
				if !retryableWorkspaceError(err) {
					return nil, err
				}
			}
		}
		if err := sleepContext(ctx, backoff); err != nil {
			return nil, contextError("wait ready", err)
		}
		backoff = min(backoff*2, substrateReadyMaxPollInterval)
		if backoff <= 0 {
			backoff = substrateReadyInitialPollInterval
		}
	}
}

func (e *SubstrateWorkspaceExecutor) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return nil, NewError("exec", ErrorKindInvalidArgument, "command is required", false, nil)
	}
	actorID := substrateActorID(req.Ref)
	if actorID == "" {
		return nil, NewError("exec", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}
	if req.Resident {
		return nil, NewError(
			"exec",
			ErrorKindFailedPrecondition,
			"Substrate resident execution is not supported yet",
			false,
			nil,
		)
	}

	var resp substrateExecResponse
	body := substrateExecRequest{
		Command:        append([]string(nil), req.Command...),
		Env:            copyStringMap(req.Env),
		WorkDir:        req.WorkDir,
		Stdin:          append([]byte(nil), req.Stdin...),
		TimeoutSeconds: int64(req.Timeout / time.Second),
		MaxOutputBytes: req.MaxOutputBytes,
		Detach:         true,
	}
	if err := e.daemonRequest(ctx, actorID, http.MethodPost, "/v1/exec", body, &resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, contextError("exec", ctxErr)
		}
		return nil, err
	}
	if resp.ExecID != "" {
		polled, err := e.pollExec(ctx, actorID, resp.ExecID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, contextError("exec", ctxErr)
			}
			return nil, err
		}
		resp = *polled
	}
	result := &ExecResult{
		Ref:             req.Ref,
		Command:         append([]string(nil), req.Command...),
		Stdout:          resp.Stdout,
		Stderr:          resp.Stderr,
		ExitCode:        resp.ExitCode,
		StartedAt:       resp.StartedAt,
		FinishedAt:      resp.FinishedAt,
		StdoutTruncated: resp.StdoutTruncated,
		StderrTruncated: resp.StderrTruncated,
	}
	if result.ExitCode != 0 {
		return result, NewError("exec", ErrorKindCommandFailed, fmt.Sprintf("command exited with code %d", result.ExitCode), false, nil)
	}
	return result, nil
}

func (e *SubstrateWorkspaceExecutor) pollExec(ctx context.Context, actorID, execID string) (*substrateExecResponse, error) {
	backoff := substrateExecInitialPollInterval
	for {
		var resp substrateExecResponse
		if err := e.daemonRequest(ctx, actorID, http.MethodGet, "/v1/exec/"+url.PathEscape(execID), nil, &resp); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, contextError("exec", ctxErr)
			}
			if !retryableWorkspaceError(err) {
				return nil, err
			}
		} else if !resp.Running {
			return &resp, nil
		}
		if err := sleepContext(ctx, backoff); err != nil {
			return nil, contextError("exec", err)
		}
		backoff = min(backoff*2, substrateExecMaxPollInterval)
		if backoff <= 0 {
			backoff = substrateExecInitialPollInterval
		}
	}
}

func (e *SubstrateWorkspaceExecutor) Upload(ctx context.Context, req UploadRequest) (*UploadResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if len(req.Artifacts) == 0 {
		return nil, NewError("upload", ErrorKindInvalidArgument, "at least one artifact is required", false, nil)
	}
	actorID := substrateActorID(req.Ref)
	if actorID == "" {
		return nil, NewError("upload", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}
	files := make([]substrateUploadFile, 0, len(req.Artifacts))
	for _, artifact := range req.Artifacts {
		files = append(files, substrateUploadFile{
			Path:    artifact.Path,
			Data:    append([]byte(nil), artifact.Data...),
			Mode:    artifact.Mode,
			ModTime: artifact.ModTime,
		})
	}
	var resp substrateUploadResponse
	authToken := e.handoffToken
	if req.BootstrapHandoff {
		bootstrapToken, err := e.requireBootstrapToken("upload")
		if err != nil {
			return nil, err
		}
		authToken = bootstrapToken
		mintedToken, err := e.mintSessionIdentityHandoffToken(ctx, req.Ref)
		if err != nil {
			return nil, err
		}
		if mintedToken != "" {
			e.handoffToken = mintedToken
			replaceSubstrateHandoffUploadToken(files, mintedToken)
		}
	}
	if err := e.daemonRequestWithAuthToken(
		ctx,
		actorID,
		http.MethodPut,
		"/v1/files",
		substrateUploadRequest{Files: files},
		&resp,
		authToken,
	); err != nil {
		return nil, err
	}
	return &UploadResult{Ref: req.Ref, Artifacts: resp.Artifacts}, nil
}

func (e *SubstrateWorkspaceExecutor) mintSessionIdentityHandoffToken(ctx context.Context, ref WorkspaceRef) (string, error) {
	hasClient := e.sessionIdentity != nil
	hasToken := strings.TrimSpace(e.sessionIdentityToken) != ""
	if !hasClient && !hasToken && !e.sessionIdentityRequired {
		return "", nil
	}
	if !hasClient || !hasToken {
		return "", NewError(
			"mint session identity",
			ErrorKindFailedPrecondition,
			"Substrate SessionIdentity is configured incompletely",
			false,
			nil,
		)
	}
	actorID := substrateActorID(ref)
	if actorID == "" {
		return "", NewError("mint session identity", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}
	token, err := e.sessionIdentity.MintJWT(ctx, substrateMintJWTRequest{
		Audience:  append([]string(nil), e.sessionIdentityAudience...),
		AppID:     e.sessionIdentityAppID,
		UserID:    e.sessionIdentityUserID,
		SessionID: actorID,
	}, e.sessionIdentityToken)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", NewError("mint session identity", ErrorKindFailedPrecondition, "Substrate SessionIdentity returned an empty JWT", false, nil)
	}
	return token, nil
}

func replaceSubstrateHandoffUploadToken(files []substrateUploadFile, token string) {
	if len(files) == 1 {
		files[0].Data = []byte(token)
		return
	}
	for i := range files {
		if strings.Contains(files[i].Path, "handoff-token") {
			files[i].Data = []byte(token)
		}
	}
}

func (e *SubstrateWorkspaceExecutor) Download(ctx context.Context, req DownloadRequest) (*DownloadResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	actorID := substrateActorID(req.Ref)
	if actorID == "" {
		return nil, NewError("download", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}
	var resp substrateDownloadResponse
	if err := e.daemonRequest(ctx, actorID, http.MethodPost, "/v1/files/download", substrateDownloadRequest{Paths: req.Paths}, &resp); err != nil {
		return nil, err
	}
	return &DownloadResult{Ref: req.Ref, Artifacts: resp.Artifacts}, nil
}

func (e *SubstrateWorkspaceExecutor) Release(ctx context.Context, req ReleaseRequest) (*ReleaseResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	actorID := substrateActorID(req.Ref)
	if actorID == "" {
		return nil, NewError("release", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}
	if strings.TrimSpace(req.SnapshotCheckpointURI) != "" {
		return nil, NewError(
			"release",
			ErrorKindFailedPrecondition,
			"explicit Substrate snapshot checkpoint is not available through the public control API yet",
			false,
			nil,
		)
	}
	if !req.SkipScrub {
		if err := e.scrubDaemon(ctx, actorID); err != nil {
			return nil, NewError("release", ErrorKindFailedPrecondition, "failed to scrub workspace before release", false, err)
		}
	}
	actor, err := e.suspendActorAndWait(ctx, actorID)
	if err != nil {
		if restoreErr := e.restoreHandoffToken(ctx, actorID); restoreErr != nil {
			return nil, NewError(
				"release",
				ErrorKindFailedPrecondition,
				"failed to restore workspace handoff token after release failure",
				true,
				errors.Join(err, restoreErr),
			)
		}
		return nil, err
	}
	if req.Retain {
		return &ReleaseResult{Ref: substrateRef(req.Ref.Namespace, actor), Retained: true, Phase: PhaseRetained, Message: releaseMessage(req.Reason, "workspace retained")}, nil
	}
	return &ReleaseResult{Ref: substrateRef(req.Ref.Namespace, actor), Released: true, Phase: PhaseReleased, Message: releaseMessage(req.Reason, "workspace released")}, nil
}

func (e *SubstrateWorkspaceExecutor) Delete(ctx context.Context, req DeleteRequest) (*DeleteResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	actorID := substrateActorID(req.Ref)
	if actorID == "" {
		return nil, NewError("delete", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}

	actor, err := e.control.GetActor(ctx, actorID)
	if err != nil {
		if IsKind(err, ErrorKindNotFound) {
			return &DeleteResult{Ref: req.Ref, Deleted: false, Phase: PhaseDeleted, Message: "workspace already deleted"}, nil
		}
		return nil, err
	}
	scrubbed := false
	var scrubErr error
	if actor.Status == substrateStatusRunning && !req.SkipScrub {
		if err := e.scrubDaemon(ctx, actorID); err != nil {
			scrubErr = err
		} else {
			scrubbed = true
		}
	}
	if actor.Status != substrateStatusSuspended {
		if actor, err = e.suspendActorAndWait(ctx, actorID); err != nil {
			if scrubbed {
				if restoreErr := e.restoreHandoffToken(ctx, actorID); restoreErr != nil {
					return nil, NewError(
						"delete",
						ErrorKindFailedPrecondition,
						"failed to restore workspace handoff token after delete failure",
						true,
						errors.Join(err, restoreErr),
					)
				}
			}
			if scrubErr != nil {
				return nil, NewError(
					"delete",
					ErrorKindFailedPrecondition,
					"failed to delete workspace after scrub failed",
					true,
					errors.Join(scrubErr, err),
				)
			}
			return nil, err
		}
	}
	if err := e.control.DeleteActor(ctx, actorID); err != nil {
		if scrubErr != nil {
			return nil, NewError(
				"delete",
				ErrorKindFailedPrecondition,
				"failed to delete workspace after scrub failed",
				true,
				errors.Join(scrubErr, err),
			)
		}
		return nil, err
	}
	return &DeleteResult{Ref: substrateRef(req.Ref.Namespace, actor), Deleted: true, Phase: PhaseDeleted, Message: releaseMessage(req.Reason, "workspace deleted")}, nil
}

func (e *SubstrateWorkspaceExecutor) suspendActorAndWait(ctx context.Context, actorID string) (*substrateActor, error) {
	actor, err := e.control.SuspendActor(ctx, actorID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, contextError("suspend actor", ctxErr)
		}
		observed, getErr := e.control.GetActor(ctx, actorID)
		if getErr != nil || !substrateActorSuspendingOrSuspended(observed) {
			return nil, err
		}
		actor = observed
	}
	if actor.Status == substrateStatusSuspended {
		return actor, nil
	}
	return e.waitActorStatus(ctx, actorID, substrateStatusSuspended)
}

func (e *SubstrateWorkspaceExecutor) waitActorStatus(
	ctx context.Context,
	actorID string,
	expected string,
) (*substrateActor, error) {
	backoff := substrateReadyInitialPollInterval
	for {
		actor, err := e.control.GetActor(ctx, actorID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, contextError("wait actor status", ctxErr)
			}
			return nil, err
		}
		if actor.Status == expected {
			return actor, nil
		}
		if err := sleepContext(ctx, backoff); err != nil {
			return nil, contextError("wait actor status", err)
		}
		backoff = min(backoff*2, substrateReadyMaxPollInterval)
		if backoff <= 0 {
			backoff = substrateReadyInitialPollInterval
		}
	}
}

func substrateActorSuspendingOrSuspended(actor *substrateActor) bool {
	if actor == nil {
		return false
	}
	return actor.Status == substrateStatusSuspending || actor.Status == substrateStatusSuspended
}

func (e *SubstrateWorkspaceExecutor) Describe(ctx context.Context, req DescribeRequest) (*Description, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	actorID := substrateActorID(req.Ref)
	if actorID == "" {
		return nil, NewError("describe", ErrorKindInvalidArgument, "actor id is required", false, nil)
	}
	actor, err := e.control.GetActor(ctx, actorID)
	if err != nil {
		if IsKind(err, ErrorKindNotFound) {
			return &Description{Ref: req.Ref, Phase: PhaseDeleted, DeletedAt: e.now(), Message: "workspace deleted"}, nil
		}
		return nil, err
	}
	retained := substrateActorRetained(actor)
	placement, density := e.substrateTelemetry(ctx, actor)
	return &Description{
		Ref:       substrateRef(req.Ref.Namespace, actor),
		Template:  TemplateRef{Namespace: actor.TemplateNamespace, Name: actor.TemplateName},
		Phase:     substratePhase(actor),
		Retained:  retained,
		Message:   "workspace described",
		Placement: placement,
		Density:   density,
	}, nil
}

// SubstratePoolTelemetry reports safe actor/worker density for a single Orka pool.
func (e *SubstrateWorkspaceExecutor) SubstratePoolTelemetry(
	ctx context.Context,
	prefix string,
	template TemplateRef,
	workerPool TemplateRef,
) (Density, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "-")
	workers, err := e.control.ListWorkers(ctx)
	if err != nil {
		return Density{}, err
	}
	actors, err := e.control.ListActors(ctx)
	if err != nil {
		return Density{}, err
	}
	filteredActors := make([]substrateActor, 0, len(actors))
	actorIDs := make(map[string]struct{}, len(actors))
	for _, actor := range actors {
		actorID := strings.TrimSpace(actor.ActorID)
		if prefix != "" && !strings.HasPrefix(actorID, prefix+"-") {
			continue
		}
		if strings.TrimSpace(template.Namespace) != "" && strings.TrimSpace(actor.TemplateNamespace) != strings.TrimSpace(template.Namespace) {
			continue
		}
		if strings.TrimSpace(template.Name) != "" && strings.TrimSpace(actor.TemplateName) != strings.TrimSpace(template.Name) {
			continue
		}
		filteredActors = append(filteredActors, actor)
		actorIDs[actorID] = struct{}{}
	}
	filteredWorkers := make([]substrateWorker, 0, len(workers))
	for _, worker := range workers {
		if strings.TrimSpace(workerPool.Name) != "" && strings.TrimSpace(worker.WorkerPool) != strings.TrimSpace(workerPool.Name) {
			continue
		}
		if strings.TrimSpace(workerPool.Namespace) != "" && strings.TrimSpace(worker.WorkerNamespace) != strings.TrimSpace(workerPool.Namespace) {
			continue
		}
		if workerActorID := strings.TrimSpace(worker.ActorID); workerActorID != "" {
			if _, ok := actorIDs[workerActorID]; !ok {
				continue
			}
		} else if strings.TrimSpace(workerPool.Name) == "" {
			continue
		}
		filteredWorkers = append(filteredWorkers, worker)
	}
	return substrateDensity(filteredWorkers, filteredActors), nil
}

// EnsureSubstrateActors creates deterministic actor records for a pool target.
func (e *SubstrateWorkspaceExecutor) EnsureSubstrateActors(
	ctx context.Context,
	prefix string,
	target int,
	template TemplateRef,
) (int, error) {
	if target <= 0 {
		return 0, nil
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "-")
	if prefix == "" {
		return 0, NewError("ensure substrate actors", ErrorKindInvalidArgument, "actor prefix is required", false, nil)
	}
	created := 0
	for i := range target {
		actorID := deterministicSubstratePoolActorID(prefix, i)
		if actor, err := e.control.GetActor(ctx, actorID); err == nil {
			if err := validateSubstrateActorTemplateForOp("ensure substrate actors", actor, template); err != nil {
				return created, err
			}
			continue
		} else if !IsKind(err, ErrorKindNotFound) {
			return created, err
		}
		if _, err := e.control.CreateActor(ctx, actorID, template.Namespace, template.Name); err != nil {
			if IsKind(err, ErrorKindAlreadyExists) {
				actor, getErr := e.control.GetActor(ctx, actorID)
				if getErr != nil {
					return created, getErr
				}
				if err := validateSubstrateActorTemplateForOp("ensure substrate actors", actor, template); err != nil {
					return created, err
				}
				continue
			}
			return created, err
		}
		created++
	}
	return created, nil
}

// ConvergeSubstrateActors creates missing deterministic actors below target and
// deletes deterministic pool actors at or above target.
func (e *SubstrateWorkspaceExecutor) ConvergeSubstrateActors(
	ctx context.Context,
	prefix string,
	target int,
	template TemplateRef,
) (int, int, error) {
	if target < 0 {
		return 0, 0, NewError("converge substrate actors", ErrorKindInvalidArgument, "actor target must be non-negative", false, nil)
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "-")
	if prefix == "" {
		return 0, 0, NewError("converge substrate actors", ErrorKindInvalidArgument, "actor prefix is required", false, nil)
	}

	created := 0
	if target > 0 {
		var err error
		created, err = e.EnsureSubstrateActors(ctx, prefix, target, template)
		if err != nil {
			return created, 0, err
		}
	}

	deleted, err := e.PruneSubstrateActors(ctx, prefix, target)
	if err != nil {
		return created, deleted, err
	}
	return created, deleted, nil
}

// PruneSubstrateActors deletes deterministic pool actors at or above target.
func (e *SubstrateWorkspaceExecutor) PruneSubstrateActors(
	ctx context.Context,
	prefix string,
	target int,
) (int, error) {
	if target < 0 {
		return 0, NewError("prune substrate actors", ErrorKindInvalidArgument, "actor target must be non-negative", false, nil)
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "-")
	if prefix == "" {
		return 0, NewError("prune substrate actors", ErrorKindInvalidArgument, "actor prefix is required", false, nil)
	}

	actors, err := e.control.ListActors(ctx)
	if err != nil {
		return 0, err
	}
	actorsByOrdinal := make(map[int]string)
	ordinals := make([]int, 0)
	for _, actor := range actors {
		ordinal, ok := substratePoolActorOrdinal(actor.ActorID, prefix)
		if !ok || ordinal < target {
			continue
		}
		if _, exists := actorsByOrdinal[ordinal]; exists {
			continue
		}
		actorsByOrdinal[ordinal] = strings.TrimSpace(actor.ActorID)
		ordinals = append(ordinals, ordinal)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ordinals)))

	deleted := 0
	for _, ordinal := range ordinals {
		if err := e.control.DeleteActor(ctx, actorsByOrdinal[ordinal]); err != nil {
			if IsKind(err, ErrorKindNotFound) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func deterministicSubstratePoolActorID(prefix string, ordinal int) string {
	return fmt.Sprintf("%s-%05d", prefix, ordinal)
}

func substratePoolActorOrdinal(actorID, prefix string) (int, bool) {
	actorID = strings.TrimSpace(actorID)
	prefix = strings.Trim(strings.TrimSpace(prefix), "-")
	suffix, ok := strings.CutPrefix(actorID, prefix+"-")
	if !ok || len(suffix) != 5 {
		return 0, false
	}
	ordinal := 0
	for _, ch := range suffix {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		ordinal = ordinal*10 + int(ch-'0')
	}
	return ordinal, true
}

func (e *SubstrateWorkspaceExecutor) substrateTelemetry(ctx context.Context, actor *substrateActor) (Placement, Density) {
	lookupCtx, cancel := substratePlacementLookupContext(ctx)
	if lookupCtx == nil {
		return substratePlacement(actor, nil), Density{}
	}
	defer cancel()
	workers, err := e.control.ListWorkers(lookupCtx)
	if err != nil {
		return substratePlacement(actor, nil), Density{}
	}
	actors, err := e.control.ListActors(lookupCtx)
	if err != nil {
		return substratePlacement(actor, workers), Density{}
	}
	return substratePlacement(actor, workers), substrateDensity(workers, actors)
}

func substratePlacementLookupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), substratePlacementLookupTimeout)
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= substratePlacementLookupTimeout {
			return nil, func() {}
		}
		return context.WithTimeout(ctx, substratePlacementLookupTimeout)
	}
	return context.WithTimeout(ctx, substratePlacementLookupTimeout)
}

func substratePlacement(actor *substrateActor, workers []substrateWorker) Placement {
	placement := Placement{}
	if actor != nil {
		placement.WorkerNamespace = strings.TrimSpace(actor.PodNamespace)
		placement.WorkerPodName = strings.TrimSpace(actor.PodName)
		placement.PodIP = strings.TrimSpace(actor.PodIP)
	}
	if actor == nil {
		return placement
	}
	for _, worker := range workers {
		if strings.TrimSpace(worker.ActorID) != strings.TrimSpace(actor.ActorID) {
			continue
		}
		if namespace := strings.TrimSpace(worker.WorkerNamespace); namespace != "" {
			placement.WorkerNamespace = namespace
		}
		if pool := strings.TrimSpace(worker.WorkerPool); pool != "" {
			placement.WorkerPool = pool
		}
		if pod := strings.TrimSpace(worker.WorkerPod); pod != "" {
			placement.WorkerPodName = pod
		}
		if ip := strings.TrimSpace(worker.IP); ip != "" {
			placement.PodIP = ip
		}
		return placement
	}
	return placement
}

func substrateDensity(workers []substrateWorker, actors []substrateActor) Density {
	workerCount := len(workers)
	actorCount := len(actors)
	if workerCount == 0 && actorCount == 0 {
		return Density{}
	}
	density := Density{
		WorkerCount: workerCount,
		ActorCount:  actorCount,
	}
	for _, actor := range actors {
		switch strings.TrimSpace(actor.Status) {
		case substrateStatusRunning, substrateStatusResuming, substrateStatusSuspending:
			density.RunningActorCount++
		case substrateStatusSuspended:
			density.SuspendedActorCount++
		}
	}
	if workerCount > 0 {
		density.ActorsPerWorker = fmt.Sprintf("%.2f", float64(actorCount)/float64(workerCount))
	}
	return density
}

func (e *SubstrateWorkspaceExecutor) scrubDaemon(ctx context.Context, actorID string) error {
	return e.daemonRequest(ctx, actorID, http.MethodPost, "/v1/scrub", substrateScrubRequest{Paths: defaultSubstrateScrubPaths()}, nil)
}

func (e *SubstrateWorkspaceExecutor) restoreHandoffToken(ctx context.Context, actorID string) error {
	if strings.TrimSpace(e.handoffToken) == "" {
		return nil
	}
	bootstrapToken, err := e.requireBootstrapToken("restore handoff token")
	if err != nil {
		return err
	}
	restoreCtx := ctx
	if restoreCtx == nil || restoreCtx.Err() != nil {
		restoreCtx = context.Background()
	}
	restoreCtx, cancel := context.WithTimeout(restoreCtx, 10*time.Second)
	defer cancel()

	return e.daemonRequestWithAuthToken(
		restoreCtx,
		actorID,
		http.MethodPut,
		"/v1/files",
		substrateUploadRequest{
			Files: []substrateUploadFile{{
				Path: substrateHandoffTokenUploadPath,
				Data: []byte(e.handoffToken),
				Mode: 0o600,
			}},
		},
		nil,
		bootstrapToken,
	)
}

func (e *SubstrateWorkspaceExecutor) daemonRequest(ctx context.Context, actorID, method, path string, body any, out any) error {
	return e.daemonRequestWithAuthToken(ctx, actorID, method, path, body, out, e.handoffToken)
}

func (e *SubstrateWorkspaceExecutor) daemonRequestWithAuthToken(
	ctx context.Context,
	actorID string,
	method string,
	path string,
	body any,
	out any,
	authToken string,
) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return NewError("daemon request", ErrorKindInvalidArgument, "failed to encode request", false, err)
		}
		reader = bytes.NewReader(data)
	}
	endpoint, err := e.daemonURL(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return NewError("daemon request", ErrorKindInvalidArgument, "failed to create request", false, err)
	}
	req.Host = actorID + "." + e.actorDNSSuffix
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(authToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(authToken))
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return NewError("daemon request", ErrorKindUnknown, "daemon request failed", true, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return NewError("daemon request", ErrorKindUnknown, fmt.Sprintf("daemon returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data))), resp.StatusCode >= 500, nil)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return NewError("daemon request", ErrorKindUnknown, "failed to decode response", false, err)
	}
	return nil
}

func (e *SubstrateWorkspaceExecutor) requireBootstrapToken(op string) (string, error) {
	token := strings.TrimSpace(e.bootstrapToken)
	if token == "" {
		return "", NewError(op, ErrorKindFailedPrecondition, "workspace bootstrap token is required", false, nil)
	}
	return token, nil
}

func retryableWorkspaceError(err error) bool {
	var workspaceErr *Error
	if errors.As(err, &workspaceErr) {
		return workspaceErr.Retryable
	}
	return true
}

func (e *SubstrateWorkspaceExecutor) daemonURL(path string) (string, error) {
	base, err := url.Parse(e.routerURL)
	if err != nil {
		return "", NewError("daemon request", ErrorKindInvalidArgument, "invalid router URL", false, err)
	}
	cleanPath := "/" + strings.TrimLeft(path, "/")
	base.Path = strings.TrimRight(base.Path, "/") + cleanPath
	return base.String(), nil
}

func substrateActorID(ref WorkspaceRef) string {
	if strings.TrimSpace(ref.ID) != "" {
		return strings.TrimSpace(ref.ID)
	}
	return strings.TrimSpace(ref.ClaimName)
}

func substrateRef(namespace string, actor *substrateActor) WorkspaceRef {
	if actor == nil {
		return WorkspaceRef{Namespace: namespace}
	}
	return WorkspaceRef{
		Namespace: namespace,
		ClaimName: actor.ActorID,
		ID:        actor.ActorID,
	}
}

func substrateActorRetained(actor *substrateActor) bool {
	return actor != nil && actor.Status == substrateStatusSuspended
}

func substratePhase(actor *substrateActor) Phase {
	if actor == nil {
		return PhaseDeleted
	}
	switch actor.Status {
	case substrateStatusResuming:
		return PhasePending
	case substrateStatusRunning:
		return PhaseReady
	case substrateStatusSuspending:
		return PhaseReleased
	case substrateStatusSuspended:
		return PhaseRetained
	default:
		return PhaseFailed
	}
}

func defaultSubstrateScrubPaths() []string {
	return []string{
		"/app/orka-agent-worker",
		"/app/orka-sa-token",
		"/app/orka-transaction-token",
		"/app/orka-context-subject-token",
		"/app/orka-git-askpass",
		"/app/orka-workspace-handoff-token",
		"/app/" + substrateSessionCertUploadPath,
		"/app/" + substrateSessionKeyUploadPath,
	}
}

type substrateExecRequest struct {
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env,omitempty"`
	WorkDir        string            `json:"workDir,omitempty"`
	Stdin          []byte            `json:"stdin,omitempty"`
	TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int64             `json:"maxOutputBytes,omitempty"`
	Detach         bool              `json:"detach,omitempty"`
	Resident       bool              `json:"resident,omitempty"`
	ResidentKey    string            `json:"residentKey,omitempty"`
}

type substrateExecResponse struct {
	ExecID          string    `json:"execId,omitempty"`
	Running         bool      `json:"running,omitempty"`
	Stdout          string    `json:"stdout"`
	Stderr          string    `json:"stderr"`
	ExitCode        int       `json:"exitCode"`
	StdoutTruncated bool      `json:"stdoutTruncated"`
	StderrTruncated bool      `json:"stderrTruncated"`
	StartedAt       time.Time `json:"startedAt"`
	FinishedAt      time.Time `json:"finishedAt"`
}

type substrateUploadRequest struct {
	Files []substrateUploadFile `json:"files"`
}

type substrateUploadFile struct {
	Path    string    `json:"path"`
	Data    []byte    `json:"data"`
	Mode    uint32    `json:"mode,omitempty"`
	ModTime time.Time `json:"modTime,omitempty"`
}

type substrateUploadResponse struct {
	Artifacts []Artifact `json:"artifacts"`
}

type substrateDownloadRequest struct {
	Paths []string `json:"paths,omitempty"`
}

type substrateDownloadResponse struct {
	Artifacts []DownloadedArtifact `json:"artifacts"`
}

type substrateScrubRequest struct {
	Paths []string `json:"paths"`
}

type grpcSubstrateControlClient struct {
	conn   *grpc.ClientConn
	client ateapipb.ControlClient
}

type grpcSubstrateSessionIdentityClient struct {
	conn   *grpc.ClientConn
	client ateapipb.SessionIdentityClient
}

func newGRPCSubstrateControlClient(cfg SubstrateConfig) (*grpcSubstrateControlClient, error) {
	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "API endpoint is required", false, nil)
	}
	transportCredentials, err := substrateTransportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.APIEndpoint, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return nil, NewError("configure substrate", ErrorKindUnknown, "failed to create Substrate API client", false, err)
	}
	return &grpcSubstrateControlClient{conn: conn, client: ateapipb.NewControlClient(conn)}, nil
}

func newGRPCSubstrateSessionIdentityClient(cfg SubstrateConfig) (*grpcSubstrateSessionIdentityClient, error) {
	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "API endpoint is required", false, nil)
	}
	transportCredentials, err := substrateTransportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.APIEndpoint, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return nil, NewError("configure substrate", ErrorKindUnknown, "failed to create Substrate SessionIdentity client", false, err)
	}
	return &grpcSubstrateSessionIdentityClient{conn: conn, client: ateapipb.NewSessionIdentityClient(conn)}, nil
}

func (c *grpcSubstrateControlClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *grpcSubstrateSessionIdentityClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func substrateTransportCredentials(cfg SubstrateConfig) (credentials.TransportCredentials, error) {
	if cfg.APIInsecureSkipVerify {
		return credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}), nil //nolint:gosec // explicit local smoke-test option
	}
	if strings.TrimSpace(cfg.APICAFile) == "" {
		return nil, NewError(
			"configure substrate",
			ErrorKindInvalidArgument,
			"Substrate API trust requires a CA file or insecure skip verify",
			false,
			nil,
		)
	}
	data, err := os.ReadFile(cfg.APICAFile)
	if err != nil {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "failed to read Substrate API CA file", false, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, NewError("configure substrate", ErrorKindInvalidArgument, "Substrate API CA file has no PEM certificates", false, nil)
	}
	return credentials.NewTLS(&tls.Config{RootCAs: pool}), nil
}

func (c *grpcSubstrateControlClient) GetActor(ctx context.Context, actorID string) (*substrateActor, error) {
	resp, err := c.client.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: actorID})
	if err != nil {
		return nil, substrateControlError("get actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

func (c *grpcSubstrateControlClient) CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*substrateActor, error) {
	resp, err := c.client.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId:                actorID,
		ActorTemplateNamespace: templateNamespace,
		ActorTemplateName:      templateName,
	})
	if err != nil {
		return nil, substrateControlError("create actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

func (c *grpcSubstrateControlClient) ResumeActor(ctx context.Context, actorID string, boot bool) (*substrateActor, error) {
	resp, err := c.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorId: actorID, Boot: boot})
	if err != nil {
		return nil, substrateControlError("resume actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

func (c *grpcSubstrateControlClient) SuspendActor(ctx context.Context, actorID string) (*substrateActor, error) {
	resp, err := c.client.SuspendActor(ctx, &ateapipb.SuspendActorRequest{ActorId: actorID})
	if err != nil {
		return nil, substrateControlError("suspend actor", err)
	}
	return substrateActorFromProto(resp.GetActor()), nil
}

func (c *grpcSubstrateControlClient) DeleteActor(ctx context.Context, actorID string) error {
	_, err := c.client.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: actorID})
	if err != nil {
		return substrateControlError("delete actor", err)
	}
	return nil
}

func (c *grpcSubstrateControlClient) ListWorkers(ctx context.Context) ([]substrateWorker, error) {
	resp, err := c.client.ListWorkers(ctx, &ateapipb.ListWorkersRequest{})
	if err != nil {
		return nil, substrateControlError("list workers", err)
	}
	workers := make([]substrateWorker, 0, len(resp.GetWorkers()))
	for _, worker := range resp.GetWorkers() {
		workers = append(workers, substrateWorkerFromProto(worker))
	}
	return workers, nil
}

func (c *grpcSubstrateControlClient) ListActors(ctx context.Context) ([]substrateActor, error) {
	resp, err := c.client.ListActors(ctx, &ateapipb.ListActorsRequest{})
	if err != nil {
		return nil, substrateControlError("list actors", err)
	}
	actors := make([]substrateActor, 0, len(resp.GetActors()))
	for _, actor := range resp.GetActors() {
		if converted := substrateActorFromProto(actor); converted != nil {
			actors = append(actors, *converted)
		}
	}
	return actors, nil
}

func (c *grpcSubstrateSessionIdentityClient) MintJWT(
	ctx context.Context,
	req substrateMintJWTRequest,
	bearerToken string,
) (string, error) {
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(bearerToken))
	resp, err := c.client.MintJWT(ctx, &ateapipb.MintJWTRequest{
		Audience:  append([]string(nil), req.Audience...),
		AppId:     req.AppID,
		UserId:    req.UserID,
		SessionId: req.SessionID,
	})
	if err != nil {
		return "", substrateControlError("mint session identity", err)
	}
	return resp.GetSessionJwt(), nil
}

func substrateActorFromProto(actor *ateapipb.Actor) *substrateActor {
	if actor == nil {
		return nil
	}
	return &substrateActor{
		ActorID:            actor.GetActorId(),
		TemplateNamespace:  actor.GetActorTemplateNamespace(),
		TemplateName:       actor.GetActorTemplateName(),
		Status:             actor.GetStatus().String(),
		PodNamespace:       actor.GetAteomPodNamespace(),
		PodName:            actor.GetAteomPodName(),
		PodIP:              actor.GetAteomPodIp(),
		LastSnapshot:       actor.GetLastSnapshot(),
		InProgressSnapshot: actor.GetInProgressSnapshot(),
	}
}

func substrateWorkerFromProto(worker *ateapipb.Worker) substrateWorker {
	if worker == nil {
		return substrateWorker{}
	}
	return substrateWorker{
		WorkerNamespace: worker.GetWorkerNamespace(),
		WorkerPool:      worker.GetWorkerPool(),
		WorkerPod:       worker.GetWorkerPod(),
		ActorID:         worker.GetActorId(),
		IP:              worker.GetIp(),
	}
}

func substrateControlError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(op, ErrorKindTimeout, "operation timed out", true, err)
	}
	if errors.Is(err, context.Canceled) {
		return NewError(op, ErrorKindCanceled, "operation canceled", true, err)
	}
	st, ok := status.FromError(err)
	if !ok {
		return NewError(op, ErrorKindUnknown, "Substrate control API failed", true, err)
	}
	switch st.Code() {
	case codes.NotFound:
		return NewError(op, ErrorKindNotFound, st.Message(), false, err)
	case codes.AlreadyExists:
		return NewError(op, ErrorKindAlreadyExists, st.Message(), false, err)
	case codes.InvalidArgument:
		return NewError(op, ErrorKindInvalidArgument, st.Message(), false, err)
	case codes.FailedPrecondition:
		return NewError(op, ErrorKindFailedPrecondition, st.Message(), false, err)
	case codes.DeadlineExceeded:
		return NewError(op, ErrorKindTimeout, st.Message(), true, err)
	case codes.Canceled:
		return NewError(op, ErrorKindCanceled, st.Message(), true, err)
	default:
		return NewError(op, ErrorKindUnknown, st.Message(), true, err)
	}
}
