//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	workspaceapi "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

const externalFakeProviderUnitImage = "registry.test:5000/orka-workspace-fake@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestExternalFakeWorkspaceProviderManifestsValidateInputs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name, field, value string
	}{
		{name: "empty namespace", field: "namespace"},
		{name: "invalid namespace", field: "namespace", value: "Not-A-Namespace"},
		{name: "empty fixture", field: "fixture"},
		{name: "invalid fixture", field: "fixture", value: "smoke/test"},
		{name: "long names", field: "fixture", value: strings.Repeat("x", 42)},
		{name: "empty Core account", field: "account"},
		{name: "invalid Core account", field: "account", value: "../core"},
		{name: "empty image", field: "image"},
		{name: "mutable image", field: "image", value: "registry.test/fake:latest"},
		{name: "short digest", field: "image", value: "registry.test/fake@sha256:abc"},
		{name: "invalid repository", field: "image", value: "Invalid/REPO@sha256:" + strings.Repeat("a", 64)},
		{name: "whitespace", field: "image", value: " " + externalFakeProviderUnitImage},
		{name: "uppercase digest", field: "image", value: "registry.test/fake@sha256:" + strings.Repeat("A", 64)},
		{name: "other algorithm", field: "image", value: "registry.test/fake@sha512:" + strings.Repeat("a", 128)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			namespace, image, fixtureID, coreAccount := "smoke", externalFakeProviderUnitImage, "smoke", "core"
			switch test.field {
			case "namespace":
				namespace = test.value
			case "fixture":
				fixtureID = test.value
			case "account":
				coreAccount = test.value
			case "image":
				image = test.value
			}
			manifests, names, err := externalFakeWorkspaceProviderManifests(namespace, image, fixtureID, coreAccount)
			if err == nil || manifests != nil || names != (externalFakeWorkspaceProviderFixtureNames{}) {
				t.Fatalf("invalid inputs returned manifests=%v, names=%+v, err=%v", manifests, names, err)
			}
		})
	}
}

func TestExternalFakeWorkspaceProviderManifestsPinDeploymentImage(t *testing.T) {
	t.Parallel()

	for _, image := range []string{
		externalFakeProviderUnitImage,
		strings.Replace(externalFakeProviderUnitImage, "@", ":build@", 1),
	} {
		manifests, names := externalFakeProviderUnitFixture(t, image, "controller-from-deployment")
		deployment := externalFakeProviderUnitObject[appsv1.Deployment](t, manifests, "Deployment", names.Deployment)
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 ||
			len(deployment.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("expected one standalone provider replica: %+v", deployment.Spec)
		}
		pod := deployment.Spec.Template.Spec
		container := pod.Containers[0]
		if container.Image != externalFakeProviderUnitImage || container.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Fatalf("image=%q, pullPolicy=%q; want tag-free digest pin with IfNotPresent",
				container.Image, container.ImagePullPolicy)
		}
		if container.Name != "provider" || pod.ServiceAccountName != names.ServiceAccount ||
			!reflect.DeepEqual(container.Args, []string{"--leader-elect=true", "--leader-election-namespace=smoke"}) {
			t.Fatalf("provider command/identity differs from pinned binary: %+v", pod)
		}
		if len(pod.Volumes) != 0 || len(container.Env) != 0 || len(container.Command) != 0 {
			t.Fatal("provider fixture must not override the binary or inject credentials/runtime configuration")
		}
		if !reflect.DeepEqual(deployment.Spec.Selector.MatchLabels, deployment.Spec.Template.Labels) ||
			deployment.Spec.Template.Labels[externalFakeWorkspaceProviderFixtureLabel] != names.Provider {
			t.Fatal("Deployment must select only its fixture-labelled provider Pod")
		}
		if container.ReadinessProbe.HTTPGet.Path != "/readyz" || container.LivenessProbe.HTTPGet.Path != "/healthz" ||
			container.ReadinessProbe.HTTPGet.Port.StrVal != "health" || container.Ports[0].ContainerPort != 8081 {
			t.Fatal("provider probes must use the pinned manager's health listener")
		}
		if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
			pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
			container.SecurityContext == nil || *container.SecurityContext.AllowPrivilegeEscalation ||
			!*container.SecurityContext.ReadOnlyRootFilesystem ||
			!reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Fatal("provider must retain the standalone bundle's unprivileged security settings")
		}
	}
}

func TestExternalFakeWorkspaceProviderManifestsScopeRBACOwnership(t *testing.T) {
	t.Parallel()

	manifests, names := externalFakeProviderUnitFixture(t, externalFakeProviderUnitImage, "controller-from-deployment")
	role := externalFakeProviderUnitObject[rbacv1.ClusterRole](t, manifests, "ClusterRole", names.ProviderRole)
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{"workspace.orka.ai"}, Resources: []string{
			"executionworkspaceproviders", "executionworkspaceclasses", "executionworkspacepools", "executionworkspaces",
		}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"workspace.orka.ai"}, Resources: []string{"executionworkspaces"}, Verbs: []string{"patch"}},
		{APIGroups: []string{"workspace.orka.ai"}, Resources: []string{
			"executionworkspaceproviders/status", "executionworkspacepools/status", "executionworkspaces/status",
		}, Verbs: []string{"get", "patch", "update"}},
		{APIGroups: []string{"workspace.orka.ai"}, Resources: []string{"executionworkspaceproviders"},
			ResourceNames: []string{names.Provider}, Verbs: []string{"provider-status"}},
		{APIGroups: []string{"fake.workspace.orka.ai"}, Resources: []string{"fakeproviderconfigs", "fakepoolparameters"},
			Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"},
			Verbs: []string{"get", "list", "watch", "create", "update"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "create", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"list"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
	}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("provider RBAC differs from pinned standalone bundle: %+v", role.Rules)
	}
	binding := externalFakeProviderUnitObject[rbacv1.ClusterRoleBinding](
		t, manifests, "ClusterRoleBinding", names.ProviderRole)
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: names.ProviderRole}) ||
		!reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: names.ServiceAccount, Namespace: "smoke"},
		}) {
		t.Fatalf("provider role bound to wrong identity: %+v", binding)
	}
	leader := externalFakeProviderUnitObject[rbacv1.Role](t, manifests, "Role", names.LeaderRole)
	if leader.Namespace != "smoke" || !reflect.DeepEqual(leader.Rules, []rbacv1.PolicyRule{{
		APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch"},
	}}) {
		t.Fatalf("leader must get only namespaced Lease access: %+v", leader)
	}
	leaderBinding := externalFakeProviderUnitObject[rbacv1.RoleBinding](t, manifests, "RoleBinding", names.LeaderRole)
	if leaderBinding.Namespace != "smoke" ||
		leaderBinding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: names.LeaderRole}) ||
		!reflect.DeepEqual(leaderBinding.Subjects, binding.Subjects) {
		t.Fatalf("leader RoleBinding differs from provider identity: %+v", leaderBinding)
	}
}

func TestExternalFakeWorkspaceProviderManifestsBindInputCoreServiceAccount(t *testing.T) {
	t.Parallel()

	for _, coreAccount := range []string{"custom-controller", "another.controller"} {
		manifests, names := externalFakeProviderUnitFixture(t, externalFakeProviderUnitImage, coreAccount)
		role := externalFakeProviderUnitObject[rbacv1.ClusterRole](t, manifests, "ClusterRole", names.CoreRole)
		if !reflect.DeepEqual(role.Rules, []rbacv1.PolicyRule{{
			APIGroups: []string{"fake.workspace.orka.ai"}, Resources: []string{"fakeproviderconfigs"},
			ResourceNames: []string{names.Config}, Verbs: []string{"get"},
		}}) {
			t.Fatalf("Core's additional grant must read only fixture parameters: %+v", role)
		}
		binding := externalFakeProviderUnitObject[rbacv1.ClusterRoleBinding](
			t, manifests, "ClusterRoleBinding", names.CoreRole)
		if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: names.CoreRole}) ||
			!reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{
				{Kind: "ServiceAccount", Name: coreAccount, Namespace: "smoke"},
			}) {
			t.Fatalf("Core role must target caller-supplied ServiceAccount: %+v", binding)
		}
		profileRole := externalFakeProviderUnitObject[rbacv1.Role](t, manifests, "Role", names.CoreRole)
		if profileRole.Namespace != "smoke" || !reflect.DeepEqual(profileRole.Rules, []rbacv1.PolicyRule{{
			APIGroups: []string{"fake.workspace.orka.ai"}, Resources: []string{"fakepoolparameters"},
			ResourceNames: []string{names.Profile}, Verbs: []string{"get"},
		}}) {
			t.Fatalf("Core profile reads must stay in the fixture namespace: %+v", profileRole)
		}
		profileBinding := externalFakeProviderUnitObject[rbacv1.RoleBinding](t, manifests, "RoleBinding", names.CoreRole)
		if profileBinding.Namespace != "smoke" || profileBinding.RoleRef.Kind != "Role" ||
			profileBinding.RoleRef.Name != names.CoreRole || !reflect.DeepEqual(profileBinding.Subjects, binding.Subjects) {
			t.Fatalf("Core profile reader must bind the exact namespaced grant: %+v", profileBinding)
		}
	}
}

func TestExternalFakeWorkspaceProviderManifestsClassUsesProductionPodTemplate(t *testing.T) {
	t.Parallel()

	manifests, names := externalFakeProviderUnitFixture(t, externalFakeProviderUnitImage, "core")
	provider := externalFakeProviderUnitObject[workspaceapi.ExecutionWorkspaceProvider](
		t, manifests, "ExecutionWorkspaceProvider", names.Provider)
	if provider.Spec.ControllerName != externalFakeWorkspaceProviderControllerName ||
		provider.Spec.ServiceAccountRef == nil ||
		*provider.Spec.ServiceAccountRef != (workspaceapi.ProviderServiceAccountReference{
			Namespace: "smoke", Name: names.ServiceAccount,
		}) ||
		provider.Spec.LifecycleState != workspaceapi.ExecutionWorkspaceProviderActive ||
		!reflect.DeepEqual(provider.Spec.RequiredContracts, []string{
			workspaceapi.ContractVersionV1, workspaceprovider.LifecycleContractV1,
		}) ||
		provider.Spec.ParametersRef != (workspaceapi.TypedObjectReference{
			Group: "fake.workspace.orka.ai", Kind: "FakeProviderConfig", Name: names.Config,
		}) {
		t.Fatalf("provider contract does not select the standalone fake identity: %+v", provider.Spec)
	}
	for kind, name := range map[string]string{"FakeProviderConfig": names.Config, "FakePoolParameters": names.Profile} {
		object := externalFakeProviderUnitManifest(t, manifests, kind, name)
		if object["apiVersion"] != "fake.workspace.orka.ai/v1alpha1" || !reflect.DeepEqual(object["spec"], map[string]any{}) {
			t.Fatalf("%s must be an empty identity, not a fixture PodTemplate: %+v", kind, object)
		}
	}
	class := externalFakeProviderUnitObject[workspaceapi.ExecutionWorkspaceClass](
		t, manifests, "ExecutionWorkspaceClass", names.Class)
	if class.Namespace != "smoke" || class.Spec.ProviderRef == nil || class.Spec.ProviderRef.Name != names.Provider ||
		class.Spec.ParametersRef == nil || *class.Spec.ParametersRef != (workspaceapi.TypedObjectReference{
		Group: "fake.workspace.orka.ai", Kind: "FakePoolParameters", Name: names.Profile,
	}) || class.Spec.PoolRef != nil || class.Spec.Mode != workspaceapi.ExecutionWorkspaceModeInteractive ||
		!reflect.DeepEqual(class.Spec.RequiredFeatures, []workspaceapi.ExecutionWorkspaceFeature{
			workspaceapi.WorkspaceFeatureACPRuntime,
		}) ||
		!reflect.DeepEqual(class.Spec.AllowedReuseScopes, []workspaceapi.WorkspaceReuseScope{
			workspaceapi.WorkspaceReuseScopeNone,
		}) {
		t.Fatalf("class must require only direct acp.runtime.v2 materialization without reuse: %+v", class.Spec)
	}
	lifecycle := class.Spec.Lifecycle
	if lifecycle.DefaultOnDetach != workspaceapi.WorkspaceOnDetachDelete ||
		!reflect.DeepEqual(lifecycle.AllowedOnDetach, []workspaceapi.WorkspaceOnDetach{
			workspaceapi.WorkspaceOnDetachDelete,
		}) ||
		lifecycle.DetachTimeout.Duration != time.Minute || lifecycle.IdleTimeout != nil || lifecycle.MaxLifetime != nil ||
		lifecycle.DeletionPolicy != (workspaceapi.ExecutionWorkspaceDeletionPolicy{
			ProviderResources: workspaceapi.WorkspaceDeletionActionDelete,
			PersistentVolumes: workspaceapi.WorkspaceDeletionActionDelete,
			Checkpoints:       workspaceapi.WorkspaceDeletionActionDelete,
		}) {
		t.Fatalf("class must delete ephemeral compute without suspension/retention: %+v", lifecycle)
	}
}

func TestExternalFakeWorkspaceProviderManifestsAreIsolatedAndPredictable(t *testing.T) {
	t.Parallel()

	manifests, names := externalFakeProviderUnitFixture(t, externalFakeProviderUnitImage, "core")
	if len(manifests) != 14 || names.LeaderElectionLease != "orka-workspace-fake.workspace.orka.ai" {
		t.Fatalf("wrong resource inventory/Lease identity: count=%d, names=%+v", len(manifests), names)
	}
	seen := map[string]bool{}
	for _, manifest := range manifests {
		metadata := manifest["metadata"].(map[string]any)
		kind := manifest["kind"].(string)
		identity := kind + "/" + metadata["name"].(string)
		if seen[identity] {
			t.Fatalf("duplicate manifest %s", identity)
		}
		seen[identity] = true
		clusterScoped := kind == "ClusterRole" || kind == "ClusterRoleBinding" ||
			kind == "FakeProviderConfig" || kind == "ExecutionWorkspaceProvider"
		if ns, present := metadata["namespace"]; (clusterScoped && present) || (!clusterScoped && ns != "smoke") {
			t.Fatalf("%s has incorrect scope: %+v", identity, metadata)
		}
		if metadata["labels"].(map[string]any)[externalFakeWorkspaceProviderFixtureLabel] != names.Provider {
			t.Fatalf("%s lacks the precise fixture cleanup label", identity)
		}
		if _, present := manifest["status"]; present {
			t.Fatalf("%s must not forge Core/provider status evidence", identity)
		}
		if _, present := metadata["ownerReferences"]; present {
			t.Fatalf("%s must not forge an allocation owner reference", identity)
		}
	}
	if !seen["ExecutionWorkspaceClass/"+names.Class] || !seen["FakePoolParameters/"+names.Profile] ||
		!seen["Deployment/"+names.Deployment] {
		t.Fatal("predictable cleanup names do not identify returned resources")
	}
	firstJSON, err := json.Marshal(manifests)
	if err != nil {
		t.Fatal(err)
	}
	repeated, repeatedNames := externalFakeProviderUnitFixture(t, externalFakeProviderUnitImage, "core")
	repeatedJSON, err := json.Marshal(repeated)
	if err != nil || string(firstJSON) != string(repeatedJSON) || repeatedNames != names {
		t.Fatal("fixture manifests/names must be deterministic")
	}
	firstLabels := manifests[0]["metadata"].(map[string]any)["labels"].(map[string]any)
	firstLabels[externalFakeWorkspaceProviderFixtureLabel] = "mutated"
	repeatedLabel := repeated[0]["metadata"].(map[string]any)["labels"].(map[string]any)
	otherLabel := manifests[1]["metadata"].(map[string]any)["labels"].(map[string]any)
	if repeatedLabel[externalFakeWorkspaceProviderFixtureLabel] != names.Provider ||
		otherLabel[externalFakeWorkspaceProviderFixtureLabel] != names.Provider {
		t.Fatal("fixture metadata must not alias other resources or future calls")
	}
	_, otherNames, err := externalFakeWorkspaceProviderManifests("smoke", externalFakeProviderUnitImage, "other", "core")
	if err != nil || otherNames.Provider == names.Provider || otherNames.CoreRole == names.CoreRole {
		t.Fatalf("distinct fixture IDs must isolate registration/RBAC names: %+v, %v", otherNames, err)
	}
}

func externalFakeProviderUnitFixture(
	t *testing.T, image, coreAccount string,
) ([]map[string]any, externalFakeWorkspaceProviderFixtureNames) {
	t.Helper()
	manifests, names, err := externalFakeWorkspaceProviderManifests("smoke", image, "codex", coreAccount)
	if err != nil {
		t.Fatal(err)
	}
	return manifests, names
}

func externalFakeProviderUnitManifest(t *testing.T, manifests []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, manifest := range manifests {
		if manifest["kind"] == kind && manifest["metadata"].(map[string]any)["name"] == name {
			return manifest
		}
	}
	t.Fatalf("manifest %s/%s not found", kind, name)
	return nil
}

func externalFakeProviderUnitObject[T any](t *testing.T, manifests []map[string]any, kind, name string) T {
	t.Helper()
	data, err := json.Marshal(externalFakeProviderUnitManifest(t, manifests, kind, name))
	if err != nil {
		t.Fatal(err)
	}
	var object T
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}
