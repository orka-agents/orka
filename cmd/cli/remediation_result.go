package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	kubevalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

const (
	remediationMaxTaskResultBytes = 1 << 20
	// The ordinary Task result endpoint JSON-escapes the stored result string.
	remediationMaxTaskWireBytes = 6*remediationMaxTaskResultBytes + 1024
)

type remediationTaskTransport struct {
	base http.RoundTripper
}

type remediationTaskRequestError struct {
	message string
}

func (e *remediationTaskRequestError) Error() string { return e.message }

func printRemediationTaskResult(ctx context.Context, cmd *cobra.Command, api *remediationClient, status *contract.Status) error {
	if status == nil || !contract.TerminalPhase(status.Phase) {
		return errors.New("the remediation run has no terminal Task result")
	}
	result, err := api.parentTaskResult(ctx, status.TaskName, status.TaskUID)
	if err != nil {
		return err
	}
	// Ordinary Task reads may use the controller cache. The remediation service
	// checks the retained parent identity against its authoritative API reader.
	confirmed, err := api.statusRequest(ctx, http.MethodGet, "/"+status.ID, status.ID, nil)
	if err != nil {
		return fmt.Errorf("parent Task output could not be confirmed: %w", err)
	}
	if confirmed.TaskName != status.TaskName || confirmed.TaskUID != status.TaskUID ||
		confirmed.Phase != status.Phase || confirmed.Outcome != status.Outcome || confirmed.EvidenceScope != status.EvidenceScope {
		return errors.New("the remediation parent Task or terminal outcome changed while reading its result")
	}
	if err := printTaskResult(cmd, result); err != nil {
		return errors.New("could not print the parent Task result")
	}
	return nil
}

func (t remediationTaskTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Accept", remediationJSONMIME)
	request.Header.Set("Cache-Control", "no-store")
	response, err := t.base.RoundTrip(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, &remediationTaskRequestError{message: "parent Task output request did not complete"}
	}
	if response == nil || response.Body == nil {
		return nil, &remediationTaskRequestError{message: "parent Task output response is unavailable"}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := readRemediationTaskResponse(response)
	if err != nil {
		return nil, &remediationTaskRequestError{message: err.Error()}
	}
	copy := *response
	copy.Header = response.Header.Clone()
	copy.Header.Set("Content-Length", strconv.Itoa(len(body)))
	copy.ContentLength = int64(len(body))
	copy.Body = io.NopCloser(bytes.NewReader(body))
	return &copy, nil
}

func readRemediationTaskResponse(response *http.Response) ([]byte, error) {
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, errors.New("parent Task output authentication failed")
	case http.StatusForbidden:
		return nil, errors.New("parent Task output is not authorized; get permission on tasks is required")
	case http.StatusNotFound:
		return nil, errors.New("parent Task or its stored result is unavailable")
	default:
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return nil, errors.New("parent Task output redirects are refused")
		}
		return nil, fmt.Errorf("parent Task output returned HTTP %d", response.StatusCode)
	}
	contentTypes := response.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, errors.New("parent Task response must use the JSON content type")
	}
	contentType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || contentType != remediationJSONMIME {
		return nil, errors.New("parent Task response must use the JSON content type")
	}
	if response.ContentLength > remediationMaxTaskWireBytes {
		return nil, errors.New("parent Task response exceeds its size limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, remediationMaxTaskWireBytes+1))
	if err != nil {
		return nil, errors.New("parent Task response could not be read")
	}
	if len(body) > remediationMaxTaskWireBytes {
		return nil, errors.New("parent Task response exceeds its size limit")
	}
	if _, err := remediationJSONObject(body); err != nil {
		return nil, errors.New("parent Task response is not a valid JSON object")
	}
	return body, nil
}

func (api *remediationClient) taskClient() *client.Client {
	base := *api.baseURL
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	httpClient := *api.http
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpClient.Transport = remediationTaskTransport{base: transport}
	result := client.NewWithNamespace(base.String(), api.token, api.namespace)
	result.HTTPClient = &httpClient
	return result
}

func (api *remediationClient) parentTaskResult(ctx context.Context, name, uid string) (*client.TaskResultResponse, error) {
	if !validRemediationTaskReference(name, uid) {
		return nil, errors.New("the remediation run has no exact parent Task result reference")
	}
	tasks := api.taskClient()
	options := client.GetOptions{Namespace: api.namespace}
	if err := checkRemediationTaskIdentity(ctx, tasks, options, name, uid); err != nil {
		return nil, err
	}
	result, err := tasks.GetTaskResult(ctx, name, options)
	if err != nil {
		return nil, safeRemediationTaskError(err)
	}
	if result == nil || strings.TrimSpace(result.Result) == "" {
		return nil, errors.New("the parent Task has no stored result")
	}
	if len(result.Result) > remediationMaxTaskResultBytes || !utf8.ValidString(result.Result) {
		return nil, errors.New("parent Task result is invalid or exceeds its 1 MiB limit")
	}
	if err := checkRemediationTaskIdentity(ctx, tasks, options, name, uid); err != nil {
		return nil, err
	}
	return result, nil
}

func checkRemediationTaskIdentity(ctx context.Context, tasks *client.Client, options client.GetOptions, name, uid string) error {
	task, err := tasks.GetTask(ctx, name, options)
	if err != nil {
		return safeRemediationTaskError(err)
	}
	if task == nil {
		return errors.New("parent Task identity is unavailable")
	}
	metadata, _ := (*task)["metadata"].(map[string]any)
	taskName, _ := metadata["name"].(string)
	taskNamespace, _ := metadata["namespace"].(string)
	taskUID, _ := metadata["uid"].(string)
	if taskName != name || taskNamespace != options.Namespace || taskUID != uid {
		return errors.New("parent Task identity does not match the remediation run")
	}
	if deleted, exists := metadata["deletionTimestamp"]; exists && deleted != nil {
		return errors.New("parent Task is being deleted")
	}
	return nil
}

func safeRemediationTaskError(err error) error {
	if requestError, ok := errors.AsType[*remediationTaskRequestError](err); ok {
		return requestError
	}
	return errors.New("parent Task response could not be decoded")
}

func validRemediationTaskReference(name, uid string) bool {
	if len(kubevalidation.IsDNS1123Subdomain(name)) != 0 || uid == "" || len(uid) > 128 {
		return false
	}
	for _, character := range []byte(uid) {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}
