/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package v1alpha1 contains API schema definitions for the gateway.orka.ai API group.
// +kubebuilder:object:generate=true
// +groupName=gateway.orka.ai
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion identifies the gateway API group and version.
	GroupVersion = schema.GroupVersion{Group: "gateway.orka.ai", Version: "v1alpha1"}

	// SchemeBuilder registers gateway API objects.
	SchemeBuilder = &schemeBuilder{groupVersion: GroupVersion}

	// AddToScheme adds gateway API objects to a runtime scheme.
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
