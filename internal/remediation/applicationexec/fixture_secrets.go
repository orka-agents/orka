package applicationexec

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func (b *Backend) verifyFixtureSecretFiles(ctx context.Context, r *run, side string) error {
	policy := r.state.Request.ApplicationPolicy
	if policy == nil {
		return nil
	}
	for _, fixture := range r.state.Request.Checks.Application.Fixtures {
		if fixture.Kind != fixtureKindImage {
			continue
		}
		var profile *pv.ApplicationFixtureProfile
		for i := range policy.Fixtures {
			if policy.Fixtures[i].ID == fixture.Profile {
				profile = &policy.Fixtures[i]
			}
		}
		if profile == nil {
			return contract.ErrConflict
		}
		if len(profile.SecretFiles) == 0 {
			continue
		}
		object, err := b.boundObject(ctx, r, resourceKey(protectedSide(side), "fixture-secret-"+fixture.ID))
		if err != nil {
			return err
		}
		copy := object.(*corev1.Secret)
		if !ptr.Deref(copy.Immutable, false) || len(copy.Data) != len(profile.SecretFiles) {
			return contract.ErrConflict
		}
		for _, selection := range profile.SecretFiles {
			object, err := b.boundObject(ctx, r, resourceKey(side, "synthetic-secret-"+selection.SecretName))
			if err != nil {
				return err
			}
			source := object.(*corev1.Secret)
			if copy.Annotations["applicationexec.orka.ai/source-"+selection.ID] != string(source.UID) ||
				len(source.Data[selection.Key]) != 64 || !bytes.Equal(copy.Data[selection.ID], source.Data[selection.Key]) {
				return contract.ErrConflict
			}
		}
	}
	return nil
}

func (b *Backend) fixtureSecretFiles(ctx context.Context, r *run, side string, fixture pv.ApplicationFixture,
	profile *pv.ApplicationFixtureProfile) ([]pv.ApplicationFixtureSecretIdentity, error) {
	if profile == nil || len(profile.SecretFiles) == 0 {
		return nil, nil
	}
	if profile.ReplicaTarget != nil || fixture.Kind != fixtureKindImage {
		return nil, contract.ErrInvalid
	}
	physicalSide := protectedSide(side)
	namespace := r.effect(resourceKey(physicalSide, "namespace"))
	if namespace == nil || namespace.UID == "" {
		return nil, contract.ErrConflict
	}
	data := make(map[string][]byte)
	origins := make(map[string]string)
	for _, selection := range profile.SecretFiles {
		declared := false
		for _, definition := range r.state.Request.Checks.Application.SyntheticSecrets {
			for _, key := range definition.Keys {
				declared = declared || definition.Name == selection.SecretName && key.Name == selection.Key
			}
		}
		if !declared {
			return nil, contract.ErrInvalid
		}
		object, err := b.boundObject(ctx, r, resourceKey(side, "synthetic-secret-"+selection.SecretName))
		if err != nil {
			return nil, err
		}
		source := object.(*corev1.Secret)
		value, found := source.Data[selection.Key]
		if !found || len(value) != 64 || !ptr.Deref(source.Immutable, false) || source.Type != corev1.SecretTypeOpaque {
			return nil, contract.ErrConflict
		}
		data[selection.ID] = slices.Clone(value)
		origins["applicationexec.orka.ai/source-"+selection.ID] = string(source.UID)
	}
	name := "orka-fixture-files-" + strings.TrimPrefix(pv.Digest([]byte(fixture.ID)), "sha256:")[:24]
	expected := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, Annotations: origins},
		Immutable: new(true), Type: corev1.SecretTypeOpaque, Data: data}
	object, err := b.ensureObject(ctx, r, resourceKey(physicalSide, "fixture-secret-"+fixture.ID), expected)
	if err != nil {
		return nil, err
	}
	secret := object.(*corev1.Secret)
	if !reflect.DeepEqual(secret.Annotations, origins) || !reflect.DeepEqual(secret.Data, data) || !ptr.Deref(secret.Immutable, false) {
		return nil, contract.ErrConflict
	}
	var identities []pv.ApplicationFixtureSecretIdentity
	for _, selection := range profile.SecretFiles {
		identities = append(identities, pv.ApplicationFixtureSecretIdentity{ID: selection.ID, Namespace: namespace.Name,
			NamespaceUID: string(namespace.UID), Name: secret.Name, UID: string(secret.UID), Key: selection.ID,
			Digest: pv.Digest(secret.Data[selection.ID])})
	}
	return identities, nil
}
