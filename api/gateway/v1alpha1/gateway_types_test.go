package v1alpha1

import "testing"

func TestExtensionCapabilityDeepCopies(t *testing.T) {
	caps := GatewayCapabilities{Extensions: []string{"meetingDraft.v1"}}
	class := &GatewayClass{Spec: GatewayClassSpec{Capabilities: caps}}
	gateway := &Gateway{Status: GatewayStatus{ObservedCapabilities: &GatewayObservedCapabilities{Capabilities: caps}}}
	binding := &GatewayBinding{Status: GatewayBindingStatus{ResolvedCapabilities: &caps}}

	class.DeepCopy().Spec.Capabilities.Extensions[0] = "changed.v1"
	gateway.DeepCopy().Status.ObservedCapabilities.Capabilities.Extensions[0] = "changed.v1"
	binding.DeepCopy().Status.ResolvedCapabilities.Extensions[0] = "changed.v1"
	if caps.Extensions[0] != "meetingDraft.v1" {
		t.Fatal("generated copies alias extension capabilities")
	}
}
