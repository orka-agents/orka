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
// marker an enforcing leader keeps: a later enforcing leader keeps its
// period, and a controller without the check deletes it, so the next
// enforcing leader starts anew.
func TestEnforcementPeriodSurvivesRestartsAndEndsWhenTheCheckIsOff(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	existing := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-system", Name: EnforcementLeaseName, CreationTimestamp: started,
		Annotations: map[string]string{enforcementLeaderAnnotation: enforcementLeaderAnnotationValue},
	}}
	c := fake.NewClientBuilder().WithScheme(enforcementTestScheme(t)).WithObjects(existing).Build()
	ctx := context.Background()

	if since, err := StartEnforcementPeriod(ctx, c, "orka-system"); err != nil || !since.Equal(started.Time) {
		t.Fatalf("a later enforcing leader: since = %v, %v; want the earlier period's start %v", since, err, started.Time)
	}
	if err := EndEnforcementPeriod(ctx, c, "orka-system"); err != nil {
		t.Fatalf("a run without the check: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the marker must be gone after a run without the check: %v", err)
	}
	if err := EndEnforcementPeriod(ctx, c, "orka-system"); err != nil {
		t.Fatalf("a second run without the check: %v", err)
	}
}

// TestEnforcementPeriodIsDecidedAtEachLeadership covers controllers whose
// processes overlap: an enforcing standby saw a period when it started, a
// controller without the check then led and served unchecked requests, and
// the standby leads last. It must start a new period, not trust the one it
// saw.
func TestEnforcementPeriodIsDecidedAtEachLeadership(t *testing.T) {
	first := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	existing := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-system", Name: EnforcementLeaseName, CreationTimestamp: first,
		Annotations: map[string]string{enforcementLeaderAnnotation: enforcementLeaderAnnotationValue},
	}}
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(enforcementTestScheme(t)).WithObjects(existing).Build(), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetCreationTimestamp(now) // the API server's clock
			return c.Create(ctx, obj, opts...)
		},
	})
	ctx := context.Background()
	// The controller without the check ends the period as it leads.
	if err := EndEnforcementPeriod(ctx, c, "orka-system"); err != nil {
		t.Fatal(err)
	}
	since, err := StartEnforcementPeriod(ctx, c, "orka-system")
	if err != nil || !since.Equal(now.Time) {
		t.Fatalf("the standby leading last: since = %v, %v; want a new period from %v, not %v", since, err, now.Time, first.Time)
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
	since, err := StartEnforcementPeriod(ctx, c, "orka-system")
	if err != nil || !since.Equal(created.Time) {
		t.Fatalf("StartEnforcementPeriod() = %v, %v; want the replacement's creation %v", since, err, created.Time)
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
