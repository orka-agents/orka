package workspaceprovider

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetCondition adds or updates a condition using Kubernetes condition transition semantics.
func SetCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	if conditions == nil {
		return
	}
	apimeta.SetStatusCondition(conditions, condition)
}

// FindCondition returns the condition with the requested type, or nil when absent.
func FindCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	return apimeta.FindStatusCondition(conditions, conditionType)
}

// ConditionIsTrue reports whether the requested condition exists and is True.
func ConditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	return apimeta.IsStatusConditionTrue(conditions, conditionType)
}

// ConditionIsFalse reports whether the requested condition exists and is False.
func ConditionIsFalse(conditions []metav1.Condition, conditionType string) bool {
	return apimeta.IsStatusConditionFalse(conditions, conditionType)
}
