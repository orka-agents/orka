package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// CreateEvalRunRequest creates an EvalRun. Name, or generateName for a
// server-chosen suffix, is required.
type CreateEvalRunRequest struct {
	Name         string                   `json:"name"`
	GenerateName string                   `json:"generateName"`
	Namespace    string                   `json:"namespace"`
	Metadata     EvalRunMetadataRequest   `json:"metadata"`
	Spec         corev1alpha1.EvalRunSpec `json:"spec"`
}

// EvalRunMetadataRequest is the metadata a client may set on a new EvalRun.
type EvalRunMetadataRequest struct {
	MetadataRequest
	GenerateName string `json:"generateName,omitempty"`
}

// CreateEvalRun creates an EvalRun. It is authorized like creating the
// EvalRun; the EvalRun's own admission and the controller validate the spec.
func (h *Handlers) CreateEvalRun(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "eval runs cannot be created with transaction tokens"); err != nil {
		return err
	}
	var req CreateEvalRunRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	name, generateName := firstNonEmpty(req.Name, req.Metadata.Name), firstNonEmpty(req.GenerateName, req.Metadata.GenerateName)
	if name == "" && generateName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name or generateName is required")
	}
	namespace, err := h.resolveNamespace(c, firstNonEmpty(req.Namespace, req.Metadata.Namespace))
	if err != nil {
		return err
	}
	run := &corev1alpha1.EvalRun{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata.MetadataRequest),
		Spec:       req.Spec,
	}
	if name == "" {
		run.GenerateName = generateName
	}
	if err := h.client.Create(c.Context(), run); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "eval run already exists")
		}
		if invalid := invalidObjectError("eval run", err); invalid != nil {
			return invalid
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create eval run: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(run)
}

// GetEvalRun returns one EvalRun, including its status.
func (h *Handlers) GetEvalRun(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "eval runs cannot be read with transaction tokens"); err != nil {
		return err
	}
	run, err := h.getEvalRun(c)
	if err != nil {
		return err
	}
	return c.JSON(run)
}

func (h *Handlers) getEvalRun(c fiber.Ctx) (*corev1alpha1.EvalRun, error) {
	return h.getEvalRunFrom(c, h.client)
}

// getEvalRunUncached reads the EvalRun from the API server, so a run that
// was deleted and recreated is never seen as its earlier incarnation.
func (h *Handlers) getEvalRunUncached(c fiber.Ctx) (*corev1alpha1.EvalRun, error) {
	if h.apiReader != nil {
		return h.getEvalRunFrom(c, h.apiReader)
	}
	return h.getEvalRunFrom(c, h.client)
}

func (h *Handlers) getEvalRunFrom(c fiber.Ctx, reader client.Reader) (*corev1alpha1.EvalRun, error) {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	run := &corev1alpha1.EvalRun{}
	if err := reader.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: c.Params("name")}, run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "eval run not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get eval run: %v", err))
	}
	return run, nil
}

// refuseEvalTransactionTokens refuses transaction-token callers: no
// context-token scope covers evals.
func refuseEvalTransactionTokens(c fiber.Ctx, message string) error {
	if ui := GetUserInfo(c); ui != nil && ui.AuthType == AuthTypeContextToken {
		return fiber.NewError(fiber.StatusForbidden, message)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// evalTrialsJSONLContentType is the media type of the JSONL trial listing.
const evalTrialsJSONLContentType = "application/jsonl"

// ListEvalRunTrials returns the trial records of one EvalRun as JSON or, with
// format=jsonl or an Accept of application/jsonl, one record per line. Reading
// them is authorized like reading the EvalRun.
func (h *Handlers) ListEvalRunTrials(c fiber.Ctx) error {
	if h.evalTrialStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "eval trial store not configured")
	}
	if err := refuseEvalTransactionTokens(c, "eval trials cannot be read with transaction tokens"); err != nil {
		return err
	}
	jsonl, err := evalTrialsWantJSONL(c)
	if err != nil {
		return err
	}
	run, err := h.getEvalRun(c)
	if err != nil {
		return err
	}
	records, err := h.evalTrialStore.ListEvalTrials(c.Context(), run.Namespace, run.Name, string(run.UID))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list eval trials: %v", err))
	}
	if records == nil {
		records = []store.EvalTrialRecord{}
	}
	if !jsonl {
		return c.JSON(fiber.Map{apiFieldItems: records})
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for i := range records {
		if err := encoder.Encode(&records[i]); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to encode eval trials")
		}
	}
	c.Set(fiber.HeaderContentType, evalTrialsJSONLContentType)
	return c.Send(body.Bytes())
}

func evalTrialsWantJSONL(c fiber.Ctx) (bool, error) {
	switch format := strings.TrimSpace(c.Query("format")); format {
	case "jsonl":
		return true, nil
	case "json":
		return false, nil
	case "":
		accept := strings.ToLower(c.Get(fiber.HeaderAccept))
		return strings.Contains(accept, evalTrialsJSONLContentType) || strings.Contains(accept, "application/x-ndjson"), nil
	default:
		return false, fiber.NewError(fiber.StatusBadRequest, "format must be json or jsonl")
	}
}
