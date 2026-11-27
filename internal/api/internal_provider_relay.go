/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/providerproxy"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	providerRelayMaxRequestBytes  = defaultAPIRequestBodyLimit
	providerRelayMaxResponseBytes = 64 << 20
	providerRelayMaxConcurrent    = 64
)

var (
	providerRelayTimeout     = 30 * time.Minute
	providerRelayReadTimeout = 30 * time.Second
)

var providerRelayRoute = regexp.MustCompile(`(?i)^/internal/v1/tasks/[^/]+/[^/]+/providers/[^/]+/`)

func isProviderRelayPath(path string) bool {
	return providerRelayRoute.MatchString(path)
}

var providerRelayPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/openai/deployments/[A-Za-z0-9_.-]+/chat/completions$`),
	regexp.MustCompile(`^/openai/responses$`),
}

var providerRelayAPIVersion = regexp.MustCompile(`^[0-9A-Za-z.-]{1,64}$`)

func (h *InternalHandlers) RelayProviderRequest(c fiber.Ctx) error {
	if h.providerIdentities == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "this controller has no Provider identities")
	}
	target, err := h.providerRelayTarget(c)
	if err != nil {
		return err
	}
	namespace, taskName, slot, task, suffix, query := target.namespace, target.taskName, target.slot, target.task, target.suffix, target.query

	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	provider, err := relayedProvider(c.Context(), reader, task, slot)
	if err != nil {
		return err
	}
	if providerproxy.HasDisallowedContentEncoding(providerRelayHeader(c)) {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "compressed request bodies are not relayed")
	}
	if c.Request().Header.ContentLength() > providerRelayMaxRequestBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body is too large")
	}
	if !providerproxy.TryAcquireSlot(h.providerRelaySlots) {
		return fiber.NewError(fiber.StatusTooManyRequests, "the provider relay is at capacity")
	}
	released := false
	release := func() {
		if !released {
			released = true
			providerproxy.ReleaseSlot(h.providerRelaySlots)
		}
	}
	defer release()

	body, err := readBoundedRelayRequestBody(c)
	if err != nil {
		return err
	}

	base, err := url.Parse(provider.Spec.BaseURL)
	if err != nil {
		return fiber.NewError(fiber.StatusFailedDependency, "the Provider's baseURL is invalid")
	}
	var gateErr error
	httpClient, err := h.newProviderRelayHTTPClient(c, reader, target, provider, &gateErr)
	if err != nil {
		return fiber.NewError(fiber.StatusFailedDependency, "the Provider's baseURL cannot carry a token")
	}

	ctx, cancel := context.WithTimeout(c.Context(), providerRelayTimeout)
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, providerproxy.Target(base, suffix, query).String(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return fiber.NewError(fiber.StatusBadRequest, "invalid relay request")
	}
	providerproxy.CopyRequestHeaders(upstream.Header, providerRelayHeader(c))
	upstream.Header.Del("Accept-Encoding")
	upstream.Header.Del("Content-Length")
	upstream.Header.Del("Host")

	response, err := httpClient.Do(upstream)
	if err != nil {
		cancel()
		if gateErr != nil {
			return gateErr
		}
		var providerErr *llm.ProviderError
		if errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusServiceUnavailable {
			log.Info("provider relay could not obtain a token", "namespace", namespace, "task", taskName, "slot", slot, "provider", provider.Name)
			return fiber.NewError(fiber.StatusServiceUnavailable, "could not obtain a token for the Provider")
		}
		return fiber.NewError(fiber.StatusBadGateway, "the Provider request failed")
	}

	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		_ = response.Body.Close()
		cancel()
		return fiber.NewError(fiber.StatusBadGateway, "the Provider answered with an unsupported content encoding")
	}

	responseHeader := http.Header{}
	providerproxy.CopyResponseHeaders(responseHeader, response.Header)
	responseHeader.Del("Content-Length")
	for name, values := range responseHeader {
		for _, value := range values {
			c.Response().Header.Add(name, value)
		}
	}
	c.Status(response.StatusCode)
	released = true
	c.Response().SetBodyStream(&relayResponseBody{
		body: response.Body, remaining: providerRelayMaxResponseBytes,
		finish: func(err error) {
			_ = response.Body.Close()
			cancel()
			providerproxy.ReleaseSlot(h.providerRelaySlots)
			if err != nil {
				log.Info("provider relay response ended early", "namespace", namespace, "task", taskName, "slot", slot, "reason", err.Error())
			}
		},
	}, -1)
	return nil
}

func (h *InternalHandlers) newProviderRelayHTTPClient(c fiber.Ctx, reader client.Reader, target providerRelayRequest, provider *corev1alpha1.Provider, gateErr *error) (*http.Client, error) {
	identity, baseURL, identities := provider.EntraIdentity(), provider.Spec.BaseURL, h.providerIdentities
	providerUID, providerGeneration := provider.UID, provider.Generation
	return llm.NewBearerHTTPClientWithTransport(baseURL, func(ctx context.Context) (string, error) {
		token, tokenErr := identities.Token(ctx, identity, baseURL)
		if tokenErr != nil {
			return "", tokenErr
		}
		if err := h.revalidateProviderRelayBeforeSend(ctx, c, reader, target, providerUID, providerGeneration, identity, baseURL); err != nil {
			*gateErr = err
			return "", err
		}
		return token, nil
	}, h.providerRelayTransport)
}

func (h *InternalHandlers) revalidateProviderRelayBeforeSend(ctx context.Context, c fiber.Ctx, reader client.Reader, target providerRelayRequest, providerUID types.UID, providerGeneration int64, identity, baseURL string) error {
	liveTask, err := h.internalCallerAuthorizer().verifyTaskCaller(c, target.namespace, target.taskName)
	if err != nil {
		return err
	}
	if liveTask.Spec.Type != corev1alpha1.TaskTypeAI || liveTask.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return fiber.NewError(fiber.StatusForbidden, "the provider relay serves running tasks only")
	}
	liveProvider, err := relayedProvider(ctx, reader, liveTask, target.slot)
	if err != nil {
		return err
	}
	if liveProvider.UID != providerUID || liveProvider.Generation != providerGeneration ||
		liveProvider.EntraIdentity() != identity || liveProvider.Spec.BaseURL != baseURL {
		return fiber.NewError(fiber.StatusConflict, "the Provider changed while preparing the request")
	}
	return nil
}

func readBoundedRelayRequestBody(c fiber.Ctx) ([]byte, error) {
	if c.Request().IsBodyStream() {
		stream := c.Request().BodyStream()
		if stream == nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, "invalid request body stream")
		}
		body, err := io.ReadAll(io.LimitReader(stream, providerRelayMaxRequestBytes+1))
		if err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, "failed to read request body")
		}
		if len(body) > providerRelayMaxRequestBytes {
			return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body is too large")
		}
		return body, nil
	}
	body := c.Body()
	if len(body) > providerRelayMaxRequestBytes {
		return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body is too large")
	}
	return body, nil
}

type relayResponseBody struct {
	body      io.ReadCloser
	remaining int64
	finish    func(error)
	err       error
	once      sync.Once
}

func (b *relayResponseBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.body.Read(p)
	if int64(n) > b.remaining {
		b.err = providerproxy.ErrResponseTooLarge
		return 0, b.err
	}
	b.remaining -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

func (b *relayResponseBody) Close() error {
	b.once.Do(func() { b.finish(b.err) })
	return nil
}

type providerRelayRequest struct {
	namespace, taskName, slot string
	task                      *corev1alpha1.Task
	suffix, query             string
}

func (h *InternalHandlers) providerRelayTarget(c fiber.Ctx) (providerRelayRequest, error) {
	request := providerRelayRequest{
		namespace: strings.Clone(strings.TrimSpace(c.Params("namespace"))),
		taskName:  strings.Clone(strings.TrimSpace(c.Params("taskName"))),
		slot:      strings.Clone(strings.TrimSpace(c.Params("slot"))),
	}
	if request.namespace == "" || request.taskName == "" || !workerenv.IsProviderRelaySlot(request.slot) {
		return request, fiber.NewError(fiber.StatusBadRequest, "namespace, taskName and a provider slot are required")
	}
	if c.Method() != fiber.MethodPost {
		return request, fiber.NewError(fiber.StatusMethodNotAllowed, "only POST is relayed")
	}
	task, err := h.internalCallerAuthorizer().verifyTaskCaller(c, request.namespace, request.taskName)
	if err != nil {
		return request, err
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAI {
		return request, fiber.NewError(fiber.StatusForbidden, "the provider relay serves type: ai tasks only")
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return request, fiber.NewError(fiber.StatusForbidden, "the provider relay serves running tasks only")
	}
	request.task = task

	prefix := "/internal/v1/tasks/" + request.namespace + "/" + request.taskName + "/providers/" + request.slot
	suffix, ok := strings.CutPrefix(string(c.Request().URI().PathOriginal()), prefix)
	if !ok || providerproxy.HasUnsafePathSegment(suffix) || !providerRelayPathAllowed(suffix) {
		return request, fiber.NewError(fiber.StatusNotFound, "the provider relay does not serve this path")
	}
	request.suffix = suffix
	request.query, err = providerRelayQuery(string(c.Request().URI().QueryString()))
	return request, err
}

func providerRelayPathAllowed(path string) bool {
	for _, allowed := range providerRelayPaths {
		if allowed.MatchString(path) {
			return true
		}
	}
	return false
}

func providerRelayQuery(raw string) (string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid query")
	}
	for name := range values {
		if name != "api-version" {
			return "", fiber.NewError(fiber.StatusBadRequest, "only the api-version query parameter is relayed")
		}
	}
	versions := values["api-version"]
	if len(versions) == 0 {
		return "", nil
	}
	if len(versions) != 1 || !providerRelayAPIVersion.MatchString(versions[0]) {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid api-version")
	}
	return url.Values{"api-version": versions}.Encode(), nil
}

func providerRelayHeader(c fiber.Ctx) http.Header {
	header := http.Header{}
	for name, value := range c.Request().Header.All() {
		header.Add(string(name), string(value))
	}
	return header
}

func relayedProvider(ctx context.Context, reader client.Reader, task *corev1alpha1.Task, slot string) (*corev1alpha1.Provider, error) {
	if reader == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "provider relay is unavailable")
	}
	jobName := strings.TrimSpace(task.Status.JobName)
	if jobName == "" {
		return nil, fiber.NewError(fiber.StatusConflict, "task has no dispatched job")
	}
	job := &batchv1.Job{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: jobName}, job); err != nil {
		return nil, fiber.NewError(fiber.StatusFailedDependency, "the task's dispatched job is unavailable")
	}
	if task.Status.JobUID == "" || string(job.UID) != task.Status.JobUID {
		return nil, fiber.NewError(fiber.StatusConflict, "the task's job was replaced")
	}
	var frozen string
	found := false
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != workerContainerName {
			continue
		}
		for _, env := range container.Env {
			if env.Name == workerenv.AIProviderRelay && env.ValueFrom == nil {
				if found {
					return nil, fiber.NewError(fiber.StatusConflict, "the task's job sets the provider relay more than once")
				}
				frozen, found = env.Value, true
			}
		}
	}
	slots, err := workerenv.ParseProviderRelay(frozen)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusConflict, "the task's job carries an unreadable provider relay")
	}
	target, ok := slots[slot]
	if !ok {
		return nil, fiber.NewError(fiber.StatusForbidden, "the task's job does not relay slot "+strconv.Quote(slot))
	}
	provider := &corev1alpha1.Provider{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: target.Name}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusConflict, "the Provider the task was dispatched with no longer exists")
		}
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "failed to load the Provider")
	}
	if string(provider.UID) != target.UID {
		return nil, fiber.NewError(fiber.StatusConflict, "the Provider was replaced since the task was dispatched")
	}
	if !provider.UsesKeylessAuth() || provider.Spec.Type != corev1alpha1.ProviderTypeAzureOpenAI {
		return nil, fiber.NewError(fiber.StatusConflict, "the Provider no longer uses a workload identity")
	}
	if !provider.Status.Ready {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "the Provider is not ready")
	}
	return provider, nil
}
