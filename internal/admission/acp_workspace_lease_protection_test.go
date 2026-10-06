/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiserveradmission "k8s.io/apiserver/pkg/admission"

	"github.com/orka-agents/orka/internal/labels"
)

func TestACPWorkspaceLeaseProtectionReservesControllerLeases(t *testing.T) {
	policy := compileAdmissionPolicy(t, "acp_workspace_lease_protection.yaml", strings.NewReplacer(
		"ORKA_NAMESPACE", "orka-system",
		"CONTROLLER_SA", "orka-controller-manager",
	))
	quotaLease := labels.ACPSuspendQuotaLeaseNamePrefix + "0123456789abcdef01234567"
	retentionFence := labels.ACPWorkspaceRetentionFenceLeaseNamePrefix + "0123456789abcdef01234567"
	tests := []struct {
		name      string
		operation apiserveradmission.Operation
		username  string
		leaseName string
		generate  string
		allowed   bool
	}{
		{name: "ordinary Lease", operation: apiserveradmission.Create, username: untrustedUsername, leaseName: "worker-heartbeat", allowed: true},
		{name: "ordinary generated Lease", operation: apiserveradmission.Create, username: untrustedUsername, generate: "worker-heartbeat-", allowed: true},
		{name: "ordinary Lease delete", operation: apiserveradmission.Delete, username: untrustedUsername, leaseName: "worker-heartbeat", allowed: true},
		{name: "forged quota create", operation: apiserveradmission.Create, username: untrustedUsername, leaseName: quotaLease},
		{name: "forged quota generateName", operation: apiserveradmission.Create, username: untrustedUsername, generate: labels.ACPSuspendQuotaLeaseNamePrefix},
		{name: "forged quota update", operation: apiserveradmission.Update, username: untrustedUsername, leaseName: quotaLease},
		{name: "forged quota delete", operation: apiserveradmission.Delete, username: untrustedUsername, leaseName: quotaLease},
		{name: "forged retention create", operation: apiserveradmission.Create, username: untrustedUsername, leaseName: retentionFence},
		{name: "forged retention generateName", operation: apiserveradmission.Create, username: untrustedUsername, generate: labels.ACPWorkspaceRetentionFenceLeaseNamePrefix},
		{name: "forged retention update", operation: apiserveradmission.Update, username: untrustedUsername, leaseName: retentionFence},
		{name: "forged retention delete", operation: apiserveradmission.Delete, username: untrustedUsername, leaseName: retentionFence},
		{name: "controller quota create", operation: apiserveradmission.Create, username: trustedControllerUser, leaseName: quotaLease, allowed: true},
		{name: "controller quota update", operation: apiserveradmission.Update, username: trustedControllerUser, leaseName: quotaLease, allowed: true},
		{name: "controller retention create", operation: apiserveradmission.Create, username: trustedControllerUser, leaseName: retentionFence, allowed: true},
		{name: "controller retention update", operation: apiserveradmission.Update, username: trustedControllerUser, leaseName: retentionFence, allowed: true},
		{name: "controller retention delete", operation: apiserveradmission.Delete, username: trustedControllerUser, leaseName: retentionFence, allowed: true},
		{name: "garbage collector delete", operation: apiserveradmission.Delete, username: genericGarbageCollectorUsername, leaseName: retentionFence, allowed: true},
		{name: "namespace controller delete", operation: apiserveradmission.Delete, username: namespaceControllerUsername, leaseName: quotaLease, allowed: true},
		{name: "garbage collector update", operation: apiserveradmission.Update, username: genericGarbageCollectorUsername, leaseName: quotaLease},
		{name: "garbage collector create", operation: apiserveradmission.Create, username: kubeControllerManagerUsername, leaseName: retentionFence},
	}

	kind := coordinationv1.SchemeGroupVersion.WithKind("Lease")
	resource := coordinationv1.SchemeGroupVersion.WithResource("leases")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := &unstructured.Unstructured{}
			lease.SetGroupVersionKind(kind)
			lease.SetNamespace(admissionTestNamespace)
			lease.SetName(test.leaseName)
			lease.SetGenerateName(test.generate)
			var object, oldObject *unstructured.Unstructured
			switch test.operation {
			case apiserveradmission.Create:
				object = lease
			case apiserveradmission.Update:
				object, oldObject = lease, lease.DeepCopy()
			case apiserveradmission.Delete:
				oldObject = lease
			}
			require.Equal(t, test.allowed,
				policy.admits(t, kind, resource, test.leaseName, test.operation, test.username, "", object, oldObject))
		})
	}
}
