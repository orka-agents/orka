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
	"strings"
	"time"

	ateapipb "github.com/sozercan/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	substrateReadyInitialPollInterval = 100 * time.Millisecond
	substrateReadyMaxPollInterval     = 2 * time.Second
	substrateExecInitialPollInterval  = 250 * time.Millisecond
	substrateExecMaxPollInterval      = 2 * time.Second
	substrateDefaultHandoffTokenEnv   = "ORKA_WORKSPACE_HANDOFF_TOKEN"
	substrateDefaultBootstrapTokenEnv = "ORKA_WORKSPACE_BOOTSTRAP_TOKEN"
	substrateHandoffTokenUploadPath   = "orka-workspace-handoff-token"

	substrateStatusResuming   = "STATUS_RESUMING"
	substrateStatusRunning    = "STATUS_RUNNING"
	substrateStatusSuspending = "STATUS_SUSPENDING"
	substrateStatusSuspended  = "STATUS_SUSPENDED"
)

// SubstrateConfig configures a Substrate-backed WorkspaceExecutor.
type SubstrateConfig struct {
	APIEndpoint           string
	APICAFile             string
	APIInsecureSkipVerify bool
	RouterURL             string
	ActorDNSSuffix        string
	HandoffToken          string
	BootstrapToken        string
	HTTPClient            *http.Client
	ControlClient         substrateControlClient
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
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.HandoffToken == "" {
		cfg.HandoffToken = strings.TrimSpace(os.Getenv(substrateDefaultHandoffTokenEnv))
	}
	if cfg.BootstrapToken == "" {
		cfg.BootstrapToken = strings.TrimSpace(os.Getenv(substrateDefaultBootstrapTokenEnv))
	}
	if cfg.ControlClient == nil {
		client, err := newGRPCSubstrateControlClient(cfg)
		if err != nil {
			return nil, err
		}
		cfg.ControlClient = client
	}

	return &SubstrateWorkspaceExecutor{
		control:        cfg.ControlClient,
		httpClient:     cfg.HTTPClient,
		routerURL:      strings.TrimRight(cfg.RouterURL, "/"),
		actorDNSSuffix: strings.Trim(strings.TrimSpace(cfg.ActorDNSSuffix), "."),
		handoffToken:   cfg.HandoffToken,
		bootstrapToken: cfg.BootstrapToken,
		now:            time.Now,
	}, nil
}

type SubstrateWorkspaceExecutor struct {
	control        substrateControlClient
	httpClient     *http.Client
	routerURL      string
	actorDNSSuffix string
	handoffToken   string
	bootstrapToken string
	now            func() time.Time
}

var _ WorkspaceExecutor = (*SubstrateWorkspaceExecutor)(nil)

type substrateControlClient interface {
	GetActor(ctx context.Context, actorID string) (*substrateActor, error)
	CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*substrateActor, error)
	ResumeActor(ctx context.Context, actorID string) (*substrateActor, error)
	SuspendActor(ctx context.Context, actorID string) (*substrateActor, error)
	DeleteActor(ctx context.Context, actorID string) error
}

type substrateActor struct {
	ActorID           string
	TemplateNamespace string
	TemplateName      string
	Status            string
	PodIP             string
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
		Ref:      substrateRef(req.Template.Namespace, actor),
		Template: req.Template,
		ReuseKey: req.ReuseKey,
		Reused:   true,
		Phase:    substratePhase(actor),
		Message:  "workspace actor reattached",
	}, nil
}

func validateSubstrateActorTemplate(actor *substrateActor, template TemplateRef) error {
	if actor == nil {
		return NewError("claim", ErrorKindFailedPrecondition, "Substrate actor lookup returned no actor", false, nil)
	}
	actualNamespace := strings.TrimSpace(actor.TemplateNamespace)
	actualName := strings.TrimSpace(actor.TemplateName)
	wantNamespace := strings.TrimSpace(template.Namespace)
	wantName := strings.TrimSpace(template.Name)
	if actualNamespace == wantNamespace && actualName == wantName {
		return nil
	}
	return NewError(
		"claim",
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
	if _, err := e.control.ResumeActor(ctx, actorID); err != nil {
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
				return &ReadyResult{Ref: substrateRef(req.Ref.Namespace, actor), Phase: PhaseReady, Message: "workspace ready", ReadyAt: e.now()}, nil
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
			return nil, err
		}
		if !resp.Running {
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
	if err := e.scrubDaemon(ctx, actorID); err != nil {
		return nil, NewError("release", ErrorKindFailedPrecondition, "failed to scrub workspace before release", false, err)
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
	return &Description{
		Ref:      substrateRef(req.Ref.Namespace, actor),
		Template: TemplateRef{Namespace: actor.TemplateNamespace, Name: actor.TemplateName},
		Phase:    substratePhase(actor),
		Retained: retained,
		Message:  "workspace described",
	}, nil
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

func (c *grpcSubstrateControlClient) ResumeActor(ctx context.Context, actorID string) (*substrateActor, error) {
	resp, err := c.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorId: actorID})
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

func substrateActorFromProto(actor *ateapipb.Actor) *substrateActor {
	if actor == nil {
		return nil
	}
	return &substrateActor{
		ActorID:           actor.GetActorId(),
		TemplateNamespace: actor.GetActorTemplateNamespace(),
		TemplateName:      actor.GetActorTemplateName(),
		Status:            actor.GetStatus().String(),
		PodIP:             actor.GetAteomPodIp(),
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
