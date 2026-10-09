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
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Delete by identity only. Rendered Secret data, annotations and other payloads
// never enter delete input or diagnostics.
type e2eTeardownResource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
	} `json:"metadata"`
}

type e2eTeardownManifest struct {
	resources  []e2eTeardownResource
	namespaces []string
}

func (m e2eTeardownManifest) input() []byte {
	data, _ := json.Marshal(struct {
		APIVersion string                `json:"apiVersion"`
		Kind       string                `json:"kind"`
		Items      []e2eTeardownResource `json:"items"`
	}{"v1", "List", m.resources})
	return data
}

func filterE2ETeardownManifest(data []byte) (e2eTeardownManifest, error) {
	manifest := e2eTeardownManifest{resources: []e2eTeardownResource{}}
	var add func(json.RawMessage) error
	add = func(raw json.RawMessage) error {
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return nil
		}
		var object struct {
			e2eTeardownResource
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			return errors.New("invalid teardown manifest object; content omitted")
		}
		if object.Kind == "List" {
			for _, item := range object.Items {
				if err := add(item); err != nil {
					return err
				}
			}
			return nil
		}
		if object.APIVersion == "" || object.Kind == "" || object.Metadata.Name == "" {
			return errors.New("teardown resource lacks apiVersion, kind or name; content omitted")
		}
		if object.Kind == "Namespace" {
			if object.APIVersion != "v1" || len(validation.IsDNS1123Label(object.Metadata.Name)) != 0 {
				return errors.New("invalid deferred Namespace identity; content omitted")
			}
			if !slices.Contains(manifest.namespaces, object.Metadata.Name) {
				manifest.namespaces = append(manifest.namespaces, object.Metadata.Name)
			}
			return nil
		}
		manifest.resources = append(manifest.resources, object.e2eTeardownResource)
		return nil
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return e2eTeardownManifest{}, errors.New("cannot decode teardown manifest; content omitted")
		}
		if err := add(raw); err != nil {
			return e2eTeardownManifest{}, err
		}
	}
	if len(manifest.resources)+len(manifest.namespaces) == 0 {
		return e2eTeardownManifest{}, errors.New("empty teardown manifest")
	}
	return manifest, nil
}

type e2eTeardownCommand func(context.Context, []byte, string, ...string) ([]byte, error)
type e2eTeardownStep func(string, func() error) error

// These are the existing undeploy, uninstall and Namespace budgets, shared by
// each phase's subcommands. Reserve failure diagnostics inside those budgets.
const e2eTeardownDiagnosticBudget = 10 * time.Second

func e2eTeardownKustomize(projectDir string) string {
	if binary := os.Getenv("KUSTOMIZE"); binary != "" {
		return binary
	}
	localbin := os.Getenv("LOCALBIN")
	if localbin == "" {
		localbin = "bin"
	}
	if !filepath.IsAbs(localbin) {
		localbin = filepath.Join(projectDir, localbin)
	}
	return filepath.Join(localbin, "kustomize")
}

func teardownE2EDeployment(projectDir, managerNamespace string, step e2eTeardownStep, run e2eTeardownCommand, log io.Writer) error {
	kubectl := os.Getenv("KUBECTL")
	if kubectl == "" {
		kubectl = "kubectl"
	}
	kustomize := e2eTeardownKustomize(projectDir)
	namespaces := []string{managerNamespace}
	var workloads, crds e2eTeardownManifest
	mergeNamespaces := func(manifest e2eTeardownManifest) {
		for _, name := range manifest.namespaces {
			if !slices.Contains(namespaces, name) {
				namespaces = append(namespaces, name)
			}
		}
	}
	phase := func(budget time.Duration, stages func(context.Context, e2eTeardownStep) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		deadline, _ := ctx.Deadline()
		work, stop := context.WithDeadline(ctx, deadline.Add(-e2eTeardownDiagnosticBudget))
		defer stop()
		return stages(work, func(name string, action func() error) error {
			return step(name, func() error {
				err := action()
				if err != nil {
					resources := append(slices.Clone(workloads.resources), crds.resources...)
					collectE2ETeardownDiagnostics(ctx, kubectl, namespaces, resources, run, log)
				}
				return err
			})
		})
	}
	render := func(ctx context.Context, overlay string) (e2eTeardownManifest, error) {
		data, err := run(ctx, nil, kustomize, "build", filepath.Join(projectDir, "config", overlay))
		if err != nil {
			return e2eTeardownManifest{}, err
		}
		return filterE2ETeardownManifest(data)
	}
	deleteManifest := func(ctx context.Context, manifest e2eTeardownManifest) error {
		if len(manifest.resources) == 0 {
			return nil
		}
		deadline, _ := ctx.Deadline()
		_, err := run(ctx, manifest.input(), kubectl, "delete", "-f", "-", "--ignore-not-found=true",
			"--wait=true", "--timeout="+time.Until(deadline).String(), "--request-timeout=10s")
		return err
	}

	if err := phase(2*time.Minute, func(ctx context.Context, stage e2eTeardownStep) error {
		if err := stage("rendering workload teardown identities and deferring Namespaces", func() error {
			var err error
			workloads, err = render(ctx, "acp-production")
			mergeNamespaces(workloads)
			return err
		}); err != nil {
			return err
		}
		if err := stage("removing the admission webhook before workload deletion", func() error {
			_, err := run(ctx, nil, kubectl, "delete", "validatingwebhookconfiguration", "orka-admission",
				"--ignore-not-found=true", "--wait=true", "--timeout=20s", "--request-timeout=10s")
			return err
		}); err != nil {
			return err
		}
		return stage("deleting workload resources while retaining Namespaces", func() error {
			return deleteManifest(ctx, workloads)
		})
	}); err != nil {
		return err
	}
	if err := phase(2*time.Minute, func(ctx context.Context, stage e2eTeardownStep) error {
		if err := stage("rendering CRD uninstall identities", func() error {
			var err error
			crds, err = render(ctx, "crd")
			mergeNamespaces(crds)
			return err
		}); err != nil {
			return err
		}
		return stage("uninstalling CRDs before deferred Namespaces", func() error {
			return deleteManifest(ctx, crds)
		})
	}); err != nil {
		return err
	}
	return phase(time.Minute, func(ctx context.Context, stage e2eTeardownStep) error {
		if err := stage("deleting all deferred Namespaces", func() error {
			args := append([]string{"delete", "namespaces"}, namespaces...)
			args = append(args, "--ignore-not-found=true", "--wait=true", "--timeout=45s", "--request-timeout=10s")
			_, err := run(ctx, nil, kubectl, args...)
			return err
		}); err != nil {
			return err
		}
		return stage("verifying all deferred Namespaces are absent", func() error {
			args := append([]string{"get", "namespaces"}, namespaces...)
			args = append(args, "--ignore-not-found=true", "-o", "json", "--request-timeout=5s")
			data, err := run(ctx, nil, kubectl, args...)
			if err != nil {
				return err
			}
			// kubectl emits no output when all named resources are absent.
			if len(bytes.TrimSpace(data)) == 0 {
				return nil
			}
			var remaining struct {
				Kind  string            `json:"kind"`
				Items []json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(data, &remaining); err != nil || remaining.Kind != "List" {
				return errors.New("cannot verify deferred Namespace absence; output omitted")
			}
			if len(remaining.Items) != 0 {
				return fmt.Errorf("%d deferred Namespaces remain after deletion", len(remaining.Items))
			}
			return nil
		})
	})
}

func runE2ETeardownCommand(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	// Only direct executables, not make recipes or shell pipelines, are used by
	// deployment teardown. Bound pipe draining as well as the direct process.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cleanup command %s stopped: %w", filepath.Base(name), ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("cleanup command %s failed with exit code %d; output omitted", filepath.Base(name), exit.ExitCode())
		}
		return nil, fmt.Errorf("cleanup command %s failed; output omitted", filepath.Base(name))
	}
	return output.Bytes(), nil
}

func runBoundedE2ECleanup(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := runE2ETeardownCommand(ctx, nil, name, args...)
	return err
}

func collectE2ETeardownDiagnostics(parent context.Context, kubectl string, namespaces []string, resources []e2eTeardownResource, run e2eTeardownCommand, log io.Writer) {
	ctx, cancel := context.WithTimeout(parent, e2eTeardownDiagnosticBudget)
	defer cancel()
	read := func(label string, input []byte, args ...string) {
		if ctx.Err() != nil {
			return
		}
		query, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		data, err := run(query, input, kubectl, append(args, "--ignore-not-found=true", "-o", "json", "--request-timeout=3s")...)
		if err != nil {
			_, _ = fmt.Fprintf(log, "teardown diagnostics %s unavailable; output omitted\n", label)
			return
		}
		summary, err := safeE2ETeardownStatus(data)
		if err != nil {
			_, _ = fmt.Fprintf(log, "teardown diagnostics %s invalid; output omitted\n", label)
			return
		}
		_, _ = fmt.Fprintf(log, "teardown diagnostics %s: %s\n", label, summary)
	}
	read("Namespaces", nil, append([]string{"get", "namespaces"}, namespaces...)...)
	if len(resources) != 0 {
		read("manifest resources", (e2eTeardownManifest{resources: resources}).input(), "get", "-f", "-")
	}
	for _, name := range namespaces {
		read("namespace workloads "+name, nil, "get", "pods,jobs,persistentvolumeclaims", "-n", name)
	}
	if ctx.Err() != nil {
		_, _ = fmt.Fprintln(log, "teardown diagnostics budget exhausted")
	}
}

// Decode into a strict allowlist before logging. Never log JSON/YAML parse
// errors, status messages/reasons, annotations, container state or Secret data.
func safeE2ETeardownStatus(data []byte) ([]byte, error) {
	type status struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name              string   `json:"name"`
			Namespace         string   `json:"namespace,omitempty"`
			DeletionTimestamp *string  `json:"deletionTimestamp,omitempty"`
			Finalizers        []string `json:"finalizers,omitempty"`
		} `json:"metadata"`
		Spec struct {
			Finalizers []string `json:"finalizers,omitempty"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
		Items []json.RawMessage `json:"items"`
	}
	summaries := []map[string]any{}
	var add func(json.RawMessage) error
	add = func(raw json.RawMessage) error {
		var object status
		if err := json.Unmarshal(raw, &object); err != nil {
			return errors.New("invalid resource status; output omitted")
		}
		if object.Kind == "List" {
			for _, item := range object.Items {
				if err := add(item); err != nil {
					return err
				}
			}
			return nil
		}
		if object.Kind == "" || object.Metadata.Name == "" {
			return errors.New("resource status lacks identity; output omitted")
		}
		summary := map[string]any{
			"kind": object.Kind, "name": object.Metadata.Name, "namespace": object.Metadata.Namespace,
			"deleting": object.Metadata.DeletionTimestamp != nil, "finalizerCount": len(object.Metadata.Finalizers),
		}
		if object.Kind == "Namespace" {
			summary["namespaceFinalizerCount"] = len(object.Spec.Finalizers)
		}
		if slices.Contains([]string{"Pending", "Running", "Succeeded", "Failed", "Unknown", "Active", "Terminating", "Bound", "Lost"}, object.Status.Phase) {
			summary["phase"] = object.Status.Phase
		}
		summaries = append(summaries, summary)
		return nil
	}
	if err := add(data); err != nil {
		return nil, err
	}
	return json.Marshal(summaries)
}
