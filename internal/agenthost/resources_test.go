/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func namedHost(name, uid string) *corev1alpha1.AgentHost {
	return &corev1alpha1.AgentHost{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", UID: types.UID(uid)}}
}

func TestObjectNamesKeepAShortNameInTheConnectionSecret(t *testing.T) {
	if got := ObjectNames(namedHost("alice", "uid-1"), "orka-agent-hosts").Connection; got != "alice-connection" {
		t.Fatalf("connection Secret = %q", got)
	}
}

func TestObjectNamesStayValidAndDistinctForLongNames(t *testing.T) {
	// A 253-character name whose 242nd character is a dot.
	long := strings.Repeat("a", 241) + "." + strings.Repeat("b", 11)
	other := strings.Repeat("a", 241) + "." + strings.Repeat("c", 11)
	first := ObjectNames(namedHost(long, "uid-1"), "orka-agent-hosts")
	second := ObjectNames(namedHost(other, "uid-2"), "orka-agent-hosts")
	for _, names := range []Names{first, second} {
		if errs := validation.IsDNS1123Subdomain(names.Connection); len(errs) != 0 {
			t.Fatalf("connection Secret %q is invalid: %v", names.Connection, errs)
		}
	}
	if first.Connection == second.Connection {
		t.Fatalf("two hosts share the connection Secret %q", first.Connection)
	}
}

func TestOwnerLabelsStayValidForLongNames(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 62) + "-b", strings.Repeat("a", 62) + ".b"} {
		value := OwnerLabels(namedHost(name, "uid-1"))[LabelHostName]
		if errs := validation.IsValidLabelValue(value); len(errs) != 0 {
			t.Fatalf("host name label %q is invalid: %v", value, errs)
		}
	}
}

func TestHostNetworkPolicyKeepsHTTPSToPublicAddresses(t *testing.T) {
	policy := HostNetworkPolicy(namedHost("alice", "uid-1"), ObjectNames(namedHost("alice", "uid-1"), "orka-agent-hosts"), "nonce",
		validTemplate(), ControllerPeer{Namespace: "orka-system", PodLabels: map[string]string{"app": "orka"}})
	var https *networkingv1.NetworkPolicyEgressRule
	for i := range policy.Spec.Egress {
		for _, port := range policy.Spec.Egress[i].Ports {
			if port.Port != nil && port.Port.IntValue() == 443 {
				https = &policy.Spec.Egress[i]
			}
		}
	}
	if https == nil || len(https.To) == 0 {
		t.Fatalf("HTTPS egress has no destination limit: %+v", https)
	}
	for _, peer := range https.To {
		if peer.IPBlock == nil || len(peer.IPBlock.Except) == 0 {
			t.Fatalf("HTTPS egress peer is not a public address block: %+v", peer)
		}
	}
	if !slices.Contains(https.To[0].IPBlock.Except, "10.0.0.0/8") {
		t.Fatalf("private addresses are reachable over HTTPS: %+v", https.To[0].IPBlock)
	}
}

// Node-local DNS caches listen on link-local addresses rather than in the
// cluster DNS Pods, so DNS reaches both; link-local stays closed otherwise.
func TestHostNetworkPolicyReachesNodeLocalDNS(t *testing.T) {
	policy := HostNetworkPolicy(namedHost("alice", "uid-1"), ObjectNames(namedHost("alice", "uid-1"), "orka-agent-hosts"), "nonce",
		validTemplate(), ControllerPeer{Namespace: "orka-system", PodLabels: map[string]string{"app": "orka"}})
	for _, rule := range policy.Spec.Egress {
		linkLocal := slices.ContainsFunc(rule.To, func(peer networkingv1.NetworkPolicyPeer) bool {
			return peer.IPBlock != nil && peer.IPBlock.CIDR == "169.254.0.0/16"
		})
		dns := len(rule.Ports) > 0
		for _, port := range rule.Ports {
			dns = dns && port.Port != nil && port.Port.IntValue() == 53
		}
		switch {
		case dns && !linkLocal:
			t.Fatalf("DNS cannot reach node-local caches: %+v", rule.To)
		case linkLocal && !dns:
			t.Fatalf("link-local addresses are open beyond DNS: %+v", rule.Ports)
		}
	}
}
