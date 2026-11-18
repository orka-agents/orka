package api

import (
	"reflect"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

// ClaimRemediationModelCall consumes one non-replayable outbound-attempt permit
// for the exact current managed AI worker. No requested budget or identity is
// accepted in the body.
func (h *Handlers) ClaimRemediationModelCall(c fiber.Ctx) error {
	setRemediationPrivate(c)
	if h.remediationService == nil ||
		(reflect.ValueOf(h.remediationService).Kind() == reflect.Pointer && reflect.ValueOf(h.remediationService).IsNil()) {
		return remediationHTTPError(contract.ErrDisabled)
	}
	budget, ok := h.remediationService.(contract.ModelCallBudgetService)
	if !ok {
		return fiber.NewError(fiber.StatusNotImplemented, "remediation model-call accounting is unavailable")
	}
	if c.Request().Header.ContentLength() != 0 || len(c.BodyRaw()) != 0 {
		return remediationHTTPError(contract.ErrInvalid)
	}
	reader := h.apiReader
	if reader == nil {
		return remediationHTTPError(contract.ErrUnavailable)
	}
	authorizer := internalCallerAuthorizer{k8sReader: reader}
	namespace, name := c.Params("namespace"), c.Params("taskName")
	task, err := authorizer.verifyTaskCaller(c, namespace, name)
	if err != nil {
		return err
	}
	if err := authorizer.verifyUsageWriter(c.Context(), GetUserInfo(c), task); err != nil {
		return err
	}
	if err := budget.ClaimModelCall(c.Context(), contract.ModelCallClaim{
		Namespace: namespace, TaskName: task.Name, TaskUID: string(task.UID), JobUID: task.Status.JobUID,
		PodUID: firstUserExtra(GetUserInfo(c), "authentication.kubernetes.io/pod-uid"),
	}); err != nil {
		return remediationHTTPError(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
