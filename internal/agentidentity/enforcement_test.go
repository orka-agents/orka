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

const testLeaderLeaseUID = "leader-lease-uid"

// testLeaderLease returns the leader election Lease as leader election left
// it when holder took over at transition count transitions.
func testLeaderLease(holder string, transitions int32) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orka-system", Name: "leader", UID: testLeaderLeaseUID},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseTransitions: &transitions},
	}
}

// testThisLeader is this process as the leader election Lease names it.
var testThisLeader = &LeaderLease{Key: client.ObjectKey{Namespace: "orka-system", Name: "leader"}, HolderPrefix: "pod-b_"}

// testMarker returns a marker a leader created at created, recording the
// leader Lease transition recorded ("" for none).
func testMarker(created metav1.Time, recorded string) *coordinationv1.Lease {
	marker := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-system", Name: EnforcementLeaseName, CreationTimestamp: created, UID: "marker",
		Annotations: map[string]string{enforcementLeaderAnnotation: enforcementLeaderAnnotationValue},
	}}
	if recorded != "" {
		marker.Annotations[enforcementTransitionAnnotation] = recorded
	}
	return marker
}

// testEnforcementClient serves objects and stamps a created object with
// created, as the API server's clock would.
func testEnforcementClient(t *testing.T, created metav1.Time, objects ...client.Object) client.Client {
	t.Helper()
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(enforcementTestScheme(t)).WithObjects(objects...).Build(), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetCreationTimestamp(created)
			return c.Create(ctx, obj, opts...)
		},
	})
}

func testRecordedTransition(t *testing.T, c client.Client) string {
	t.Helper()
	marker := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "orka-system", Name: EnforcementLeaseName}, marker); err != nil {
		t.Fatal(err)
	}
	return marker.Annotations[enforcementTransitionAnnotation]
}

// TestEnforcementPeriodSurvivesRestartsAndEndsWhenTheCheckIsOff covers the
// marker an enforcing leader keeps: the next enforcing leader keeps its
// period and records itself, and a controller without the check deletes
// it, so the next enforcing leader starts anew.
func TestEnforcementPeriodSurvivesRestartsAndEndsWhenTheCheckIsOff(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	c := testEnforcementClient(t, metav1.Now(), testMarker(started, testLeaderLeaseUID+"/4"), testLeaderLease("pod-b_1", 5))
	ctx := context.Background()

	if since, err := StartEnforcementPeriod(ctx, c, "orka-system", testThisLeader); err != nil || !since.Equal(started.Time) {
		t.Fatalf("the next enforcing leader: since = %v, %v; want the earlier period's start %v", since, err, started.Time)
	}
	if got := testRecordedTransition(t, c); got != testLeaderLeaseUID+"/5" {
		t.Fatalf("recorded transition = %q, want this leader's", got)
	}
	if since, err := StartEnforcementPeriod(ctx, c, "orka-system", testThisLeader); err != nil || !since.Equal(started.Time) {
		t.Fatalf("a retry under the same leadership: since = %v, %v", since, err)
	}
	if err := EndEnforcementPeriod(ctx, c, "orka-system"); err != nil {
		t.Fatalf("a run without the check: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "orka-system", Name: EnforcementLeaseName}, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the marker must be gone after a run without the check: %v", err)
	}
	if err := EndEnforcementPeriod(ctx, c, "orka-system"); err != nil {
		t.Fatalf("a second run without the check: %v", err)
	}
}

// TestEnforcementPeriodRestartsAfterAnotherLeader covers a leader between two
// enforcing ones that may not have checked Agent use and did not delete the
// marker, for example because its deletion was slow or failed while it
// served requests. Its leadership advanced the leader Lease's count, so the
// enforcing leader after it starts a new period instead of continuing the
// marker's. A recreated leader Lease, whose count starts over, never matches
// an old count, and a marker from before counts were recorded never
// continues.
func TestEnforcementPeriodRestartsAfterAnotherLeader(t *testing.T) {
	first := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	for name, recorded := range map[string]string{
		"unchecked leader in between": testLeaderLeaseUID + "/4",
		"recreated leader Lease":      "old-lease-uid/5",
		"no recorded transition":      "",
		"unparsable record":           testLeaderLeaseUID + "/five",
	} {
		c := testEnforcementClient(t, now, testMarker(first, recorded), testLeaderLease("pod-b_1", 6))
		since, err := StartEnforcementPeriod(context.Background(), c, "orka-system", testThisLeader)
		if err != nil || !since.Equal(now.Time) {
			t.Fatalf("%s: since = %v, %v; want a new period from %v, not %v", name, since, err, now.Time, first.Time)
		}
		if got := testRecordedTransition(t, c); got != testLeaderLeaseUID+"/6" {
			t.Fatalf("%s: recorded transition = %q, want this leader's", name, got)
		}
	}
}

// TestEnforcementPeriodIsDecidedAtEachLeadership covers controllers whose
// processes overlap: an enforcing standby saw a period when it started, a
// controller without the check then led and deleted it, and the standby
// leads last. It must start a new period, not trust the one it saw.
func TestEnforcementPeriodIsDecidedAtEachLeadership(t *testing.T) {
	first := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	c := testEnforcementClient(t, now, testMarker(first, testLeaderLeaseUID+"/4"), testLeaderLease("pod-b_1", 6))
	ctx := context.Background()
	if err := EndEnforcementPeriod(ctx, c, "orka-system"); err != nil {
		t.Fatal(err)
	}
	since, err := StartEnforcementPeriod(ctx, c, "orka-system", testThisLeader)
	if err != nil || !since.Equal(now.Time) {
		t.Fatalf("the standby leading last: since = %v, %v; want a new period from %v, not %v", since, err, now.Time, first.Time)
	}
}

// TestStartEnforcementPeriodNeedsTheLeaderLease covers a controller that
// cannot show which leadership it is: one that no longer holds the leader
// Lease, or cannot read it, decides nothing and leaves the marker alone.
// Without leader election, nothing shows who served before, so every start
// begins a new period.
func TestStartEnforcementPeriodNeedsTheLeaderLease(t *testing.T) {
	first := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	ctx := context.Background()
	for name, objects := range map[string][]client.Object{
		"another holder": {testMarker(first, testLeaderLeaseUID+"/5"), testLeaderLease("pod-a_1", 6)},
		"no Lease":       {testMarker(first, testLeaderLeaseUID+"/5")},
	} {
		c := testEnforcementClient(t, now, objects...)
		if _, err := StartEnforcementPeriod(ctx, c, "orka-system", testThisLeader); err == nil {
			t.Fatalf("%s: started a period without holding the leader Lease", name)
		}
		if got := testRecordedTransition(t, c); got != testLeaderLeaseUID+"/5" {
			t.Fatalf("%s: marker changed to %q", name, got)
		}
	}
	c := testEnforcementClient(t, now, testMarker(first, ""))
	if since, err := StartEnforcementPeriod(ctx, c, "orka-system", nil); err != nil || !since.Equal(now.Time) {
		t.Fatalf("without leader election: since = %v, %v; want a new period from %v", since, err, now.Time)
	}
}

// TestStartEnforcementPeriodReplacesAMarkerNoLeaderCreated covers the leader
// starting a period: it creates the marker, keeps one it created, and
// replaces one an earlier, unfenced run created, so the start always follows
// the unchecked leader's last request.
func TestStartEnforcementPeriodReplacesAMarkerNoLeaderCreated(t *testing.T) {
	stale := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	unfenced := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-system", Name: EnforcementLeaseName, CreationTimestamp: stale, UID: "unfenced",
		Annotations: map[string]string{enforcementTransitionAnnotation: testLeaderLeaseUID + "/5"},
	}}
	created := metav1.NewTime(time.Now().Truncate(time.Second))
	c := testEnforcementClient(t, created, unfenced, testLeaderLease("pod-b_1", 5))
	ctx := context.Background()
	since, err := StartEnforcementPeriod(ctx, c, "orka-system", testThisLeader)
	if err != nil || !since.Equal(created.Time) {
		t.Fatalf("StartEnforcementPeriod() = %v, %v; want the replacement's creation %v", since, err, created.Time)
	}
	if again, err := StartEnforcementPeriod(ctx, c, "orka-system", testThisLeader); err != nil || !again.Equal(created.Time) {
		t.Fatalf("a retry under the same leadership: since = %v, %v; want the same period", again, err)
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
