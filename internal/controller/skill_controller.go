/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
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
	// skillContentSizeWarning is the threshold above which a warning condition is set
	skillContentSizeWarning = 10 * 1024 // 10KB
)

const skillPhaseReady = "Ready"

// SkillReconciler reconciles a Skill object
type SkillReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=skills,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=skills/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=skills/finalizers,verbs=update

// Reconcile validates the Skill content, computes a content hash,
// and updates the Skill's status accordingly.
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

	// Validate the skill
	if err := r.validateSkill(skill); err != nil {
		logger.Error(err, "Skill validation failed")
		return r.updateStatus(ctx, skill, "Error", "", err.Error())
	}

	// Compute content hash
	contentHash := r.computeContentHash(skill)

	// Check for content size warning
	contentSize := len(skill.Spec.Content.Inline)
	for _, v := range skill.Spec.Content.Files {
		contentSize += len(v)
	}
	r.setContentSizeCondition(skill, contentSize)

	return r.updateStatus(ctx, skill, skillPhaseReady, contentHash, "")
}

// validateSkill validates the Skill spec.
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

// computeContentHash computes a SHA-256 hash of all skill content.
func (r *SkillReconciler) computeContentHash(skill *corev1alpha1.Skill) string {
	h := sha256.New()
	h.Write([]byte(skill.Spec.Content.Inline))
	keys := make([]string, 0, len(skill.Spec.Content.Files))
	for k := range skill.Spec.Content.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := skill.Spec.Content.Files[k]
		h.Write([]byte(k))
		h.Write([]byte(v))
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

// setContentSizeCondition updates content-size warning status.
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

// updateStatus updates the Skill status and conditions.
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

// SetupWithManager sets up the controller with the Manager.
func (r *SkillReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Skill{}).
		Named("skill").
		Complete(r)
}
