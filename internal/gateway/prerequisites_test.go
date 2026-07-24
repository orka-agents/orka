package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRequireTaskSessionCutoffSchema(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	t.Run("accepts compatible Task CRD", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: TaskCRDName,
				Annotations: map[string]string{
					TaskSessionCutoffSchemaAnnotation: TaskSessionCutoffSchemaVersion,
				},
			},
			Status: establishedCRDStatus(),
		}).Build()
		if err := RequireTaskSessionCutoffSchema(context.Background(), reader); err != nil {
			t.Fatalf("RequireTaskSessionCutoffSchema() error = %v", err)
		}
	})

	t.Run("rejects stale Task CRD", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: TaskCRDName},
			Status:     establishedCRDStatus(),
		}).Build()
		err := RequireTaskSessionCutoffSchema(context.Background(), reader)
		if err == nil || !strings.Contains(err.Error(), TaskSessionCutoffSchemaAnnotation) {
			t.Fatalf("RequireTaskSessionCutoffSchema() error = %v", err)
		}
	})

	t.Run("rejects unestablished Task CRD", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: TaskCRDName, Annotations: map[string]string{
				TaskSessionCutoffSchemaAnnotation: TaskSessionCutoffSchemaVersion,
			}},
		}).Build()
		err := RequireTaskSessionCutoffSchema(context.Background(), reader)
		if err == nil || !strings.Contains(err.Error(), "not Established") {
			t.Fatalf("RequireTaskSessionCutoffSchema() error = %v", err)
		}
	})

	t.Run("rejects missing Task CRD", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).Build()
		err := RequireTaskSessionCutoffSchema(context.Background(), reader)
		if err == nil || !strings.Contains(err.Error(), "not installed") {
			t.Fatalf("RequireTaskSessionCutoffSchema() error = %v", err)
		}
	})
}

func TestRequireGatewayPrerequisites(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	compatible := func() []client.Object {
		return []client.Object{
			&apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: TaskCRDName, Annotations: map[string]string{TaskSessionCutoffSchemaAnnotation: TaskSessionCutoffSchemaVersion}},
				Status:     establishedCRDStatus(),
			},
			gatewayPrerequisiteCRD(GatewayClassCRDName, "GatewayClass", apiextensionsv1.ClusterScoped, true),
			gatewayPrerequisiteCRD(GatewayCRDName, "Gateway", apiextensionsv1.NamespaceScoped, true),
			gatewayPrerequisiteCRD(GatewayBindingCRDName, "GatewayBinding", apiextensionsv1.NamespaceScoped, true),
		}
	}

	t.Run("accepts complete CRD set", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(compatible()...).Build()
		if err := RequireGatewayPrerequisites(context.Background(), reader); err != nil {
			t.Fatalf("RequireGatewayPrerequisites() error = %v", err)
		}
	})

	t.Run("reports every missing Gateway CRD", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(compatible()[0]).Build()
		err := RequireGatewayPrerequisites(context.Background(), reader)
		if err == nil {
			t.Fatal("RequireGatewayPrerequisites() unexpectedly succeeded")
		}
		for _, name := range []string{GatewayClassCRDName, GatewayCRDName, GatewayBindingCRDName} {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q does not mention %s", err, name)
			}
		}
	})

	t.Run("rejects an unestablished Gateway CRD", func(t *testing.T) {
		objects := compatible()
		gateway := gatewayPrerequisiteCRD(GatewayCRDName, "Gateway", apiextensionsv1.NamespaceScoped, true)
		gateway.Status = apiextensionsv1.CustomResourceDefinitionStatus{}
		objects[2] = gateway
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
		err := RequireGatewayPrerequisites(context.Background(), reader)
		if err == nil || !strings.Contains(err.Error(), "not Established") {
			t.Fatalf("RequireGatewayPrerequisites() error = %v", err)
		}
	})

	t.Run("rejects an unserved Gateway version", func(t *testing.T) {
		objects := compatible()
		objects[2] = gatewayPrerequisiteCRD(GatewayCRDName, "Gateway", apiextensionsv1.NamespaceScoped, false)
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
		err := RequireGatewayPrerequisites(context.Background(), reader)
		if err == nil || !strings.Contains(err.Error(), "does not serve") {
			t.Fatalf("RequireGatewayPrerequisites() error = %v", err)
		}
	})
}

func gatewayPrerequisiteCRD(
	name, kind string, scope apiextensionsv1.ResourceScope, served bool,
) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: GatewayCRDGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: name, Kind: kind},
			Scope: scope,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: GatewayCRDVersion, Served: served, Storage: true,
			}},
		},
		Status: establishedCRDStatus(),
	}
}

func establishedCRDStatus() apiextensionsv1.CustomResourceDefinitionStatus {
	return apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
		Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue,
	}}}
}

func TestWaitForGatewayPrerequisitesRetriesTransientFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{
		&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: TaskCRDName, Annotations: map[string]string{TaskSessionCutoffSchemaAnnotation: TaskSessionCutoffSchemaVersion}},
			Status:     establishedCRDStatus(),
		},
		gatewayPrerequisiteCRD(GatewayClassCRDName, "GatewayClass", apiextensionsv1.ClusterScoped, true),
		gatewayPrerequisiteCRD(GatewayCRDName, "Gateway", apiextensionsv1.NamespaceScoped, true),
		gatewayPrerequisiteCRD(GatewayBindingCRDName, "GatewayBinding", apiextensionsv1.NamespaceScoped, true),
	}
	reader := &flakyPrerequisiteReader{
		Reader:            fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		remainingFailures: 2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := WaitForGatewayPrerequisites(ctx, reader, time.Millisecond); err != nil {
		t.Fatalf("WaitForGatewayPrerequisites() error = %v", err)
	}
	if reader.calls <= 4 {
		t.Fatalf("Get calls = %d, want a retry after transient failure", reader.calls)
	}
}

func TestGatewayPrerequisiteErrorClassification(t *testing.T) {
	if GatewayPrerequisiteErrorIsTransient(fmt.Errorf("%w: missing", ErrGatewayPrerequisitesUnavailable)) {
		t.Fatal("missing CRD error classified as transient")
	}
	if !GatewayPrerequisiteErrorIsTransient(errors.New("temporary API failure")) {
		t.Fatal("API error classified as unavailable")
	}
}

type flakyPrerequisiteReader struct {
	client.Reader
	remainingFailures int
	calls             int
}

func (r *flakyPrerequisiteReader) Get(
	ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption,
) error {
	r.calls++
	if r.remainingFailures > 0 {
		r.remainingFailures--
		return apierrors.NewServiceUnavailable("temporary prerequisite read failure")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}
