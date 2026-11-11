package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type remediationFixtureManager struct {
	manager.Manager
	client client.Client
	scheme *runtime.Scheme
	added  []manager.Runnable
}

func (m *remediationFixtureManager) GetClient() client.Client    { return m.client }
func (m *remediationFixtureManager) GetAPIReader() client.Reader { return m.client }
func (m *remediationFixtureManager) GetScheme() *runtime.Scheme  { return m.scheme }
func (m *remediationFixtureManager) Add(runnable manager.Runnable) error {
	m.added = append(m.added, runnable)
	return nil
}

func TestRemediationDefaultCompositionAcceptsQualifiedConfiguration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture-agent", Namespace: "fixture", UID: "fixture-agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "fixture-model"},
			Tools: []corev1alpha1.ToolReference{},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	mgr := &remediationFixtureManager{client: kube, scheme: scheme}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sqlite.NewDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := sqlite.NewStore(db, path)
	storage := storageclienttest.Client(t, store)
	builder := controller.NewJobBuilder(kube)
	image := "example.invalid/worker@sha256:" + strings.Repeat("a", 64)
	service, backend, err := setupRemediation(
		t.Context(), mgr, kubefake.NewSimpleClientset(), storage, builder, remediationOptions{
			Enabled: true, Namespace: "fixture", AgentName: agent.Name,
			AIWorkerImage: image, GeneralWorkerImage: image, GeneralWorkerImageID: image,
			HelperImage: image, HelperImageID: image, ToolImage: image, ToolImageID: image,
			Platform: "linux/amd64", NodeSelector: defaultRemediationNodeSelector, NetworkCanary: "192.0.2.10:8080",
		})
	require.NoError(t, err)
	require.NotNil(t, service)
	require.NotNil(t, backend)
	require.NotNil(t, builder.ValidationJobBuilder)
	require.Len(t, mgr.added, 2)
}
