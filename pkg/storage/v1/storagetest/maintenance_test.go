package storagetest

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// fakeMessages is a provider that refuses every new write while its dataset
// is migrating. With strict set it also refuses ConsumeMessages then, which
// the suite's case reads through here, as a provider that applies the
// dataset-state step to every method the catalog lists as a write does:
// ConsumeMessages is one because it marks the messages it returns as read. With leak set, a SendMessage refused while migrating
// is visible until the dataset is ready again, as with a provider that
// applied it to a copy the migration then replaced.
type fakeMessages struct {
	// The maintenance cases never bind the dataset.
	storagev1.DatasetBindingStore

	strict    bool
	leak      bool
	migrating bool
	messages  []storagev1.Message
	leaked    []storagev1.Message
	receipts  map[string]storagev1.OperationOutcome
	epoch     *storagev1.ControllerEpochMirrorState
	watermark int64
}

const fakeDataset = "dataset-1"

func fakeMaintenance(refusesReadingWrites bool) MaintenanceHarness {
	return MaintenanceHarness{
		Enter: func(_ *testing.T, p storagev1.ProviderDescriber) { p.(*fakeMessages).migrating = true },
		Exit: func(_ *testing.T, p storagev1.ProviderDescriber) {
			p.(*fakeMessages).migrating = false
			p.(*fakeMessages).leaked = nil
		},
		RefusesReadingWrites: refusesReadingWrites,
	}
}

func (p *fakeMessages) Describe(context.Context) (storagev1.ProviderInfo, error) {
	state := storagev1.DatasetReady
	if p.migrating {
		state = storagev1.DatasetMigrating
	}
	return storagev1.ProviderInfo{DatasetID: fakeDataset, State: state}, nil
}

func (p *fakeMessages) AdvanceControllerEpoch(
	_ context.Context, request storagev1.AdvanceControllerEpochRequest,
) (storagev1.ControllerEpochMirrorState, error) {
	p.epoch = &storagev1.ControllerEpochMirrorState{
		DatasetID: request.DatasetID, Epoch: request.Epoch, InstallationID: request.InstallationID,
	}
	return *p.epoch, nil
}

func (p *fakeMessages) GetControllerEpochMirror(context.Context, string) (storagev1.ControllerEpochMirrorState, error) {
	if p.epoch == nil {
		return storagev1.ControllerEpochMirrorState{}, storagev1.ErrNotFound
	}
	return *p.epoch, nil
}

func (p *fakeMessages) ResolveOperation(
	_ context.Context, request storagev1.ResolveOperationRequest,
) (storagev1.OperationOutcome, error) {
	if outcome, ok := p.receipts[request.OperationID]; ok {
		return outcome, nil
	}
	if p.migrating {
		return storagev1.OperationOutcome{}, storagev1.ErrNotReady
	}
	outcome := storagev1.OperationOutcome{
		OperationID: request.OperationID, Method: request.Method, RequestDigest: request.RequestDigest,
		State: storagev1.OperationAborted,
	}
	p.receipts[request.OperationID] = outcome
	return outcome, nil
}

func (p *fakeMessages) PruneOperationReceipts(context.Context, int) (int, error) { return 0, nil }

// write runs apply as a write: a replay returns its receipt's outcome
// without applying it again, and a new operation is refused while the
// dataset is migrating, after running leak when the provider leaks.
func (p *fakeMessages) write(ctx context.Context, method string, apply, leak func()) error {
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		return storagev1.ValidationErrorf("%s requires a write envelope", method)
	}
	if outcome, ok := p.receipts[envelope.OperationID]; ok {
		if outcome.State != storagev1.OperationCommitted {
			return storagev1.ErrOperationAborted
		}
		return nil
	}
	if p.migrating {
		if p.leak && leak != nil {
			leak()
		}
		return storagev1.ErrNotReady
	}
	apply()
	p.receipts[envelope.OperationID] = storagev1.OperationOutcome{
		OperationID: envelope.OperationID, Method: method, RequestDigest: envelope.RequestDigest,
		State: storagev1.OperationCommitted, Fence: envelope.Fence, IssuedAt: envelope.IssuedAt,
	}
	return nil
}

func (p *fakeMessages) SendMessage(ctx context.Context, message *storagev1.Message) error {
	return p.write(ctx, "SendMessage",
		func() { p.messages = append(p.messages, *message) },
		func() { p.leaked = append(p.leaked, *message) })
}

// ConsumeMessages returns every message a reader sees. A strict provider
// refuses it while the dataset is migrating, as the dataset-state step of a
// catalog write does.
func (p *fakeMessages) ConsumeMessages(context.Context, storagev1.ConsumeMessagesRequest) ([]storagev1.Message, error) {
	if p.strict && p.migrating {
		return nil, storagev1.ErrNotReady
	}
	return p.visible(), nil
}

// visible returns every message a reader sees, as a read would.
func (p *fakeMessages) visible() []storagev1.Message {
	return append(slices.Clone(p.messages), p.leaked...)
}

func (p *fakeMessages) AdvanceWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	err := p.write(ctx, "AdvanceWriteWatermark", func() { p.watermark++ }, nil)
	return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.watermark}, err
}

func (p *fakeMessages) GetWriteWatermark(context.Context) (storagev1.WriteWatermark, error) {
	return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.watermark}, nil
}

func newFakeMessages(strict, leak bool) func(*testing.T) *fakeMessages {
	return func(*testing.T) *fakeMessages {
		return &fakeMessages{strict: strict, leak: leak, receipts: map[string]storagev1.OperationOutcome{}}
	}
}

func observeFakeMessages(t *testing.T, p *fakeMessages) any {
	messages, err := p.ConsumeMessages(t.Context(), storagev1.ConsumeMessagesRequest{
		Namespace: "tenant", TaskName: "worker-b", ParentTask: "parent", Limit: storagev1.MaxMessageBatch,
	})
	require.NoError(t, err, "observe messages")
	return messages
}

func observeVisibleMessages(_ *testing.T, p *fakeMessages) any { return p.visible() }

// sendCase is a SendMessage write case observed through observe, which
// calls ConsumeMessages, a catalog write, when observeWrites is set.
func sendCase(observe, observeReads func(*testing.T, *fakeMessages) any, observeWrites bool) []writeCase[*fakeMessages] {
	return []writeCase[*fakeMessages]{{
		method: "SendMessage",
		args: func(storagev1.ControllerEpochFence) []any {
			return []any{&storagev1.Message{
				Namespace: "tenant", FromTask: "worker-a", ToTask: "worker-b", ParentTask: "parent", Content: "hello",
			}}
		},
		observe:       observe,
		observeWrites: observeWrites,
		observeReads:  observeReads,
	}}
}

// A provider that refuses ConsumeMessages while its dataset is migrating, and
// says so in its harness, passes: a case that observes through ConsumeMessages
// and has no read of its own is compared only once the dataset is ready.
func TestMaintenancePassesAProviderThatRefusesReadingWrites(t *testing.T) {
	runMaintenance(t, newFakeMessages(true, false), sendCase(observeFakeMessages, nil, true), fakeMaintenance(true))
}

const leakyMaintenanceEnv = "STORAGETEST_LEAKY_MAINTENANCE"

// The maintenance cases fail a provider whose refused write is visible while
// the dataset migrates, even though it is gone once the dataset is ready:
// through a case that observes through reads, through a case that observes
// through ConsumeMessages when the provider serves it while migrating, and
// through the reads of such a case when the provider refuses it.
func TestMaintenanceFailsAWriteVisibleWhileMigrating(t *testing.T) {
	variants := map[string]func(t *testing.T){
		"read observation": func(t *testing.T) {
			runMaintenance(t, newFakeMessages(false, true), sendCase(observeVisibleMessages, nil, false), fakeMaintenance(false))
		},
		"observation through a write the provider serves": func(t *testing.T) {
			runMaintenance(t, newFakeMessages(false, true), sendCase(observeFakeMessages, nil, true), fakeMaintenance(false))
		},
		"reads of an observation through a refused write": func(t *testing.T) {
			runMaintenance(t, newFakeMessages(true, true),
				sendCase(observeFakeMessages, observeVisibleMessages, true), fakeMaintenance(true))
		},
	}
	if variant := os.Getenv(leakyMaintenanceEnv); variant != "" {
		variants[variant](t)
		return
	}
	for name := range variants {
		t.Run(name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestMaintenanceFailsAWriteVisibleWhileMigrating$", "-test.count=1")
			command.Env = append(os.Environ(), leakyMaintenanceEnv+"="+name)
			output, err := command.CombinedOutput()
			require.Error(t, err, "the maintenance cases passed a provider whose refused write is visible while migrating:\n%s", output)
			require.Contains(t, string(output), "a refused write changed state")
		})
	}
}

// Every write case whose observation calls a method the catalog lists as a
// write sets observeWrites, and no other case does; a case's observeReads
// calls no catalog write, and only a case that sets observeWrites has one.
func TestObserveWritesMarksObservationsThroughCatalogWrites(t *testing.T) {
	writes := catalogWritesByFunction(t)
	calls := func(method string, observe any) []string {
		t.Helper()
		name := packageFunctionName(observe)
		called, known := writes[name]
		require.Truef(t, known, "the %s case observes through %s, which is not a package function", method, name)
		return called
	}
	check := func(method string, observe, observeReads any, observeWrites bool) {
		t.Helper()
		called := calls(method, observe)
		require.Equalf(t, len(called) > 0, observeWrites,
			"the %s case's observation calls catalog writes %v; observeWrites must be set exactly when it does", method, called)
		if !reflect.ValueOf(observeReads).IsNil() {
			require.Truef(t, observeWrites, "the %s case has reads to compare but no observation through writes", method)
			require.Emptyf(t, calls(method, observeReads), "the %s case's observeReads calls catalog writes", method)
		}
	}
	for _, c := range stateWriteCases {
		if c.observe != nil {
			check(c.method, c.observe, c.observeReads, c.observeWrites)
		}
	}
	for _, c := range artifactWriteCases {
		if c.observe != nil {
			check(c.method, c.observe, c.observeReads, c.observeWrites)
		}
	}
}

// packageFunctionName returns the package-level function fn is, or the one
// that returned it when fn is a closure.
func packageFunctionName(fn any) string {
	full := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	_, name, _ := strings.Cut(full[strings.LastIndex(full, "/")+1:], ".")
	if end := strings.IndexAny(name, ".["); end >= 0 {
		name = name[:end]
	}
	return name
}

// catalogWritesByFunction returns, for every package-level function of the
// suite, the catalog writes it calls on its provider, directly, in a closure
// it returns or through another package function it calls.
func catalogWritesByFunction(t *testing.T) map[string][]string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fileSet := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	direct := map[string][]string{}
	calls := map[string][]string{}
	for name, fn := range funcs {
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := call.Fun
			if index, ok := callee.(*ast.IndexExpr); ok {
				callee = index.X
			}
			switch fun := callee.(type) {
			case *ast.SelectorExpr:
				receiver, ok := fun.X.(*ast.Ident)
				if !ok || (receiver.Name != "p" && receiver.Name != "provider") {
					return true
				}
				if info, ok := storagev1.LookupMethod(fun.Sel.Name); ok && info.Kind == storagev1.MethodWrite {
					direct[name] = append(direct[name], fun.Sel.Name)
				}
			case *ast.Ident:
				if _, ok := funcs[fun.Name]; ok {
					calls[name] = append(calls[name], fun.Name)
				}
			}
			return true
		})
	}
	writes := map[string][]string{}
	for name := range funcs {
		seen := map[string]bool{}
		var found []string
		var reach func(string)
		reach = func(fn string) {
			if seen[fn] {
				return
			}
			seen[fn] = true
			found = append(found, direct[fn]...)
			for _, callee := range calls[fn] {
				reach(callee)
			}
		}
		reach(name)
		slices.Sort(found)
		writes[name] = slices.Compact(found)
	}
	return writes
}
