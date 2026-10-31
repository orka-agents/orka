package storageconfig

import (
	"errors"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Golden digests pin the canonical encoding: SHA-256 over the domain string,
// a NUL byte and the compact JSON of the bindings in contract order. They were
// computed independently of this package. Changing the encoding changes the
// digest of every unchanged configuration.
const (
	goldenDefaultDigest = "sha256:e1ad870685945b74a7cb6b7080bce6656a5d452c1ce23c24f56987730d2783be"
	goldenAdapterDigest = "sha256:b23fd30f6ee62f4fd3611c09d15b96ecdcae790a32f2840a7477440491b4d99b"
)

const adapterFile = `
bindings:
  state: postgres
  artifacts: postgres
adapters:
  postgres:
    serviceRef:
      name: orka-storage-postgres
      port: 8443
    tls:
      serverName: orka-storage-postgres.orka-system.svc
      caSecretRef:
        name: orka-storage-postgres-ca
        key: ca.crt
    clientAuth:
      bearerTokenSecretRef:
        name: orka-storage-postgres-client
        key: token
`

func loadFlags(t *testing.T, args ...string) (*Config, error) {
	t.Helper()
	var flags Flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.AddFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return flags.Load()
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "storage.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	return path
}

func requireReason(t *testing.T, err error, want Reason) {
	t.Helper()
	_ = requireConfigError(t, err, want)
}

func requireConfigError(t *testing.T, err error, want Reason) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want reason %s", want)
	}
	var configErr *Error
	if !errors.As(err, &configErr) {
		t.Fatalf("error %v is not a *storageconfig.Error", err)
	}
	if configErr.Reason != want {
		t.Fatalf("reason = %s, want %s (error: %v)", configErr.Reason, want, err)
	}
	if reason, ok := ReasonOf(err); !ok || reason != want {
		t.Fatalf("ReasonOf = %q, %v; want %q", reason, ok, want)
	}
	return configErr
}

func requireSQLiteBindings(t *testing.T, cfg *Config, path string) {
	t.Helper()
	if len(cfg.Bindings) != len(Contracts) {
		t.Fatalf("bindings = %v, want one per contract", cfg.Bindings)
	}
	for _, contract := range Contracts {
		binding := cfg.Bindings[contract]
		if binding.Contract != contract || binding.Backend != EmbeddedSQLiteBackend ||
			binding.Source.Adapter != nil || binding.Source.EmbeddedSQLite == nil ||
			binding.Source.EmbeddedSQLite.Path != path {
			t.Fatalf("binding for %s = %+v, want embedded SQLite at %s", contract, binding, path)
		}
	}
	got, err := cfg.RequireEmbeddedSQLite()
	if err != nil || got != path {
		t.Fatalf("RequireEmbeddedSQLite = %q, %v; want %q", got, err, path)
	}
}

func TestLoadDefaultBindsBothContractsToSQLite(t *testing.T) {
	cfg, err := loadFlags(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Source != SourceFlags || cfg.Path != "" {
		t.Fatalf("source = %s %q, want Flags without a path", cfg.Source, cfg.Path)
	}
	requireSQLiteBindings(t, cfg, DefaultStorePath)
	if cfg.Digest != goldenDefaultDigest {
		t.Fatalf("digest = %s, want %s", cfg.Digest, goldenDefaultDigest)
	}
}

func TestLoadFlags(t *testing.T) {
	t.Run("store path", func(t *testing.T) {
		cfg, err := loadFlags(t, "--store-path=/var/lib/orka/orka.db")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		requireSQLiteBindings(t, cfg, "/var/lib/orka/orka.db")
	})
	t.Run("explicit sqlite backend", func(t *testing.T) {
		cfg, err := loadFlags(t, "--store-backend=sqlite")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		requireSQLiteBindings(t, cfg, DefaultStorePath)
	})
	t.Run("unsupported backend", func(t *testing.T) {
		_, err := loadFlags(t, "--store-backend=postgres")
		requireReason(t, err, ReasonInvalidConfiguration)
	})
	t.Run("empty backend", func(t *testing.T) {
		_, err := loadFlags(t, "--store-backend=")
		requireReason(t, err, ReasonInvalidConfiguration)
	})
	t.Run("empty store path", func(t *testing.T) {
		_, err := loadFlags(t, "--store-path=")
		requireReason(t, err, ReasonInvalidConfiguration)
	})
	t.Run("backend flag with a configuration file", func(t *testing.T) {
		path := writeConfig(t, "bindings: {state: sqlite, artifacts: sqlite}\n")
		_, err := loadFlags(t, "--storage-config="+path, "--store-backend=sqlite")
		requireReason(t, err, ReasonInvalidConfiguration)
	})
	t.Run("missing configuration file", func(t *testing.T) {
		_, err := loadFlags(t, "--storage-config="+filepath.Join(t.TempDir(), "absent.yaml"))
		requireReason(t, err, ReasonInvalidConfiguration)
	})
	t.Run("oversized configuration file", func(t *testing.T) {
		path := writeConfig(t, "bindings: {state: sqlite, artifacts: sqlite}\n#"+strings.Repeat("x", MaxFileBytes))
		_, err := loadFlags(t, "--storage-config="+path)
		requireReason(t, err, ReasonInvalidConfiguration)
	})
}

func TestStoreBackendFlagDefaultIsSQLite(t *testing.T) {
	var flags Flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.AddFlags(fs)
	if got := fs.Lookup("store-backend").DefValue; got != EmbeddedSQLiteBackend {
		t.Fatalf("--store-backend default = %q, want %q", got, EmbeddedSQLiteBackend)
	}
	if got := fs.Lookup("store-path").DefValue; got != DefaultStorePath {
		t.Fatalf("--store-path default = %q, want %q", got, DefaultStorePath)
	}
	if got := fs.Lookup("storage-config").DefValue; got != "" {
		t.Fatalf("--storage-config default = %q, want empty", got)
	}
}

func TestLoadFileWithSQLiteMatchesDefault(t *testing.T) {
	path := writeConfig(t, "bindings:\n  state: sqlite\n  artifacts: sqlite\n")
	cfg, err := loadFlags(t, "--storage-config="+path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Source != SourceFile || cfg.Path != path {
		t.Fatalf("source = %s %q, want File %q", cfg.Source, cfg.Path, path)
	}
	requireSQLiteBindings(t, cfg, DefaultStorePath)
	if cfg.Digest != goldenDefaultDigest {
		t.Fatalf("file digest = %s, want the default digest %s", cfg.Digest, goldenDefaultDigest)
	}
}

func TestLoadFileWithAdapter(t *testing.T) {
	path := writeConfig(t, adapterFile)
	cfg, err := loadFlags(t, "--storage-config="+path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Digest != goldenAdapterDigest {
		t.Fatalf("digest = %s, want %s", cfg.Digest, goldenAdapterDigest)
	}
	for _, contract := range Contracts {
		binding := cfg.Bindings[contract]
		adapter := binding.Source.Adapter
		if binding.Backend != "postgres" || binding.Source.EmbeddedSQLite != nil || adapter == nil {
			t.Fatalf("binding for %s = %+v, want the postgres adapter", contract, binding)
		}
		if adapter.ServiceRef == nil || *adapter.ServiceRef != (ServiceRef{Name: "orka-storage-postgres", Port: 8443}) ||
			adapter.URL != "" ||
			adapter.TLS == nil || adapter.TLS.ServerName != "orka-storage-postgres.orka-system.svc" ||
			adapter.TLS.CASecretRef == nil || *adapter.TLS.CASecretRef != (SecretKeyRef{Name: "orka-storage-postgres-ca", Key: "ca.crt"}) ||
			adapter.ClientAuth == nil || adapter.ClientAuth.BearerTokenSecretRef == nil ||
			*adapter.ClientAuth.BearerTokenSecretRef != (SecretKeyRef{Name: "orka-storage-postgres-client", Key: "token"}) {
			t.Fatalf("adapter for %s = %+v", contract, adapter)
		}
	}
	if cfg.Bindings[storagev1.StateContract].Source.Adapter == cfg.Bindings[storagev1.ArtifactsContract].Source.Adapter {
		t.Fatal("bindings share one adapter value; each binding must own its copy")
	}
	_, err = cfg.RequireEmbeddedSQLite()
	configErr := requireConfigError(t, err, ReasonAdapterTransportUnavailable)
	if configErr.Contract != storagev1.StateContract || configErr.Backend != "postgres" {
		t.Fatalf("refusal names %s %q, want state.v1 postgres", configErr.Contract, configErr.Backend)
	}
}

func TestRequireEmbeddedSQLiteRefusesAnAdapterForEitherContract(t *testing.T) {
	path := writeConfig(t, `
bindings:
  state: sqlite
  artifacts: blobs
adapters:
  blobs:
    url: https://blobs.example.com:8443/orka
    clientAuth:
      bearerTokenSecretRef: {name: blobs-client, key: token}
`)
	cfg, err := loadFlags(t, "--storage-config="+path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bindings[storagev1.StateContract].Source.EmbeddedSQLite == nil {
		t.Fatalf("state binding = %+v, want embedded SQLite", cfg.Bindings[storagev1.StateContract])
	}
	if got := cfg.Bindings[storagev1.ArtifactsContract].Source.Adapter; got == nil || got.URL != "https://blobs.example.com:8443/orka" {
		t.Fatalf("artifacts adapter = %+v", got)
	}
	_, err = cfg.RequireEmbeddedSQLite()
	configErr := requireConfigError(t, err, ReasonAdapterTransportUnavailable)
	if configErr.Contract != storagev1.ArtifactsContract || configErr.Backend != "blobs" {
		t.Fatalf("refusal names %s %q, want artifacts.v1 blobs", configErr.Contract, configErr.Backend)
	}
}

func TestLoadRejectsInvalidFiles(t *testing.T) {
	credentialURL := (&url.URL{Scheme: "https", User: url.UserPassword("user", "hunter2"), Host: "pg.example.com"}).String()
	const auth = "    clientAuth: {bearerTokenSecretRef: {name: pg-client, key: token}}\n"
	const bound = "bindings: {state: pg, artifacts: pg}\nadapters:\n  pg:\n"
	for _, tc := range []struct {
		name    string
		content string
		args    []string
		want    string
		absent  string
	}{
		{name: "empty file", content: "", want: "bindings: Required value"},
		{name: "list document", content: "- state\n", want: "decode storage configuration"},
		{name: "unknown top-level field", content: "bindings: {state: sqlite, artifacts: sqlite}\nlimits: {}\n", want: "unknown field"},
		{name: "unknown nested field", content: bound + "    serviceRef: {name: pg, port: 8443}\n    tls: {serverNmae: pg}\n" + auth, want: "unknown field"},
		{name: "duplicate field", content: "bindings: {state: sqlite, artifacts: sqlite}\nbindings: {state: sqlite, artifacts: sqlite}\n", want: "decode storage configuration"},
		{name: "trailing document", content: "bindings: {state: sqlite, artifacts: sqlite}\n---\nbindings: {state: pg, artifacts: pg}\n", want: "one YAML document"},
		{name: "trailing empty document", content: "bindings: {state: sqlite, artifacts: sqlite}\n---\n", want: "one YAML document"},
		{name: "field name in another case", content: "Bindings: {state: sqlite, artifacts: sqlite}\n", want: "unknown field"},
		{name: "duplicate field in another case", content: "bindings: {state: sqlite, State: pg, artifacts: sqlite}\n", want: "unknown field"},
		{name: "unbound state", content: "bindings: {artifacts: sqlite}\n", want: "bindings.state: Required value"},
		{name: "unbound artifacts", content: "bindings: {state: sqlite}\n", want: "bindings.artifacts: Required value"},
		{name: "unknown adapter", content: "bindings: {state: sqlite, artifacts: missing}\n", want: "names no configured adapter"},
		{name: "reserved adapter name", content: "bindings: {state: sqlite, artifacts: sqlite}\nadapters:\n  sqlite:\n    serviceRef: {name: pg, port: 8443}\n" + auth, want: "reserved for embedded SQLite"},
		{name: "invalid adapter name", content: "bindings: {state: Postgres, artifacts: Postgres}\nadapters:\n  Postgres:\n    serviceRef: {name: pg, port: 8443}\n" + auth, want: "adapters[Postgres]"},
		{name: "unused adapter", content: "bindings: {state: sqlite, artifacts: sqlite}\nadapters:\n  pg:\n    serviceRef: {name: pg, port: 8443}\n" + auth, want: "is not bound to any contract"},
		{name: "null adapter", content: bound, want: "names no configured adapter"},
		{name: "service and url", content: bound + "    serviceRef: {name: pg, port: 8443}\n    url: https://pg.example.com\n" + auth, want: "exactly one of serviceRef or url"},
		{name: "neither service nor url", content: bound + auth, want: "exactly one of serviceRef or url"},
		{name: "port zero", content: bound + "    serviceRef: {name: pg}\n" + auth, want: "serviceRef.port"},
		{name: "port too large", content: bound + "    serviceRef: {name: pg, port: 70000}\n" + auth, want: "serviceRef.port"},
		{name: "port as string", content: bound + "    serviceRef: {name: pg, port: \"8443\"}\n" + auth, want: "decode storage configuration"},
		{name: "invalid service name", content: bound + "    serviceRef: {name: 1pg, port: 8443}\n" + auth, want: "serviceRef.name"},
		{name: "http url", content: bound + "    url: http://pg.example.com\n" + auth, want: "adapters[pg].url"},
		{name: "url with credentials", content: bound + "    url: " + credentialURL + "\n" + auth, want: "adapters[pg].url", absent: "hunter2"},
		{name: "url with query", content: bound + "    url: https://pg.example.com/?token=hunter2\n" + auth, want: "adapters[pg].url", absent: "hunter2"},
		{name: "url with fragment", content: bound + "    url: https://pg.example.com/#x\n" + auth, want: "adapters[pg].url"},
		{name: "url with whitespace", content: bound + "    url: \"https://pg.example.com/a b\"\n" + auth, want: "adapters[pg].url"},
		{name: "url without host", content: bound + "    url: https:///orka\n" + auth, want: "adapters[pg].url"},
		{name: "missing client auth", content: bound + "    serviceRef: {name: pg, port: 8443}\n", want: "clientAuth.bearerTokenSecretRef: Required value"},
		{name: "missing token reference", content: bound + "    serviceRef: {name: pg, port: 8443}\n    clientAuth: {}\n", want: "clientAuth.bearerTokenSecretRef: Required value"},
		{name: "token reference without key", content: bound + "    serviceRef: {name: pg, port: 8443}\n    clientAuth: {bearerTokenSecretRef: {name: pg-client}}\n", want: "bearerTokenSecretRef.key: Required value"},
		{name: "token reference with invalid name", content: bound + "    serviceRef: {name: pg, port: 8443}\n    clientAuth: {bearerTokenSecretRef: {name: PG_client, key: token}}\n", want: "bearerTokenSecretRef.name"},
		{name: "token reference with invalid key", content: bound + "    serviceRef: {name: pg, port: 8443}\n    clientAuth: {bearerTokenSecretRef: {name: pg-client, key: a/b}}\n", want: "bearerTokenSecretRef.key"},
		{name: "invalid server name", content: bound + "    serviceRef: {name: pg, port: 8443}\n    tls: {serverName: PG_server}\n" + auth, want: "tls.serverName"},
		{name: "CA reference without name", content: bound + "    serviceRef: {name: pg, port: 8443}\n    tls: {caSecretRef: {key: ca.crt}}\n" + auth, want: "tls.caSecretRef.name: Required value"},
		{name: "sqlite without store path", content: "bindings: {state: sqlite, artifacts: sqlite}\n", args: []string{"--store-path="}, want: "--store-path must be set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.content)
			_, err := loadFlags(t, append([]string{"--storage-config=" + path}, tc.args...)...)
			requireReason(t, err, ReasonInvalidConfiguration)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
				t.Fatalf("error %q echoes %q", err, tc.absent)
			}
		})
	}
}

func TestLoadAcceptsUnusedStorePathWithAdapters(t *testing.T) {
	path := writeConfig(t, adapterFile)
	cfg, err := loadFlags(t, "--storage-config="+path, "--store-path=")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Digest != goldenAdapterDigest {
		t.Fatalf("digest = %s; an unused --store-path must not change it", cfg.Digest)
	}
}

func TestLoadAcceptsOneDocumentWithMarkers(t *testing.T) {
	for _, content := range []string{
		"---\nbindings: {state: sqlite, artifacts: sqlite}\n",
		"# storage\nbindings: {state: sqlite, artifacts: sqlite}\n...\n",
	} {
		cfg, err := loadFlags(t, "--storage-config="+writeConfig(t, content))
		if err != nil {
			t.Fatalf("Load(%q): %v", content, err)
		}
		requireSQLiteBindings(t, cfg, DefaultStorePath)
	}
}
