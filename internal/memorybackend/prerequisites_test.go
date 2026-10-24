package memorybackend

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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRequirePrerequisites(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		mutate    func(*apiextensionsv1.CustomResourceDefinition)
		wantError string
	}{
		{name: "accepts compatible CRD"},
		{name: "requires schema marker", mutate: func(crd *apiextensionsv1.CustomResourceDefinition) {
			delete(crd.Annotations, corev1alpha1.MemoryBackendSchemaAnnotation)
		}, wantError: corev1alpha1.MemoryBackendSchemaAnnotation},
		{name: "requires storage version", mutate: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Spec.Versions[0].Storage = false
		}, wantError: "serve and store"},
		{name: "requires status subresource", mutate: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Spec.Versions[0].Subresources = nil
		}, wantError: "status subresource"},
		{name: "requires fixed name CEL", mutate: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema.XValidations = nil
		}, wantError: "fixed-name CEL"},
		{name: "requires immutable CEL", mutate: func(crd *apiextensionsv1.CustomResourceDefinition) {
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			spec.XValidations = spec.XValidations[:1]
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
		}, wantError: "immutable protocol/store CEL"},
		{name: "requires Established", mutate: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Status.Conditions = nil
		}, wantError: "not Established"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			crd := compatibleMemoryBackendCRD()
			if test.mutate != nil {
				test.mutate(crd)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crd).Build()
			err := RequirePrerequisites(context.Background(), reader)
			if test.wantError == "" && err != nil {
				t.Fatalf("RequirePrerequisites() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("RequirePrerequisites() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestRequirePrerequisitesReportsMissingCRD(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	err := RequirePrerequisites(context.Background(), reader)
	if err == nil || !errors.Is(err, ErrPrerequisitesUnavailable) || !strings.Contains(err.Error(), CRDName) {
		t.Fatalf("RequirePrerequisites() error = %v", err)
	}
}

func TestWaitForPrerequisitesRetriesTransientFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := &flakyReader{
		Reader:            fake.NewClientBuilder().WithScheme(scheme).WithObjects(compatibleMemoryBackendCRD()).Build(),
		remainingFailures: 2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := WaitForPrerequisites(ctx, reader, time.Millisecond); err != nil {
		t.Fatalf("WaitForPrerequisites() error = %v", err)
	}
	if reader.calls < 3 {
		t.Fatalf("Get calls = %d, want retries", reader.calls)
	}
}

func TestPrerequisiteErrorClassification(t *testing.T) {
	if PrerequisiteErrorIsTransient(fmt.Errorf("%w: stale", ErrPrerequisitesUnavailable)) {
		t.Fatal("incompatible CRD error classified as transient")
	}
	if !PrerequisiteErrorIsTransient(errors.New("temporary API failure")) {
		t.Fatal("API error classified as unavailable")
	}
}

func compatibleMemoryBackendCRD() *apiextensionsv1.CustomResourceDefinition {
	status := &apiextensionsv1.CustomResourceSubresourceStatus{}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: CRDName,
			Annotations: map[string]string{
				corev1alpha1.MemoryBackendSchemaAnnotation: corev1alpha1.MemoryBackendSchemaVersion,
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: CRDGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "memorybackends", Singular: "memorybackend", Kind: "MemoryBackend", ListKind: "MemoryBackendList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: CRDVersion, Served: true, Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{Status: status},
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type:         "object",
					XValidations: []apiextensionsv1.ValidationRule{{Rule: fixedNameCELRule}},
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {
							Type: "object",
							XValidations: []apiextensionsv1.ValidationRule{
								{Rule: protocolImmutableCELRule},
								{Rule: storeImmutableCELRule},
							},
						},
					},
				}},
			}},
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
			Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue,
		}}},
	}
}

type flakyReader struct {
	client.Reader
	remainingFailures int
	calls             int
}

func (r *flakyReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.calls++
	if r.remainingFailures > 0 {
		r.remainingFailures--
		return apierrors.NewServiceUnavailable("temporary prerequisite read failure")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}
