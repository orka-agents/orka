/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package v1alpha1 contains API Schema definitions for the core v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=core.orka.ai
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "core.orka.ai", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &schemeBuilder{groupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

type schemeBuilder struct {
	groupVersion schema.GroupVersion
	objects      []runtime.Object
}

func (b *schemeBuilder) Register(objects ...runtime.Object) {
	b.objects = append(b.objects, objects...)
}

func (b *schemeBuilder) AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(b.groupVersion, b.objects...)
	metav1.AddToGroupVersion(scheme, b.groupVersion)
	return nil
}
