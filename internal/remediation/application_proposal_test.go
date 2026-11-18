package remediation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func applicationProposal(t *testing.T) CheckProposal {
	t.Helper()
	plan := &pv.ApplicationPlan{
		Version: 1, Build: pv.ApplicationBuild{Profile: pv.ApplicationBuildGoPackage, Package: "./cmd/example", GoVersion: "go1.27.2"},
		Runtime: pv.ApplicationRuntime{Image: "example.invalid/runtime@sha256:" + strings.Repeat("a", 64),
			ImageID: "sha256:" + strings.Repeat("b", 64)},
		Resources: []json.RawMessage{json.RawMessage(
			`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"synthetic"},` +
				`"spec":{"defaultBackend":{"service":{"name":"backend","port":{"number":8080}}}}}`,
		)},
		Permissions: []pv.ApplicationPermission{{
			APIGroup: "networking.k8s.io", Resources: []string{"ingresses/status"},
			Verbs: []string{"update", "patch"}, ResourceNames: []string{"synthetic"},
		}},
	}
	proposal := CheckProposal{Scope: []string{"full synthetic behavior"}, Application: plan}
	for _, kind := range []string{pv.Reproduction, pv.Normal} {
		proposal.Checks = append(proposal.Checks, pv.Check{
			ID: kind, Kind: kind, TimeoutSeconds: 30,
			Application: &pv.ApplicationCheck{Version: 1, Kubernetes: &pv.ApplicationKubernetesCheck{
				APIVersion: "networking.k8s.io/v1", Resource: "ingresses", Name: "synthetic", Pointer: "/status/loadBalancer/ingress",
			}},
			Healthy: pv.Expectation{ExitCode: 0, Stdout: `{"exists":true,"found":true,"value":[{"hostname":"ready.invalid"}]}`},
			Failure: pv.Expectation{ExitCode: 0, Stdout: `{"exists":false,"found":false}`},
		})
		if kind == pv.Normal {
			proposal.Checks[len(proposal.Checks)-1].Application.Kubernetes.NormalValue =
				json.RawMessage(`[{"hostname":"ready.invalid"}]`)
		}
	}
	return proposal
}

func decodeApplicationProposal(t *testing.T, proposal CheckProposal) (CheckProposal, error) {
	t.Helper()
	content, err := json.Marshal(proposal)
	require.NoError(t, err)
	return DecodeCheckProposal(string(content))
}

func TestApplicationProposalFreezesExactValuesWithoutModelHashes(t *testing.T) {
	proposed := applicationProposal(t)
	frozen, err := decodeApplicationProposal(t, proposed)
	require.NoError(t, err)
	require.Equal(t, proposed.Application, frozen.Application)
	expected, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{
		Exists: true, Found: true, Value: json.RawMessage(`[{"hostname":"ready.invalid"}]`),
	})
	require.NoError(t, err)
	require.Equal(t, expected, frozen.Checks[0].Healthy)
	require.Contains(t, frozen.Checks[0].Healthy.Stdout, "valueDigest")
	require.NotContains(t, frozen.Checks[0].Healthy.Stdout, "ready.invalid")
	files, err := FreezeProposalFiles(frozen)
	require.NoError(t, err)
	require.Empty(t, files)
	repeated, err := decodeApplicationProposal(t, frozen)
	require.NoError(t, err)
	require.Equal(t, frozen, repeated, "freezing or review decoding cannot change canonical digests")
}

func TestApplicationProposalRejectsExecutableOrMixedAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*CheckProposal){
		"files": func(proposal *CheckProposal) {
			proposal.Files = []ProposedFile{{Path: "check.sh", Content: "#!/bin/sh\nexit 0\n", Executable: true}}
		},
		"requirements": func(proposal *CheckProposal) {
			proposal.Requirements = []pv.EnvironmentRequirement{{Kind: "process", Name: "sh"}}
		},
		"command": func(proposal *CheckProposal) { proposal.Checks[0].Command = []string{"/bin/true"} },
		"http": func(proposal *CheckProposal) {
			proposal.Checks[0].HTTP = &pv.HTTPCheck{Version: 1, ServerCommand: []string{"/checks/server.py"}, Path: "/"}
		},
		"no application": func(proposal *CheckProposal) { proposal.Application = nil },
		"package escape": func(proposal *CheckProposal) { proposal.Application.Build.Package = "../other" },
		"image tag":      func(proposal *CheckProposal) { proposal.Application.Runtime.Image = "example.invalid/runtime:latest" },
		"duplicate expected value": func(proposal *CheckProposal) {
			proposal.Checks[0].Healthy.Stdout = `{"exists":true,"found":true,"value":"first","value":"second"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			proposal := applicationProposal(t)
			mutate(&proposal)
			_, err := decodeApplicationProposal(t, proposal)
			require.Error(t, err)
		})
	}
}

func TestApplicationProposalRequiresNormalControl(t *testing.T) {
	proposal, err := decodeApplicationProposal(t, applicationProposal(t))
	require.NoError(t, err)
	proposal.Checks = proposal.Checks[:1]
	_, err = FreezeProposalFiles(proposal)
	require.Error(t, err)
}

func TestApplicationResourcesFreezeCanonicalObjectOrder(t *testing.T) {
	first := applicationProposal(t)
	first.Application.Resources = append(first.Application.Resources, json.RawMessage(
		`{"kind":"ConfigMap","metadata":{"name":"input"},"apiVersion":"v1","data":{"b":"second","a":"first"}}`,
	))
	second := applicationProposal(t)
	second.Application.Resources = append(second.Application.Resources, json.RawMessage(
		`{"apiVersion":"v1","data":{"a":"first","b":"second"},"metadata":{"name":"input"},"kind":"ConfigMap"}`,
	))
	left, err := decodeApplicationProposal(t, first)
	require.NoError(t, err)
	right, err := decodeApplicationProposal(t, second)
	require.NoError(t, err)
	require.Equal(t, left, right, "a review cannot accidentally change plan identity by reordering JSON object keys")
}
