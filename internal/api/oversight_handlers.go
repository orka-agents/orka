/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/oversight"
	"github.com/orka-agents/orka/internal/store"
	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Oversight is read-only observability. Nothing here holds, releases or
// changes an agent action; outcome labels only calibrate monitors.
//
// Kubernetes RBAC names the resources below in the core API group, though
// they are not custom resources: a Role grants an ingest identity "create" on
// oversightdecisions, a viewer "get" and "list", and a reviewer "create" on
// oversightdecisions/feedback.
const (
	oversightResource         = "oversightdecisions"
	oversightFeedbackResource = "oversightdecisions/feedback"

	maxOversightIngestBytes      = 1 << 20
	oversightDefaultListLimit    = 50
	oversightMaxListLimit        = 200
	oversightPostureScanLimit    = 5000
	oversightAnalysisScanLimit   = storagev1.MaxOversightListLimit
	oversightDefaultPostureRange = 24 * time.Hour
)

func (h *Handlers) requireOversight() error {
	if !h.oversightEnabled || h.oversightStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "oversight is not enabled")
	}
	return nil
}

// oversightStorageError maps a storage error to an HTTP error.
func oversightStorageError(action string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, "oversight decision not found")
	case errors.Is(err, store.ErrDuplicateMismatch):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case errors.Is(err, store.ErrTooLarge):
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, store.ErrValidation):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if temporary := temporaryStorageHTTPError(err); temporary != nil {
		return temporary
	}
	log.Error(err, "oversight storage failed", "action", action)
	return fiber.NewError(fiber.StatusInternalServerError, "failed to "+action)
}

// authorizeOversightRead resolves the namespace and checks the caller may
// read its oversight decisions.
func (h *Handlers) authorizeOversightRead(c fiber.Ctx, action, verb string) (string, error) {
	if err := h.requireOversight(); err != nil {
		return "", err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return "", err
	}
	if err := h.authorizeContextTokenAction(c, action, h.contextTokenAuthorization.TaskListScopes); err != nil {
		return "", err
	}
	if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, GetUserInfo(c), namespace, verb,
		corev1alpha1.GroupVersion.Group, oversightResource, ""); err != nil {
		return "", fiber.NewError(fiber.StatusForbidden, "not authorized to read oversight decisions")
	}
	return namespace, nil
}

// IngestOversightDecisions handles POST /internal/v1/oversight/{namespace}/decisions.
// An oversight pipeline submits decisions here. The caller must be a
// ServiceAccount of the namespace that RBAC allows to create
// oversightdecisions there; a worker ServiceAccount is always refused, so an
// agent can never write records about its own actions.
func (h *Handlers) IngestOversightDecisions(c fiber.Ctx) error {
	if err := h.requireOversight(); err != nil {
		return err
	}
	namespace := strings.TrimSpace(c.Params("namespace"))
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if h.watchNamespace != "" && namespace != h.watchNamespace {
		// Reads are limited to the watched namespace; records elsewhere could
		// never be shown.
		return fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}
	if err := (internalCallerAuthorizer{}).verifyNamespace(c, namespace); err != nil {
		return err
	}
	userInfo := GetUserInfo(c)
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "oversight ingest requires a Kubernetes ServiceAccount")
	}
	if isWorkerServiceAccount(userInfo) {
		log.Info("oversight ingest refused for a worker ServiceAccount", "username", userInfo.Username, "namespace", namespace)
		return fiber.NewError(fiber.StatusForbidden, "workers cannot submit oversight decisions")
	}
	if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, userInfo, namespace, "create",
		corev1alpha1.GroupVersion.Group, oversightResource, ""); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "not authorized to submit oversight decisions")
	}

	body, err := readBoundedBody(c, maxOversightIngestBytes)
	if err != nil {
		return err
	}
	var req oversightv1.IngestRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body: trailing data")
	}
	if len(req.Decisions) == 0 || len(req.Decisions) > oversightv1.MaxBatchDecisions {
		return fiber.NewError(fiber.StatusBadRequest,
			fmt.Sprintf("a batch holds 1 to %d decisions", oversightv1.MaxBatchDecisions))
	}
	records := make([]storagev1.OversightDecisionRecord, 0, len(req.Decisions))
	for i, decision := range req.Decisions {
		if decision.Namespace != "" && decision.Namespace != namespace {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("decisions[%d]: namespace does not match the path", i))
		}
		_, record, err := oversight.Prepare(namespace, decision)
		if err != nil {
			if errors.Is(err, storagev1.ErrTooLarge) {
				return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("decisions[%d]: %v", i, err))
			}
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("decisions[%d]: %v", i, err))
		}
		records = append(records, record)
	}
	results, err := h.oversightStore.AppendOversightDecisions(c.Context(), namespace, records)
	if err != nil {
		return oversightStorageError("store oversight decisions", err)
	}
	response := oversightv1.IngestResponse{Results: make([]oversightv1.IngestResult, 0, len(results))}
	for _, result := range results {
		if result.Duplicate {
			response.Duplicates++
		} else {
			response.Ingested++
		}
		response.Results = append(response.Results, oversightv1.IngestResult{ID: result.ID, Seq: result.Seq, Duplicate: result.Duplicate})
	}
	return c.JSON(response)
}

func readBoundedBody(c fiber.Ctx, limit int) ([]byte, error) {
	var data []byte
	if stream := c.Request().BodyStream(); stream != nil {
		read, err := io.ReadAll(io.LimitReader(stream, int64(limit)+1))
		if err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, "failed to read request body")
		}
		data = read
	} else {
		data = append([]byte(nil), c.Body()...)
	}
	if len(data) == 0 {
		return nil, fiber.NewError(fiber.StatusBadRequest, "empty request body")
	}
	if len(data) > limit {
		return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", limit))
	}
	return data, nil
}

// oversightQueryFilter reads q= and window= into a filter of namespace.
func oversightQueryFilter(c fiber.Ctx, namespace string, now time.Time) (storagev1.OversightDecisionFilter, []oversightv1.QueryTerm, error) {
	filter, terms, err := oversight.ParseQuery(c.Query("q", ""), now)
	if err != nil {
		return filter, nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if raw := strings.TrimSpace(c.Query("window", "")); raw != "" && raw != "all" {
		window, err := oversight.ParseWindow(raw)
		if err != nil {
			return filter, nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		if since := now.Add(-window); since.After(filter.ObservedSince) {
			filter.ObservedSince = since
		}
	}
	if task := strings.TrimSpace(c.Query("taskName", "")); task != "" {
		filter.TaskNames = append(filter.TaskNames, task)
	}
	filter.Namespace = namespace
	return filter, terms, nil
}

func queryInt(c fiber.Ctx, name string, fallback, maxValue int) (int, error) {
	raw := strings.TrimSpace(c.Query(name, ""))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fiber.NewError(fiber.StatusBadRequest, name+" must be a positive integer")
	}
	return min(value, maxValue), nil
}

func querySeq(c fiber.Ctx, name string) (int64, error) {
	raw := strings.TrimSpace(c.Query(name, ""))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fiber.NewError(fiber.StatusBadRequest, name+" must be a non-negative integer")
	}
	return value, nil
}

// oversightViews decodes records with their latest labels.
func (h *Handlers) oversightViews(ctx context.Context, namespace string, records []storagev1.OversightDecisionRecord) ([]oversightv1.DecisionView, error) {
	feedback := map[string]storagev1.OversightFeedbackRecord{}
	if len(records) > 0 {
		var err error
		if feedback, err = h.oversightStore.ListLatestOversightFeedback(ctx, namespace, oversight.RecordIDs(records)); err != nil {
			return nil, oversightStorageError("read oversight feedback", err)
		}
	}
	views, err := oversight.Views(records, feedback)
	if err != nil {
		log.Error(err, "decode oversight decisions", "namespace", namespace)
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to decode oversight decisions")
	}
	return views, nil
}

// ListOversightDecisions handles GET /api/v1/oversight/decisions.
// Results are newest first; continue= pages back. q= is the search grammar.
func (h *Handlers) ListOversightDecisions(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "listOversightDecisions", "list")
	if err != nil {
		return err
	}
	filter, terms, err := oversightQueryFilter(c, namespace, time.Now().UTC())
	if err != nil {
		return err
	}
	limit, err := queryInt(c, "limit", oversightDefaultListLimit, oversightMaxListLimit)
	if err != nil {
		return err
	}
	if filter.BeforeSeq, err = querySeq(c, "continue"); err != nil {
		return err
	}
	filter.Descending, filter.Limit = true, limit+1
	records, err := h.oversightStore.ListOversightDecisions(c.Context(), filter)
	if err != nil {
		return oversightStorageError("list oversight decisions", err)
	}
	response := oversightv1.DecisionListResponse{Namespace: namespace, Query: terms}
	if len(records) > limit {
		records = records[:limit]
		response.Continue = strconv.FormatInt(records[len(records)-1].Seq, 10)
	}
	if response.Items, err = h.oversightViews(c.Context(), namespace, records); err != nil {
		return err
	}
	return c.JSON(response)
}

// GetOversightDecision handles GET /api/v1/oversight/decisions/{id}.
func (h *Handlers) GetOversightDecision(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "getOversightDecision", "get")
	if err != nil {
		return err
	}
	id, err := oversightDecisionIDParam(c)
	if err != nil {
		return err
	}
	view, err := h.oversightDecision(c.Context(), namespace, id)
	if err != nil {
		return err
	}
	return c.JSON(view)
}

func (h *Handlers) oversightDecision(ctx context.Context, namespace, id string) (oversightv1.DecisionView, error) {
	record, err := h.oversightStore.GetOversightDecision(ctx, namespace, id)
	if err != nil {
		return oversightv1.DecisionView{}, oversightStorageError("read the oversight decision", err)
	}
	views, err := h.oversightViews(ctx, namespace, []storagev1.OversightDecisionRecord{record})
	if err != nil {
		return oversightv1.DecisionView{}, err
	}
	return views[0], nil
}

// SimilarOversightDecisions handles GET /api/v1/oversight/decisions/{id}/similar.
// It ranks decisions by their deterministic behavioural signature, so each
// result can say exactly which features it shares.
func (h *Handlers) SimilarOversightDecisions(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "similarOversightDecisions", "list")
	if err != nil {
		return err
	}
	limit, err := queryInt(c, "limit", oversightDefaultListLimit, oversightMaxListLimit)
	if err != nil {
		return err
	}
	id, err := oversightDecisionIDParam(c)
	if err != nil {
		return err
	}
	source, err := h.oversightDecision(c.Context(), namespace, id)
	if err != nil {
		return err
	}
	base, _, err := oversightQueryFilter(c, namespace, time.Now().UTC())
	if err != nil {
		return err
	}
	signature := source.Signature()
	base.Descending, base.Limit = true, oversightAnalysisScanLimit
	exact := base
	exact.SignatureHash = source.SignatureHash
	byFeature := base
	if byFeature.AnyFacets = oversight.SimilarCandidates(namespace, signature); len(byFeature.AnyFacets) == 0 {
		byFeature.AnyFacets = []storagev1.OversightFacetMatch{{Kind: oversightv1.FeatureTool, Value: source.Action.ToolName}}
	}
	seen := map[int64]bool{}
	var candidates []storagev1.OversightDecisionRecord
	truncated := false
	for _, filter := range []storagev1.OversightDecisionFilter{exact, byFeature} {
		records, err := h.oversightStore.ListOversightDecisions(c.Context(), filter)
		if err != nil {
			return oversightStorageError("list similar oversight decisions", err)
		}
		truncated = truncated || len(records) >= filter.Limit
		for _, record := range records {
			if !seen[record.Seq] {
				seen[record.Seq] = true
				candidates = append(candidates, record)
			}
		}
	}
	views, err := h.oversightViews(c.Context(), namespace, candidates)
	if err != nil {
		return err
	}
	return c.JSON(oversightv1.SimilarResponse{
		Namespace: namespace, Source: source, Signature: signature,
		Items: oversight.RankSimilar(source, views, limit), Searched: len(views), Truncated: truncated,
	})
}

// GroupOversightDecisions handles GET /api/v1/oversight/decisions/groups?by=agent|task.
// It answers "has this happened before, and where?" for a query.
func (h *Handlers) GroupOversightDecisions(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "groupOversightDecisions", "list")
	if err != nil {
		return err
	}
	by := c.Query("by", oversight.GroupByAgent)
	if by != oversight.GroupByAgent && by != oversight.GroupByTask {
		return fiber.NewError(fiber.StatusBadRequest, "by must be agent or task")
	}
	filter, terms, err := oversightQueryFilter(c, namespace, time.Now().UTC())
	if err != nil {
		return err
	}
	filter.Descending, filter.Limit = true, oversightAnalysisScanLimit
	records, err := h.oversightStore.ListOversightDecisions(c.Context(), filter)
	if err != nil {
		return oversightStorageError("group oversight decisions", err)
	}
	views, err := oversight.Views(records, nil)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to decode oversight decisions")
	}
	return c.JSON(oversightv1.GroupsResponse{
		Namespace: namespace, By: by, Query: terms, Groups: oversight.Group(views, by),
		Searched: len(views), Truncated: len(records) >= filter.Limit,
	})
}

// GetOversightPosture handles GET /api/v1/oversight/posture?window=24h.
func (h *Handlers) GetOversightPosture(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "getOversightPosture", "list")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	windowName := c.Query("window", "24h")
	window := oversightDefaultPostureRange
	if windowName != "" {
		if window, err = oversight.ParseWindow(windowName); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
	}
	filter := storagev1.OversightDecisionFilter{
		Namespace: namespace, ObservedSince: now.Add(-window), RunID: c.Query("runID", ""),
		Descending: true, Limit: storagev1.MaxOversightListLimit,
	}
	var records []storagev1.OversightDecisionRecord
	incomplete := false
	for {
		page, err := h.oversightStore.ListOversightDecisions(c.Context(), filter)
		if err != nil {
			return oversightStorageError("read oversight posture", err)
		}
		records = append(records, page...)
		if len(page) < filter.Limit {
			break
		}
		if len(records) >= oversightPostureScanLimit {
			incomplete = true
			break
		}
		filter.BeforeSeq = page[len(page)-1].Seq
	}
	views, err := h.oversightViews(c.Context(), namespace, records)
	if err != nil {
		return err
	}
	return c.JSON(oversight.ComputePosture(namespace, windowName, now, views, incomplete))
}

// GetOversightConfig handles GET /api/v1/oversight/config: the search keys a
// client may offer and the operator's charter declarations, read from the
// producer declarations ConfigMap: charters.json holds a JSON list of
// CharterDeclaration, and previewCharterRevisions a comma-separated list of
// revisions labelled "preview rule check, audit only".
func (h *Handlers) GetOversightConfig(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "getOversightConfig", "list")
	if err != nil {
		return err
	}
	response := oversightv1.ConfigResponse{
		QueryKeys: oversight.QueryKeys(), MaxBulkFeedback: oversightv1.MaxBulkFeedback,
		DemoCharterRevisions: []string{}, Charters: []oversightv1.CharterDeclaration{},
		Context: []oversightv1.ContextLabel{}, Concerns: []oversightv1.ConcernDeclaration{},
	}
	cm, found, err := h.oversightProducersConfigMap(c.Context(), namespace)
	if err != nil {
		return err
	}
	if !found {
		return c.JSON(response)
	}
	previews := map[string]bool{}
	for revision := range strings.SplitSeq(cm.Data[oversightPreviewChartersKey], ",") {
		if revision = strings.TrimSpace(revision); revision != "" {
			previews[revision] = true
		}
	}
	if raw := cm.Data[oversightChartersKey]; raw != "" {
		var charters []oversightv1.CharterDeclaration
		if err := json.Unmarshal([]byte(raw), &charters); err != nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity,
				fmt.Sprintf("%s/%s %s is not a JSON list of charters", namespace, oversightProducersConfigMap, oversightChartersKey))
		}
		for _, charter := range charters {
			if charter.Revision == "" || len(response.Charters) >= maxOversightProducers {
				continue
			}
			if len(charter.Bounds) > maxOversightProducers {
				charter.Bounds = charter.Bounds[:maxOversightProducers]
			}
			if charter.Preview {
				previews[charter.Revision] = true
			}
			response.Charters = append(response.Charters, charter)
		}
	}
	if raw := cm.Data[oversightContextKey]; raw != "" {
		var labels []oversightv1.ContextLabel
		if err := json.Unmarshal([]byte(raw), &labels); err != nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity,
				fmt.Sprintf("%s/%s %s is not a JSON list of labels", namespace, oversightProducersConfigMap, oversightContextKey))
		}
		for _, label := range labels {
			if label.Name == "" || len(response.Context) >= maxOversightContextLabels {
				continue
			}
			label.Name = truncateOversightLabel(label.Name)
			label.Value = truncateOversightLabel(label.Value)
			response.Context = append(response.Context, label)
		}
	}
	if raw := cm.Data[oversightConcernsKey]; raw != "" {
		var concerns []oversightv1.ConcernDeclaration
		if err := json.Unmarshal([]byte(raw), &concerns); err != nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity,
				fmt.Sprintf("%s/%s %s is not a JSON list of concerns", namespace, oversightProducersConfigMap, oversightConcernsKey))
		}
		for _, concern := range concerns {
			if concern.ID == "" || concern.Name == "" || len(response.Concerns) >= maxOversightProducers {
				continue
			}
			concern.Name = truncateOversightLabel(concern.Name)
			concern.Meaning = truncateOversightText(concern.Meaning, oversightv1.MaxCharterSentenceBytes)
			response.Concerns = append(response.Concerns, concern)
		}
	}
	for revision := range previews {
		response.DemoCharterRevisions = append(response.DemoCharterRevisions, revision)
	}
	slices.Sort(response.DemoCharterRevisions)
	return c.JSON(response)
}

// RecordOversightFeedback handles POST /api/v1/oversight/decisions/{id}/feedback.
func (h *Handlers) RecordOversightFeedback(c fiber.Ctx) error {
	var req oversightv1.FeedbackRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	id, err := oversightDecisionIDParam(c)
	if err != nil {
		return err
	}
	return h.recordOversightFeedback(c, []string{id}, req.Label, req.Note)
}

// oversightDecisionIDParam returns the decision ID path segment, decoded:
// clients escape the "@" between a Task name and its action sequence.
func oversightDecisionIDParam(c fiber.Ctx) (string, error) {
	id, err := url.PathUnescape(c.Params("id"))
	if err != nil {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid decision id")
	}
	return id, nil
}

// BulkOversightFeedback handles POST /api/v1/oversight/decisions/bulk-feedback.
func (h *Handlers) BulkOversightFeedback(c fiber.Ctx) error {
	var req oversightv1.BulkFeedbackRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if len(req.IDs) == 0 || len(req.IDs) > oversightv1.MaxBulkFeedback {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("ids holds 1 to %d decisions", oversightv1.MaxBulkFeedback))
	}
	return h.recordOversightFeedback(c, req.IDs, req.Label, req.Note)
}

func (h *Handlers) recordOversightFeedback(c fiber.Ctx, ids []string, label oversightv1.FeedbackLabel, note string) error {
	if err := h.requireOversight(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	userInfo := GetUserInfo(c)
	if userInfo == nil || userInfo.ContextToken != nil || isWorkerServiceAccount(userInfo) {
		return fiber.NewError(fiber.StatusForbidden, "outcome labels come from people")
	}
	if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, userInfo, namespace, "create",
		corev1alpha1.GroupVersion.Group, oversightFeedbackResource, ""); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "not authorized to label oversight decisions")
	}
	feedback := oversightv1.DecisionFeedback{Label: label, Note: note}
	if err := feedback.Validate(); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	reviewer := userInfo.Username
	if reviewer == "" {
		reviewer = userInfo.Subject
	}
	records := make([]storagev1.OversightFeedbackRecord, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			continue
		}
		seen[id] = true
		records = append(records, storagev1.OversightFeedbackRecord{
			DecisionID: id, Label: string(label), Note: oversight.RedactNote(note), Reviewer: reviewer,
		})
	}
	if len(records) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "no decision named")
	}
	now := time.Now().UTC()
	stored, err := h.oversightStore.RecordOversightFeedback(c.Context(), namespace, records, now)
	if err != nil {
		return oversightStorageError("record oversight feedback", err)
	}
	feedback.Reviewer, feedback.RecordedAt, feedback.Note = reviewer, now, records[0].Note
	return c.JSON(oversightv1.FeedbackResponse{Recorded: len(stored), Feedback: feedback})
}

// StreamOversightDecisions handles GET /api/v1/oversight/decisions/stream?after=N.
// It replays matching decisions after the cursor, then polls for new ones,
// writing each as an SSE "decision" frame whose id is its Seq.
func (h *Handlers) StreamOversightDecisions(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "streamOversightDecisions", "list")
	if err != nil {
		return err
	}
	filter, _, err := oversightQueryFilter(c, namespace, time.Now().UTC())
	if err != nil {
		return err
	}
	if filter.AfterSeq, err = querySeq(c, "after"); err != nil {
		return err
	}
	filter.Limit = oversightMaxListLimit
	pollInterval := h.eventStreamPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultEventStreamPollInterval
	}
	heartbeatEvery := h.eventStreamHeartbeatEvery
	if heartbeatEvery <= 0 {
		heartbeatEvery = defaultEventStreamHeartbeatEvery
	}
	// SendStreamWriter outlives the handler; nothing may keep a reference to
	// Fiber's recycled request buffers.
	filter = cloneOversightFilter(filter)
	namespace = strings.Clone(namespace)

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		writeAvailable := func() bool {
			for {
				records, err := h.oversightStore.ListOversightDecisions(ctx, filter)
				if err != nil {
					log.Error(err, "list oversight decisions for stream", "namespace", namespace)
					return true
				}
				if len(records) == 0 {
					return true
				}
				views, err := h.oversightViews(ctx, namespace, records)
				if err != nil {
					return false
				}
				for _, view := range views {
					data, err := json.Marshal(view)
					if err != nil {
						continue
					}
					if _, err := fmt.Fprintf(w, "id: %d\nevent: decision\ndata: %s\n\n", view.Seq, data); err != nil {
						return false
					}
					filter.AfterSeq = max(filter.AfterSeq, view.Seq)
				}
				if err := w.Flush(); err != nil {
					return false
				}
				if len(records) < filter.Limit {
					return true
				}
			}
		}
		if _, err := fmt.Fprint(w, "event: ready\ndata: {}\n\n"); err != nil || w.Flush() != nil {
			return
		}
		if !writeAvailable() {
			return
		}
		poll := time.NewTicker(pollInterval)
		defer poll.Stop()
		heartbeat := time.NewTicker(heartbeatEvery)
		defer heartbeat.Stop()
		for {
			select {
			case <-poll.C:
				if !writeAvailable() {
					return
				}
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || w.Flush() != nil {
					return
				}
			}
		}
	})
}

func cloneOversightFilter(f storagev1.OversightDecisionFilter) storagev1.OversightDecisionFilter {
	cloneAll := func(values []string) []string {
		out := make([]string, len(values))
		for i, value := range values {
			out[i] = strings.Clone(value)
		}
		return out
	}
	cloneMatches := func(matches []storagev1.OversightFacetMatch) []storagev1.OversightFacetMatch {
		out := make([]storagev1.OversightFacetMatch, len(matches))
		for i, match := range matches {
			out[i] = storagev1.OversightFacetMatch{Kind: strings.Clone(match.Kind), Value: strings.Clone(match.Value), Glob: match.Glob}
		}
		return out
	}
	f.Namespace, f.RunID, f.TaskPrefix = strings.Clone(f.Namespace), strings.Clone(f.RunID), strings.Clone(f.TaskPrefix)
	f.SignatureHash, f.Feedback = strings.Clone(f.SignatureHash), strings.Clone(f.Feedback)
	f.IDs, f.TaskNames, f.AgentNames = cloneAll(f.IDs), cloneAll(f.TaskNames), cloneAll(f.AgentNames)
	f.WorkloadIdentities, f.ToolNames, f.Dispositions = cloneAll(f.WorkloadIdentities), cloneAll(f.ToolNames), cloneAll(f.Dispositions)
	f.Text = cloneAll(f.Text)
	f.Facets, f.AnyFacets = cloneMatches(f.Facets), cloneMatches(f.AnyFacets)
	return f
}

// oversightProducersConfigMap is the operator-owned ConfigMap, in each
// namespace, that declares oversight producers under producersKey as a JSON
// list of ProducerInfo declarations. It is provisional: the eventual home is
// an admin-owned provider resource with Orka-earned trust states.
const (
	oversightProducersConfigMap = "orka-oversight-producers"
	oversightProducersKey       = "producers.json"
	oversightPreviewChartersKey = "previewCharterRevisions"
	oversightChartersKey        = "charters.json"
	// oversightContextKey holds operator-declared labels for the environment.
	oversightContextKey       = "context.json"
	oversightConcernsKey      = "concerns.json"
	maxOversightContextLabels = 8
	maxOversightLabelBytes    = 80
	maxOversightProducers     = 64
)

// ListOversightProducers handles GET /api/v1/oversight/producers: the
// declared producers merged with those seen in the last 24 hours. Declared
// trust states are never reported as verified.
func (h *Handlers) ListOversightProducers(c fiber.Ctx) error {
	namespace, err := h.authorizeOversightRead(c, "listOversightProducers", "list")
	if err != nil {
		return err
	}
	response := oversightv1.ProducersResponse{Namespace: namespace, Producers: []oversightv1.ProducerInfo{}}
	index := map[string]int{}
	declared, found, err := h.declaredOversightProducers(c.Context(), namespace)
	if err != nil {
		return err
	}
	response.Declared = found
	for _, producer := range declared {
		if producer.ProviderRef == "" || len(response.Producers) >= maxOversightProducers {
			continue
		}
		producer.TrustVerified, producer.Evaluations, producer.Failures, producer.ObservedTrustStates = false, 0, 0, nil
		if _, dup := index[producer.ProviderRef]; dup {
			continue
		}
		index[producer.ProviderRef] = len(response.Producers)
		response.Producers = append(response.Producers, producer)
	}
	records, err := h.oversightStore.ListOversightDecisions(c.Context(), storagev1.OversightDecisionFilter{
		Namespace: namespace, ObservedSince: time.Now().UTC().Add(-oversightDefaultPostureRange),
		Descending: true, Limit: storagev1.MaxOversightListLimit,
	})
	if err != nil {
		return oversightStorageError("list oversight producers", err)
	}
	views, err := oversight.Views(records, nil)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to decode oversight decisions")
	}
	observe := func(role, ref, model, family, digest, trust, status string) {
		i, ok := index[ref]
		if !ok {
			if len(response.Producers) >= maxOversightProducers {
				return
			}
			i = len(response.Producers)
			index[ref] = i
			response.Producers = append(response.Producers, oversightv1.ProducerInfo{ProviderRef: ref, Role: role})
		}
		p := &response.Producers[i]
		p.Evaluations++
		if status != oversightv1.EvaluationStatusOK {
			p.Failures++
		}
		if p.ModelID == "" {
			p.ModelID, p.ModelFamily, p.ModelDigest = model, family, digest
		}
		if trust != "" && !slices.Contains(p.ObservedTrustStates, trust) {
			p.ObservedTrustStates = append(p.ObservedTrustStates, trust)
		}
	}
	for i := range views {
		for _, s := range views[i].Scorers {
			observe("scorer", s.ProviderRef, s.ModelID, s.ModelFamily, s.ModelDigest, s.TrustState, s.Status)
		}
		for _, a := range views[i].Advisors {
			observe("advisor", a.ProviderRef, a.ModelID, a.ModelFamily, a.ModelDigest, a.TrustState, a.Status)
		}
		for _, f := range views[i].CharterFindings {
			observe("rules", f.ProducerRef, "", "", "", "", oversightv1.EvaluationStatusOK)
		}
	}
	return c.JSON(response)
}

func (h *Handlers) oversightProducersConfigMap(ctx context.Context, namespace string) (*corev1.ConfigMap, bool, error) {
	reader := h.apiReader
	if reader == nil {
		reader = h.client
	}
	if reader == nil {
		return nil, false, nil
	}
	var cm corev1.ConfigMap
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: oversightProducersConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		log.Error(err, "read oversight producer declarations", "namespace", namespace)
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, "failed to read oversight producer declarations")
	}
	return &cm, true, nil
}

func (h *Handlers) declaredOversightProducers(ctx context.Context, namespace string) ([]oversightv1.ProducerInfo, bool, error) {
	cm, found, err := h.oversightProducersConfigMap(ctx, namespace)
	if err != nil || !found {
		return nil, found, err
	}
	raw := cm.Data[oversightProducersKey]
	if raw == "" {
		return nil, true, nil
	}
	var declared []oversightv1.ProducerInfo
	if err := json.Unmarshal([]byte(raw), &declared); err != nil {
		return nil, true, fiber.NewError(fiber.StatusUnprocessableEntity,
			fmt.Sprintf("%s/%s %s is not a JSON list of producers", namespace, oversightProducersConfigMap, oversightProducersKey))
	}
	return declared, true, nil
}

func truncateOversightLabel(s string) string { return truncateOversightText(s, maxOversightLabelBytes) }

func truncateOversightText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
