/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

func hostProvider(name string) *corev1alpha1.ConnectorProvider {
	return &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testUserNamespace},
		Spec: corev1alpha1.ConnectorProviderSpec{
			AgentHost: &corev1alpha1.ConnectorAgentHost{TemplateRef: corev1alpha1.LocalObjectReference{Name: "copilotd"}},
		},
	}
}

func linkedConnection(provider string) *corev1alpha1.Connection {
	subject := corev1alpha1.ConnectionSubject{Issuer: "https://issuer.example", Subject: "alice"}
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name:      connectors.ConnectionName(provider, subject.Issuer, subject.Subject),
			Namespace: testUserNamespace, Generation: 1,
		},
		Spec:   corev1alpha1.ConnectionSpec{ProviderRef: corev1alpha1.LocalObjectReference{Name: provider}, Subject: subject},
		Status: corev1alpha1.ConnectionStatus{GrantSequence: 1},
	}
	for _, kind := range []string{corev1alpha1.ConnectionConditionReady, corev1alpha1.ConnectionConditionProviderResolved,
		corev1alpha1.ConnectionConditionScopesGranted} {
		meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{Type: kind, Status: metav1.ConditionTrue, Reason: "Test", ObservedGeneration: 1})
	}
	return connection
}

func newProvisioner(t *testing.T, connection *corev1alpha1.Connection, objects ...client.Object) (*AgentHostProvisioner, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(agentHostScheme(t)).WithObjects(append(objects, connection)...).Build()
	return &AgentHostProvisioner{
		Client:          c,
		HostCredentials: &fakeHostCredentials{token: "ghu_first", connection: connection.Name},
		GitHubAccounts:  fakeGitHubAccounts{"ghu_first": {ID: 1001, Login: "Alice"}},
	}, c
}

func provision(t *testing.T, p *AgentHostProvisioner, connection *corev1alpha1.Connection) {
	t.Helper()
	if _, err := p.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)}); err != nil {
		t.Fatal(err)
	}
}

func hostsIn(t *testing.T, c client.Client) []corev1alpha1.AgentHost {
	t.Helper()
	hosts := &corev1alpha1.AgentHostList{}
	if err := c.List(context.Background(), hosts, client.InNamespace(testUserNamespace)); err != nil {
		t.Fatal(err)
	}
	return hosts.Items
}

func TestProvisionerGivesALinkItsHostOnce(t *testing.T) {
	connection := linkedConnection("github")
	p, c := newProvisioner(t, connection, hostProvider("github"))
	provision(t, p, connection)
	provision(t, p, connection)
	hosts := hostsIn(t, c)
	if len(hosts) != 1 {
		t.Fatalf("hosts = %d, want 1", len(hosts))
	}
	host := hosts[0]
	if host.Name != "gh-alice" || host.Spec.TemplateRef.Name != "copilotd" || host.Spec.GitHubTokenSecretRef != nil ||
		host.Spec.GitHubTokenFrom == nil || host.Spec.GitHubTokenFrom.ConnectionRef.Name != connection.Name {
		t.Fatalf("host = %s %+v", host.Name, host.Spec)
	}
	if len(host.OwnerReferences) != 0 {
		t.Fatal("deleting a Connection must never delete its host")
	}
}

func TestProvisionerCreatesNoHost(t *testing.T) {
	for name, setup := range map[string]func(*corev1alpha1.Connection) []client.Object{
		"the provider serves no hosts": func(*corev1alpha1.Connection) []client.Object {
			provider := hostProvider("github")
			provider.Spec.AgentHost = nil
			return []client.Object{provider}
		},
		"the person declined one": func(connection *corev1alpha1.Connection) []client.Object {
			connection.Annotations = map[string]string{corev1alpha1.ConnectionAnnotationAgentHost: corev1alpha1.ConnectionAgentHostDeclined}
			return []client.Object{hostProvider("github")}
		},
		"the link is not ready": func(connection *corev1alpha1.Connection) []client.Object {
			meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
				Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionFalse, Reason: "Test", ObservedGeneration: 1})
			return []client.Object{hostProvider("github")}
		},
		"two providers serve hosts": func(*corev1alpha1.Connection) []client.Object {
			return []client.Object{hostProvider("github"), hostProvider("github-two")}
		},
		"the name is taken": func(*corev1alpha1.Connection) []client.Object {
			taken := testHost()
			taken.Name = "gh-alice"
			return []client.Object{hostProvider("github"), taken}
		},
	} {
		t.Run(name, func(t *testing.T) {
			connection := linkedConnection("github")
			objects := setup(connection)
			p, c := newProvisioner(t, connection, objects...)
			provision(t, p, connection)
			for _, host := range hostsIn(t, c) {
				if fedBy(&host, connection) {
					t.Fatalf("created %s", host.Name)
				}
			}
		})
	}
}

func TestProvisionerRecreatesADeletedHost(t *testing.T) {
	connection := linkedConnection("github")
	p, c := newProvisioner(t, connection, hostProvider("github"))
	provision(t, p, connection)
	host := &corev1alpha1.AgentHost{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "gh-alice"}, host); err != nil {
		t.Fatal(err)
	}
	if requests := hostConnection(context.Background(), host); len(requests) != 1 || requests[0].Name != connection.Name {
		t.Fatalf("a host's changes must reach its Connection: %v", requests)
	}
	if err := c.Delete(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	provision(t, p, connection)
	if len(hostsIn(t, c)) != 1 {
		t.Fatal("a deleted host is provisioned again while its link stays")
	}
}

// A link blocked by another host's name tries again later, and a provider
// change revisits every link in the namespace, whichever provider they use.
func TestProvisionerRetriesBlockedLinks(t *testing.T) {
	connection := linkedConnection("github")
	taken := testHost()
	taken.Name = "gh-alice"
	other := linkedConnection("github-two")
	other.Name = "github-two-bob"
	p, c := newProvisioner(t, connection, hostProvider("github"), taken, other)
	result, err := p.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("result = %+v, err = %v; a link blocked by a taken name must retry", result, err)
	}
	if err := c.Delete(context.Background(), taken); err != nil {
		t.Fatal(err)
	}
	provision(t, p, connection)
	if len(hostsIn(t, c)) != 1 {
		t.Fatal("the link did not take the freed name")
	}
	requests := p.connectionsIn(context.Background(), hostProvider("github-two"))
	if len(requests) != 2 {
		t.Fatalf("a provider change enqueued %v, want every link in the namespace", requests)
	}
}

// Every GitHub login, managed user logins included, names a valid host, and
// no two logins share one.
func TestProvisionerNamesHostsForEveryLogin(t *testing.T) {
	for login, want := range map[string]string{
		"Alice":      "gh-alice",
		"alice-acme": "gh-alice-acme",
		"alice_acme": "gh-alice--acme",
	} {
		if got := agentHostNameFor(login); got != want {
			t.Fatalf("agentHostNameFor(%q) = %q, want %q", login, got, want)
		}
	}
	connection := linkedConnection("github")
	p, c := newProvisioner(t, connection, hostProvider("github"))
	p.GitHubAccounts = fakeGitHubAccounts{"ghu_first": {ID: 1001, Login: "alice_acme"}}
	provision(t, p, connection)
	if hosts := hostsIn(t, c); len(hosts) != 1 || hosts[0].Name != "gh-alice--acme" {
		t.Fatalf("hosts = %+v", hosts)
	}
}
