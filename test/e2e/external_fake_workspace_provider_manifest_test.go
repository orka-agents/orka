//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"crypto/sha256"
	"fmt"

	distributionref "github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	externalFakeWorkspaceProviderControllerName      = "fake.workspace.orka.ai"
	externalFakeWorkspaceProviderFixtureLabel        = "orka.ai/e2e-external-fake-workspace"
	externalFakeWorkspaceProviderLeaderElectionLease = "orka-workspace-fake.workspace.orka.ai"
)

// Role fields name both the role and its corresponding binding. ProviderRole
// is cluster-scoped; CoreRole names cluster config and namespaced profile
// reader grants. LeaderRole and its binding are namespaced.
// The pinned binary uses a fixed Lease name, independent of the fixture ID.
type externalFakeWorkspaceProviderFixtureNames struct {
	Provider            string
	Class               string
	Deployment          string
	ServiceAccount      string
	Config              string
	Profile             string
	ProviderRole        string
	LeaderRole          string
	CoreRole            string
	LeaderElectionLease string
}

// externalFakeWorkspaceProviderManifests bootstraps the separately built fake
// provider from orka-workspace v0.1.0-alpha.5 (c10e4ee49890). The caller
// installs the shared and fake CRDs/admission policies and supplies an existing
// namespace.
// Core's ServiceAccount is in that namespace and already has its normal RBAC.
//
// The fake config/profile are identity-only objects. Core constructs the real
// Codex PodTemplate; the provider materializes it without reading credentials.
// There is no fixture runtime, bootstrap Secret, pool, checkpoint, or suspension.
func externalFakeWorkspaceProviderManifests(
	namespace, imageRef, fixtureID, coreServiceAccount string,
) ([]map[string]any, externalFakeWorkspaceProviderFixtureNames, error) {
	var names externalFakeWorkspaceProviderFixtureNames
	if errs := validation.IsDNS1123Label(namespace); len(errs) != 0 {
		return nil, names, fmt.Errorf("invalid external fake provider namespace: %v", errs)
	}
	if errs := validation.IsDNS1123Label(fixtureID); len(errs) != 0 {
		return nil, names, fmt.Errorf("invalid external fake provider fixture ID: %v", errs)
	}
	name := "e2e-external-fake-" + fixtureID
	if errs := validation.IsDNS1123Label(name + "-core"); len(errs) != 0 {
		return nil, names, fmt.Errorf("external fake provider fixture ID makes resource names too long: %v", errs)
	}
	if errs := validation.IsDNS1123Subdomain(coreServiceAccount); len(errs) != 0 {
		return nil, names, fmt.Errorf("invalid external fake provider Core ServiceAccount: %v", errs)
	}
	named, err := distributionref.ParseNormalizedNamed(imageRef)
	if err != nil {
		return nil, names, fmt.Errorf("external fake provider image must be a SHA-256 digest-pinned image reference")
	}
	digested, pinned := named.(distributionref.Canonical)
	if !pinned || digested.Digest().Algorithm() != "sha256" || len(digested.Digest().Encoded()) != sha256.Size*2 {
		return nil, names, fmt.Errorf("external fake provider image must be a SHA-256 digest-pinned image reference")
	}
	// Match Core's tag-free image identity, including local registry references.
	canonicalImage, err := distributionref.WithDigest(distributionref.TrimNamed(named), digested.Digest())
	if err != nil {
		return nil, names, fmt.Errorf("canonicalize external fake provider image: %w", err)
	}

	names = externalFakeWorkspaceProviderFixtureNames{
		Provider: name, Class: name, Deployment: name, ServiceAccount: name,
		Config: name, Profile: name, ProviderRole: name, LeaderRole: name,
		CoreRole: name + "-core", LeaderElectionLease: externalFakeWorkspaceProviderLeaderElectionLease,
	}
	resource := func(apiVersion, kind, resourceName string, namespaced bool) map[string]any {
		metadata := map[string]any{
			"name": resourceName,
			"labels": map[string]any{
				externalFakeWorkspaceProviderFixtureLabel: name,
			},
		}
		if namespaced {
			metadata["namespace"] = namespace
		}
		return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}
	}
	binding := func(kind, roleKind, roleName, serviceAccount string, namespaced bool) map[string]any {
		object := resource("rbac.authorization.k8s.io/v1", kind, roleName, namespaced)
		object["roleRef"] = map[string]any{
			"apiGroup": "rbac.authorization.k8s.io", "kind": roleKind, "name": roleName,
		}
		object["subjects"] = []any{map[string]any{
			"kind": "ServiceAccount", "name": serviceAccount, "namespace": namespace,
		}}
		return object
	}

	account := resource("v1", "ServiceAccount", names.ServiceAccount, true)
	providerRole := resource("rbac.authorization.k8s.io/v1", "ClusterRole", names.ProviderRole, false)
	// These are the pinned provider bundle's controller permissions. Pod access
	// must include Core's runtime namespace, which need not be the Task namespace.
	// In particular, this identity never gets Secrets, core admit, or configure.
	providerRole["rules"] = []any{
		map[string]any{"apiGroups": []any{"workspace.orka.ai"},
			"resources": []any{
				"executionworkspaceproviders", "executionworkspaceclasses", "executionworkspacepools", "executionworkspaces",
			},
			"verbs": []any{"get", "list", "watch"}},
		map[string]any{"apiGroups": []any{"workspace.orka.ai"},
			"resources": []any{"executionworkspaces"}, "verbs": []any{"patch"}},
		map[string]any{"apiGroups": []any{"workspace.orka.ai"},
			"resources": []any{
				"executionworkspaceproviders/status", "executionworkspacepools/status", "executionworkspaces/status",
			},
			"verbs": []any{"get", "patch", "update"}},
		map[string]any{"apiGroups": []any{"workspace.orka.ai"},
			"resources":     []any{"executionworkspaceproviders"},
			"resourceNames": []any{names.Provider}, "verbs": []any{"provider-status"}},
		map[string]any{"apiGroups": []any{externalFakeWorkspaceProviderControllerName},
			"resources": []any{"fakeproviderconfigs", "fakepoolparameters"}, "verbs": []any{"get", "list", "watch"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"},
			"verbs": []any{"get", "list", "watch", "create", "update"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get", "create", "delete"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"serviceaccounts"}, "verbs": []any{"get"}},
		map[string]any{"apiGroups": []any{"networking.k8s.io"},
			"resources": []any{"networkpolicies"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"events"}, "verbs": []any{"create", "patch"}},
	}
	leaderRole := resource("rbac.authorization.k8s.io/v1", "Role", names.LeaderRole, true)
	leaderRole["rules"] = []any{map[string]any{
		"apiGroups": []any{"coordination.k8s.io"}, "resources": []any{"leases"},
		"verbs": []any{"get", "list", "watch", "create", "update", "patch"},
	}}
	coreRole := resource("rbac.authorization.k8s.io/v1", "ClusterRole", names.CoreRole, false)
	// Core resolves these two named parameter objects with uncached Get calls.
	coreRole["rules"] = []any{map[string]any{
		"apiGroups":     []any{externalFakeWorkspaceProviderControllerName},
		"resources":     []any{"fakeproviderconfigs"},
		"resourceNames": []any{name}, "verbs": []any{"get"},
	}}

	coreProfileRole := resource("rbac.authorization.k8s.io/v1", "Role", names.CoreRole, true)
	coreProfileRole["rules"] = []any{map[string]any{
		"apiGroups":     []any{externalFakeWorkspaceProviderControllerName},
		"resources":     []any{"fakepoolparameters"},
		"resourceNames": []any{names.Profile}, "verbs": []any{"get"},
	}}

	deployment := resource("apps/v1", "Deployment", names.Deployment, true)
	deployment["spec"] = map[string]any{
		"replicas": int64(1),
		"selector": map[string]any{"matchLabels": map[string]any{
			externalFakeWorkspaceProviderFixtureLabel: name,
		}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{
				externalFakeWorkspaceProviderFixtureLabel: name,
			}},
			"spec": map[string]any{
				"serviceAccountName": names.ServiceAccount,
				"securityContext": map[string]any{
					"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"},
				},
				"containers": []any{map[string]any{
					"name": "provider", "image": canonicalImage.String(), "imagePullPolicy": "IfNotPresent",
					"args":           []any{"--leader-elect=true", "--leader-election-namespace=" + namespace},
					"ports":          []any{map[string]any{"containerPort": int64(8081), "name": "health"}},
					"livenessProbe":  map[string]any{"httpGet": map[string]any{"path": "/healthz", "port": "health"}},
					"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": "health"}},
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "20m", "memory": "64Mi"},
						"limits":   map[string]any{"memory": "256Mi"},
					},
					"securityContext": map[string]any{
						"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
						"capabilities": map[string]any{"drop": []any{"ALL"}},
					},
				}},
			},
		},
	}
	config := resource(externalFakeWorkspaceProviderControllerName+"/v1alpha1", "FakeProviderConfig", names.Config, false)
	config["spec"] = map[string]any{}
	provider := resource("workspace.orka.ai/v1alpha1", "ExecutionWorkspaceProvider", names.Provider, false)
	provider["spec"] = map[string]any{
		"controllerName": externalFakeWorkspaceProviderControllerName,
		"serviceAccountRef": map[string]any{
			"namespace": namespace, "name": names.ServiceAccount,
		},
		"lifecycleState":    "Active",
		"requiredContracts": []any{"workspace.orka.ai/v1", "orka.workspace.lifecycle.v1"},
		"parametersRef": map[string]any{
			"group": externalFakeWorkspaceProviderControllerName, "kind": "FakeProviderConfig", "name": names.Config,
		},
	}
	profile := resource(externalFakeWorkspaceProviderControllerName+"/v1alpha1", "FakePoolParameters", names.Profile, true)
	profile["spec"] = map[string]any{}
	class := resource("workspace.orka.ai/v1alpha1", "ExecutionWorkspaceClass", names.Class, true)
	class["spec"] = map[string]any{
		"providerRef": map[string]any{"name": names.Provider},
		"parametersRef": map[string]any{
			"group": externalFakeWorkspaceProviderControllerName, "kind": "FakePoolParameters", "name": names.Profile,
		},
		"mode": "Interactive", "requiredFeatures": []any{"acp.runtime.v2"}, "allowedReuseScopes": []any{"None"},
		"lifecycle": map[string]any{
			"defaultOnDetach": "Delete", "allowedOnDetach": []any{"Delete"}, "detachTimeout": "1m",
			"deletionPolicy": map[string]any{
				"providerResources": "Delete", "persistentVolumes": "Delete", "checkpoints": "Delete",
			},
		},
	}

	return []map[string]any{
		account, providerRole,
		binding("ClusterRoleBinding", "ClusterRole", names.ProviderRole, names.ServiceAccount, false),
		leaderRole, binding("RoleBinding", "Role", names.LeaderRole, names.ServiceAccount, true),
		coreRole, binding("ClusterRoleBinding", "ClusterRole", names.CoreRole, coreServiceAccount, false),
		coreProfileRole, binding("RoleBinding", "Role", names.CoreRole, coreServiceAccount, true),
		deployment, config, provider, profile, class,
	}, names, nil
}
