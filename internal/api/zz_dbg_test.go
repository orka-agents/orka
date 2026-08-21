package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

var _ = apierrors.NewBadRequest

func TestDebugRecovery(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "idempotent-repair", Namespace: "demo", UID: types.UID("idempotent-repair-uid"), Generation: 3,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	taskCreateCalls := 0
	firstVerificationFailed := false
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				task, ok := obj.(*corev1alpha1.Task)
				if !ok {
					return c.Create(ctx, obj, opts...)
				}
				_ = task
				taskCreateCalls++
				if taskCreateCalls == 1 {
					return errors.New("simulated ambiguous task create response")
				}
				return c.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1alpha1.Task); ok && taskCreateCalls == 1 && !firstVerificationFailed {
					firstVerificationFailed = true
					return errors.New("simulated task verification outage")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	securityStore := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{Client: cl, SecurityStore: securityStore})
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), current))

	firstRun, firstErr := handlers.createSecurityScanRun(context.Background(), nil, current, "")
	t.Logf("first: run=%v err=%v", firstRun, firstErr)

	app := fiber.New()
	app.Post("/security/repositories/:name/scans", handlers.CreateManualSecurityScan)
	req := httptest.NewRequest(http.MethodPost, "/security/repositories/"+scan.Name+"/scans?namespace="+scan.Namespace, nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	t.Logf("recovery status=%d body=%s", resp.StatusCode, string(body))
	_ = fiber.StatusCreated
}
