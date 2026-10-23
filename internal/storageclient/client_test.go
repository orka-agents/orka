package storageclient_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/tools"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Every application store interface that production wiring passes the
// storage client as, or type-asserts it for, must stay satisfied.
var (
	_ storagev1.StateProvider                         = (*storageclient.Client)(nil)
	_ storagev1.ArtifactProvider                      = (*storageclient.Client)(nil)
	_ store.ResultStore                               = (*storageclient.Client)(nil)
	_ store.PromptResultReceiptStore                  = (*storageclient.Client)(nil)
	_ store.SessionStore                              = (*storageclient.Client)(nil)
	_ store.SessionTurnCommitter                      = (*storageclient.Client)(nil)
	_ store.FencedSessionWriteStore                   = (*storageclient.Client)(nil)
	_ store.SessionSoulReader                         = (*storageclient.Client)(nil)
	_ store.SessionSoulWriter                         = (*storageclient.Client)(nil)
	_ store.SessionLineageStore                       = (*storageclient.Client)(nil)
	_ store.AgentExecutionSnapshotStore               = (*storageclient.Client)(nil)
	_ store.AgentExecutionSnapshotLifecycleStore      = (*storageclient.Client)(nil)
	_ store.NativeSessionImportStore                  = (*storageclient.Client)(nil)
	_ store.ConnectorCredentialStore                  = (*storageclient.Client)(nil)
	_ store.ConnectorConsentStore                     = (*storageclient.Client)(nil)
	_ store.MemoryStore                               = (*storageclient.Client)(nil)
	_ store.MemoryProposalStore                       = (*storageclient.Client)(nil)
	_ store.SecurityStore                             = (*storageclient.Client)(nil)
	_ store.RepositoryMonitorStore                    = (*storageclient.Client)(nil)
	_ tools.RepositoryValidationBindingStore          = (*storageclient.Client)(nil)
	_ store.UsageStore                                = (*storageclient.Client)(nil)
	_ store.MessageStore                              = (*storageclient.Client)(nil)
	_ store.PlanStore                                 = (*storageclient.Client)(nil)
	_ store.ArtifactStore                             = (*storageclient.Client)(nil)
	_ store.AtomicExecutionEventPlanStore             = (*storageclient.Client)(nil)
	_ store.GatewayEventStore                         = (*storageclient.Client)(nil)
	_ store.GatewayDeliveryStore                      = (*storageclient.Client)(nil)
	_ store.GatewayTaskCleanupReceiptStore            = (*storageclient.Client)(nil)
	_ store.GatewayTaskCleanupReceiptMaintenanceStore = (*storageclient.Client)(nil)
	_ store.GatewaySessionCleanupCandidateStore       = (*storageclient.Client)(nil)
	_ store.TaskDataTransactionStore                  = (*storageclient.Client)(nil)
	_ store.ControllerEpochMirror                     = (*storageclient.Client)(nil)
	_ kube.SQLitePersistence                          = (*storageclient.Client)(nil)
	_ store.HealthChecker                             = (*storageclient.Client)(nil)
	_ taskTokenUsageReader                            = (*storageclient.Client)(nil)
)

// taskTokenUsageReader is the controller's local view of Task token
// attribution, which it type-asserts on the client.
type taskTokenUsageReader interface {
	TaskTokenUsage(ctx context.Context, namespace, taskUID string) (int64, int64, error)
}

// sqliteOnlyMethods are SQLite store methods that production code does not
// reach through the application storage client: process lifecycle and key
// configuration owned by startup, a test helper, and the SQL duplicates of
// Kubernetes-authoritative control records. Production wiring uses the
// Kubernetes control store for those records (see the storage ownership doc).
var sqliteOnlyMethods = map[string]string{
	"Close":                           "lifecycle",
	"DatasetInfo":                     "lifecycle",
	"NeedLeaderElection":              "lifecycle",
	"SetAgentExecutionSnapshotCipher": "startup key configuration",
	"SetNativeSessionMaxBytes":        "startup configuration",
	"RequireWriteEnvelopes":           "startup configuration",
	"Start":                           "lifecycle",
	"AcquireLockUntil":                "SQLite test helper",

	"CompareAndSwapControllerEpoch":     "controller epoch control record",
	"GetControllerEpoch":                "controller epoch control record",
	"CreatePromptAttempt":               "prompt attempt control record",
	"GetPromptAttempt":                  "prompt attempt control record",
	"TransitionPromptAttemptDelivery":   "prompt attempt control record",
	"TransitionPromptAttemptExecution":  "prompt attempt control record",
	"RecoverPromptAttemptPreSubmission": "prompt attempt control record",
	"PreparePromptAttemptReclamation":   "prompt attempt control record",
	"ReclaimPromptAttempts":             "prompt attempt control record",
	"CreateSessionControl":              "session control record",
	"GetSessionControl":                 "session control record",
	"AcquireSessionMutationLease":       "session control record",
	"ReleaseSessionMutationLease":       "session control record",
	"CommitSessionRuntimeGeneration":    "session control record",
	"ReconcileSessionControl":           "session control record",
	"CreateSessionTurn":                 "session control record",
	"FinalizeSessionTurn":               "session control record",
	"ResumeSessionTurnFinalization":     "session control record",
	"CreateBranchClaim":                 "branch claim control record",
	"CreateBranchClaimWithResult":       "branch claim control record",
	"GetBranchClaim":                    "branch claim control record",
	"CompareAndSwapBranchClaim":         "branch claim control record",
	"ReclaimBranchClaim":                "branch claim control record",
	"CreatePublication":                 "publication control record",
	"GetPublication":                    "publication control record",
	"TransitionPublication":             "publication control record",
	"SetPublicationPRIntent":            "publication control record",
	"SetPublicationPRReceipt":           "publication control record",
	"ReserveExternalEffect":             "external effect control record",
	"GetExternalEffect":                 "external effect control record",
	"GetExternalEffectByIdentity":       "external effect control record",
	"TransitionExternalEffect":          "external effect control record",
	"EnqueueOutboxProjection":           "epoch-validating outbox control path",
	"ClaimOutboxProjections":            "epoch-validating outbox control path",
	"CompleteOutboxProjection":          "epoch-validating outbox control path",
}

func TestClientKeepsEveryApplicationMethodOfTheEmbeddedStore(t *testing.T) {
	clientType := reflect.TypeFor[*storageclient.Client]()
	sqliteType := reflect.TypeFor[*sqlite.Store]()
	var missing []string
	for method := range sqliteType.Methods() {
		if _, ok := sqliteOnlyMethods[method.Name]; ok {
			continue
		}
		if _, ok := clientType.MethodByName(method.Name); !ok {
			missing = append(missing, method.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("storage client lacks embedded store methods: %v", missing)
	}
}

func openSQLite(t *testing.T) *sqlite.Store {
	t.Helper()
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func TestNewRejectsMissingOrSplitEmbeddedStores(t *testing.T) {
	first := openSQLite(t)
	second := openSQLite(t)
	if _, err := storageclient.New(storageclient.Options{State: first, Artifacts: first}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("missing embedded store error = %v, want validation", err)
	}
	if _, err := storageclient.New(storageclient.Options{State: first, Artifacts: second, Embedded: first}); !errors.Is(err, storageclient.ErrSplitEmbeddedStore) {
		t.Fatalf("split artifact store error = %v, want %v", err, storageclient.ErrSplitEmbeddedStore)
	}
	if _, err := storageclient.New(storageclient.Options{State: second, Artifacts: first, Embedded: first}); !errors.Is(err, storageclient.ErrSplitEmbeddedStore) {
		t.Fatalf("split state store error = %v, want %v", err, storageclient.ErrSplitEmbeddedStore)
	}
	if _, err := storageclient.NewEmbedded(nil); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("nil embedded backend error = %v, want validation", err)
	}
}

func TestNewRejectsTypedNilProviders(t *testing.T) {
	var typedNil *sqlite.Store
	if _, err := storageclient.NewEmbedded(typedNil); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("typed-nil embedded backend error = %v, want validation", err)
	}
	if _, err := storageclient.New(storageclient.Options{State: typedNil, Artifacts: typedNil, Embedded: typedNil}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("typed-nil providers error = %v, want validation", err)
	}
	valid := openSQLite(t)
	for name, options := range map[string]storageclient.Options{
		"state":     {State: typedNil, Artifacts: valid, Embedded: valid},
		"artifacts": {State: valid, Artifacts: typedNil, Embedded: valid},
		"embedded":  {State: valid, Artifacts: valid, Embedded: typedNil},
	} {
		if _, err := storageclient.New(options); !errors.Is(err, store.ErrValidation) {
			t.Fatalf("typed-nil %s provider error = %v, want validation", name, err)
		}
	}
}

// uncomparableStore is a value-typed provider whose dynamic type cannot be
// compared with ==.
type uncomparableStore struct {
	*sqlite.Store
	labels map[string]string
}

// wrappedStore has a comparable type, so whether == panics depends on the
// provider it wraps.
type wrappedStore struct {
	storageclient.EmbeddedStore
}

// taggedStore has a comparable type, so whether == panics depends on the
// value held in tag.
type taggedStore struct {
	*sqlite.Store
	tag any
}

func TestNewRejectsProvidersItCannotProveAreOneStore(t *testing.T) {
	for name, backend := range map[string]storageclient.EmbeddedStore{
		"uncomparable type": uncomparableStore{Store: openSQLite(t), labels: map[string]string{}},
		"comparable wrapper around uncomparable provider": wrappedStore{
			EmbeddedStore: uncomparableStore{Store: openSQLite(t), labels: map[string]string{}},
		},
		"interface field holding a map": taggedStore{Store: openSQLite(t), tag: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := storageclient.NewEmbedded(backend)
			if !errors.Is(err, storageclient.ErrSplitEmbeddedStore) {
				t.Fatalf("NewEmbedded error = %v, want %v", err, storageclient.ErrSplitEmbeddedStore)
			}
		})
	}
}

func TestNewAcceptsComparableValueProviders(t *testing.T) {
	for name, backend := range map[string]storageclient.EmbeddedStore{
		"wrapper around a store pointer":   wrappedStore{EmbeddedStore: openSQLite(t)},
		"interface field holding a string": taggedStore{Store: openSQLite(t), tag: "primary"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := storageclient.NewEmbedded(backend); err != nil {
				t.Fatalf("NewEmbedded error = %v, want nil", err)
			}
		})
	}
}

func TestClientRoutesCallsToTheEmbeddedStore(t *testing.T) {
	backend := openSQLite(t)
	backend.RequireWriteEnvelopes()
	client := storageclienttest.NewEmbedded(t, backend)
	ctx := context.Background()
	if err := client.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if err := client.SaveResult(ctx, "tenant", "task", []byte("result")); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	got, err := backend.GetResult(ctx, "tenant", "task")
	if err != nil || string(got) != "result" {
		t.Fatalf("backend result = %q, %v", got, err)
	}
	if err := client.SaveArtifact(ctx, store.SaveArtifactRequest{Owner: store.ArtifactOwner{Namespace: "tenant", TaskName: "task"}, Filename: "a.txt", ContentType: "text/plain", Data: []byte("artifact")}); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	data, contentType, err := backend.GetArtifact(ctx, store.ArtifactKey{Owner: store.ArtifactOwner{Namespace: "tenant", TaskName: "task"}, Filename: "a.txt"})
	if err != nil || string(data) != "artifact" || contentType != "text/plain" {
		t.Fatalf("backend artifact = %q, %q, %v", data, contentType, err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := client.HealthCheck(ctx); err == nil {
		t.Fatal("HealthCheck after close succeeded")
	}
}
