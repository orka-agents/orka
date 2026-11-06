package outboundaccess

import (
	"context"
	"errors"
	"testing"

	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestConnectionRequiredErrorTellsTheAgentWhatThePersonMustDo(t *testing.T) {
	named := func(kind ConnectionRequiredKind) *ConnectionRequiredError {
		return &ConnectionRequiredError{Kind: kind, Provider: "azure", DisplayName: "Azure", Namespace: "team-a"}
	}
	notReady := named(ConnectionNotReady)
	notReady.State = "Revoked"
	ambiguous := named(ConnectionAmbiguous)
	ambiguous.Links = 2
	for name, tc := range map[string]struct {
		err  *ConnectionRequiredError
		want string
	}{
		"not linked": {named(ConnectionNotLinked),
			"Azure is not linked for the person who asked. They need to run `orka connect azure -n team-a`, then run this Task again."},
		"expired": {named(ConnectionExpired),
			"The Azure link for the person who asked has expired. They need to run `orka connect azure -n team-a`, then run this Task again."},
		"not ready": {notReady,
			"The Azure link for the person who asked is not ready (Revoked). They need to run `orka connect azure -n team-a`, then run this Task again."},
		"ambiguous": {ambiguous,
			"The person who asked has 2 links to Azure, so Orka uses none. They need to keep one (`orka connection list`, then `orka connection delete <name>`), then run this Task again."},
		"relinked": {named(ConnectionRelinked),
			"The Azure link for the person who asked was re-linked after this Task started. Run this Task again."},
		"no display name": {&ConnectionRequiredError{Kind: ConnectionNotLinked, Provider: "azure"},
			"azure is not linked for the person who asked. They need to run `orka connect azure`, then run this Task again."},
		"no provider": {&ConnectionRequiredError{Kind: ConnectionExpired},
			"The linked account of the person who asked has expired. They need to link it again, then run this Task again."},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

// The resolver names the provider, its display name, and the namespace in a
// refusal, whether the Task carries no link or the credential source refused
// the one it carries.
func TestKubernetesResolverNamesTheProviderInAConnectionRefusal(t *testing.T) {
	f := newConnectionModeFixture(t)
	provider := acceptedProvider()
	provider.Spec.DisplayName = "GitHub"
	reader := ctrlfake.NewClientBuilder().WithScheme(f.scheme).WithObjects(f.policy, provider).Build()
	const want = "They need to run `orka connect github -n tenant`, then run this Task again."

	unlinked := f.base
	unlinked.FrozenConnections = nil
	_, err := (&KubernetesResolver{Reader: reader, Connections: &fakeConnectionSource{}}).Resolve(context.Background(), unlinked)
	refusal, ok := errors.AsType[*ConnectionRequiredError](err)
	if !ok || refusal.Error() != "GitHub is not linked for the person who asked. "+want {
		t.Fatalf("no link: err = %v", err)
	}

	source := &fakeConnectionSource{err: &ConnectionRequiredError{Kind: ConnectionExpired}}
	_, err = (&KubernetesResolver{Reader: reader, Connections: source}).Resolve(context.Background(), f.base)
	refusal, ok = errors.AsType[*ConnectionRequiredError](err)
	if !ok || refusal.Error() != "The GitHub link for the person who asked has expired. "+want {
		t.Fatalf("source refusal: err = %v", err)
	}
}
