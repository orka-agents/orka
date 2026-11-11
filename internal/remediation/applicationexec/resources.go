package applicationexec

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func namespaceName(r *run, side string) string {
	data, _ := json.Marshal([]string{r.state.TargetUID, string(r.object.UID), side})
	return "app-" + strings.TrimPrefix(pv.Digest(data), "sha256:")[:32]
}

func (b *Backend) ensureClaim(ctx context.Context, r *run) error {
	holder, _ := json.Marshal([]string{r.state.ControlUID, string(r.object.UID), string(r.state.Task.UID)})
	// This fixed target-side lease never expires or transfers on a timer.
	// Another run cannot acquire the cluster while any owned effect is unknown.
	claim := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "orka-application-execution", Namespace: metav1.NamespaceSystem},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: new(string(holder))}}
	object, err := b.ensureObject(ctx, r, "target-claim", claim)
	if err != nil {
		return err
	}
	actual, ok := object.(*coordinationv1.Lease)
	if !ok || ptr.Deref(actual.Spec.HolderIdentity, "") != string(holder) || actual.Spec.LeaseDurationSeconds != nil {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) ensureNamespace(ctx context.Context, r *run, side string) error {
	name := namespaceName(r, side)
	expected := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		"pod-security.kubernetes.io/enforce":         podSecurityRestricted,
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/audit":           podSecurityRestricted,
		"pod-security.kubernetes.io/warn":            podSecurityRestricted,
	}}}
	object, err := b.ensureObject(ctx, r, resourceKey(side, "namespace"), expected)
	if err != nil {
		return err
	}
	for key, value := range expected.Labels {
		if object.GetLabels()[key] != value {
			return ErrIsolation
		}
	}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "orka-bounds", Namespace: name},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePods: resource.MustParse("32"), corev1.ResourceRequestsCPU: resource.MustParse("16"),
			corev1.ResourceLimitsCPU: resource.MustParse("16"), corev1.ResourceRequestsMemory: resource.MustParse("16Gi"),
			corev1.ResourceLimitsMemory: resource.MustParse("16Gi"), corev1.ResourceRequestsStorage: resource.MustParse("2Gi"),
			corev1.ResourcePersistentVolumeClaims: resource.MustParse("2"),
			corev1.ResourceServices:               resource.MustParse("32"), corev1.ResourceServicesNodePorts: resource.MustParse("0"),
			corev1.ResourceServicesLoadBalancers:    resource.MustParse("0"),
			corev1.ResourceName("count/jobs.batch"): resource.MustParse("32"),
			corev1.ResourceName("count/configmaps"): resource.MustParse("64"),
			corev1.ResourceName("count/secrets"):    resource.MustParse("8"),
		}}}
	if policy := r.state.Request.ApplicationPolicy; policy != nil {
		quota.Spec.Hard[corev1.ResourceName("count/secrets")] = resource.MustParse(strconv.Itoa(max(8, policy.MaxSyntheticSecrets+4)))
		for _, rule := range policy.Resources {
			group, _, found := strings.Cut(rule.APIVersion, "/")
			key := "count/" + rule.Resource
			if found {
				key += "." + group
			}
			if fixture, _, err := replicaProfile(r.state.Request.Checks.Application, r.state.Request.ApplicationPolicy); err != nil {
				return err
			} else if fixture != nil {
				quota.Spec.Hard[corev1.ResourceName("count/deployments.apps")] = resource.MustParse("1")
				quota.Spec.Hard[corev1.ResourceName("count/replicasets.apps")] = resource.MustParse("2")
				quota.Spec.Hard[corev1.ResourceName("count/horizontalpodautoscalers.autoscaling")] = resource.MustParse("2")
				quota.Spec.Hard[corev1.ResourceName("count/pods")] = resource.MustParse("48")
			}
			if rule.CRD != nil && rule.Namespaced {
				quota.Spec.Hard[corev1.ResourceName(key)] = resource.MustParse("32")
			}
		}
	}
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "quota"), quota); err != nil {
		return err
	}
	return nil
}

func (b *Backend) ensureSideInfrastructure(ctx context.Context, r *run, side string, application bool) error {
	if err := b.ensureNamespace(ctx, r, side); err != nil {
		return err
	}
	namespace := namespaceName(r, side)
	for _, name := range []string{roleWorker, roleSubject, roleObserver} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "orka-" + name, Namespace: namespace},
			AutomountServiceAccountToken: new(false)}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "sa-"+name), sa); err != nil {
			return err
		}
	}
	subject := []rbacv1.PolicyRule(nil)
	if application {
		var err error
		subject, err = subjectRules(r.state.Request.Checks.Application, r.state.Request.Checks.Checks, r.state.Request.ApplicationPolicy)
		if err != nil {
			return err
		}
	}
	observer := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{resourcePods, "serviceaccounts", resourceConfigMaps, resourceServices, resourceEndpoints}, Verbs: []string{verbGet}},
		{APIGroups: []string{networkingv1.GroupName}, Resources: []string{resourceIngresses}, Verbs: []string{verbGet}},
	}
	for _, definition := range []struct {
		name  string
		rules []rbacv1.PolicyRule
	}{{roleSubject, subject}, {roleObserver, observer}} {
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "orka-" + definition.name, Namespace: namespace}, Rules: definition.rules}
		object, err := b.ensureObject(ctx, r, resourceKey(side, "role-"+definition.name), role)
		if err != nil {
			return err
		}
		actual := object.(*rbacv1.Role)
		actualJSON, _ := json.Marshal(actual.Rules)
		expectedJSON, _ := json.Marshal(definition.rules)
		if string(actualJSON) != string(expectedJSON) {
			return contract.ErrConflict
		}
		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "orka-" + definition.name, Namespace: namespace},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: role.Name},
			Subjects: []rbacv1.Subject{{Kind: serviceAccountKind, Name: "orka-" + definition.name, Namespace: namespace}}}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "binding-"+definition.name), binding); err != nil {
			return err
		}
	}
	if application {
		if err := b.ensureSubjectClusterReads(ctx, r, side); err != nil {
			return err
		}
	}
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: target\n  cluster:\n    server: " + b.config.TargetAPIServer +
		"\n    certificate-authority: /target/ca.crt\ncontexts:\n- name: target\n  context:\n    cluster: target\n    user: target\n    namespace: " +
		namespace + "\ncurrent-context: target\nusers:\n- name: target\n  user:\n    tokenFile: /target/token\n"
	targetConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "orka-target", Namespace: namespace}, Immutable: new(true),
		Data: map[string]string{kubeconfigKey: kubeconfig, caCertificateKey: string(b.config.TargetCAData), namespaceKey: namespace}}
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "target-config"), targetConfig); err != nil {
		return err
	}
	for _, policy := range b.networkPolicies(r, side, application) {
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "network-"+policy.Name), policy); err != nil {
			return err
		}
	}
	if !application {
		return nil
	}
	for _, volume := range []struct{ name, size string }{{sourceVolumeName, "1Gi"}, {artifactsVolumeName, "512Mi"}} {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "orka-" + volume.name, Namespace: namespace},
			Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: new(b.config.StorageClass), VolumeMode: new(corev1.PersistentVolumeFilesystem),
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(volume.size)}}}}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, "pvc-"+volume.name), pvc); err != nil {
			return err
		}
	}
	return nil
}

func cidr(ip netip.Addr) string { return netip.PrefixFrom(ip, ip.BitLen()).String() }

func netPort(port int) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt(port))}
}

func (b *Backend) networkPolicies(r *run, side string, application bool) []*networkingv1.NetworkPolicy {
	namespace := namespaceName(r, side)
	newPolicy := func(name string, selector metav1.LabelSelector) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: networkingv1.NetworkPolicySpec{PodSelector: selector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}}}
	}
	role := func(name string) metav1.LabelSelector {
		return metav1.LabelSelector{MatchLabels: map[string]string{roleLabel: name}}
	}
	policies := []*networkingv1.NetworkPolicy{newPolicy("orka-deny", metav1.LabelSelector{})}
	apiIP, apiPort, _ := apiAddress(b.config.TargetAPIServer)
	apiRule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr(apiIP)}}},
		Ports: []networkingv1.NetworkPolicyPort{netPort(apiPort)}}
	for _, name := range []string{roleObserver, rolePositive, roleSubject} {
		policy := newPolicy("orka-"+name, role(name))
		// Subject API access is only to the independently pinned target API.
		// Even a plan with no permissions is probed with its unprivileged SA.
		policy.Spec.Egress = append(policy.Spec.Egress, apiRule)
		if name == rolePositive {
			canary, _ := netip.ParseAddr(b.config.CanaryHost)
			policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
				To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr(canary)}}},
				Ports: []networkingv1.NetworkPolicyPort{netPort(b.config.CanaryPort)}})
		}
		if application && name != rolePositive {
			plan := r.state.Request.Checks.Application
			for _, fixture := range plan.Fixtures {
				if fixture.Kind == fixtureKindReplica {
					continue
				}
				policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{pv.ApplicationTargetLabel: fixture.ID}}}},
					Ports: []networkingv1.NetworkPolicyPort{netPort(fixture.Port)}})
			}
			if name == roleObserver {
				for _, port := range plan.Runtime.Ports {
					policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
						To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{pv.ApplicationTargetLabel: subjectTarget}}}},
						Ports: []networkingv1.NetworkPolicyPort{netPort(port)}})
				}
			}
			if name == roleSubject {
				for _, port := range plan.Runtime.Ports {
					observer := role(roleObserver)
					policy.Spec.Ingress = append(policy.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{
						From: []networkingv1.NetworkPolicyPeer{{PodSelector: &observer}}, Ports: []networkingv1.NetworkPolicyPort{netPort(port)}})
				}
			}
		}
		policies = append(policies, policy)
	}
	prepare := newPolicy("orka-prepare", role(rolePrepare))
	for _, endpoint := range b.config.SourceEndpoints {
		address, _ := netip.ParseAddr(endpoint.Address)
		prepare.Spec.Egress = append(prepare.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr(address)}}},
			Ports: []networkingv1.NetworkPolicyPort{netPort(443)}})
	}
	policies = append(policies, prepare)
	if application {
		for _, fixture := range r.state.Request.Checks.Application.Fixtures {
			if fixture.Kind == fixtureKindReplica {
				continue
			}
			policy := newPolicy("orka-fixture-"+fixture.ID, metav1.LabelSelector{MatchLabels: map[string]string{pv.ApplicationTargetLabel: fixture.ID}})
			subject, observer := role(roleSubject), role(roleObserver)
			policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &subject}, {PodSelector: &observer}},
				Ports: []networkingv1.NetworkPolicyPort{netPort(fixture.Port)}}}
			policies = append(policies, policy)
		}
	}
	return policies
}

func (b *Backend) ensureFixtures(ctx context.Context, r *run, side, prefix string, resources []json.RawMessage) error {
	for i, raw := range resources {
		object, err := pv.ValidateApplicationResourceWithPolicy(raw, r.state.Request.ApplicationPolicy)
		if err != nil {
			return contract.ErrInvalid
		}
		object.SetNamespace(namespaceName(r, side))
		gvk := object.GroupVersionKind()
		var typed client.Object = object
		if instance, err := b.scheme.New(gvk); err == nil {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, instance); err != nil {
				return contract.ErrInvalid
			}
			var ok bool
			typed, ok = instance.(client.Object)
			if !ok {
				return contract.ErrInvalid
			}
		}
		if _, err := b.ensureObject(ctx, r, resourceKey(side, prefix+"-"+strconv.Itoa(i)), typed); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) serviceAliases(ctx context.Context, r *run, side string) ([]corev1.HostAlias, error) {
	var aliases []corev1.HostAlias
	for i, raw := range r.state.Request.Checks.Application.Resources {
		objectDefinition, err := pv.ValidateApplicationResourceWithPolicy(raw, r.state.Request.ApplicationPolicy)
		if err != nil {
			return nil, contract.ErrInvalid
		}
		if objectDefinition.GetKind() != "Service" {
			continue
		}
		object, err := b.boundObject(ctx, r, resourceKey(side, "resource-"+strconv.Itoa(i)))
		if err != nil {
			return nil, err
		}
		service := object.(*corev1.Service)
		if net.ParseIP(service.Spec.ClusterIP) == nil {
			return nil, contract.ErrConflict
		}
		aliases = append(aliases, corev1.HostAlias{IP: service.Spec.ClusterIP, Hostnames: []string{
			service.Name, service.Name + "." + service.Namespace, service.Name + "." + service.Namespace + ".svc",
			service.Name + "." + service.Namespace + ".svc.cluster.local",
		}})
	}
	slices.SortFunc(aliases, func(a, b corev1.HostAlias) int { return strings.Compare(a.Hostnames[0], b.Hostnames[0]) })
	return aliases, nil
}
