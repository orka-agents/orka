/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const skillTestNamespace = "default"

func setupSkillReconciler(objs ...runtime.Object) *SkillReconciler {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&corev1alpha1.Skill{}).
		Build()
	return &SkillReconciler{
		Client: c,
		Scheme: scheme,
	}
}

func TestSkillReconcilerReconcileReady(t *testing.T) {
	ctx := context.Background()
	skill := &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "code-review",
			Namespace: skillTestNamespace,
		},
		Spec: corev1alpha1.SkillSpec{
			DisplayName: "Code Review",
			Description: "Review code safely",
			Content: corev1alpha1.SkillContent{
				Inline: "Check correctness and security.",
			},
		},
	}

	r := setupSkillReconciler(skill)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      skill.Name,
		Namespace: skill.Namespace,
	}})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	updated := &corev1alpha1.Skill{}
	if err := r.Get(ctx, types.NamespacedName{Name: skill.Name, Namespace: skill.Namespace}, updated); err != nil {
		t.Fatalf("get updated skill failed: %v", err)
	}

	if updated.Status.Phase != testConditionReady {
		t.Fatalf("phase = %q, want Ready", updated.Status.Phase)
	}
	if !strings.HasPrefix(updated.Status.ContentHash, "sha256:") {
		t.Fatalf("contentHash = %q, want sha256:*", updated.Status.ContentHash)
	}
	ready := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v, want ConditionTrue", ready)
	}
	sizeCond := meta.FindStatusCondition(updated.Status.Conditions, "ContentSizeWarning")
	if sizeCond == nil || sizeCond.Status != metav1.ConditionFalse {
		t.Fatalf("ContentSizeWarning condition = %#v, want ConditionFalse", sizeCond)
	}
}

func TestSkillReconcilerReconcileLargeContentWarning(t *testing.T) {
	ctx := context.Background()
	large := strings.Repeat("a", skillContentSizeWarning+1)
	skill := &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "large-skill",
			Namespace: skillTestNamespace,
		},
		Spec: corev1alpha1.SkillSpec{
			Description: "Large skill",
			Content: corev1alpha1.SkillContent{
				Inline: large,
			},
		},
	}

	r := setupSkillReconciler(skill)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      skill.Name,
		Namespace: skill.Namespace,
	}})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	updated := &corev1alpha1.Skill{}
	if err := r.Get(ctx, types.NamespacedName{Name: skill.Name, Namespace: skill.Namespace}, updated); err != nil {
		t.Fatalf("get updated skill failed: %v", err)
	}
	sizeCond := meta.FindStatusCondition(updated.Status.Conditions, "ContentSizeWarning")
	if sizeCond == nil || sizeCond.Status != metav1.ConditionTrue {
		t.Fatalf("ContentSizeWarning condition = %#v, want ConditionTrue", sizeCond)
	}
}

func TestSkillReconcilerReconcileInvalidFilePath(t *testing.T) {
	ctx := context.Background()
	skill := &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-path-skill",
			Namespace: skillTestNamespace,
		},
		Spec: corev1alpha1.SkillSpec{
			Description: "Bad path",
			Content: corev1alpha1.SkillContent{
				Inline: "ok",
				Files: map[string]string{
					"../escape.md": "bad",
				},
			},
		},
	}

	r := setupSkillReconciler(skill)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      skill.Name,
		Namespace: skill.Namespace,
	}})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	updated := &corev1alpha1.Skill{}
	if err := r.Get(ctx, types.NamespacedName{Name: skill.Name, Namespace: skill.Namespace}, updated); err != nil {
		t.Fatalf("get updated skill failed: %v", err)
	}
	if updated.Status.Phase != "Error" {
		t.Fatalf("phase = %q, want Error", updated.Status.Phase)
	}
	ready := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition = %#v, want ConditionFalse", ready)
	}
}

func TestSkillContentHashDeterministic(t *testing.T) {
	r := setupSkillReconciler()
	s1 := &corev1alpha1.Skill{
		Spec: corev1alpha1.SkillSpec{
			Content: corev1alpha1.SkillContent{
				Inline: "base",
				Files: map[string]string{
					"b.md": "2",
					"a.md": "1",
				},
			},
		},
	}
	s2 := &corev1alpha1.Skill{
		Spec: corev1alpha1.SkillSpec{
			Content: corev1alpha1.SkillContent{
				Inline: "base",
				Files: map[string]string{
					"a.md": "1",
					"b.md": "2",
				},
			},
		},
	}

	h1 := r.computeContentHash(s1)
	h2 := r.computeContentHash(s2)
	if h1 != h2 {
		t.Fatalf("hash should be deterministic: %q != %q", h1, h2)
	}
}
