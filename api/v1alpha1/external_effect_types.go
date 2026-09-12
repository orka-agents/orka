/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ExternalEffectControlState is the durable state of one idempotent operation
// performed outside the controller's SQLite transaction boundary.
// +kubebuilder:validation:Enum=Pending;InFlight;Succeeded;Failed;OutcomeUnknown
type ExternalEffectControlState string

// ExternalEffectSpec is the immutable canonical identity and request binding.
// The identity namespace intentionally duplicates metadata.namespace so a
// serialized record remains self-describing and can be checked fail-closed.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="external effect spec is immutable"
type ExternalEffectSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Kind string `json:"kind"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	IdentityNamespace string `json:"identityNamespace"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	AggregateID string `json:"aggregateId"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	OperationID string `json:"operationId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`
}

// ExternalEffectStatus contains the mutable state, response, lease, and epoch
// fence for one canonical external effect.
// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state != 'InFlight' || (has(self.leaseOwner) && size(self.leaseOwner) > 0 && has(self.leaseExpiresAt))",message="in-flight external effects require a lease owner and expiry"
// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state == 'InFlight' || ((!has(self.leaseOwner) || size(self.leaseOwner) == 0) && !has(self.leaseExpiresAt))",message="non-in-flight external effects must clear lease fields"
type ExternalEffectStatus struct {
	// +optional
	State ExternalEffectControlState `json:"state,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ResponseDigest string `json:"responseDigest,omitempty"`

	// Response stores a bounded JSON response for idempotent replay. Large
	// response bodies should remain in the artifact store and be referenced by
	// a compact receipt instead.
	// +optional
	Response *apiextensionsv1.JSON `json:"response,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=1024
	LeaseOwner string `json:"leaseOwner,omitempty"`

	// +optional
	LeaseExpiresAt *metav1.Time `json:"leaseExpiresAt,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=0
	Attempts int64 `json:"attempts,omitempty"`

	ControlRecordMutationStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=false
// +k8s:deepcopy-gen=false
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=eeffect
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Attempts",type=integer,JSONPath=`.status.attempts`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExternalEffect is the Kubernetes-authoritative canonical idempotency record
// for an operation outside SQLite.
type ExternalEffect struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExternalEffectSpec   `json:"spec"`
	Status ExternalEffectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=false
// +k8s:deepcopy-gen=false

// ExternalEffectList contains a list of ExternalEffect.
type ExternalEffectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExternalEffect `json:"items"`
}

// DeepCopyInto is hand-written so this new root type is usable before the
// repository owner regenerates zz_generated.deepcopy.go.
func (in *ExternalEffect) DeepCopyInto(out *ExternalEffect) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Status.Response != nil {
		out.Status.Response = &apiextensionsv1.JSON{}
		if in.Status.Response.Raw != nil {
			out.Status.Response.Raw = append([]byte(nil), in.Status.Response.Raw...)
		}
	}
	if in.Status.LeaseExpiresAt != nil {
		out.Status.LeaseExpiresAt = in.Status.LeaseExpiresAt.DeepCopy()
	}
	deepCopyControlRecordMutationStatus(&in.Status.ControlRecordMutationStatus, &out.Status.ControlRecordMutationStatus)
}

// DeepCopy creates an independent copy.
func (in *ExternalEffect) DeepCopy() *ExternalEffect {
	if in == nil {
		return nil
	}
	out := new(ExternalEffect)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *ExternalEffect) DeepCopyObject() runtime.Object {
	if copy := in.DeepCopy(); copy != nil {
		return copy
	}
	return nil
}

// DeepCopyInto copies a list and all of its items.
func (in *ExternalEffectList) DeepCopyInto(out *ExternalEffectList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ExternalEffect, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy creates an independent list copy.
func (in *ExternalEffectList) DeepCopy() *ExternalEffectList {
	if in == nil {
		return nil
	}
	out := new(ExternalEffectList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *ExternalEffectList) DeepCopyObject() runtime.Object {
	if copy := in.DeepCopy(); copy != nil {
		return copy
	}
	return nil
}

func deepCopyControlRecordMutationStatus(in, out *ControlRecordMutationStatus) {
	if in.CreatedAt != nil {
		out.CreatedAt = in.CreatedAt.DeepCopy()
	}
	if in.UpdatedAt != nil {
		out.UpdatedAt = in.UpdatedAt.DeepCopy()
	}
}

func init() {
	SchemeBuilder.Register(&ExternalEffect{}, &ExternalEffectList{})
}
