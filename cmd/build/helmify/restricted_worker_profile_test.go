package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestRestrictedAIWorkerProfile(t *testing.T) {
	for _, tc := range []struct{ release, namespace, fullname string }{
		{"test", "orka-test", "test-orka"},
		{"other", "other-namespace", "bounded"},
	} {
		t.Run(tc.fullname, func(t *testing.T) {
			rendered, err := helmTemplateStaticChartForRelease(t, tc.release, tc.namespace,
				"--set", "workers.ai.rbacProfile=restricted", "--set", "fullnameOverride="+tc.fullname)
			if err != nil {
				t.Fatal(err, rendered)
			}
			docs := decodeWorkerRBACDocuments(t, rendered)
			name := tc.fullname + "-ai-worker-role"
			account := tc.fullname + "-ai-worker"
			requireWorkerServiceAccount(t, docs, "ai", account, tc.namespace)
			var roles, bindings int
			for _, doc := range docs {
				if doc.Kind == "ClusterRole" && doc.Metadata.Name == name {
					roles++
					want := workerPermissions([]rbacv1.PolicyRule{
						{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch"}},
						{APIGroups: []string{"core.orka.ai"}, Resources: []string{"tools", "outboundaccesspolicies"},
							Verbs: []string{"get", "list"}},
						{APIGroups: []string{"core.orka.ai"}, Resources: []string{"agents", "agentruntimes"}, Verbs: []string{"get"}},
						{APIGroups: []string{"core.orka.ai"}, Resources: []string{"tasks"}, Verbs: []string{"get", "list", "watch"}},
					})
					if !workerRoleUsesExplicitRules(doc) || !reflect.DeepEqual(workerPermissions(doc.Rules), want) {
						t.Error("restricted worker must have exactly the read-only allowlist, without aggregation")
					}
				}
				isBinding := doc.Kind == "RoleBinding" || doc.Kind == "ClusterRoleBinding"
				if isBinding && workerBindingHasSubject(doc, account, tc.namespace) {
					bindings++
					if doc.Kind != "RoleBinding" || doc.Metadata.Namespace != tc.namespace ||
						doc.RoleRef != (rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name}) ||
						!workerBindingHasOnlySubject(doc, account, tc.namespace) {
						t.Error("restricted worker has an unexpected effective binding")
					}
				}
			}
			if roles != 1 || bindings != 1 {
				t.Fatalf("restricted role/bindings = %d/%d, want 1/1", roles, bindings)
			}
		})
	}
}

func TestRestrictedWorkerContextAuthorization(t *testing.T) {
	for _, mode := range []string{"", "enforce"} {
		for _, configured := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "no-issuer", true: "issuer"}[configured], func(t *testing.T) {
				args := []string{"--set", "workers.ai.rbacProfile=restricted", "--show-only", "templates/deployment.yaml"}
				if mode != "" {
					args = append(args, "--set", "controller.contextToken.authzMode="+mode)
				}
				if configured {
					args = append(args, "--set", "controller.contextToken.profile=transaction-token",
						"--set", "controller.contextToken.issuer=https://issuer.example.test",
						"--set", "controller.contextToken.audience=orka")
				}
				rendered, err := helmTemplateStaticChart(t, args...)
				if err != nil {
					t.Fatal(err, rendered)
				}
				if strings.Count(rendered, "--context-token-authz-mode=enforce") != 1 ||
					strings.Count(rendered, "--context-token-authz-mode=") != 1 {
					t.Error("restricted profile must explicitly enforce context-token grants exactly once, even without an issuer")
				}
			})
		}
	}
}

func TestRestrictedWorkerRejectsUnsafeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, setting, want string
	}{
		{"unknown", "workers.ai.rbacProfile=restriced", "workers.ai.rbacProfile"},
		{"boolean", "workers.ai.rbacProfile=true", "workers.ai.rbacProfile"},
		{"no chart RBAC", "rbac.create=false", "rbac.create"},
		{"context off", "controller.contextToken.authzMode=off", "controller-wide context-token"},
		{"context audit", "controller.contextToken.authzMode=audit", "controller-wide context-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, "--set", "workers.ai.rbacProfile=restricted", "--set", tc.setting)
			if err == nil || !strings.Contains(output, tc.want) {
				t.Fatalf("render error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDefaultWorkerProfileUnchanged(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", "worker_role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--set", "workers.ai.rbacProfile=default"}} {
		rendered, err := helmTemplateStaticChart(t, args...)
		if err != nil {
			t.Fatal(err, rendered)
		}
		docs := decodeWorkerRBACDocuments(t, rendered)
		for _, tier := range []string{"ai", "vendor", "container"} {
			checkWorkerRBACParity(t, decodeWorkerRBACDocuments(t, string(shared)), docs, tier, "orka-test")
		}
		if strings.Contains(rendered, "--context-token-authz-mode=") {
			t.Error("default profile must not change context-token policy")
		}
	}
	for _, mode := range []string{"off", "audit", "enforce"} {
		rendered, err := helmTemplateStaticChart(t, "--set", "controller.contextToken.authzMode="+mode)
		if err != nil {
			t.Fatal(err, rendered)
		}
		if !strings.Contains(rendered, "--context-token-authz-mode="+mode) {
			t.Errorf("default worker profile changed explicit context mode %q", mode)
		}
	}
	// --reuse-values can replace chart defaults with values from an older release.
	chart := t.TempDir()
	if err := os.CopyFS(chart, os.DirFS("static")); err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := yaml.Unmarshal(values, &old); err != nil {
		t.Fatal(err)
	}
	delete(old["workers"].(map[string]any)["ai"].(map[string]any), "rbacProfile")
	values, err = yaml.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "values.yaml"), values, 0o600); err != nil {
		t.Fatal(err)
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal(err) // earlier render helpers require Helm for this test
	}
	args := append([]string{"template", "test", chart, "--namespace", "orka-test"}, staticChartDefaultArgs()...)
	rendered, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatal(err, string(rendered))
	}
	checkWorkerRBACParity(t, decodeWorkerRBACDocuments(t, string(shared)),
		decodeWorkerRBACDocuments(t, string(rendered)), "ai", "orka-test")
}
