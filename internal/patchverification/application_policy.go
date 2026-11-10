package patchverification

import verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"

type ApplicationPolicy = verificationv1.ApplicationPolicy

type ApplicationResourceRule = verificationv1.ApplicationResourceRule

type ApplicationCRDIdentity = verificationv1.ApplicationCRDIdentity

type ApplicationActorPolicy = verificationv1.ApplicationActorPolicy

type ApplicationFixtureProfile = verificationv1.ApplicationFixtureProfile

type ApplicationReplicaTargetProfile = verificationv1.ApplicationReplicaTargetProfile

type ApplicationSubjectTLSBundle = verificationv1.ApplicationSubjectTLSBundle

type ApplicationSubjectTLSIdentity = verificationv1.ApplicationSubjectTLSIdentity

type ApplicationFixtureSecretFile = verificationv1.ApplicationFixtureSecretFile

type ApplicationFixtureSecretIdentity struct {
	ID           string `json:"id"`
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Key          string `json:"key"`
	Digest       string `json:"digest"`
}

type ApplicationSyntheticSecret = verificationv1.ApplicationSyntheticSecret

type ApplicationSyntheticSecretKey = verificationv1.ApplicationSyntheticSecretKey

type ApplicationAction = verificationv1.ApplicationAction

type ApplicationActorIdentity = verificationv1.ApplicationActorIdentity

type ApplicationActionObservation = verificationv1.ApplicationActionObservation

type ApplicationActionTarget = verificationv1.ApplicationActionTarget

type ApplicationProtectedMarkerRef = verificationv1.ApplicationProtectedMarkerRef

type ApplicationServiceIdentity struct {
	Namespace    string            `json:"namespace"`
	NamespaceUID string            `json:"namespaceUID"`
	Name         string            `json:"name"`
	UID          string            `json:"uid"`
	Port         int               `json:"port"`
	Selector     map[string]string `json:"selector"`
}
