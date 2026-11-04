package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// evalTrialsJSONLContentType is the media type of the JSONL trial listing.
const evalTrialsJSONLContentType = "application/jsonl"

// ListEvalRunTrials returns the trial records of one EvalRun as JSON or, with
// format=jsonl or an Accept of application/jsonl, one record per line. Reading
// them is authorized like reading the EvalRun.
func (h *Handlers) ListEvalRunTrials(c fiber.Ctx) error {
	if h.evalTrialStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "eval trial store not configured")
	}
	// No transaction-token scope covers evals, so token callers are refused.
	if ui := GetUserInfo(c); ui != nil && ui.AuthType == AuthTypeContextToken {
		return fiber.NewError(fiber.StatusForbidden, "eval trials cannot be read with transaction tokens")
	}
	jsonl, err := evalTrialsWantJSONL(c)
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	run := &corev1alpha1.EvalRun{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: c.Params("name")}, run); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "eval run not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get eval run: %v", err))
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
