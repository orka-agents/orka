//go:build unix

package enforcementprovider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func consumedFixture(t *testing.T) (*synthetic, *MemoryStore, Orchestrator, Attempt) {
	t.Helper()
	provider, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID)
	if err != nil {
		t.Fatal(err)
	}
	return provider, store, orchestrator, attempt
}

func replaceMemoryAttempt(store *MemoryStore, attempt Attempt) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.attempts[attempt.Context.AttemptID] = cloneAttempt(attempt)
}

func TestRecoveryRejectsEveryTamperedReleaseObligationWithoutProviderSideEffects(t *testing.T) {
	cases := map[string]func(*Attempt){
		"authorization": func(a *Attempt) { a.Authorization.Context.Audience = "tampered" },
		"descriptor":    func(a *Attempt) { a.Descriptor.Signature[0] ^= 1 },
		"activation":    func(a *Attempt) { a.ActivationID = "tampered" },
		"proof":         func(a *Attempt) { a.Proof.Signature[0] ^= 1 },
		"proof digest":  func(a *Attempt) { a.ProofDigest = "proof-tampered" },
		"release id":    func(a *Attempt) { a.ReleaseID = "release-tampered" },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			provider, store, orchestrator, attempt := consumedFixture(t)
			tamper(&attempt)
			replaceMemoryAttempt(store, attempt)
			result := orchestrator.Recover([]Attempt{attempt})
			if result[attempt.Context.AttemptID] == nil {
				t.Fatal("tampered release obligation recovered")
			}
			if len(provider.releaseRequests) != 0 || len(provider.cleanupRequests) != 0 {
				t.Fatalf("provider side effect occurred: release=%d cleanup=%d", len(provider.releaseRequests), len(provider.cleanupRequests))
			}
		})
	}
}

func TestRecoveryRejectsEveryTamperedCleanupObligationWithoutProviderSideEffects(t *testing.T) {
	cases := map[string]func(*Attempt){
		"authorization": func(a *Attempt) { a.Authorization.Context.Challenge = "tampered" },
		"descriptor":    func(a *Attempt) { a.Descriptor.PublicKey[0] ^= 1 },
		"activation":    func(a *Attempt) { a.ActivationID = "tampered" },
		"proof":         func(a *Attempt) { a.Proof.AuthorizationDigest = "tampered" },
		"proof digest":  func(a *Attempt) { a.ProofDigest = "proof-tampered" },
		"release id":    func(a *Attempt) { a.ReleaseID = "release-tampered" },
		"cleanup id":    func(a *Attempt) { a.CleanupID = "cleanup-tampered" },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			provider, store, orchestrator, attempt := consumedFixture(t)
			attempt.Phase = PhaseCleanupPending
			attempt.CleanupID = stableID("cleanup-", attempt.Context.AttemptID, attempt.ActivationID, attempt.ReleaseID)
			tamper(&attempt)
			replaceMemoryAttempt(store, attempt)
			result := orchestrator.Recover([]Attempt{attempt})
			if result[attempt.Context.AttemptID] == nil {
				t.Fatal("tampered cleanup obligation recovered")
			}
			if len(provider.releaseRequests) != 0 || len(provider.cleanupRequests) != 0 {
				t.Fatalf("provider side effect occurred: release=%d cleanup=%d", len(provider.releaseRequests), len(provider.cleanupRequests))
			}
		})
	}
}

func TestProtocolV2RejectsV1NegotiationAndFrameDowngrade(t *testing.T) {
	provider, trust, _ := newSynthetic()
	descriptor := provider.d
	descriptor.ProtocolVersions = []string{"orka.enforcement-provider/v1"}
	SignDescriptor(&descriptor, provider.priv)
	if err := Negotiate(descriptor, trust, descriptor.Capabilities, time.Now()); err == nil || !strings.Contains(err.Error(), "unsupported protocol") {
		t.Fatalf("v1 peer negotiated v2 semantics: %v", err)
	}
	if transportFrameVersion != 2 {
		t.Fatalf("transport frame version = %d", transportFrameVersion)
	}
}

func TestUnixTransportRejectsV1ResponseFrame(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "socket")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "provider.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	serverErr := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()
		request, readErr := readTransportFrame(connection)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		var frame requestFrame
		if decodeErr := strictDecodeJSON(request, &frame); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		if frame.Version != transportFrameVersion {
			serverErr <- fmt.Errorf("request frame version = %d", frame.Version)
			return
		}
		_, writeErr := connection.Write([]byte(`{"version":1,"id":"1","ok":true,"result":{"state":"active"}}` + "\n"))
		serverErr <- writeErr
	}()
	provider := NewUnixProvider(path, uint32(os.Geteuid()), time.Second).(*unixProvider)
	var result struct {
		State string `json:"state"`
	}
	if err = provider.call("GetStatus", map[string]any{"activationId": "a"}, &result); err == nil || !strings.Contains(err.Error(), "binding mismatch") {
		t.Fatalf("v1 response frame accepted by v2 client: %v", err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestStrictNestedResultsRejectDuplicateUnknownTrailingDepthAndSize(t *testing.T) {
	type lifecycleResult struct {
		State string `json:"state"`
	}
	valid := []byte(`{"state":"active"}`)
	var out lifecycleResult
	if err := strictDecodeJSON(valid, &out); err != nil || out.State != "active" {
		t.Fatalf("valid result rejected: %#v %v", out, err)
	}
	cases := map[string][]byte{
		"duplicate": []byte(`{"state":"active","state":"released"}`),
		"unknown":   []byte(`{"state":"active","extra":true}`),
		"trailing":  []byte(`{"state":"active"}{}`),
		"depth":     append([]byte(strings.Repeat("[", 66)), []byte(strings.Repeat("]", 66))...),
		"oversize":  make([]byte, maxTransportFrame+1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := strictDecodeJSON(input, &out); err == nil {
				t.Fatal("malformed nested result accepted")
			}
		})
	}
	var proof proofResultWire
	proofJSON := []byte(`{"proofId":"p","authorizationId":"a","activationId":"x","workloadId":"w","providerId":"provider","audience":"aud","challenge":"c","policyDigest":"d","readBackDigest":"r","authorizationDigest":"ad","keyId":"k","epoch":1,"issuedAt":"2026-01-01T00:00:00Z","expiresAt":"2026-01-01T00:01:00Z","signature":"AA=="}`)
	if err := strictDecodeJSON(proofJSON, &proof); err != nil || proof.IssuedAt.IsZero() || proof.ExpiresAt.IsZero() {
		t.Fatalf("documented proof result rejected: %#v %v", proof, err)
	}

	// Every actual operation result type passes through strictDecodeJSON in
	// unixProvider.call; unknown fields must fail for each contract.
	for name, target := range map[string]any{
		"descriptor":  &descriptorWire{},
		"preparation": &preparationResultWire{},
		"activation":  &activationResultWire{},
		"proof":       &proofResultWire{},
		"readback":    &readBackResultWire{},
		"status":      &statusResultWire{},
		"lifecycle":   &emptyResultWire{},
	} {
		t.Run(name+" unknown", func(t *testing.T) {
			if err := strictDecodeJSON([]byte(`{"unexpected":true}`), target); err == nil {
				t.Fatal("unknown nested field accepted")
			}
		})
	}
}

func TestFileStoreRejectsHostileParentAndLockSymlink(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "hostile")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(filepath.Join(parent, "state.json")); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("hostile parent accepted: %v", err)
	}
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(private, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(private, "state.json.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(filepath.Join(private, "state.json")); err == nil {
		t.Fatal("lock symlink accepted")
	}
}

func TestLockedPathReplacementIsDetected(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "state.lock")
	file, err := acquireStoreLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = releaseStoreLock(file) }()
	if err = os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = verifyLockedPath(path, file); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replacement not detected: %v", err)
	}
}

func TestFileStoreFailsClosedAfterLockPathReplacement(t *testing.T) {
	path := secureStorePath(t, "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err = os.Rename(path+".lock", path+".lock.old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(Context{AttemptID: "attempt"}); err == nil || !strings.Contains(err.Error(), "lock ownership lost") {
		t.Fatalf("store operated after lock replacement: %v", err)
	}
}

func TestFileStoreConcurrentInitialCreationHasOneOwner(t *testing.T) {
	if os.Getenv("ORKA_FILESTORE_INITIAL_RACE_HELPER") == "1" {
		start := os.Getenv("ORKA_FILESTORE_INITIAL_RACE_START")
		for range 500 {
			if _, err := os.Stat(start); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		store, err := NewFileStore(os.Getenv("ORKA_FILESTORE_INITIAL_RACE_PATH"))
		if err != nil {
			_, _ = os.Stdout.WriteString("REJECT\n")
			return
		}
		_, _ = os.Stdout.WriteString("OPEN\n")
		time.Sleep(500 * time.Millisecond)
		_ = store.Close()
		return
	}
	path := secureStorePath(t, "state.json")
	start := filepath.Join(filepath.Dir(path), "start")
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for index := range commands {
		command := exec.Command(os.Args[0], "-test.run=^TestFileStoreConcurrentInitialCreationHasOneOwner$")
		command.Env = append(os.Environ(),
			"ORKA_FILESTORE_INITIAL_RACE_HELPER=1",
			"ORKA_FILESTORE_INITIAL_RACE_START="+start,
			"ORKA_FILESTORE_INITIAL_RACE_PATH="+path,
		)
		command.Stdout = &outputs[index]
		command.Stderr = &outputs[index]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[index] = command
	}
	if err := os.WriteFile(start, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := 0
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", index, err, outputs[index].String())
		}
		if strings.Contains(outputs[index].String(), "OPEN") {
			opened++
		}
	}
	if opened != 1 {
		t.Fatalf("concurrent initial owners = %d, outputs: %q / %q", opened, outputs[0].String(), outputs[1].String())
	}
}

func TestFileStoreFailsClosedAfterParentPathReplacement(t *testing.T) {
	path := secureStorePath(t, "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	parent := filepath.Dir(path)
	moved := parent + ".old"
	if err = os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get("missing"); err == nil || !strings.Contains(err.Error(), "parent ownership lost") {
		t.Fatalf("store operated after parent replacement: %v", err)
	}
}

func TestControllerStartupFailsClosedAfterLockPathReplacement(t *testing.T) {
	provider, trust, _ := newSynthetic()
	path := secureStorePath(t, "state.json")
	controller, err := NewController(path, provider, trust, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close() }()
	if err = os.Rename(path+".lock", path+".lock.old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = controller.Start(); err == nil || !strings.Contains(err.Error(), "lock ownership lost") {
		t.Fatalf("controller became ready after lock replacement: %v", err)
	}
	if controller.ready {
		t.Fatal("controller is ready after recovery enumeration failed")
	}
}

func TestStrictResultJSONIsValidJSON(t *testing.T) {
	// Guard the adversarial fixtures themselves from accidentally testing only
	// syntax failures where a semantic strictness failure is intended.
	for _, input := range [][]byte{[]byte(`{"state":"active","state":"released"}`), []byte(`{"state":"active","extra":true}`)} {
		if !json.Valid(input) {
			t.Fatalf("invalid test fixture: %s", input)
		}
	}
}
