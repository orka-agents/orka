package agentidentity

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestEnforcementPeriodSurvivesRestartsAndEndsWhenTheCheckIsOff covers the
// marker an enforcing leader keeps: a restart keeps the period, and a run
// without the check deletes it, so the next enforcing run starts anew.
func TestEnforcementPeriodSurvivesRestartsAndEndsWhenTheCheckIsOff(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	existing := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-system", Name: EnforcementLeaseName, CreationTimestamp: started,
		Annotations: map[string]string{enforcementLeaderAnnotation: "true"},
	}}
	c := fake.NewClientBuilder().WithScheme(enforcementTestScheme(t)).WithObjects(existing).Build()
	ctx := context.Background()

	since, ok, err := EstablishEnforcementPeriod(ctx, c, "orka-system", true)
	if err != nil || !ok || !since.Equal(started.Time) {
		t.Fatalf("restart: since = %v, %t, %v; want the earlier period's start %v", since, ok, err, started.Time)
	}
	if since, ok, err := EstablishEnforcementPeriod(ctx, c, "orka-system", false); err != nil || !ok || !since.IsZero() {
		t.Fatalf("run without the check: since = %v, %t, %v", since, ok, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the marker must be gone after a run without the check: %v", err)
	}
	if _, _, err := EstablishEnforcementPeriod(ctx, c, "orka-system", false); err != nil {
		t.Fatalf("a second run without the check: %v", err)
	}
	// Before it leads, an enforcing run starts no period: the leader it
	// replaces may still serve requests without the check.
	if _, ok, err := EstablishEnforcementPeriod(ctx, c, "orka-system", true); err != nil || ok {
		t.Fatalf("an enforcing run with no period: started = %t, %v; want it left to the leader", ok, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("an enforcing run must not create the marker before it leads: %v", err)
	}
}

// TestStartEnforcementPeriodReplacesAMarkerNoLeaderCreated covers the leader
// starting a period: it creates the marker, keeps one a leader created, and
// replaces one an earlier, unfenced run created, so the start always follows
// the unchecked leader's last request.
func TestStartEnforcementPeriodReplacesAMarkerNoLeaderCreated(t *testing.T) {
	stale := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	unfenced := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-system", Name: EnforcementLeaseName, CreationTimestamp: stale, UID: "unfenced",
	}}
	created := metav1.NewTime(time.Now().Truncate(time.Second))
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(enforcementTestScheme(t)).WithObjects(unfenced).Build(), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetCreationTimestamp(created) // the API server's clock
			return c.Create(ctx, obj, opts...)
		},
	})
	ctx := context.Background()
	if _, ok, err := EstablishEnforcementPeriod(ctx, c, "orka-system", true); err != nil || ok {
		t.Fatalf("an unfenced marker: started = %t, %v; want no period", ok, err)
	}
	since, err := StartEnforcementPeriod(ctx, c, "orka-system")
	if err != nil || !since.Equal(created.Time) {
		t.Fatalf("StartEnforcementPeriod() = %v, %v; want the replacement's creation %v", since, err, created.Time)
	}
	if since, ok, err := EstablishEnforcementPeriod(ctx, c, "orka-system", true); err != nil || !ok || !since.Equal(created.Time) {
		t.Fatalf("restart after the leader started it: since = %v, %t, %v", since, ok, err)
	}
	if again, err := StartEnforcementPeriod(ctx, c, "orka-system"); err != nil || !again.Equal(created.Time) {
		t.Fatalf("a later leader: since = %v, %v; want the same period", again, err)
	}
}

func enforcementTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}
