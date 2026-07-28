/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package v1alpha1 contains API schema definitions for the fake.workspace v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=fake.workspace.orka.ai
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "fake.workspace.orka.ai", Version: "v1alpha1"}

	// SchemeBuilder registers this API group's objects without adding controller-runtime dependencies.
	SchemeBuilder = &schemeBuilder{groupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the supplied scheme.
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
