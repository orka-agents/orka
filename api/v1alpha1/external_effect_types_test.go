package v1alpha1

import (
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestExternalEffectDeepCopyIsIndependentAndRegistered(t *testing.T) {
	expires := metav1.NewTime(time.Date(2026, time.July, 25, 6, 0, 0, 0, time.UTC))
	created := expires.DeepCopy()
	updated := expires.DeepCopy()
	original := &ExternalEffect{
		ObjectMeta: metav1.ObjectMeta{Name: "effect", Namespace: "tenant-a", Labels: map[string]string{"a": "b"}},
		Spec:       ExternalEffectSpec{ID: "effect-1", Kind: "PullRequest", IdentityNamespace: "tenant-a", AggregateID: "publication-1", OperationID: "op-1", RequestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Status: ExternalEffectStatus{
			State:          "InFlight",
			Response:       &apiextensionsv1.JSON{Raw: []byte(`{"ok":true}`)},
			LeaseExpiresAt: &expires,
			ControlRecordMutationStatus: ControlRecordMutationStatus{
				CreatedAt: created,
				UpdatedAt: updated,
			},
		},
	}
	copy := original.DeepCopy()
	copy.Labels["a"] = "changed"
	copy.Status.Response.Raw[2] = 'X'
	copy.Status.LeaseExpiresAt.Time = copy.Status.LeaseExpiresAt.Add(time.Hour)
	copy.Status.CreatedAt.Time = copy.Status.CreatedAt.Add(time.Hour)

	if original.Labels["a"] != "b" {
		t.Fatalf("labels were aliased: %#v", original.Labels)
	}
	if string(original.Status.Response.Raw) != `{"ok":true}` {
		t.Fatalf("response bytes were aliased: %s", original.Status.Response.Raw)
	}
	if !original.Status.LeaseExpiresAt.Equal(&expires) {
		t.Fatalf("lease expiry was aliased: %s", original.Status.LeaseExpiresAt)
	}
	if !original.Status.CreatedAt.Equal(created) {
		t.Fatalf("mutation timestamp was aliased: %s", original.Status.CreatedAt)
	}

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if _, err := scheme.New(GroupVersion.WithKind("ExternalEffect")); err != nil {
		t.Fatalf("ExternalEffect is not registered: %v", err)
	}
}
