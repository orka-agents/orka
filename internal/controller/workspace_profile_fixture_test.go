package controller

import (
	"context"
	"encoding/json"

	authorizationv1 "k8s.io/api/authorization/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var acpFixtureGroupVersion = schema.GroupVersion{Group: "test.workspace.orka.ai", Version: "v1alpha1"}

//nolint:unparam // Test fixture registration matches AddToScheme callers.
func addACPFixtureAPIToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(acpFixtureGroupVersion, &RuntimeProviderConfig{}, &RuntimeProviderConfigList{}, &RuntimeWorkspaceProfile{}, &RuntimeWorkspaceProfileList{})
	metav1.AddToGroupVersion(s, acpFixtureGroupVersion)
	return nil
}

type RuntimeProviderBackend string

const (
	RuntimeProviderBackendAgentSandbox RuntimeProviderBackend = "agent-sandbox"
	RuntimeProviderBackendSubstrate    RuntimeProviderBackend = "substrate"
)

type RuntimeProviderConfigSpec struct {
	Backend RuntimeProviderBackend `json:"backend"`
}

type RuntimeProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RuntimeProviderConfigSpec `json:"spec"`
}

type RuntimeProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeProviderConfig `json:"items"`
}

type SubstrateTemplateReference struct {
	Name string `json:"name"`

	Namespace string `json:"namespace,omitempty"`
}

type SubstrateSuspendMode string

const SubstrateSuspendModeDataOnly SubstrateSuspendMode = "DataOnly"

type SubstrateSuspendPolicy struct {
	Mode SubstrateSuspendMode `json:"mode"`
}

type SubstrateProfileSpec struct {
	TemplateRef SubstrateTemplateReference `json:"templateRef"`

	Suspend *SubstrateSuspendPolicy `json:"suspend,omitempty"`
}

type AgentSandboxDurableVolume struct {
	StorageClassName string `json:"storageClassName,omitempty"`

	AccessModes []string `json:"accessModes,omitempty"`

	Capacity string `json:"capacity"`
}

type AgentSandboxSuspendPolicy struct {
	Mode SubstrateSuspendMode `json:"mode"`

	Volume AgentSandboxDurableVolume `json:"volume"`
}

type RetentionPolicy struct {
	MaxSuspendedWorkspaces *int32 `json:"maxSuspendedWorkspaces,omitempty"`
}

type AgentSandboxProfileSpec struct {
	Suspend *AgentSandboxSuspendPolicy `json:"suspend,omitempty"`
}

type RuntimeWorkspaceProfileSpec struct {
	Suspend   *SubstrateSuspendPolicy `json:"suspend,omitempty"`
	Substrate *SubstrateProfileSpec   `json:"substrate,omitempty"`

	AgentSandbox *AgentSandboxProfileSpec `json:"agentSandbox,omitempty"`

	Retention *RetentionPolicy `json:"retention,omitempty"`
}

type RuntimeWorkspaceProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RuntimeWorkspaceProfileSpec `json:"spec,omitempty"`
}

type RuntimeWorkspaceProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeWorkspaceProfile `json:"items"`
}

func (in *RuntimeProviderConfig) DeepCopy() *RuntimeProviderConfig {
	if in == nil {
		return nil
	}
	out := new(RuntimeProviderConfig)
	raw, _ := json.Marshal(in)
	_ = json.Unmarshal(raw, out)
	return out
}
func (in *RuntimeProviderConfig) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *RuntimeProviderConfigList) DeepCopy() *RuntimeProviderConfigList {
	if in == nil {
		return nil
	}
	out := new(RuntimeProviderConfigList)
	raw, _ := json.Marshal(in)
	_ = json.Unmarshal(raw, out)
	return out
}
func (in *RuntimeProviderConfigList) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *RuntimeWorkspaceProfile) DeepCopy() *RuntimeWorkspaceProfile {
	if in == nil {
		return nil
	}
	out := new(RuntimeWorkspaceProfile)
	raw, _ := json.Marshal(in)
	_ = json.Unmarshal(raw, out)
	return out
}
func (in *RuntimeWorkspaceProfile) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *RuntimeWorkspaceProfileList) DeepCopy() *RuntimeWorkspaceProfileList {
	if in == nil {
		return nil
	}
	out := new(RuntimeWorkspaceProfileList)
	raw, _ := json.Marshal(in)
	_ = json.Unmarshal(raw, out)
	return out
}
func (in *RuntimeWorkspaceProfileList) DeepCopyObject() runtime.Object { return in.DeepCopy() }

type authorizedACPFixtureClient struct{ client.Client }

func (c authorizedACPFixtureClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if sar, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
		sar.Status.Allowed = true
		return nil
	}
	return c.Client.Create(ctx, obj, opts...)
}
func (c authorizedACPFixtureClient) RESTMapper() apimeta.RESTMapper {
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{acpFixtureGroupVersion})
	mapper.Add(acpFixtureGroupVersion.WithKind(acpWorkspaceProviderConfigKind), apimeta.RESTScopeRoot)
	mapper.Add(acpFixtureGroupVersion.WithKind(acpWorkspaceProviderProfileKind), apimeta.RESTScopeNamespace)
	return mapper
}
