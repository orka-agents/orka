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

// ACPSuspendQuotaLeaseValidator reserves ACP workspace coordination Leases for
// exact controller identities. Owner references still let Kubernetes cleanup
// controllers delete them during garbage collection.
type ACPSuspendQuotaLeaseValidator struct {
	config ExecutionModeConfig
}

func (v *ACPSuspendQuotaLeaseValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update && req.Operation != admissionv1.Delete {
		return admission.Allowed("not an ACP workspace coordination Lease write")
	}
	if !strings.HasPrefix(req.Name, labels.ACPSuspendQuotaLeaseNamePrefix) &&
		!strings.HasPrefix(req.Name, labels.ACPWorkspaceRetentionFenceLeaseNamePrefix) {
		return admission.Allowed("Lease is not reserved for ACP workspace coordination")
	}
	if req.Operation == admissionv1.Delete && isKubernetesCleanupController(req.UserInfo.Username) {
		return admission.Allowed("Kubernetes cleanup controller may delete the ACP workspace coordination Lease")
	}
	if !v.config.controller(req.UserInfo.Username) {
		return admission.Denied("only an authorized controller identity may create, update, or delete ACP workspace coordination Leases")
	}
	return admission.Allowed("authorized controller owns the ACP workspace coordination Lease write")
}
