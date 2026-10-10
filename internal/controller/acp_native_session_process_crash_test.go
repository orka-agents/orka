//go:build !windows

package controller

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const nativeProcessCrashChildEnv = "ORKA_NATIVE_PROCESS_CRASH_CHILD"

// Only this store call is intercepted. The child has already received the
// runtime's capture receipt and persisted its capture intent to the API server.
// It acknowledges the boundary over a pipe and cannot unwind or close SQLite.
// The parent must terminate it with SIGKILL to release this barrier.
type nativeProcessCrashStore struct {
	store.DurableControlStore
	boundary string
	signal   func(string)
}

func (s *nativeProcessCrashStore) FinalizeSessionTurn(ctx context.Context, request store.FinalizeSessionTurnRequest) (*store.SessionTurn, error) {
	crash := request.NativeSession != nil && request.Key.PromptID == "prompt-turn"
	if crash && s.boundary == "capture-before-commit" {
		s.signal(s.boundary)
		select {}
	}
	turn, err := s.DurableControlStore.FinalizeSessionTurn(ctx, request)
	if crash && err == nil && s.boundary == "commit-before-delete" {
		s.signal(s.boundary)
		select {}
	}
	return turn, err
}

type nativeProcessCrashConfig struct {
	Kubeconfig string
	Database   string
	Namespace  string
	PoolName   string
	Boundary   string
	Recover    bool
}

type nativeProcessCrashAck struct {
	Stage string `json:"stage"`
	PID   int    `json:"pid"`
	Epoch int64  `json:"epoch,omitempty"`
}

func openNativeProcessCrashPersistence(path string) (*sqlite.Store, *sql.DB, error) {
	db, err := sqlite.NewDB(path)
	if err != nil {
		return nil, nil, err
	}
	persistence := sqlite.NewStore(db, "native-process-crash")
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x61}, sqlite.AgentExecutionSnapshotKeyBytes))
	if err == nil {
		err = persistence.SetAgentExecutionSnapshotCipher(cipher)
	}
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return persistence, db, nil
}

func nativeProcessCrashPersistence(t *testing.T, path string) (*sqlite.Store, *sql.DB) {
	t.Helper()
	persistence, db, err := openNativeProcessCrashPersistence(path)
	require.NoError(t, err)
	return persistence, db
}

func nativeProcessCrashScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return scheme
}

// This entry point runs in the current test executable, never in the parent
// process. Its runtime server and Kubernetes API server belong to the parent.
func TestACPDispatcherNativeProcessCrashChild(t *testing.T) {
	configPath := os.Getenv(nativeProcessCrashChildEnv)
	if configPath == "" {
		return
	}
	body, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var config nativeProcessCrashConfig
	require.NoError(t, json.Unmarshal(body, &config))
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	apiConfig, err := nativeProcessCrashRESTConfig(config.Kubeconfig)
	require.NoError(t, err)
	kube, err := client.New(apiConfig, client.Options{Scheme: nativeProcessCrashScheme(t)})
	require.NoError(t, err)
	persistence, db := nativeProcessCrashPersistence(t, config.Database)
	defer func() { require.NoError(t, db.Close()) }()
	events := json.NewEncoder(os.NewFile(3, "crash-acknowledgements"))
	releases := bufio.NewScanner(os.NewFile(4, "parent-releases"))
	var epoch int64
	signal := func(stage string) {
		require.NoError(t, events.Encode(nativeProcessCrashAck{Stage: stage, PID: os.Getpid(), Epoch: epoch}))
	}
	awaitRelease := func() {
		require.True(t, releases.Scan(), "parent must explicitly release the acknowledged barrier")
		require.Equal(t, "continue", releases.Text())
	}
	epochs := NewControllerEpochManager(persistence, fmt.Sprintf("process-crash-%d", os.Getpid()))
	epochCtx, stopEpoch := context.WithCancel(ctx)
	defer stopEpoch()
	go func() { _ = epochs.Start(epochCtx) }()
	fence, err := epochs.CurrentFence(ctx)
	require.NoError(t, err)
	epoch = fence.Epoch
	controls := store.DurableControlStore(persistence)
	if !config.Recover {
		controls = &nativeProcessCrashStore{DurableControlStore: persistence, boundary: config.Boundary, signal: signal}
	}
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controls, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
		NewSessionUID: func() (string, error) { return "native-process-crash-owner", nil },
	})
	require.NoError(t, err)
	dispatcher := &ACPDispatcher{
		Client: kube, APIReader: kube, Store: controls, ResultStore: persistence,
		EventStore: persistence, PlanStore: persistence, Snapshots: persistence, Sessions: continuity, Epochs: epochs,
	}
	if config.Recover {
		task := &corev1alpha1.Task{}
		require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: "turn"}, task))
		// A surviving runtime still fenced to the dead leader must not settle or
		// delete anything. The parent observes this before advancing its fixture.
		err = dispatcher.recoverStaleTask(ctx, task, fence)
		require.ErrorIs(t, err, store.ErrNotReady)
		signal("old-epoch-fail-closed")
		awaitRelease()
		require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(task), task))
		require.NoError(t, dispatcher.recoverStaleTask(ctx, task, fence))
		signal("recovered")
		return
	}
	for _, name := range []string{"prior", "turn"} {
		task := &corev1alpha1.Task{}
		require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: name}, task))
		agent := &corev1alpha1.Agent{}
		require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: "agent"}, agent))
		task = prepareBoundACPDispatcherTaskForTest(t, ctx, kube, kube.Scheme(), persistence, task, agent,
			ACPRuntimeImages{Codex: "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)})
		key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}
		id, err := key.CanonicalID()
		require.NoError(t, err)
		_, err = persistence.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
			ID: id, Key: key, RequestDigest: task.Status.Execution.RequestDigest,
			BindingDigest: task.Status.AgentExecutionBinding.BindingDigest, SnapshotDigest: task.Status.AgentExecutionBinding.Snapshot.Digest,
			ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
		}), fence)
		require.NoError(t, err)
		dispatchQueuedTask(ctx, t, dispatcher, task)
		if name == "prior" {
			signal("prior-checkpoint")
			awaitRelease()
		}
	}
	t.Fatal("crash boundary returned instead of blocking until SIGKILL")
}

type nativeProcessCrashProcess struct {
	cmd      *exec.Cmd
	acks     <-chan nativeProcessCrashAck
	releases *os.File
	done     <-chan error
}

func startNativeProcessCrashChild(t *testing.T, config nativeProcessCrashConfig, logDir string) *nativeProcessCrashProcess {
	t.Helper()
	body, err := json.Marshal(config)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "child.json")
	require.NoError(t, os.WriteFile(configPath, body, 0o600))
	eventRead, eventWrite, err := os.Pipe()
	require.NoError(t, err)
	releaseRead, releaseWrite, err := os.Pipe()
	require.NoError(t, err)
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(binary, "-test.run=^TestACPDispatcherNativeProcessCrashChild$", "-test.v", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), nativeProcessCrashChildEnv+"="+configPath)
	cmd.ExtraFiles = []*os.File{eventWrite, releaseRead}
	logName := config.Boundary
	if config.Recover {
		logName += "-recovery"
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, logName+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	require.NoError(t, eventWrite.Close())
	require.NoError(t, releaseRead.Close())
	acks := make(chan nativeProcessCrashAck, 4)
	go func() {
		defer close(acks)
		decoder := json.NewDecoder(eventRead)
		for {
			var ack nativeProcessCrashAck
			if decoder.Decode(&ack) != nil {
				return
			}
			acks <- ack
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = releaseWrite.Close()
		_ = eventRead.Close()
		_ = logFile.Close()
	})
	return &nativeProcessCrashProcess{cmd: cmd, acks: acks, releases: releaseWrite, done: done}
}

func (p *nativeProcessCrashProcess) await(t *testing.T, stage string) nativeProcessCrashAck {
	t.Helper()
	select {
	case ack, ok := <-p.acks:
		require.True(t, ok, "child exited before %s; inspect subprocess log", stage)
		require.Equal(t, stage, ack.Stage)
		require.Equal(t, p.cmd.Process.Pid, ack.PID)
		return ack
	case <-time.After(60 * time.Second):
		t.Fatalf("child did not acknowledge %s", stage)
		return nativeProcessCrashAck{}
	}
}

func (p *nativeProcessCrashProcess) release(t *testing.T) {
	t.Helper()
	_, err := io.WriteString(p.releases, "continue\n")
	require.NoError(t, err)
}

// Each captured response is retained by the parent, keyed by the immutable
// operation ID and digest. Reconciliation returns that receipt, never recaptures.
type nativeProcessCrashRuntime struct {
	mu         sync.Mutex
	epoch      atomic.Int64
	creates    []harnessv2.CreateRuntimeSessionRequest
	prompts    []harnessv2.StartPromptRequest
	captures   []harnessv2.CaptureNativeSessionRequest
	reconciles []harnessv2.CaptureNativeSessionRequest
	receipts   map[harnessv2.OperationID]harnessv2.CaptureNativeSessionResponse
	deletes    []harnessv2.DeleteRuntimeSessionRequest
	atDelete   []store.NativeSessionRecord
}

func newNativeProcessCrashRuntime(t *testing.T, config nativeProcessCrashConfig, plan ACPRuntimePlan, poolUID string,
	prior, completed harnessv2.NativeSessionSnapshot) (*httptest.Server, *nativeProcessCrashRuntime) {
	t.Helper()
	observed := &nativeProcessCrashRuntime{receipts: make(map[harnessv2.OperationID]harnessv2.CaptureNativeSessionResponse)}
	observed.epoch.Store(1)
	snapshot := prior
	base := newDispatcherRuntimeServerForPoolWithOptions(t, plan.Profile, plan.Digest, poolUID, dispatcherRuntimeServerOptions{
		nativeSnapshot: &snapshot,
		onPrompt: func(request harnessv2.StartPromptRequest) {
			observed.mu.Lock()
			defer observed.mu.Unlock()
			observed.prompts = append(observed.prompts, request)
		},
		onDelete: func(request harnessv2.DeleteRuntimeSessionRequest) {
			persistence, db, err := openNativeProcessCrashPersistence(config.Database)
			if err != nil {
				t.Errorf("open checkpoint at DELETE: %v", err)
				return
			}
			defer func() { _ = db.Close() }()
			control, err := persistence.GetSessionControl(t.Context(), config.Namespace, "conversation")
			if err != nil {
				t.Errorf("read Session control at DELETE: %v", err)
				return
			}
			native, err := persistence.GetNativeSession(t.Context(), config.Namespace, "conversation", control.SessionUID)
			if err != nil {
				t.Errorf("DELETE requires the committed checkpoint: %v", err)
				return
			}
			observed.mu.Lock()
			defer observed.mu.Unlock()
			observed.deletes = append(observed.deletes, request)
			observed.atDelete = append(observed.atDelete, *native)
		},
	}, func(request harnessv2.CreateRuntimeSessionRequest) {
		persistence, db, err := openNativeProcessCrashPersistence(config.Database)
		if err != nil {
			t.Errorf("open Session cleanup identity: %v", err)
			return
		}
		defer func() { _ = db.Close() }()
		if err := persistence.BindSessionCleanupIdentity(t.Context(), config.Namespace, "conversation", string(request.Metadata.Fence.RuntimeSessionUID)); err != nil {
			t.Errorf("bind Session cleanup identity: %v", err)
			return
		}
		observed.mu.Lock()
		defer observed.mu.Unlock()
		observed.creates = append(observed.creates, request)
	})
	t.Cleanup(base.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath {
			response := httptest.NewRecorder()
			base.Config.Handler.ServeHTTP(response, r)
			var status harnessv2.StatusResponse
			if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
				t.Errorf("decode scripted status: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			status.Fence.ControllerEpoch = uint64(observed.epoch.Load())
			writeDispatcherJSON(w, status)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/native-session") {
			base.Config.Handler.ServeHTTP(w, r)
			return
		}
		var request harnessv2.CaptureNativeSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode scripted capture: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		observed.mu.Lock()
		defer observed.mu.Unlock()
		if int64(request.Metadata.Fence.ControllerEpoch) != observed.epoch.Load() {
			t.Errorf("capture used stale runtime authority")
			w.WriteHeader(http.StatusConflict)
			return
		}
		if request.OriginalOperationID != "" {
			observed.reconciles = append(observed.reconciles, request)
			var original *harnessv2.CaptureNativeSessionRequest
			for index := range observed.captures {
				if observed.captures[index].Metadata.OperationID == request.OriginalOperationID {
					original = &observed.captures[index]
				}
			}
			if original == nil || original.Metadata.RequestDigest != request.OriginalRequestDigest ||
				original.Metadata.Fence.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID ||
				original.Metadata.Fence.RuntimeSessionGeneration != request.Metadata.Fence.RuntimeSessionGeneration {
				t.Errorf("reconciliation must identify the exact immutable capture receipt")
				w.WriteHeader(http.StatusConflict)
				return
			}
			receipt := observed.receipts[request.OriginalOperationID]
			receipt.Classification = harnessv2.Classification{Class: harnessv2.RequestClassificationDuplicate, Phase: harnessv2.OperationPhaseApplied}
			writeDispatcherJSON(w, receipt)
			return
		}
		if observed.receipts[request.Metadata.OperationID].Protocol != "" {
			t.Errorf("fresh capture may execute only once")
			w.WriteHeader(http.StatusConflict)
			return
		}
		if len(observed.captures) > 0 {
			snapshot = completed
		}
		body, err := json.Marshal(request)
		if err != nil {
			t.Errorf("encode scripted capture: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		response := httptest.NewRecorder()
		base.Config.Handler.ServeHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Errorf("scripted capture returned HTTP %d", response.Code)
			w.WriteHeader(response.Code)
			return
		}
		var receipt harnessv2.CaptureNativeSessionResponse
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Errorf("decode scripted capture receipt: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		observed.captures = append(observed.captures, request)
		observed.receipts[request.Metadata.OperationID] = receipt
		writeDispatcherJSON(w, receipt)
	}))
	t.Cleanup(server.Close)
	return server, observed
}

// Both the parent and killed/replacement processes must connect directly to
// the same local API. A loopback server URL alone does not exclude a proxy.
func nativeProcessCrashRESTConfig(path string) (*rest.Config, error) {
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	if config.Proxy != nil {
		return nil, fmt.Errorf("crash-test kubeconfig proxies are not allowed")
	}
	target, err := url.Parse(config.Host)
	if err != nil {
		return nil, err
	}
	if host := target.Hostname(); host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, fmt.Errorf("crash-test API must be local to the test host")
	}
	// Do not fall back to ambient HTTP(S)_PROXY settings in client-go.
	config.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	return config, nil
}

func TestNativeProcessCrashRESTConfig(t *testing.T) {
	for _, test := range []struct {
		name, server, proxy string
		wantError           bool
	}{
		{name: "ipv4", server: "https://127.0.0.1:6443"},
		{name: "localhost", server: "https://localhost:6443"},
		{name: "ipv6", server: "https://[::1]:6443"},
		{name: "nonlocal", server: "https://cluster.example:6443", wantError: true},
		{name: "loopback-with-proxy", server: "https://127.0.0.1:6443", proxy: "socks5://proxy.example:1080", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			selected := clientcmdapi.Config{
				Clusters:       map[string]*clientcmdapi.Cluster{"api": {Server: test.server, ProxyURL: test.proxy}},
				Contexts:       map[string]*clientcmdapi.Context{"kind-crash-test": {Cluster: "api"}},
				CurrentContext: "kind-crash-test",
			}
			require.NoError(t, clientcmd.WriteToFile(selected, path))
			config, err := nativeProcessCrashRESTConfig(path)
			if test.wantError {
				require.Error(t, err)
				require.Nil(t, config)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, config.Proxy, "ambient proxies must be bypassed")
			request, err := http.NewRequest(http.MethodGet, test.server, nil)
			require.NoError(t, err)
			proxy, err := config.Proxy(request)
			require.NoError(t, err)
			require.Nil(t, proxy)
		})
	}
}

func nativeProcessCrashAPI(t *testing.T) (client.Client, string) {
	t.Helper()
	path := os.Getenv("ORKA_NATIVE_CRASH_KUBECONFIG")
	var config *rest.Config
	var err error
	if path != "" {
		selected, loadErr := clientcmd.LoadFromFile(path)
		require.NoError(t, loadErr)
		require.True(t, strings.HasPrefix(selected.CurrentContext, "kind-"), "external crash tests require a disposable kind context")
		config, err = nativeProcessCrashRESTConfig(path)
		require.NoError(t, err)
		t.Log("using supplied isolated Kubernetes API; no CRDs or cluster configuration are changed")
	} else {
		environment := &envtest.Environment{
			CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
		}
		config, err = environment.Start()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, environment.Stop()) })
		path = filepath.Join(t.TempDir(), "envtest.kubeconfig")
		kubeconfig := clientcmdapi.Config{
			Clusters:  map[string]*clientcmdapi.Cluster{"api": {Server: config.Host, CertificateAuthorityData: config.CAData}},
			AuthInfos: map[string]*clientcmdapi.AuthInfo{"controller": {ClientCertificateData: config.CertData, ClientKeyData: config.KeyData}},
			Contexts:  map[string]*clientcmdapi.Context{"test": {Cluster: "api", AuthInfo: "controller"}}, CurrentContext: "test",
		}
		require.NoError(t, clientcmd.WriteToFile(kubeconfig, path))
		require.NoError(t, os.Chmod(path, 0o600))
	}
	kube, err := client.New(config, client.Options{Scheme: nativeProcessCrashScheme(t)})
	require.NoError(t, err)
	return kube, path
}

func nativeProcessCrashCheckpoint(t *testing.T, config nativeProcessCrashConfig) *store.NativeSessionRecord {
	t.Helper()
	persistence, db := nativeProcessCrashPersistence(t, config.Database)
	defer func() { require.NoError(t, db.Close()) }()
	control, err := persistence.GetSessionControl(t.Context(), config.Namespace, "conversation")
	require.NoError(t, err)
	checkpoint, err := persistence.GetNativeSession(t.Context(), config.Namespace, "conversation", control.SessionUID)
	require.NoError(t, err)
	history, err := persistence.LoadTranscript(t.Context(), config.Namespace, "conversation", 100)
	require.NoError(t, err)
	require.Len(t, history, checkpoint.MessageCount)
	require.Equal(t, history[len(history)-1].ID, checkpoint.ThroughMessageID)
	return checkpoint
}

func assertNativeProcessCrashSettlement(t *testing.T, config nativeProcessCrashConfig, task *corev1alpha1.Task, committed bool) {
	t.Helper()
	persistence, db := nativeProcessCrashPersistence(t, config.Database)
	defer func() { require.NoError(t, db.Close()) }()
	attemptID, err := promptAttemptIDFromTask(task)
	require.NoError(t, err)
	turn := nativeRecoveryTurn(t, t.Context(), persistence, attemptID)
	control, err := persistence.GetSessionControl(t.Context(), config.Namespace, "conversation")
	require.NoError(t, err)
	if committed {
		require.Equal(t, store.SessionTurnFinalized, turn.State)
		require.Equal(t, store.SessionTurnAssistantResult, turn.TerminalKind)
		require.NotEmpty(t, turn.NativeSessionDigest)
		require.NotEmpty(t, turn.TerminalContent)
		require.Nil(t, control.Lease)
	} else {
		require.Equal(t, store.SessionTurnOpen, turn.State)
		require.Empty(t, turn.NativeSessionDigest)
		require.Empty(t, turn.TerminalContent)
		require.NotNil(t, control.Lease, "the unsettled turn must fence out newer Tasks")
	}
}

func TestACPDispatcherNativeSessionProcessCrash(t *testing.T) {
	kube, kubeconfig := nativeProcessCrashAPI(t)
	for _, boundary := range []string{"capture-before-commit", "commit-before-delete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := t.Context()
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "native-process-crash-"}}
			require.NoError(t, kube.Create(ctx, namespace))
			t.Cleanup(func() {
				// Only resources created in this test's unique namespace are removed.
				tasks := &corev1alpha1.TaskList{}
				require.NoError(t, kube.List(context.Background(), tasks, client.InNamespace(namespace.Name)))
				for index := range tasks.Items {
					task := &tasks.Items[index]
					task.Finalizers = nil
					require.NoError(t, kube.Update(context.Background(), task))
				}
				require.NoError(t, kube.Delete(context.Background(), namespace))
			})
			cache := filepath.Join(os.Getenv("HOME"), ".cache", "orka-pr732-crash")
			require.NoError(t, os.MkdirAll(cache, 0o700))
			logDir, err := os.MkdirTemp(cache, "controller-")
			require.NoError(t, err)
			t.Logf("subprocess logs: %s", logDir)
			config := nativeProcessCrashConfig{
				Kubeconfig: kubeconfig, Database: filepath.Join(t.TempDir(), "controller.db"), Namespace: namespace.Name, Boundary: boundary,
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: "agent"},
				Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Name: acpTestModel}, Runtime: &corev1alpha1.AgentCLIRuntime{
					Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
				}},
			}
			require.NoError(t, kube.Create(ctx, agent))
			priorTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: "prior"}, Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent, Prompt: "prior prompt", AgentRuntime: &corev1alpha1.AgentRuntimeSpec{},
				AgentRef: &corev1alpha1.AgentReference{Name: agent.Name}, SessionRef: &corev1alpha1.SessionReference{Name: "conversation", Create: true, Append: true},
			}}
			plan := frozenACPDispatcherPlanForTest(t, priorTask, agent, ACPRuntimeImages{Codex: "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)})
			config.PoolName = plan.PoolName
			pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: plan.PoolName}, Spec: corev1alpha1.RuntimePoolSpec{
				TrustDomain:      corev1alpha1.RuntimePoolTrustDomain{Namespace: namespace.Name, Identity: namespace.Name + "/default"},
				RuntimeNamespace: namespace.Name, Runtime: corev1alpha1.RuntimePoolRuntimeSpec{Image: plan.Image, Profile: RuntimePoolProfileFromPlan(plan)}, DesiredReplicas: 1,
			}}
			require.NoError(t, kube.Create(ctx, pool))
			priorSnapshot := storetest.NativeSessionSnapshot(t, "native state before the killed turn")
			completedSnapshot := storetest.NativeSessionSnapshot(t, "native state after the killed turn")
			priorSnapshot.WorkingDirectory, completedSnapshot.WorkingDirectory = "/workspace", "/workspace"
			server, observed := newNativeProcessCrashRuntime(t, config, plan, string(pool.UID), priorSnapshot, completedSnapshot)
			endpoint, err := url.Parse(server.URL)
			require.NoError(t, err)
			pool.Status = corev1alpha1.RuntimePoolStatus{
				Lifecycle: corev1alpha1.RuntimePoolLifecycleServing, AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
				ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
					PodNamespace: namespace.Name, PodName: "runtime-pod", PodUID: "pod-uid", PodAddress: endpoint.Host,
					BootID: "boot-id", RuntimeInstanceID: "pod-uid.boot-id", ControllerEpoch: 1, ProviderTokenGeneration: strings.Repeat("a", 16),
					ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(plan.Digest),
					ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
				},
			}
			require.NoError(t, kube.Status().Update(ctx, pool))
			for _, epoch := range []int64{1, 2} {
				require.NoError(t, kube.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: fmt.Sprintf("pool-auth-e%d", epoch), Labels: map[string]string{
						runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
					}},
					Data: map[string][]byte{runtimePoolControllerTokenKey: bytes.Repeat([]byte{'t'}, 32), runtimePoolCapabilitySecretKey: bytes.Repeat([]byte{'s'}, 32)},
				}))
			}
			for _, name := range []string{"prior", "turn"} {
				task := priorTask.DeepCopy()
				task.Name, task.Spec.Prompt = name, name+" prompt"
				task.Labels = map[string]string{acpRuntimeTaskPoolLabel: plan.PoolName}
				require.NoError(t, kube.Create(ctx, task))
				task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
					State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: "prompt-" + name,
					RuntimePoolName: plan.PoolName, RuntimePoolUID: string(pool.UID), RequestDigest: testControlDigestForDispatcher(name), ControllerEpoch: 1,
				}}
				require.NoError(t, kube.Status().Update(ctx, task))
			}
			victim := startNativeProcessCrashChild(t, config, logDir)
			first := victim.await(t, "prior-checkpoint")
			require.Equal(t, int64(1), first.Epoch)
			prior := nativeProcessCrashCheckpoint(t, config)
			require.Equal(t, priorSnapshot.Data, prior.Snapshot.Data)
			require.Equal(t, 2, prior.MessageCount)
			victim.release(t)
			victim.await(t, boundary)
			current := &corev1alpha1.Task{}
			require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: "turn"}, current))
			require.NotEmpty(t, current.Annotations[nativeCaptureIntentAnnotation], "capture intent must survive in the real API")
			require.True(t, current.Status.Execution.RuntimeSessionRecreationPending)
			var intent nativeCaptureIntent
			require.NoError(t, json.Unmarshal([]byte(current.Annotations[nativeCaptureIntentAnnotation]), &intent))
			observed.mu.Lock()
			require.Len(t, observed.captures, 2)
			require.Len(t, observed.prompts, 2)
			require.Len(t, observed.deletes, 1, "the killed turn cannot delete before commit is returned")
			receipt := observed.receipts[intent.Request.Metadata.OperationID]
			require.Equal(t, intent.Request, observed.captures[1])
			require.Equal(t, completedSnapshot.Data, receipt.Snapshot.Data)
			require.Equal(t, completedSnapshot.ProviderSessionID, receipt.Snapshot.ProviderSessionID)
			observed.mu.Unlock()
			// No injected failure, cancellation or graceful close reaches the child.
			require.NoError(t, victim.cmd.Process.Signal(syscall.SIGKILL))
			select {
			case waitErr := <-victim.done:
				exit, ok := waitErr.(*exec.ExitError)
				require.True(t, ok, "victim must die, not return from the test")
				status, ok := exit.Sys().(syscall.WaitStatus)
				require.True(t, ok)
				require.True(t, status.Signaled())
				require.Equal(t, syscall.SIGKILL, status.Signal())
				t.Logf("controller PID %d terminated by SIGKILL at %s", first.PID, boundary)
			case <-time.After(10 * time.Second):
				t.Fatal("SIGKILL did not terminate controller child")
			}
			apiBeforeKill := current.DeepCopy()
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(current), current))
			require.Equal(t, apiBeforeKill.UID, current.UID)
			require.Equal(t, apiBeforeKill.ResourceVersion, current.ResourceVersion, "API state survives process death without reconstruction")
			require.Equal(t, apiBeforeKill.Status, current.Status)
			require.Equal(t, apiBeforeKill.Annotations, current.Annotations)
			afterKill := nativeProcessCrashCheckpoint(t, config)
			assertNativeProcessCrashSettlement(t, config, current, boundary == "commit-before-delete")
			if boundary == "capture-before-commit" {
				require.Equal(t, prior, afterKill, "uncommitted capture must not damage the prior checkpoint")
			} else {
				require.Equal(t, 4, afterKill.MessageCount)
				require.Equal(t, receipt.Snapshot, afterKill.Snapshot)
				require.Equal(t, string(intent.Request.Metadata.OperationID), afterKill.SourceOperationID)
			}
			config.Recover = true
			restarted := startNativeProcessCrashChild(t, config, logDir)
			restart := restarted.await(t, "old-epoch-fail-closed")
			require.NotEqual(t, first.PID, restart.PID, "recovery requires a new OS process")
			require.Equal(t, int64(2), restart.Epoch)
			require.Equal(t, afterKill, nativeProcessCrashCheckpoint(t, config))
			assertNativeProcessCrashSettlement(t, config, current, boundary == "commit-before-delete")
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(current), current))
			require.Equal(t, apiBeforeKill.ResourceVersion, current.ResourceVersion)
			observed.mu.Lock()
			require.Len(t, observed.deletes, 1)
			require.Len(t, observed.captures, 2)
			require.Len(t, observed.prompts, 2)
			observed.mu.Unlock()
			// The surviving scripted runtime acknowledges the new control epoch.
			// No Task status, capture intent or receipt is reconstructed here.
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(pool), pool))
			pool.Status.ActiveInstance.ControllerEpoch = restart.Epoch
			require.NoError(t, kube.Status().Update(ctx, pool))
			observed.epoch.Store(restart.Epoch)
			restarted.release(t)
			restarted.await(t, "recovered")
			require.NoError(t, <-restarted.done)
			checkpoint := nativeProcessCrashCheckpoint(t, config)
			require.Equal(t, receipt.Snapshot, checkpoint.Snapshot)
			require.Equal(t, 4, checkpoint.MessageCount)
			require.NotEqual(t, prior.ThroughMessageID, checkpoint.ThroughMessageID)
			require.Equal(t, int64(intent.Request.Metadata.Fence.RuntimeSessionGeneration), checkpoint.RuntimeSessionGeneration)
			require.Equal(t, string(intent.Request.Metadata.OperationID), checkpoint.SourceOperationID)
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(current), current))
			require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.Phase)
			require.True(t, taskScopedRuntimeSessionCleanupComplete(current))
			persistence, db := nativeProcessCrashPersistence(t, config.Database)
			defer func() { require.NoError(t, db.Close()) }()
			attemptID, err := promptAttemptIDFromTask(current)
			require.NoError(t, err)
			turn := nativeRecoveryTurn(t, ctx, persistence, attemptID)
			require.Equal(t, store.SessionTurnFinalized, turn.State)
			require.Equal(t, store.NativeSessionCaptureDigest(checkpoint), turn.NativeSessionDigest)
			control, err := persistence.GetSessionControl(ctx, config.Namespace, "conversation")
			require.NoError(t, err)
			require.Nil(t, control.Lease)
			requireNativeRecoveryNoFallback(t, ctx, persistence, current)
			observed.mu.Lock()
			defer observed.mu.Unlock()
			require.Len(t, observed.creates, 2, "recovery must not allocate another provider thread")
			require.Len(t, observed.prompts, 2, "recovery must not replay the accepted prompt")
			require.Len(t, observed.prompts[1].Input.Content, 1, "native continuation must not replay canonical history")
			require.Equal(t, "turn prompt", observed.prompts[1].Input.Content[0].Text)
			require.NotNil(t, observed.creates[1].NativeRestore)
			require.Equal(t, prior.Snapshot.Data, observed.creates[1].NativeRestore.Snapshot.Data)
			require.Equal(t, prior.Snapshot.ProviderSessionID, observed.creates[1].NativeRestore.Snapshot.ProviderSessionID)
			require.Greater(t, observed.creates[1].Metadata.Fence.RuntimeSessionGeneration, observed.creates[0].Metadata.Fence.RuntimeSessionGeneration)
			require.Len(t, observed.captures, 2, "receipt recovery must not recapture")
			if boundary == "capture-before-commit" {
				require.Len(t, observed.reconciles, 1)
				require.Equal(t, intent.Request.Metadata.OperationID, observed.reconciles[0].OriginalOperationID)
			} else {
				require.Empty(t, observed.reconciles, "committed checkpoint does not need runtime recapture or receipt recovery")
				require.Equal(t, afterKill, checkpoint)
			}
			require.Equal(t, receipt, observed.receipts[intent.Request.Metadata.OperationID], "immutable runtime receipt must survive both controller processes")
			require.Len(t, observed.deletes, 2)
			deleted := observed.deletes[1].Metadata.Fence
			require.Equal(t, restart.Epoch, int64(deleted.ControllerEpoch))
			require.Equal(t, intent.Request.Metadata.Fence.RuntimeSessionUID, deleted.RuntimeSessionUID)
			require.Equal(t, intent.Request.Metadata.Fence.RuntimeSessionGeneration, deleted.RuntimeSessionGeneration)
			require.Equal(t, *checkpoint, observed.atDelete[1], "checkpoint must be loadable when DELETE arrives")
			t.Logf("recovery PID %d reopened SQLite at epoch %d; exact receipt retained, one prompt and capture per turn, generation %d deleted", restart.PID, restart.Epoch, deleted.RuntimeSessionGeneration)
		})
	}
}
