/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestHandlers_ListTasks_UsesManagerAPIReaderForRealKubernetesContinuation(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: apiEnvtestBinaryDirectory(t),
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, testEnv.Stop())
	})

	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		PprofBindAddress:       "0",
		WebhookServer:          webhook.NewServer(webhook.Options{Port: 0}),
	})
	require.NoError(t, err)

	managerCtx, cancelManager := context.WithCancel(context.Background())
	managerDone := make(chan error, 1)
	go func() {
		managerDone <- mgr.Start(managerCtx)
	}()
	t.Cleanup(func() {
		cancelManager()
		select {
		case managerErr := <-managerDone:
			require.NoError(t, managerErr)
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop within 10 seconds")
		}
	})

	directClient, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	for _, name := range []string{"real-task-1", "real-task-2"} {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1alpha1.TaskSpec{
				Type:  corev1alpha1.TaskTypeContainer,
				Image: "busybox:1.36",
			},
		}
		require.NoError(t, directClient.Create(context.Background(), task))
	}

	cachedPage := &corev1alpha1.TaskList{}
	require.Eventually(t, func() bool {
		cachedPage = &corev1alpha1.TaskList{}
		return mgr.GetClient().List(
			context.Background(),
			cachedPage,
			client.InNamespace("default"),
			client.Limit(1),
		) == nil && len(cachedPage.Items) > 0
	}, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, cacheUnsupportedContinue, cachedPage.Continue)

	handlers := NewHandlers(HandlersConfig{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
	})
	app := newListTestApp(handlers)

	first, status := listPage[corev1alpha1.Task](t, app, "/tasks?limit=1")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.Metadata.Continue)
	require.NotEqual(t, cacheUnsupportedContinue, first.Metadata.Continue)

	second, status := listPage[corev1alpha1.Task](t, app, pathWithContinue("/tasks?limit=1", first.Metadata.Continue))
	require.Equal(t, http.StatusOK, status)
	require.Len(t, second.Items, 1)
	require.NotEqual(t, first.Items[0].Name, second.Items[0].Name)
	require.Empty(t, second.Metadata.Continue)

	createEnvtestTool(t, directClient, "snapshot-tool-1")
	toolPath := "/tools?limit=1"
	toolPage, status := listPage[toolListTestItem](t, app, toolPath)
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, toolPage.Metadata.Continue)
	createEnvtestTool(t, directClient, "snapshot-tool-2")

	toolNames := make([]string, 0, len(builtinToolsList)+1)
	for pageNumber := 1; pageNumber <= 20; pageNumber++ {
		for _, tool := range toolPage.Items {
			toolNames = append(toolNames, tool.Name)
		}
		if toolPage.Metadata.Continue == "" {
			break
		}
		toolPath = pathWithContinue("/tools?limit=1", toolPage.Metadata.Continue)
		toolPage, status = listPage[toolListTestItem](t, app, toolPath)
		require.Equal(t, http.StatusOK, status)
	}
	wantToolNames := make([]string, 0, len(builtinToolsList)+1)
	for _, tool := range builtinToolsList {
		wantToolNames = append(wantToolNames, tool["name"].(string))
	}
	wantToolNames = append(wantToolNames, "snapshot-tool-1")
	require.Equal(t, wantToolNames, toolNames)
}

func createEnvtestTool(t *testing.T, c client.Client, name string) {
	t.Helper()
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1alpha1.ToolSpec{
			Description: "snapshot test tool",
			HTTP:        &corev1alpha1.HTTPExecution{URL: "https://example.com/tool"},
		},
	}
	require.NoError(t, c.Create(context.Background(), tool))
}

func apiEnvtestBinaryDirectory(t *testing.T) string {
	t.Helper()
	if configured := os.Getenv("KUBEBUILDER_ASSETS"); configured != "" {
		return configured
	}

	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	require.NoError(t, err, "run make setup-envtest before running API pagination tests")
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	t.Fatalf("no envtest binaries found under %s; run make setup-envtest", base)
	return ""
}
