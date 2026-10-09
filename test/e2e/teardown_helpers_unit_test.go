//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const teardownWorkloadFixture = `
# Empty YAML documents are valid.
---
apiVersion: v1
kind: Namespace
metadata:
  name: orka-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller
  namespace: orka-system
spec:
  secret: fixture-private-value
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Namespace
  metadata:
    name: orka-runtimes
- apiVersion: v1
  kind: Secret
  metadata:
    name: auth
    namespace: orka-system
    annotations:
      private: fixture-private-value
  stringData:
    token: fixture-private-value
- apiVersion: v1
  kind: Namespace
  metadata:
    name: orka-system
---
null
`

const teardownCRDFixture = `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"tasks.core.orka.ai"}}`

func TestE2ETeardownManifestFiltering(t *testing.T) {
	t.Parallel()
	manifest, err := filterE2ETeardownManifest([]byte(teardownWorkloadFixture))
	require.NoError(t, err)
	require.Equal(t, []string{"orka-system", "orka-runtimes"}, manifest.namespaces)
	require.Len(t, manifest.resources, 2)
	require.Equal(t, "Deployment", manifest.resources[0].Kind)
	require.Equal(t, "controller", manifest.resources[0].Metadata.Name)
	require.Equal(t, "Secret", manifest.resources[1].Kind)
	require.Equal(t, "orka-system", manifest.resources[1].Metadata.Namespace)
	var input struct {
		Kind  string                `json:"kind"`
		Items []e2eTeardownResource `json:"items"`
	}
	require.NoError(t, json.Unmarshal(manifest.input(), &input))
	require.Equal(t, "List", input.Kind)
	require.Equal(t, manifest.resources, input.Items)
	require.NotContains(t, string(manifest.input()), "fixture-private-value")
	require.NotContains(t, string(manifest.input()), "Namespace")
}

func TestE2ETeardownManifestRejectsInvalidInputWithoutContent(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"", "---\n# empty\n", "stringData: [fixture-private-value", `"fixture-private-value"`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{},"data":{"key":"fixture-private-value"}}`,
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"--fixture-private-value"}}`,
		`{"apiVersion":"v1","kind":"List","items":[{"kind":"Secret","metadata":{"name":"fixture-private-value"}}]}`,
		// Typed lists must not sneak Namespace deletion into workload input.
		`{"apiVersion":"v1","kind":"NamespaceList","items":[{"metadata":{"name":"fixture-private-value"}}]}`,
	} {
		_, err := filterE2ETeardownManifest([]byte(input))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "fixture-private-value")
	}
}

func TestE2ETeardownNamespaceOnlyManifest(t *testing.T) {
	t.Parallel()
	manifest, err := filterE2ETeardownManifest([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"orka-runtimes"}}`))
	require.NoError(t, err)
	require.Empty(t, manifest.resources)
	require.Equal(t, []string{"orka-runtimes"}, manifest.namespaces)
}

func TestE2ETeardownKustomizeResolution(t *testing.T) {
	projectDir := t.TempDir()
	for _, tc := range []struct {
		name, localbin, override, want string
	}{
		{"default", "", "", filepath.Join(projectDir, "bin", "kustomize")},
		{"absolute LOCALBIN", "/custom/tools", "", "/custom/tools/kustomize"},
		{"relative LOCALBIN", "tools", "", filepath.Join(projectDir, "tools", "kustomize")},
		{"explicit KUSTOMIZE", "/custom/tools", "fixture-kustomize", "fixture-kustomize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOCALBIN", tc.localbin)
			t.Setenv("KUSTOMIZE", tc.override)
			require.Equal(t, tc.want, e2eTeardownKustomize(projectDir))
		})
	}
}

func TestE2ETeardownDeploymentOrderAndBudgets(t *testing.T) {
	// Binary overrides are process-wide, so these orchestration tests are serial.
	t.Setenv("KUBECTL", "fixture-kubectl")
	t.Setenv("KUSTOMIZE", "fixture-kustomize")
	var calls []string
	var stages []string
	var deadlines []time.Time
	var deleteInputs []e2eTeardownManifest
	run := func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		require.NoError(t, ctx.Err())
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		deadlines = append(deadlines, deadline)
		switch {
		case name == "fixture-kustomize":
			require.Equal(t, "build", args[0])
			calls = append(calls, "render "+filepath.Base(args[1]))
			if filepath.Base(args[1]) == "acp-production" {
				return []byte(teardownWorkloadFixture), nil
			}
			require.Equal(t, filepath.Join("project", "config", "crd"), args[1])
			return []byte(teardownCRDFixture), nil
		case args[0] == "delete" && args[1] == "validatingwebhookconfiguration":
			require.Equal(t, "fixture-kubectl", name)
			calls = append(calls, "delete webhook")
		case args[0] == "delete" && args[1] == "-f":
			manifest, err := filterE2ETeardownManifest(input)
			require.NoError(t, err)
			require.Empty(t, manifest.namespaces)
			deleteInputs = append(deleteInputs, manifest)
			calls = append(calls, "delete "+manifest.resources[0].Kind)
			require.Contains(t, args, "--wait=true")
			require.Contains(t, args, "--ignore-not-found=true")
			require.NotContains(t, string(input), "fixture-private-value")
		case args[0] == "delete" && args[1] == "namespaces":
			require.Equal(t, []string{"orka-system", "orka-runtimes"}, args[2:4])
			require.Contains(t, args, "--timeout=45s")
			require.Contains(t, args, "--wait=true")
			calls = append(calls, "delete namespaces")
		case args[0] == "get" && args[1] == "namespaces":
			require.Equal(t, []string{"orka-system", "orka-runtimes"}, args[2:4])
			calls = append(calls, "verify namespaces")
			return []byte(`{"kind":"List","items":[]}`), nil
		default:
			t.Fatalf("unexpected teardown command %s", name)
		}
		return nil, nil
	}
	start := time.Now()
	err := teardownE2EDeployment("project", "orka-system", func(name string, action func() error) error {
		stages = append(stages, name)
		return action()
	}, run, io.Discard)
	require.NoError(t, err)
	require.Len(t, stages, 7)
	require.Equal(t, []string{
		"render acp-production", "delete webhook", "delete Deployment",
		"render crd", "delete CustomResourceDefinition", "delete namespaces", "verify namespaces",
	}, calls)
	require.Equal(t, "Secret", deleteInputs[0].resources[1].Kind)
	// Subcommands in a phase share a deadline, not a fresh budget each.
	require.Equal(t, deadlines[0], deadlines[1])
	require.Equal(t, deadlines[0], deadlines[2])
	require.Equal(t, deadlines[3], deadlines[4])
	require.Equal(t, deadlines[5], deadlines[6])
	for i, budget := range []time.Duration{110 * time.Second, 110 * time.Second, 50 * time.Second} {
		index := []int{0, 3, 5}[i]
		require.InDelta(t, budget.Seconds(), deadlines[index].Sub(start).Seconds(), 1)
	}
}

func TestE2ETeardownDeletesEveryDeferredNamespaceIncludingFallback(t *testing.T) {
	for _, response := range []string{"", "  \n", `{"kind":"List","items":[]}`} {
		var namespaceCommands int
		run := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
			if args[0] == "build" {
				if filepath.Base(args[1]) == "acp-production" {
					return []byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"orka-runtimes"}}`), nil
				}
				// Future bundles may add another Namespace. Defer it too.
				return []byte(teardownCRDFixture + `
---
apiVersion: v1
kind: Namespace
metadata:
  name: additional-e2e
`), nil
			}
			if args[1] == "namespaces" {
				namespaceCommands++
				require.Equal(t, []string{"orka-system", "orka-runtimes", "additional-e2e"}, args[2:5])
				if args[0] == "get" {
					return []byte(response), nil
				}
			}
			return nil, nil
		}
		err := teardownE2EDeployment("project", "orka-system", func(_ string, action func() error) error { return action() }, run, io.Discard)
		require.NoError(t, err)
		require.Equal(t, 2, namespaceCommands)
	}
}

func TestE2ETeardownFailureStopsAndCapturesSafeDiagnostics(t *testing.T) {
	for _, failure := range []string{"render workload", "webhook", "workload", "render CRD", "CRD", "namespace", "verification"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("KUBECTL", "fixture-kubectl")
			t.Setenv("KUSTOMIZE", "fixture-kustomize")
			var log bytes.Buffer
			failed := false
			var attempted []string
			var completed []string
			var failureDeadline time.Time
			run := func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
				require.NoError(t, ctx.Err())
				if failed {
					require.Equal(t, "get", args[0], "only diagnostic reads may follow failure")
					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					require.LessOrEqual(t, time.Until(deadline), 3*time.Second)
					require.LessOrEqual(t, deadline.Sub(failureDeadline), e2eTeardownDiagnosticBudget)
					return []byte(`{"kind":"Namespace","metadata":{"name":"orka-runtimes","deletionTimestamp":"now","finalizers":["fixture-private-value"],"annotations":{"token":"fixture-private-value"}},"spec":{"finalizers":["kubernetes"]},"status":{"phase":"Terminating","message":"fixture-private-value"}}`), nil
				}
				var current string
				switch {
				case name == "fixture-kustomize" && filepath.Base(args[1]) == "acp-production":
					current = "render workload"
				case name == "fixture-kustomize":
					current = "render CRD"
				case args[1] == "validatingwebhookconfiguration":
					current = "webhook"
				case args[1] == "-f":
					current = "workload"
					if bytes.Contains(input, []byte("CustomResourceDefinition")) {
						current = "CRD"
					}
				case args[0] == "delete":
					current = "namespace"
				default:
					current = "verification"
				}
				attempted = append(attempted, current)
				if current == failure {
					failed = true
					failureDeadline, _ = ctx.Deadline()
					return nil, errors.New("fixture command failure; output omitted")
				}
				if current == "render workload" {
					return []byte(teardownWorkloadFixture), nil
				}
				if current == "render CRD" {
					return []byte(teardownCRDFixture), nil
				}
				return nil, nil
			}
			err := teardownE2EDeployment("project", "orka-system", func(name string, action func() error) error {
				err := action()
				if err == nil {
					completed = append(completed, name)
				}
				return err
			}, run, &log)
			require.Error(t, err)
			require.Equal(t, failure, attempted[len(attempted)-1])
			require.Len(t, completed, len(attempted)-1)
			require.Contains(t, log.String(), `"name":"orka-runtimes"`)
			require.Contains(t, log.String(), `"phase":"Terminating"`)
			require.Contains(t, log.String(), `"namespaceFinalizerCount":1`)
			require.NotContains(t, log.String(), "fixture-private-value")
		})
	}
}

func TestE2ETeardownVerificationRejectsRemainingNamespaces(t *testing.T) {
	for _, response := range []string{
		`{"kind":"List","items":[{"kind":"Namespace","metadata":{"name":"orka-runtimes"}}]}`,
		`{"kind":"Secret","data":{"key":"fixture-private-value"}}`,
		"fixture-private-value invalid JSON",
	} {
		var log bytes.Buffer
		run := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
			if args[0] == "build" {
				if filepath.Base(args[1]) == "acp-production" {
					return []byte(teardownWorkloadFixture), nil
				}
				return []byte(teardownCRDFixture), nil
			}
			if args[0] == "get" {
				return []byte(response), nil
			}
			return nil, nil
		}
		err := teardownE2EDeployment("project", "orka-system", func(_ string, action func() error) error { return action() }, run, &log)
		require.Error(t, err)
		require.NotContains(t, err.Error()+log.String(), "fixture-private-value")
	}
}

func TestE2ETeardownDiagnosticsOmitUntrustedOutput(t *testing.T) {
	t.Parallel()
	var log bytes.Buffer
	queries := 0
	collectE2ETeardownDiagnostics(context.Background(), "kubectl", []string{"orka-system"}, nil,
		func(ctx context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
			require.NoError(t, ctx.Err())
			queries++
			if queries == 1 {
				return []byte("fixture-private-value"), errors.New("fixture-private-value")
			}
			return []byte("fixture-private-value invalid JSON"), nil
		}, &log)
	require.Equal(t, 2, queries)
	require.NotContains(t, log.String(), "fixture-private-value")
	require.Contains(t, log.String(), "unavailable")
	require.Contains(t, log.String(), "invalid")
}

func TestE2ETeardownCommandBoundedAndSafe(t *testing.T) {
	t.Parallel()
	processArgs := func(mode string) []string {
		return []string{"-test.run=^TestE2ETeardownCommandProcess$", "--", mode}
	}
	t.Run("success", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, err := runE2ETeardownCommand(ctx, []byte("fixture input"), os.Args[0], processArgs("echo")...)
		require.NoError(t, err)
		require.Equal(t, "fixture input", string(data))
	})
	t.Run("exit status without stdout or stderr", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, err := runE2ETeardownCommand(ctx, nil, os.Args[0], processArgs("fail")...)
		require.ErrorContains(t, err, "exit code 7")
		require.Nil(t, data)
		require.NotContains(t, err.Error(), "fixture-private-value")
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		data, err := runE2ETeardownCommand(ctx, nil, os.Args[0], processArgs("sleep")...)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Nil(t, data)
		require.Less(t, time.Since(start), 2*time.Second)
		require.NotContains(t, err.Error(), "fixture-private-value")
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := runE2ETeardownCommand(ctx, nil, os.Args[0], processArgs("sleep")...)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("start failure", func(t *testing.T) {
		_, err := runE2ETeardownCommand(context.Background(), nil, filepath.Join(t.TempDir(), "missing"), "fixture-private-value")
		require.ErrorContains(t, err, "failed; output omitted")
		require.NotContains(t, err.Error(), "fixture-private-value")
	})
}

// A real direct subprocess exercises CommandContext without kubectl, make,
// shell descendants, Docker or a cluster. This test is inert in the parent.
func TestE2ETeardownCommandProcess(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 || !strings.HasPrefix(os.Args[index-1], "-test.run=^TestE2ETeardownCommandProcess") {
		return
	}
	switch os.Args[index+1] {
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	case "fail":
		_, _ = fmt.Fprint(os.Stdout, "fixture-private-value stdout")
		_, _ = fmt.Fprint(os.Stderr, "fixture-private-value stderr")
		os.Exit(7)
	case "sleep":
		_, _ = fmt.Fprint(os.Stderr, "fixture-private-value stderr")
		time.Sleep(time.Hour)
	}
	os.Exit(8)
}
