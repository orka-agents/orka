/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/orka-agents/orka/internal/labels"
)

const ACPSuspendQuotaLeaseWebhookPath = "/validate-coordination-k8s-io-v1-acp-suspend-quota-lease"

// ACPSuspendQuotaLeaseValidator reserves the class-owned quota transaction
// Lease for exact controller identities. The class owner reference still lets
// Kubernetes cleanup controllers delete it during garbage collection.
type ACPSuspendQuotaLeaseValidator struct {
	config ExecutionModeConfig
}

func (v *ACPSuspendQuotaLeaseValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update && req.Operation != admissionv1.Delete {
		return admission.Allowed("not a suspension quota Lease write")
	}
	if !strings.HasPrefix(req.Name, labels.ACPSuspendQuotaLeaseNamePrefix) {
		return admission.Allowed("Lease is not reserved for suspension quota coordination")
	}
	if req.Operation == admissionv1.Delete && isKubernetesCleanupController(req.UserInfo.Username) {
		return admission.Allowed("Kubernetes cleanup controller may delete the suspension quota Lease")
	}
	if !v.config.controller(req.UserInfo.Username) {
		return admission.Denied("only an authorized controller identity may create, update, or delete suspension quota Leases")
	}
	return admission.Allowed("authorized controller owns the suspension quota Lease write")
}
