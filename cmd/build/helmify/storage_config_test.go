package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/storageconfig"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const storageAdapterValues = `
storage:
  bindings:
    state: postgres
    artifacts: sqlite
  adapters:
    postgres:
      serviceRef: {name: orka-storage-postgres, port: 8443}
      tls:
        serverName: orka-storage-postgres.orka-test.svc
        caSecretRef: {name: orka-storage-postgres-ca, key: ca.crt}
      clientAuth:
        bearerTokenSecretRef: {name: orka-storage-postgres-client, key: token}
`

type renderedStorage struct {
	deployment appsv1.Deployment
	configMap  *corev1.ConfigMap
}

func (r renderedStorage) controller(t *testing.T) corev1.Container {
	t.Helper()
	if len(r.deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("controller Deployment has %d containers", len(r.deployment.Spec.Template.Spec.Containers))
	}
	return r.deployment.Spec.Template.Spec.Containers[0]
}

func writeStorageValues(t *testing.T, values string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "storage-values.yaml")
	if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
		t.Fatalf("write values: %v", err)
	}
	return path
}

func renderStorage(t *testing.T, values string) renderedStorage {
	t.Helper()
	args := []string{}
	if values != "" {
		args = append(args, "--values", writeStorageValues(t, values))
	}
	output := requireHelmRender(t, args...)
	var rendered renderedStorage
	for document := range strings.SplitSeq(output, "\n---\n") {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(document), &meta); err != nil {
			continue
		}
		switch {
		case meta.Kind == "Deployment" && meta.Metadata.Name == "test-orka-controller":
			if err := yaml.Unmarshal([]byte(document), &rendered.deployment); err != nil {
				t.Fatalf("decode controller Deployment: %v", err)
			}
		case meta.Kind == "ConfigMap" && meta.Metadata.Name == "test-orka-storage":
			rendered.configMap = &corev1.ConfigMap{}
			if err := yaml.Unmarshal([]byte(document), rendered.configMap); err != nil {
				t.Fatalf("decode storage ConfigMap: %v", err)
			}
		}
	}
	if rendered.deployment.Name == "" {
		t.Fatalf("render has no controller Deployment:\n%s", output)
	}
	return rendered
}

// loadRenderedStorage feeds the rendered file and flags to the controller's
// own loader, so the chart and the controller agree on the format.
func loadRenderedStorage(t *testing.T, rendered renderedStorage) (*storageconfig.Config, error) {
	t.Helper()
	controller := rendered.controller(t)
	args := []string{}
	for _, arg := range controller.Args {
		if strings.HasPrefix(arg, "--store-") || strings.HasPrefix(arg, "--storage-") {
			args = append(args, arg)
		}
	}
	for i, arg := range args {
		if path, ok := strings.CutPrefix(arg, "--storage-config="); ok {
			if rendered.configMap == nil {
				t.Fatalf("controller reads %s but no storage ConfigMap was rendered", path)
			}
			local := filepath.Join(t.TempDir(), "storage.yaml")
			if err := os.WriteFile(local, []byte(rendered.configMap.Data["storage.yaml"]), 0o600); err != nil {
				t.Fatalf("write rendered configuration: %v", err)
			}
			args[i] = "--storage-config=" + local
		}
	}
	var flags storageconfig.Flags
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	flags.AddFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse rendered storage flags %v: %v", args, err)
	}
	return flags.Load()
}

func TestStaticChartDefaultStorageKeepsSQLiteFlags(t *testing.T) {
	for name, values := range map[string]string{
		"chart defaults":                 "",
		"storage block absent":           "storage: null\n",
		"explicit sqlite bindings":       "storage: {bindings: {state: sqlite, artifacts: sqlite}, adapters: {}}\n",
		"bindings absent from the block": "storage: {adapters: {}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			rendered := renderStorage(t, values)
			if rendered.configMap != nil {
				t.Fatalf("default storage rendered a storage ConfigMap: %+v", rendered.configMap)
			}
			controller := rendered.controller(t)
			if !slices.Contains(controller.Args, "--store-backend=sqlite") ||
				!slices.Contains(controller.Args, "--store-path=/data/orka.db") {
				t.Fatalf("controller args %v lack the default SQLite flags", controller.Args)
			}
			for _, arg := range controller.Args {
				if strings.HasPrefix(arg, "--storage-config") {
					t.Fatalf("default storage passes %s", arg)
				}
			}
			for _, volume := range rendered.deployment.Spec.Template.Spec.Volumes {
				if volume.Name == "storage-config" {
					t.Fatal("default storage mounts a storage configuration volume")
				}
			}
			if _, ok := rendered.deployment.Spec.Template.Annotations["orka.ai/storage-config-checksum"]; ok {
				t.Fatal("default storage sets a storage configuration checksum")
			}
			cfg, err := loadRenderedStorage(t, rendered)
			if err != nil {
				t.Fatalf("controller rejects the default flags: %v", err)
			}
			if cfg.Source != storageconfig.SourceFlags {
				t.Fatalf("source = %s, want Flags", cfg.Source)
			}
			if path, err := cfg.RequireEmbeddedSQLite(); err != nil || path != "/data/orka.db" {
				t.Fatalf("RequireEmbeddedSQLite = %q, %v", path, err)
			}
		})
	}
}

func TestStaticChartRendersStorageConfiguration(t *testing.T) {
	rendered := renderStorage(t, storageAdapterValues)
	if rendered.configMap == nil {
		t.Fatal("adapter binding rendered no storage ConfigMap")
	}
	controller := rendered.controller(t)
	if slices.Contains(controller.Args, "--store-backend=sqlite") {
		t.Fatalf("controller args %v combine --store-backend with --storage-config", controller.Args)
	}
	if !slices.Contains(controller.Args, "--storage-config=/etc/orka/storage/storage.yaml") ||
		!slices.Contains(controller.Args, "--store-path=/data/orka.db") {
		t.Fatalf("controller args %v lack --storage-config or the SQLite path for artifacts", controller.Args)
	}
	mounted := false
	for _, mount := range controller.VolumeMounts {
		if mount.Name == "storage-config" {
			mounted = mount.MountPath == "/etc/orka/storage" && mount.ReadOnly
		}
	}
	if !mounted {
		t.Fatalf("storage configuration is not mounted read-only at /etc/orka/storage: %+v", controller.VolumeMounts)
	}
	var volume *corev1.Volume
	for i := range rendered.deployment.Spec.Template.Spec.Volumes {
		if rendered.deployment.Spec.Template.Spec.Volumes[i].Name == "storage-config" {
			volume = &rendered.deployment.Spec.Template.Spec.Volumes[i]
		}
	}
	if volume == nil || volume.ConfigMap == nil || volume.ConfigMap.Name != "test-orka-storage" ||
		len(volume.ConfigMap.Items) != 1 || volume.ConfigMap.Items[0].Key != "storage.yaml" {
		t.Fatalf("storage configuration volume = %+v", volume)
	}
	data := rendered.configMap.Data["storage.yaml"]
	sum := sha256.Sum256([]byte(strings.TrimSuffix(data, "\n")))
	checksum := rendered.deployment.Spec.Template.Annotations["orka.ai/storage-config-checksum"]
	if checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum annotation = %q, want the SHA-256 of the rendered file", checksum)
	}
	if len(rendered.configMap.Data) != 1 || len(rendered.configMap.BinaryData) != 0 {
		t.Fatalf("storage ConfigMap holds %v, want only storage.yaml", rendered.configMap.Data)
	}
}

func TestStaticChartStorageConfigurationLoadsInTheController(t *testing.T) {
	rendered := renderStorage(t, storageAdapterValues)
	cfg, err := loadRenderedStorage(t, rendered)
	data := ""
	if rendered.configMap != nil {
		data = rendered.configMap.Data["storage.yaml"]
	}
	if err != nil {
		t.Fatalf("controller rejects the rendered configuration: %v\n%s", err, data)
	}
	if cfg.Source != storageconfig.SourceFile {
		t.Fatalf("source = %s, want File", cfg.Source)
	}
	state := cfg.Bindings[storagev1.StateContract]
	adapter := state.Source.Adapter
	if state.Backend != "postgres" || adapter == nil || adapter.ServiceRef == nil || adapter.TLS == nil ||
		adapter.TLS.CASecretRef == nil || adapter.ClientAuth == nil || adapter.ClientAuth.BearerTokenSecretRef == nil {
		t.Fatalf("state binding = %+v", state)
	}
	if *adapter.ServiceRef != (storageconfig.ServiceRef{Name: "orka-storage-postgres", Port: 8443}) ||
		adapter.TLS.ServerName != "orka-storage-postgres.orka-test.svc" ||
		*adapter.TLS.CASecretRef != (storageconfig.SecretKeyRef{Name: "orka-storage-postgres-ca", Key: "ca.crt"}) ||
		*adapter.ClientAuth.BearerTokenSecretRef != (storageconfig.SecretKeyRef{
			Name: "orka-storage-postgres-client", Key: "token",
		}) {
		t.Fatalf("state adapter = %+v", adapter)
	}
	artifacts := cfg.Bindings[storagev1.ArtifactsContract]
	if artifacts.Backend != storageconfig.EmbeddedSQLiteBackend || artifacts.Source.EmbeddedSQLite == nil ||
		artifacts.Source.EmbeddedSQLite.Path != "/data/orka.db" {
		t.Fatalf("artifacts binding = %+v", artifacts)
	}
	_, err = cfg.RequireEmbeddedSQLite()
	var configErr *storageconfig.Error
	if !errors.As(err, &configErr) || configErr.Reason != storageconfig.ReasonAdapterTransportUnavailable ||
		configErr.Contract != storagev1.StateContract {
		t.Fatalf("RequireEmbeddedSQLite error = %v, want AdapterTransportUnavailable for state.v1", err)
	}
}

func TestStaticChartStorageAdaptersOnlyOmitSQLitePath(t *testing.T) {
	values := strings.Replace(storageAdapterValues, "artifacts: sqlite", "artifacts: postgres", 1)
	rendered := renderStorage(t, values)
	for _, arg := range rendered.controller(t).Args {
		if strings.HasPrefix(arg, "--store-") {
			t.Fatalf("adapter-only storage passes %s", arg)
		}
	}
	cfg, err := loadRenderedStorage(t, rendered)
	if err != nil {
		t.Fatalf("controller rejects the rendered configuration: %v", err)
	}
	for _, contract := range storageconfig.Contracts {
		if cfg.Bindings[contract].Backend != "postgres" {
			t.Fatalf("%s binding = %+v, want postgres", contract, cfg.Bindings[contract])
		}
	}
}

func TestStaticChartStorageChecksumFollowsConfiguration(t *testing.T) {
	base := renderStorage(t, storageAdapterValues).deployment.Spec.Template.Annotations["orka.ai/storage-config-checksum"]
	changed := renderStorage(t, strings.Replace(storageAdapterValues, "port: 8443", "port: 9443", 1)).
		deployment.Spec.Template.Annotations["orka.ai/storage-config-checksum"]
	if base == "" || changed == "" || base == changed {
		t.Fatalf("checksum %q did not change to a new value after a port change (got %q)", base, changed)
	}
}

func TestStaticChartStoragePassesUnknownAdapterFieldsToTheController(t *testing.T) {
	values := strings.Replace(storageAdapterValues, "serverName:", "serverNmae:", 1)
	rendered := renderStorage(t, values)
	_, err := loadRenderedStorage(t, rendered)
	var configErr *storageconfig.Error
	if !errors.As(err, &configErr) || configErr.Reason != storageconfig.ReasonInvalidConfiguration ||
		!strings.Contains(err.Error(), "serverNmae") {
		t.Fatalf("loader error = %v, want an InvalidConfiguration naming the misspelled field", err)
	}
}

func TestStaticChartRejectsInvalidStorageValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values string
		want   string
	}{
		{
			name:   "unknown adapter",
			values: "storage: {bindings: {state: postgres, artifacts: sqlite}}\n",
			want:   `storage.bindings.state names "postgres", which is not in storage.adapters`,
		},
		{
			name:   "empty binding",
			values: "storage: {bindings: {state: \"\", artifacts: sqlite}}\n",
			want:   "storage.bindings.state must be",
		},
		{
			name:   "non-string binding",
			values: "storage: {bindings: {state: sqlite, artifacts: 1}}\n",
			want:   "storage.bindings.artifacts must be",
		},
		{
			name:   "unused adapter",
			values: "storage: {adapters: {postgres: {url: \"https://pg.example.com\"}}}\n",
			want:   "storage.adapters.postgres is not bound to any contract",
		},
		{
			name: "reserved adapter name",
			values: "storage:\n  bindings: {state: sqlite, artifacts: sqlite}\n" +
				"  adapters: {sqlite: {url: \"https://pg.example.com\"}}\n",
			want: "storage.adapters.sqlite is reserved",
		},
		{
			name:   "adapters as a list",
			values: "storage: {adapters: [postgres]}\n",
			want:   "storage.adapters must be a map",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, "--values", writeStorageValues(t, tc.values))
			if err == nil || !strings.Contains(output, tc.want) {
				t.Fatalf("helm template error = %v, want %q:\n%s", err, tc.want, output)
			}
		})
	}
}
