// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

// The shared core admission policy makes the workspace taint permanent for
// every identity, including one that holds the core "admit" verb (envtest's
// system:masters client holds every verb), while other core writes and the
// first taint write stay admitted.
func TestWorkspaceCoreAdmissionPolicyMakesTheOperatorTaintPermanent(t *testing.T) {
	ctx := context.Background()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop policy API server: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "policy", "workspace_core_admission_policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	documents := strings.Split(string(manifest), "\n---\n")
	if len(documents) != 2 {
		t.Fatalf("policy manifest has %d documents, want the policy and its binding", len(documents))
	}
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	if err := yaml.UnmarshalStrict([]byte(documents[0]), policy); err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict([]byte(documents[1]), binding); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "taint-policy"}}); err != nil {
		t.Fatal(err)
	}

	taint := operatoraccess.EncodeTaint(operatoraccess.Taint{AuditID: "op-1", TaskUID: "task-uid"})
	tainted := validExecutionWorkspace("taint-policy", "tainted", "class", "provider")
	tainted.Annotations = map[string]string{operatoraccess.TaintAnnotation: taint}
	if err := c.Create(ctx, tainted); err != nil {
		t.Fatal(err)
	}
	update := func(name string, mutate func(*workspacev1alpha1.ExecutionWorkspace), options ...client.UpdateOption) error {
		current := &workspacev1alpha1.ExecutionWorkspace{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "taint-policy", Name: name}, current); err != nil {
			return err
		}
		mutate(current)
		return c.Update(ctx, current, options...)
	}
	clearTaint := func(w *workspacev1alpha1.ExecutionWorkspace) { delete(w.Annotations, operatoraccess.TaintAnnotation) }

	// Policy activation is asynchronous; probe with dry runs so an update
	// admitted before activation never clears the fixture's marker.
	deadline := time.Now().Add(time.Minute)
	for {
		err := update("tainted", clearTaint, client.DryRunAll)
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			if !strings.Contains(err.Error(), "operator access taint") {
				t.Fatalf("taint removal denied by the wrong rule: %v", err)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the core admission policy never refused clearing the operator taint")
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := update("tainted", clearTaint); err == nil {
		t.Fatal("an identity holding the admit verb cleared the operator taint")
	}
	if err := update("tainted", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[operatoraccess.TaintAnnotation] = operatoraccess.EncodeTaint(operatoraccess.Taint{AuditID: "op-2"})
	}); err == nil {
		t.Fatal("an identity holding the admit verb rewrote the operator taint")
	}
	if err := update("tainted", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations = nil
	}); err == nil {
		t.Fatal("an identity holding the admit verb dropped every annotation, including the taint")
	}
	if err := update("tainted", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	}); err != nil {
		t.Fatalf("a core write that keeps the taint was refused: %v", err)
	}

	clean := validExecutionWorkspace("taint-policy", "clean", "class", "provider")
	if err := c.Create(ctx, clean); err != nil {
		t.Fatal(err)
	}
	if err := update("clean", func(w *workspacev1alpha1.ExecutionWorkspace) {
		if w.Annotations == nil {
			w.Annotations = map[string]string{}
		}
		w.Annotations[operatoraccess.TaintAnnotation] = taint
	}); err != nil {
		t.Fatalf("the first taint write was refused: %v", err)
	}
	if err := c.Delete(ctx, tainted); err != nil {
		t.Fatalf("deleting a tainted workspace was refused: %v", err)
	}
}
