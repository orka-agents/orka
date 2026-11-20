package storageconfig

import (
	"strings"
	"testing"
)

func digestOf(t *testing.T, content string, args ...string) string {
	t.Helper()
	path := writeConfig(t, content)
	cfg, err := loadFlags(t, append([]string{"--storage-config=" + path}, args...)...)
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, content)
	}
	return cfg.Digest
}

func TestDigestIgnoresFieldOrderAndFormatting(t *testing.T) {
	reordered := `
adapters:
  postgres:
    clientAuth: {bearerTokenSecretRef: {key: token, name: orka-storage-postgres-client}}
    tls:
      caSecretRef: {key: ca.crt, name: orka-storage-postgres-ca}
      serverName: orka-storage-postgres.orka-system.svc
    serviceRef: {port: 8443, name: orka-storage-postgres}
bindings: {artifacts: postgres, state: postgres}
`
	if got := digestOf(t, reordered); got != goldenAdapterDigest {
		t.Fatalf("reordered digest = %s, want %s", got, goldenAdapterDigest)
	}
}

func TestDigestCoversEveryBoundSetting(t *testing.T) {
	seen := map[string]string{goldenAdapterDigest: "base"}
	t.Run("adapter name", func(t *testing.T) {
		renamed := strings.NewReplacer(": postgres\n", ": pg\n", "  postgres:\n", "  pg:\n").Replace(adapterFile)
		if strings.Contains(renamed, ": postgres\n") || !strings.Contains(renamed, "orka-storage-postgres") {
			t.Fatalf("rename fixture is wrong:\n%s", renamed)
		}
		got := digestOf(t, renamed)
		if _, ok := seen[got]; ok {
			t.Fatalf("renamed adapter digest %s collides", got)
		}
		seen[got] = "adapter name"
	})
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "service name", old: "name: orka-storage-postgres\n", new: "name: orka-storage-pg\n"},
		{name: "service port", old: "port: 8443", new: "port: 9443"},
		{name: "server name", old: "serverName: orka-storage-postgres.orka-system.svc", new: "serverName: pg.orka-system.svc"},
		{name: "CA Secret", old: "name: orka-storage-postgres-ca", new: "name: other-ca"},
		{name: "CA key", old: "key: ca.crt", new: "key: tls.crt"},
		{name: "token Secret", old: "name: orka-storage-postgres-client", new: "name: other-client"},
		{name: "token key", old: "key: token", new: "key: bearer"},
		{name: "artifacts binding", old: "  artifacts: postgres", new: "  artifacts: sqlite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(adapterFile, tc.old) {
				t.Fatalf("fixture has no %q", tc.old)
			}
			got := digestOf(t, strings.ReplaceAll(adapterFile, tc.old, tc.new))
			if previous, ok := seen[got]; ok {
				t.Fatalf("digest %s for %s equals %s", got, tc.name, previous)
			}
			seen[got] = tc.name
		})
	}
	t.Run("url endpoint", func(t *testing.T) {
		withURL := strings.Replace(adapterFile, "    serviceRef:\n      name: orka-storage-postgres\n      port: 8443\n",
			"    url: https://orka-storage-postgres.example.com:8443\n", 1)
		got := digestOf(t, withURL)
		if _, ok := seen[got]; ok {
			t.Fatalf("url digest %s collides", got)
		}
		seen[got] = "url"
		if other := digestOf(t, strings.Replace(withURL, ":8443", ":9443", 1)); other == got {
			t.Fatal("changing the URL port did not change the digest")
		}
	})
	t.Run("sqlite path", func(t *testing.T) {
		const sqliteFile = "bindings: {state: sqlite, artifacts: sqlite}\n"
		if a, b := digestOf(t, sqliteFile), digestOf(t, sqliteFile, "--store-path=/var/lib/orka/orka.db"); a == b {
			t.Fatal("changing --store-path did not change the digest of a SQLite binding")
		}
	})
	t.Run("removing optional TLS", func(t *testing.T) {
		withoutTLS := strings.Replace(adapterFile, `    tls:
      serverName: orka-storage-postgres.orka-system.svc
      caSecretRef:
        name: orka-storage-postgres-ca
        key: ca.crt
`, "", 1)
		without := digestOf(t, withoutTLS)
		if without == goldenAdapterDigest {
			t.Fatal("removing the TLS settings did not change the digest")
		}
		withEmptyTLS := strings.Replace(withoutTLS, "    clientAuth:", "    tls: {}\n    clientAuth:", 1)
		if !strings.Contains(withEmptyTLS, "tls: {}") {
			t.Fatalf("fixture has no clientAuth block:\n%s", withoutTLS)
		}
		if got := digestOf(t, withEmptyTLS); got != without {
			t.Fatalf("an empty tls block changed the digest: %s, want %s", got, without)
		}
	})
}

func TestDigestRequiresEveryContract(t *testing.T) {
	_, err := computeDigest(map[string]Binding{})
	requireReason(t, err, ReasonInvalidConfiguration)
}
