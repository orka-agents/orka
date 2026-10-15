package enforcementprovider

import (
	"path/filepath"
	"testing"
	"time"
)

func contextMutations() map[string]func(*Context) {
	return map[string]func(*Context){
		"request":      func(c *Context) { c.RequestID = "changed-request" },
		"workload":     func(c *Context) { c.WorkloadID = "changed-workload" },
		"provider":     func(c *Context) { c.ProviderID = "changed-provider/v1" },
		"audience":     func(c *Context) { c.Audience = "changed-audience" },
		"challenge":    func(c *Context) { c.Challenge = "changed-challenge" },
		"idempotency":  func(c *Context) { c.IdempotencyKey = "changed-key" },
		"policy-bytes": func(c *Context) { c.PolicyBytes = append([]byte(nil), c.PolicyBytes...); c.PolicyBytes[0] ^= 1 },
		"policy-digest": func(c *Context) {
			c.PolicyDigest = "sha256:changed"
		},
		"capabilities": func(c *Context) { c.RequiredCapabilities = []string{"changed/v1"} },
	}
}

func TestMemoryStoreRejectsAlteredSameAttemptContext(t *testing.T) {
	_, _, authorization := newSynthetic()
	store := NewMemoryStore()
	if _, err := store.Create(authorization.Context); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range contextMutations() {
		t.Run(name, func(t *testing.T) {
			changed := authorization.Context
			changed.PolicyBytes = append([]byte(nil), changed.PolicyBytes...)
			changed.RequiredCapabilities = append([]string(nil), changed.RequiredCapabilities...)
			mutate(&changed)
			if _, err := store.Create(changed); err == nil {
				t.Fatal("accepted altered same-attempt context")
			}
		})
	}
}

func TestFileStoreRejectsAlteredSameAttemptContextAfterRestart(t *testing.T) {
	_, _, authorization := newSynthetic()
	path := filepath.Join(t.TempDir(), "store.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(authorization.Context); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range contextMutations() {
		t.Run(name, func(t *testing.T) {
			restarted, openErr := NewFileStore(path)
			if openErr != nil {
				t.Fatal(openErr)
			}
			changed := authorization.Context
			changed.PolicyBytes = append([]byte(nil), changed.PolicyBytes...)
			changed.RequiredCapabilities = append([]string(nil), changed.RequiredCapabilities...)
			mutate(&changed)
			if _, createErr := restarted.Create(changed); createErr == nil {
				t.Fatal("accepted altered same-attempt context after restart")
			}
			if closeErr := restarted.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		})
	}
}

func TestAuthorizeRejectsAlteredSameIDAuthorization(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	orchestrator := Orchestrator{Store: NewMemoryStore(), Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatalf("identical retry failed: %v", err)
	}
	mutations := map[string]func(*Authorization){
		"issued-at":  func(a *Authorization) { a.IssuedAt = a.IssuedAt.Add(time.Millisecond) },
		"not-before": func(a *Authorization) { a.NotBefore = a.NotBefore.Add(time.Millisecond) },
		"expires-at": func(a *Authorization) { a.ExpiresAt = a.ExpiresAt.Add(time.Millisecond) },
		"epoch":      func(a *Authorization) { a.Epoch++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := authorization
			mutate(&changed)
			if _, err := orchestrator.Authorize(changed); err == nil {
				t.Fatal("accepted altered authorization with reused ID")
			}
		})
	}
}

func TestActivateBindsAuthorizationToAuthenticatedProviderAndEpoch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Authorization)
	}{
		{"provider", func(a *Authorization) { a.Context.ProviderID = "other/v1" }},
		{"epoch", func(a *Authorization) { a.Epoch++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, trust, authorization := newSynthetic()
			test.mutate(&authorization)
			store := NewMemoryStore()
			orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
			if _, err := orchestrator.Authorize(authorization); err == nil {
				t.Fatal("accepted authorization not bound to authenticated descriptor")
			}
			if _, err := store.Get(authorization.Context.AttemptID); err == nil {
				t.Fatal("persisted invalid provider/epoch authorization")
			}
		})
	}
}

func TestAuthorizeComparesFullAuthorizationAfterActivationAndRestart(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	path := filepath.Join(t.TempDir(), "store.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err = orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	if _, err = orchestrator.Activate(authorization.Context.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restartedStore, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := Orchestrator{Store: restartedStore, Provider: provider, Trust: trust, Now: time.Now}
	if _, err = restarted.Authorize(authorization); err != nil {
		t.Fatalf("identical authorization retry after restart failed: %v", err)
	}
	changed := authorization
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Second)
	if _, err = restarted.Authorize(changed); err == nil {
		t.Fatal("accepted altered authorization after activation and restart")
	}
}
