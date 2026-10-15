package enforcementprovider

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreRejectsVersionWithoutDurableDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enforcement.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"attempts":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(path); err == nil {
		t.Fatal("accepted obsolete enforcement store schema")
	}
}

func TestFileStoreRestartPreservesReleaseObligation(t *testing.T) {
	p, trust, a := newSynthetic()
	path := filepath.Join(t.TempDir(), "enforcement.json")
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	o := Orchestrator{Store: st, Provider: p, Trust: trust, Now: time.Now}
	if _, err = o.Authorize(a); err != nil {
		t.Fatal(err)
	}
	x, err := o.Activate(a.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	x, err = o.ConsumeProof(a.Context.AttemptID, x.ProofID)
	if err != nil {
		t.Fatal(err)
	}
	releaseID := x.ReleaseID
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := Orchestrator{Store: reopened, Provider: p, Trust: trust, Now: time.Now}
	x, err = restarted.Release(a.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if x.Phase != PhaseReleased || x.ReleaseID != releaseID {
		t.Fatalf("lost release obligation: %#v", x)
	}
}

func TestStartupRecoveryExecutesOnlyDurableReleaseObligation(t *testing.T) {
	p, trust, a := newSynthetic()
	path := filepath.Join(t.TempDir(), "enforcement.json")
	st, _ := NewFileStore(path)
	o := Orchestrator{Store: st, Provider: p, Trust: trust, Now: time.Now}
	if _, err := o.Authorize(a); err != nil {
		t.Fatal(err)
	}
	x, _ := o.Activate(a.Context.AttemptID)
	x, _ = o.ConsumeProof(a.Context.AttemptID, x.ProofID)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := Orchestrator{Store: reopened, Provider: p, Trust: trust, Now: time.Now}
	result := restarted.Recover(reopened.List())
	if result[a.Context.AttemptID] != nil {
		t.Fatal(result)
	}
	x, _ = reopened.Get(a.Context.AttemptID)
	if x.Phase != PhaseReleased {
		t.Fatal(x.Phase)
	}
}
