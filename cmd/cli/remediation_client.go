package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	kubevalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

const (
	remediationMaxReportBytes   = 64 << 10
	remediationMaxPatchBytes    = 256 << 10
	remediationMaxRequestBytes  = 2 << 20
	remediationMaxResponseBytes = 16 << 10
	remediationJSONMIME         = "application/json"
	remediationPatchMIME        = "application/octet-stream"
)

type remediationClient struct {
	baseURL   *url.URL
	namespace string
	token     string
	http      *http.Client
}

func remediationAPI(cmd *cobra.Command) (*remediationClient, error) {
	for _, flag := range []string{"token", "txn-token", "txn-token-file", "kubeconfig"} {
		if cmd.Flags().Changed(flag) {
			return nil, errors.New("remediation uses saved login or --token-file with --server and --namespace; inline tokens, transaction tokens, and kubeconfig are not supported")
		}
	}
	server, _ := cmd.Flags().GetString("server")
	namespace, _ := cmd.Flags().GetString("namespace")
	tokenFile, _ := cmd.Flags().GetString("token-file")
	var cfg orkaConfig
	if server == "" || namespace == "" || tokenFile == "" {
		var err error
		cfg, err = loadRemediationConfig()
		if err != nil {
			return nil, err
		}
	}
	if server == "" {
		server = cfg.Server
	}
	if server == "" {
		server = defaultServer
	}
	if namespace == "" {
		namespace = cfg.Namespace
	}
	if namespace == "" {
		namespace = defaultNamespace
	}
	if len(kubevalidation.IsDNS1123Label(namespace)) != 0 {
		return nil, errors.New("namespace must be a canonical DNS label")
	}
	base, err := url.Parse(server)
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return nil, errors.New("server must be an HTTP(S) URL without credentials, query parameters, or a fragment")
	}
	token := cfg.Token
	if tokenFile != "" {
		raw, err := readRemediationFile(tokenFile, 16<<10, true)
		if err != nil {
			return nil, err
		}
		token = string(raw)
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 16<<10 || strings.IndexFunc(token, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) != -1 {
		return nil, errors.New("authentication requires a Kubernetes token; use saved login or a private --token-file")
	}
	return &remediationClient{
		baseURL: base, namespace: namespace, token: token,
		http: &http.Client{
			Timeout: 90 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func loadRemediationConfig() (orkaConfig, error) {
	var cfg orkaConfig
	path := configPath()
	if path == "" {
		return cfg, errors.New("could not locate saved login configuration")
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	raw, err := readRemediationFile(path, 64<<10, true)
	if err != nil {
		return cfg, errors.New("saved login configuration is unavailable or not private")
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, errors.New("saved login configuration is invalid")
	}
	return cfg, nil
}

func (api *remediationClient) request(ctx context.Context, method, suffix string, body []byte, limit int64, responseType string) ([]byte, error) {
	target := *api.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/api/v1/remediations" + suffix
	target.RawPath = ""
	query := target.Query()
	query.Set("namespace", api.namespace)
	target.RawQuery = query.Encode()
	// Do not supply GetBody or idempotency headers: net/http must not replay a
	// creation after a transport failure, even when the requestID is stable.
	var reader io.Reader
	if len(body) != 0 {
		reader = io.LimitReader(bytes.NewReader(body), int64(len(body)))
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, errors.New("could not construct remediation request")
	}
	if len(body) != 0 {
		request.ContentLength = int64(len(body))
		request.Header.Set("Content-Type", remediationJSONMIME)
	}
	request.Header.Set("Authorization", "Bearer "+api.token)
	request.Header.Set("Accept", responseType)
	request.Header.Set("Cache-Control", "no-store")
	response, err := api.http.Do(request)
	if err != nil {
		return nil, errors.New("remediation request did not complete; its remote outcome may be uncertain")
	}
	defer func() { _ = response.Body.Close() }()
	expected := http.StatusOK
	if method == http.MethodPost {
		expected = http.StatusAccepted
	}
	if response.StatusCode != expected {
		return nil, remediationHTTPError(response.StatusCode)
	}
	contentTypes := response.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, errors.New("remediation response has an unexpected content type")
	}
	contentType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || contentType != responseType {
		return nil, errors.New("remediation response has an unexpected content type")
	}
	if response.ContentLength > limit {
		return nil, errors.New("remediation response exceeds its size limit")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, errors.New("remediation response could not be read; its remote outcome may be uncertain")
	}
	if int64(len(content)) > limit {
		return nil, errors.New("remediation response exceeds its size limit")
	}
	if responseType == remediationPatchMIME && (len(bytes.TrimSpace(content)) == 0 || !utf8.Valid(content)) {
		return nil, errors.New("candidate patch response must be nonempty UTF-8")
	}
	if responseType == remediationPatchMIME {
		if err := validateRemediationPatch(content); err != nil {
			return nil, err
		}
	}
	return content, nil
}

func (api *remediationClient) statusRequest(ctx context.Context, method, suffix, id string, request *contract.Request) (*contract.Status, error) {
	if !validRemediationID(id) {
		return nil, errors.New("invalid remediation run ID")
	}
	var body []byte
	if request != nil {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(request); err != nil || encoded.Len() > remediationMaxRequestBytes {
			return nil, errors.New("remediation request exceeds its size limit or is invalid")
		}
		body = encoded.Bytes()
	}
	content, err := api.request(ctx, method, suffix, body, remediationMaxResponseBytes, remediationJSONMIME)
	if err != nil {
		return nil, err
	}
	var status contract.Status
	if _, err := remediationJSONObject(content); err != nil || json.Unmarshal(content, &status) != nil ||
		status.ID != id || status.Namespace != api.namespace || !validRemediationPhase(status.Phase) ||
		!validRemediationStage(status.Stage) || !validRemediationFeedback(&status) {
		return nil, errors.New("server returned an invalid remediation status or a different run identity")
	}
	if request != nil && request.RequiredEvidenceScope != "" &&
		contract.RequireEvidenceScope(request.RequiredEvidenceScope, status.EvidenceScope) != nil {
		return nil, errors.New("server did not preserve the required evidence scope")
	}
	return &status, nil
}

func remediationHTTPError(status int) error {
	switch status {
	case http.StatusBadRequest:
		return errors.New("the server rejected the remediation request")
	case http.StatusUnprocessableEntity:
		return errors.New("the configured remediation backend does not support the required evidence scope; no run was admitted")
	case http.StatusUnauthorized:
		return errors.New("authentication failed; use saved Kubernetes login or a private --token-file")
	case http.StatusForbidden:
		return errors.New("the remediation action is not authorized")
	case http.StatusNotFound:
		return errors.New("the remediation API is disabled or the exact run was not found")
	case http.StatusConflict:
		return errors.New("the request conflicts with retained state; do not change its request ID to bypass the conflict")
	case http.StatusRequestEntityTooLarge:
		return errors.New("the remediation request exceeds the server's size limit")
	default:
		if status >= 300 && status < 400 {
			return errors.New("remediation redirects are refused")
		}
		return fmt.Errorf("remediation server returned HTTP %d; the remote outcome may be uncertain", status)
	}
}

func validRemediationID(id string) bool {
	return len(id) <= 63 && len(kubevalidation.IsDNS1123Subdomain(id)) == 0
}

func validRemediationPhase(phase string) bool {
	return phase == contract.PhasePending || phase == contract.PhaseRunning || contract.TerminalPhase(phase)
}

func validRemediationStage(stage string) bool {
	switch stage {
	case contract.StageReproduce, contract.StageGenerate, contract.StageVerify, contract.StageComplete:
		return true
	default:
		return false
	}
}

func validRemediationFeedback(status *contract.Status) bool {
	if status == nil || ((status.TaskName != "" || status.TaskUID != "") &&
		!validRemediationTaskReference(status.TaskName, status.TaskUID)) ||
		(status.EvidenceScope != "" && !status.EvidenceScope.Valid()) {
		return false
	}
	switch status.Outcome {
	case "", contract.OutcomeReproduced, contract.OutcomeNotReproduced, contract.OutcomeUnableToValidate,
		contract.OutcomeUnableToVerify, contract.OutcomeUnverified, contract.OutcomeVerified,
		contract.OutcomeCancelled, contract.OutcomeTimedOut:
	default:
		return false
	}
	if status.Reason == "" {
		return true
	}
	if len(status.Reason) > 64 || status.Reason[0] < 'a' || status.Reason[0] > 'z' {
		return false
	}
	for _, character := range []byte(status.Reason) {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}
