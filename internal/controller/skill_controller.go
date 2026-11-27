/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"path"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	skillContentSizeWarning = 10 * 1024
)

const skillPhaseReady = "Ready"

type SkillReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=skills,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=skills/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=skills/finalizers,verbs=update

func (r *SkillReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	skill := &corev1alpha1.Skill{}
	if err := r.Get(ctx, req.NamespacedName, skill); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Skill", "skill", skill.Name)

	if err := r.validateSkill(skill); err != nil {
		logger.Error(err, "Skill validation failed")
		return r.updateStatus(ctx, skill, "Error", "", err.Error())
	}

	contentHash := r.computeContentHash(skill)

	contentSize := len(skill.Spec.Content.Inline)
	for _, v := range skill.Spec.Content.Files {
		contentSize += len(v)
	}
	r.setContentSizeCondition(skill, contentSize)

	return r.updateStatus(ctx, skill, skillPhaseReady, contentHash, "")
}

func (r *SkillReconciler) validateSkill(skill *corev1alpha1.Skill) error {
	if skill.Spec.Description == "" {
		return fmt.Errorf("description is required")
	}
	if skill.Spec.Content.Inline == "" {
		return fmt.Errorf("content.inline is required")
	}
	for filePath := range skill.Spec.Content.Files {
		cleanPath := path.Clean(filePath)
		if filePath == "" || strings.HasPrefix(filePath, "/") || cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
			return fmt.Errorf("invalid content.files path %q", filePath)
		}
	}
	return nil
}

func (r *SkillReconciler) computeContentHash(skill *corev1alpha1.Skill) string {
	h := sha256.New()
	writeField := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:", len(s))
		_, _ = io.WriteString(h, s)
	}
	writeField(skill.Spec.Content.Inline)
	keys := make([]string, 0, len(skill.Spec.Content.Files))
	for k := range skill.Spec.Content.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(k)
		writeField(skill.Spec.Content.Files[k])
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func (r *SkillReconciler) setContentSizeCondition(skill *corev1alpha1.Skill, size int) {
	condition := metav1.Condition{
		Type:               "ContentSizeWarning",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: skill.Generation,
	}
	if size > skillContentSizeWarning {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "ContentExceedsRecommendedSize"
		condition.Message = fmt.Sprintf("Skill content is %d bytes (recommended < %d bytes). Large skills waste LLM context.", size, skillContentSizeWarning)
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ContentWithinRecommendedSize"
		condition.Message = fmt.Sprintf("Skill content is within the recommended size (< %d bytes).", skillContentSizeWarning)
	}
	meta.SetStatusCondition(&skill.Status.Conditions, condition)
}

func (r *SkillReconciler) updateStatus(ctx context.Context, skill *corev1alpha1.Skill, phase, contentHash, errMsg string) (ctrl.Result, error) {
	original := skill.Status.DeepCopy()

	skill.Status.Phase = phase
	skill.Status.ContentHash = contentHash
	skill.Status.ObservedGeneration = skill.Generation

	condition := metav1.Condition{
		Type:               skillPhaseReady,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: skill.Generation,
	}

	if phase == skillPhaseReady {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "ContentValid"
		condition.Message = "Skill content validated successfully"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ValidationFailed"
		condition.Message = errMsg
	}

	meta.SetStatusCondition(&skill.Status.Conditions, condition)

	if original != nil && reflect.DeepEqual(*original, skill.Status) {
		return ctrl.Result{}, nil
	}

	if err := r.Status().Update(ctx, skill); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SkillReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Skill{}).
		Named("skill").
		Complete(r)
}
