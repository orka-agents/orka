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
	namespaceLedger := labels.TaskAdmissionNamespaceLedgerNamePrefix + "0123456789abcdef0123"
	childLedger := labels.TaskAdmissionChildrenLedgerNamePrefix + "0123456789abcdef0123"
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
		{name: "forged enforcement marker create", operation: apiserveradmission.Create, username: untrustedUsername, leaseName: "orka-agent-use-enforcement"},
		{name: "forged enforcement marker delete", operation: apiserveradmission.Delete, username: untrustedUsername, leaseName: "orka-agent-use-enforcement"},
		{name: "controller enforcement marker create", operation: apiserveradmission.Create, username: trustedControllerUser, leaseName: "orka-agent-use-enforcement", allowed: true},
		{name: "controller enforcement marker delete", operation: apiserveradmission.Delete, username: trustedControllerUser, leaseName: "orka-agent-use-enforcement", allowed: true},
		// Task admission ledgers: a forged or removed grant could admit Tasks
		// past a namespace or child limit.
		{name: "forged namespace ledger create", operation: apiserveradmission.Create, username: untrustedUsername, leaseName: namespaceLedger},
		{name: "forged namespace ledger generateName", operation: apiserveradmission.Create, username: untrustedUsername, generate: labels.TaskAdmissionNamespaceLedgerNamePrefix},
		{name: "forged namespace ledger update", operation: apiserveradmission.Update, username: untrustedUsername, leaseName: namespaceLedger},
		{name: "forged namespace ledger delete", operation: apiserveradmission.Delete, username: untrustedUsername, leaseName: namespaceLedger},
		{name: "forged child ledger create", operation: apiserveradmission.Create, username: untrustedUsername, leaseName: childLedger},
		{name: "forged child ledger generateName", operation: apiserveradmission.Create, username: untrustedUsername, generate: labels.TaskAdmissionChildrenLedgerNamePrefix},
		{name: "forged child ledger update", operation: apiserveradmission.Update, username: untrustedUsername, leaseName: childLedger},
		{name: "forged child ledger delete", operation: apiserveradmission.Delete, username: untrustedUsername, leaseName: childLedger},
		{name: "controller namespace ledger create", operation: apiserveradmission.Create, username: trustedControllerUser, leaseName: namespaceLedger, allowed: true},
		{name: "controller namespace ledger update", operation: apiserveradmission.Update, username: trustedControllerUser, leaseName: namespaceLedger, allowed: true},
		{name: "controller namespace ledger delete", operation: apiserveradmission.Delete, username: trustedControllerUser, leaseName: namespaceLedger, allowed: true},
		{name: "controller child ledger create", operation: apiserveradmission.Create, username: trustedControllerUser, leaseName: childLedger, allowed: true},
		{name: "controller child ledger update", operation: apiserveradmission.Update, username: trustedControllerUser, leaseName: childLedger, allowed: true},
		{name: "controller child ledger delete", operation: apiserveradmission.Delete, username: trustedControllerUser, leaseName: childLedger, allowed: true},
		{name: "garbage collector ledger update", operation: apiserveradmission.Update, username: genericGarbageCollectorUsername, leaseName: childLedger},
		{name: "namespace controller ledger delete", operation: apiserveradmission.Delete, username: namespaceControllerUsername, leaseName: namespaceLedger, allowed: true},
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
