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
)

// TestEnforcementPeriodSurvivesRestartsAndEndsWhenTheCheckIsOff covers the
// marker an enforcing controller keeps: a restart keeps the period, and a run
// without the check deletes it, so the next enforcing run starts anew.
func TestEnforcementPeriodSurvivesRestartsAndEndsWhenTheCheckIsOff(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	started := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	existing := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-system", Name: EnforcementLeaseName, CreationTimestamp: started,
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	ctx := context.Background()

	since, err := EstablishEnforcementPeriod(ctx, c, "orka-system", true)
	if err != nil || !since.Equal(started.Time) {
		t.Fatalf("restart: since = %v, %v; want the earlier period's start %v", since, err, started.Time)
	}
	if since, err := EstablishEnforcementPeriod(ctx, c, "orka-system", false); err != nil || !since.IsZero() {
		t.Fatalf("run without the check: since = %v, %v", since, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the marker must be gone after a run without the check: %v", err)
	}
	if _, err := EstablishEnforcementPeriod(ctx, c, "orka-system", false); err != nil {
		t.Fatalf("a second run without the check: %v", err)
	}
	// The fake client sets no creation time on create, which the controller
	// treats as an error rather than a period with no start.
	if _, err := EstablishEnforcementPeriod(ctx, c, "orka-system", true); err == nil {
		t.Fatal("a marker without a creation time must not start a period")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &coordinationv1.Lease{}); err != nil {
		t.Fatalf("an enforcing run creates the marker: %v", err)
	}
}
