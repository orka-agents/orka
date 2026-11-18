package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// The RepositoryMonitor CRD itself rejects a repair command without the
// validation image that must run it, and admits it with one.
func TestRepositoryMonitorRepairCommandRequiresImageOnAPIServer(t *testing.T) {
	apiClient := newArchivedSessionAPIClient(t)
	ctx := t.Context()
	monitor := func(name, image string) *corev1alpha1.RepositoryMonitor {
		return &corev1alpha1.RepositoryMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1alpha1.RepositoryMonitorSpec{
				RepoURL:    "https://github.com/orka-agents/orka",
				Validation: corev1alpha1.RepositoryMonitorValidationSpec{Image: image, RepairCommand: "make test"},
			},
		}
	}
	err := apiClient.Create(ctx, monitor("repair-command-without-image", ""))
	if err == nil || !strings.Contains(err.Error(), "repairCommand requires image") {
		t.Fatalf("create without image error = %v, want repairCommand requires image", err)
	}
	if err := apiClient.Create(ctx, monitor("repair-command-with-image", repositoryMonitorValidationTestImage)); err != nil {
		t.Fatalf("create with image: %v", err)
	}
}
