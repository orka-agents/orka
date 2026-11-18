package applicationexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const fixturePortName = "fixture"

func protectedSide(side string) string { return side + "-observer" }

func observerSide(r *run, side string) string {
	if r.state.Request.ApplicationPolicy != nil {
		return protectedSide(side)
	}
	return side
}

func actorName(id string) string {
	return "orka-actor-" + strings.TrimPrefix(pv.Digest([]byte(id)), "sha256:")[:20]
}

func fixtureServiceName(id string) string {
	return "orka-fixture-" + strings.TrimPrefix(pv.Digest([]byte(id)), "sha256:")[:20]
}

func (b *Backend) ensureProtected(ctx context.Context, r *run, side string) error {
	policy := r.state.Request.ApplicationPolicy
	if policy == nil {
		return nil
	}
	protected := protectedSide(side)
	if err := b.ensureSideInfrastructure(ctx, r, protected, false); err != nil {
		return err
	}
	namespace, subjectNamespace := namespaceName(r, protected), namespaceName(r, side)
	accounts := []string{markerAccountName}
	for _, actor := range policy.Actors {
		accounts = append(accounts, actorName(actor.ID))
	}
	for _, name := range accounts {
		object := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, AutomountServiceAccountToken: new(false)}
		if _, err := b.ensureObject(ctx, r, resourceKey(protected, "account-"+name), object); err != nil {
			return err
		}
	}
	markerRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "orka-protected-markers", Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{verbGet}, ResourceNames: []string{markerSecretName}}}}
	if _, err := b.ensureObject(ctx, r, resourceKey(protected, "marker-role"), markerRole); err != nil {
		return err
	}
	markerBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "orka-protected-markers", Namespace: namespace},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: markerRole.Name},
		Subjects: []rbacv1.Subject{{Kind: serviceAccountKind, Namespace: namespace, Name: observerAccountName},
			{Kind: serviceAccountKind, Namespace: namespace, Name: markerAccountName}}}
	if _, err := b.ensureObject(ctx, r, resourceKey(protected, "marker-binding"), markerBinding); err != nil {
		return err
	}
	readRules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{resourcePods, "serviceaccounts", resourceServices, resourceEndpoints, resourceConfigMaps}, Verbs: []string{verbGet}},
		{APIGroups: []string{networkingv1.GroupName}, Resources: []string{resourceIngresses}, Verbs: []string{verbGet}}}
	for _, resource := range policy.Resources {
		if !resource.Namespaced {
			continue
		}
		if resource.APIVersion == appsAPIVersion && resource.Resource == resourceDeployments {
			continue
		}
		group, _, found := strings.Cut(resource.APIVersion, "/")
		if !found {
			group = ""
		}
		readRules = append(readRules, rbacv1.PolicyRule{APIGroups: []string{group}, Resources: []string{resource.Resource}, Verbs: []string{verbGet}})
	}
	if fixture, profile, err := replicaProfile(r.state.Request.Checks.Application, policy); err != nil {
		return err
	} else if fixture != nil {
		readRules = append(readRules,
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{resourcePods}, Verbs: []string{verbList}},
			rbacv1.PolicyRule{APIGroups: []string{appsv1.GroupName}, Resources: []string{"replicasets"}, Verbs: []string{verbGet, verbList}},
			rbacv1.PolicyRule{APIGroups: []string{appsv1.GroupName}, Resources: []string{resourceDeployments}, Verbs: []string{verbGet}, ResourceNames: []string{profile.ReplicaTarget.Name}})
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "orka-remote-observer", Namespace: subjectNamespace}, Rules: readRules}
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "remote-observer-role"), role); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: subjectNamespace},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: serviceAccountKind, Namespace: namespace, Name: observerAccountName}}}
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "remote-observer-binding"), binding); err != nil {
		return err
	}
	for _, actor := range policy.Actors {
		rules := make([]rbacv1.PolicyRule, 0, len(actor.Permissions))
		for _, permission := range actor.Permissions {
			for _, resource := range permission.Resources {
				if clusterReadResource(policy, permission.APIGroup, resource) != nil {
					return contract.ErrInvalid
				}
			}
			rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{permission.APIGroup}, Resources: slices.Clone(permission.Resources),
				Verbs: slices.Clone(permission.Verbs), ResourceNames: slices.Clone(permission.ResourceNames)})
		}
		name := actorName(actor.ID)
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: subjectNamespace}, Rules: rules}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "actor-role-"+actor.ID), role); err != nil {
			return err
		}
		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: subjectNamespace},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: name},
			Subjects: []rbacv1.Subject{{Kind: serviceAccountKind, Namespace: namespace, Name: name}}}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "actor-binding-"+actor.ID), binding); err != nil {
			return err
		}
	}
	if err := b.ensureProtectedSecrets(ctx, r, side); err != nil {
		return err
	}
	for _, fixture := range r.state.Request.Checks.Application.Fixtures {
		if fixture.Kind == fixtureKindReplica {
			continue
		}
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fixtureServiceName(fixture.ID), Namespace: namespace},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP,
				Selector: map[string]string{runLabel: string(r.object.UID), pv.ApplicationTargetLabel: fixture.ID},
				Ports:    []corev1.ServicePort{{Name: fixturePortName, Port: int32(fixture.Port), TargetPort: intstr.FromInt(fixture.Port), Protocol: corev1.ProtocolTCP}}}}
		if _, err := b.ensureObject(ctx, r, resourceKey(protected, "fixture-service-"+fixture.ID), service); err != nil {
			return err
		}
	}
	for _, networkPolicy := range b.protectedPolicies(r, side) {
		keySide := side
		if networkPolicy.Namespace == namespace {
			keySide = protected
		}
		if _, err := b.ensureObject(ctx, r, resourceKey(keySide, "network-"+networkPolicy.Name), networkPolicy); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) ensureProtectedSecrets(ctx context.Context, r *run, side string) error {
	protected := protectedSide(side)
	key := resourceKey(protected, "marker-secret")
	plan := r.state.Request.Checks.Application
	if r.effect(key) == nil {
		ids := make(map[string]bool)
		for _, secret := range plan.SyntheticSecrets {
			for _, entry := range secret.Keys {
				ids[entry.MarkerID] = true
			}
		}
		for _, fixture := range plan.Fixtures {
			if fixture.Kind == "" || fixture.Kind == fixtureKindMarker {
				ids[fixture.ID] = true
			}
		}
		if len(ids) == 0 {
			return nil
		}
		data := make(map[string][]byte, len(ids))
		for id := range ids {
			nonce := make([]byte, 32)
			if _, err := rand.Read(nonce); err != nil {
				return contract.ErrUnavailable
			}
			data[id] = []byte(hex.EncodeToString(nonce))
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: markerSecretName, Namespace: namespaceName(r, protected)},
			Immutable: new(true), Type: corev1.SecretTypeOpaque, Data: data}
		if _, err := b.ensureObject(ctx, r, key, secret); err != nil {
			return err
		}
	}
	if r.effect(key) == nil {
		return nil
	}
	object, err := b.boundObject(ctx, r, key)
	if err != nil {
		return err
	}
	markers := object.(*corev1.Secret)
	if !ptr.Deref(markers.Immutable, false) || markers.Type != corev1.SecretTypeOpaque {
		return contract.ErrConflict
	}
	for _, definition := range plan.SyntheticSecrets {
		data := make(map[string][]byte)
		for _, entry := range definition.Keys {
			value, found := markers.Data[entry.MarkerID]
			if !found || len(value) != 64 {
				return contract.ErrConflict
			}
			data[entry.Name] = slices.Clone(value)
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: definition.Name, Namespace: namespaceName(r, side)},
			Immutable: new(true), Type: corev1.SecretTypeOpaque, Data: data}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "synthetic-secret-"+definition.Name), secret); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) markerReference(ctx context.Context, r *run, side, markerID string) (*pv.ApplicationProtectedMarkerRef, error) {
	protected := protectedSide(side)
	object, err := b.boundObject(ctx, r, resourceKey(protected, "marker-secret"))
	if err != nil {
		return nil, err
	}
	secret := object.(*corev1.Secret)
	value, found := secret.Data[markerID]
	namespace := r.effect(resourceKey(protected, "namespace"))
	if namespace == nil || !found || len(value) != 64 || !ptr.Deref(secret.Immutable, false) {
		return nil, contract.ErrConflict
	}
	return &pv.ApplicationProtectedMarkerRef{Namespace: secret.Namespace, NamespaceUID: string(namespace.UID),
		Name: secret.Name, UID: string(secret.UID), Key: markerID, Digest: pv.Digest(value)}, nil
}

func (b *Backend) actorIdentities(r *run, side string, check pv.Check) ([]pv.ApplicationActorIdentity, error) {
	protected := protectedSide(side)
	namespace := r.effect(resourceKey(protected, "namespace"))
	if namespace == nil || namespace.UID == "" {
		return nil, contract.ErrConflict
	}
	var identities []pv.ApplicationActorIdentity
	seen := make(map[string]bool)
	for _, action := range check.Application.Actions {
		if seen[action.ActorID] {
			continue
		}
		seen[action.ActorID] = true
		entry := r.effect(resourceKey(protected, "account-"+actorName(action.ActorID)))
		if entry == nil || entry.UID == "" {
			return nil, contract.ErrConflict
		}
		identities = append(identities, pv.ApplicationActorIdentity{ID: action.ActorID, Namespace: namespace.Name, NamespaceUID: string(namespace.UID),
			ServiceAccountName: entry.Name, ServiceAccountUID: string(entry.UID)})
	}
	return identities, nil
}

func (b *Backend) protectedPolicies(r *run, side string) []*networkingv1.NetworkPolicy {
	subjectNamespace, namespace := namespaceName(r, side), namespaceName(r, protectedSide(side))
	peer := func(ns, role string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: ns}},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{roleLabel: role}}}
	}
	policy := func(ns, name, role string) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{roleLabel: role}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}}}
	}
	subject := policy(subjectNamespace, "orka-protected-fixtures", roleSubject)
	observer := policy(namespace, "orka-observer-subject", roleObserver)
	marker := policy(namespace, "orka-marker-api", roleMarker)
	catalogue := policy(namespace, "orka-catalogue-peers", roleFixture)
	apiIP, apiPort, _ := apiAddress(b.config.TargetAPIServer)
	marker.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr(apiIP)}}},
		Ports: []networkingv1.NetworkPolicyPort{netPort(apiPort)}}}
	result := []*networkingv1.NetworkPolicy{subject, observer, marker, catalogue}
	for _, port := range r.state.Request.Checks.Application.Runtime.Ports {
		observer.Spec.Egress = append(observer.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{peer(subjectNamespace, roleSubject)},
			Ports: []networkingv1.NetworkPolicyPort{netPort(port)}})
		subject.Spec.Ingress = append(subject.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{peer(namespace, roleObserver)},
			Ports: []networkingv1.NetworkPolicyPort{netPort(port)}})
	}
	for _, fixture := range r.state.Request.Checks.Application.Fixtures {
		if fixture.Kind == fixtureKindReplica {
			continue
		}
		fixturePeer := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: namespace}},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{pv.ApplicationTargetLabel: fixture.ID}}}
		subject.Spec.Egress = append(subject.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{fixturePeer},
			Ports: []networkingv1.NetworkPolicyPort{netPort(fixture.Port)}})
		observer.Spec.Egress = append(observer.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{fixturePeer},
			Ports: []networkingv1.NetworkPolicyPort{netPort(fixture.Port)}})
		catalogue.Spec.Egress = append(catalogue.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{fixturePeer},
			Ports: []networkingv1.NetworkPolicyPort{netPort(fixture.Port)}})
		incoming := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "orka-incoming-" + strings.TrimPrefix(pv.Digest([]byte(fixture.ID)), "sha256:")[:16], Namespace: namespace},
			Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{pv.ApplicationTargetLabel: fixture.ID}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{peer(subjectNamespace, roleSubject), peer(namespace, roleObserver), peer(namespace, roleFixture)},
					Ports: []networkingv1.NetworkPolicyPort{netPort(fixture.Port)}}}}}
		result = append(result, incoming)
	}
	return result
}

func (b *Backend) fixtureAliases(ctx context.Context, r *run, side string) ([]corev1.HostAlias, error) {
	var aliases []corev1.HostAlias
	if r.state.Request.ApplicationPolicy == nil {
		return aliases, nil
	}
	for _, fixture := range r.state.Request.Checks.Application.Fixtures {
		if fixture.Kind == fixtureKindReplica {
			continue
		}
		object, err := b.boundObject(ctx, r, resourceKey(protectedSide(side), "fixture-service-"+fixture.ID))
		if err != nil {
			return nil, err
		}
		service := object.(*corev1.Service)
		if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
			return nil, contract.ErrUnavailable
		}
		aliases = append(aliases, corev1.HostAlias{IP: service.Spec.ClusterIP, Hostnames: []string{fixture.ID, service.Name,
			fmt.Sprintf("%s.%s.svc.cluster.local", service.Name, service.Namespace)}})
	}
	return aliases, nil
}

func policyDigest(policy *pv.ApplicationPolicy) string {
	content, _ := json.Marshal(policy)
	return pv.Digest(content)
}
