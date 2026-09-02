/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/orka-agents/orka/internal/labels"
)

func TestACPSuspendQuotaLeaseValidatorProtectsReservedLeases(t *testing.T) {
	validator := &ACPSuspendQuotaLeaseValidator{
		config: ExecutionModeConfig{ControllerUsernames: []string{trustedControllerUser}}.normalized(),
	}
	quotaLease := labels.ACPSuspendQuotaLeaseNamePrefix + "0123456789abcdef01234567"
	retentionFence := labels.ACPWorkspaceRetentionFenceLeaseNamePrefix + "0123456789abcdef01234567"
	tests := []struct {
		name      string
		operation admissionv1.Operation
		username  string
		leaseName string
		generate  string
		allowed   bool
	}{
		{name: "ordinary Lease", operation: admissionv1.Create, username: untrustedUsername, leaseName: "worker-heartbeat", allowed: true},
		{name: "ordinary generated Lease", operation: admissionv1.Create, username: untrustedUsername, generate: "worker-heartbeat-", allowed: true},
		{name: "forged quota create", operation: admissionv1.Create, username: untrustedUsername, leaseName: quotaLease},
		{name: "forged quota generateName", operation: admissionv1.Create, username: untrustedUsername, generate: labels.ACPSuspendQuotaLeaseNamePrefix},
		{name: "forged quota update", operation: admissionv1.Update, username: untrustedUsername, leaseName: quotaLease},
		{name: "forged quota delete", operation: admissionv1.Delete, username: untrustedUsername, leaseName: quotaLease},
		{name: "forged retention create", operation: admissionv1.Create, username: untrustedUsername, leaseName: retentionFence},
		{name: "forged retention generateName", operation: admissionv1.Create, username: untrustedUsername, generate: labels.ACPWorkspaceRetentionFenceLeaseNamePrefix},
		{name: "forged retention update", operation: admissionv1.Update, username: untrustedUsername, leaseName: retentionFence},
		{name: "forged retention delete", operation: admissionv1.Delete, username: untrustedUsername, leaseName: retentionFence},
		{name: "controller quota create", operation: admissionv1.Create, username: trustedControllerUser, leaseName: quotaLease, allowed: true},
		{name: "controller retention create", operation: admissionv1.Create, username: trustedControllerUser, leaseName: retentionFence, allowed: true},
		{name: "controller retention update", operation: admissionv1.Update, username: trustedControllerUser, leaseName: retentionFence, allowed: true},
		{name: "controller retention delete", operation: admissionv1.Delete, username: trustedControllerUser, leaseName: retentionFence, allowed: true},
		{name: "garbage collector delete", operation: admissionv1.Delete, username: genericGarbageCollectorUsername, leaseName: retentionFence, allowed: true},
		{name: "unhandled operation", operation: admissionv1.Connect, username: untrustedUsername, leaseName: retentionFence, allowed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
				Name: test.leaseName, GenerateName: test.generate,
			}})
			require.NoError(t, err)
			response := validator.Handle(context.Background(), ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: test.operation,
				Name:      test.leaseName,
				Namespace: admissionTestNamespace,
				UserInfo:  authenticationv1.UserInfo{Username: test.username},
				Object:    runtime.RawExtension{Raw: raw},
			}})
			require.Equal(t, test.allowed, response.Allowed, response.Result.Message)
			if !test.allowed {
				require.Contains(t, response.Result.Message, "only an authorized controller identity")
			}
		})
	}
}
