package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestFakeParameterTypesRegister(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	for _, object := range []runtime.Object{&FakeProviderConfig{}, &FakePoolParameters{}} {
		gvks, _, err := scheme.ObjectKinds(object)
		if err != nil || len(gvks) != 1 || gvks[0].Group != GroupVersion.Group {
			t.Fatalf("registered GVKs for %T = %v, err=%v", object, gvks, err)
		}
	}
}
