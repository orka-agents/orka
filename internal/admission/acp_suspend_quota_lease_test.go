/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/orka-agents/orka/internal/labels"
)

func TestACPSuspendQuotaLeaseValidatorProtectsReservedLeases(t *testing.T) {
	validator := &ACPSuspendQuotaLeaseValidator{
		config: ExecutionModeConfig{ControllerUsernames: []string{trustedControllerUser}}.normalized(),
	}
	reserved := labels.ACPSuspendQuotaLeaseNamePrefix + "0123456789abcdef01234567"
	tests := []struct {
		name      string
		operation admissionv1.Operation
		username  string
		leaseName string
		allowed   bool
	}{
		{name: "ordinary Lease", operation: admissionv1.Create, username: untrustedUsername, leaseName: "worker-heartbeat", allowed: true},
		{name: "forged create", operation: admissionv1.Create, username: untrustedUsername, leaseName: reserved},
		{name: "forged update", operation: admissionv1.Update, username: untrustedUsername, leaseName: reserved},
		{name: "forged delete", operation: admissionv1.Delete, username: untrustedUsername, leaseName: reserved},
		{name: "controller create", operation: admissionv1.Create, username: trustedControllerUser, leaseName: reserved, allowed: true},
		{name: "controller update", operation: admissionv1.Update, username: trustedControllerUser, leaseName: reserved, allowed: true},
		{name: "controller delete", operation: admissionv1.Delete, username: trustedControllerUser, leaseName: reserved, allowed: true},
		{name: "garbage collector delete", operation: admissionv1.Delete, username: genericGarbageCollectorUsername, leaseName: reserved, allowed: true},
		{name: "unhandled operation", operation: admissionv1.Connect, username: untrustedUsername, leaseName: reserved, allowed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validator.Handle(context.Background(), ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: test.operation,
				Name:      test.leaseName,
				Namespace: admissionTestNamespace,
				UserInfo:  authenticationv1.UserInfo{Username: test.username},
			}})
			require.Equal(t, test.allowed, response.Allowed, response.Result.Message)
			if !test.allowed {
				require.Contains(t, response.Result.Message, "only an authorized controller identity")
			}
		})
	}
}
