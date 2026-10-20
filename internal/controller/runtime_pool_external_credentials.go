// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const externalPrivateCredentialIntentsAnnotation = "orka.ai/external-private-credential-intents"

type externalPrivateCredentialIntent struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Role         string    `json:"role"`
	Epoch        int64     `json:"epoch"`
	DataSHA256   string    `json:"dataSHA256"`
	CreateIssued bool      `json:"createIssued"`
	UID          types.UID `json:"uid,omitempty"`
}

func externalCredentialDataDigest(data map[string][]byte) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		panic("marshal private credential data")
	}
	return store.CanonicalBytesDigest(encoded)
}

func externalCredentialIntents(pool *corev1alpha1.RuntimePool) ([]externalPrivateCredentialIntent, error) {
	encoded := pool.Annotations[externalPrivateCredentialIntentsAnnotation]
	if encoded == "" {
		return nil, nil
	}
	var intents []externalPrivateCredentialIntent
	if err := json.Unmarshal([]byte(encoded), &intents); err != nil {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	seen := map[types.NamespacedName]bool{}
	for _, intent := range intents {
		key := types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}
		if len(validation.IsDNS1123Label(intent.Namespace)) != 0 || !validSHA256Digest(intent.DataSHA256) || intent.Epoch < 1 || seen[key] || (intent.UID != "" && !intent.CreateIssued) {
			return nil, workspaceprovider.ErrStaleIdentity
		}
		suffix := ""
		switch intent.Role {
		case runtimePoolAuthLabel:
			suffix = runtimePoolAuthSuffixPattern.FindString(intent.Name)
			nameEpoch, _, _ := strings.Cut(strings.TrimPrefix(suffix, "auth-e"), "-")
			if nameEpoch != strconv.FormatInt(intent.Epoch, 10) {
				return nil, workspaceprovider.ErrStaleIdentity
			}
		case runtimePoolProviderCredentialLabel:
			suffix = runtimePoolProviderSuffixPattern.FindString(intent.Name)
			if !strings.HasPrefix(suffix, "provider-e"+strconv.FormatInt(intent.Epoch, 10)+"-g") {
				return nil, workspaceprovider.ErrStaleIdentity
			}
		default:
			return nil, workspaceprovider.ErrStaleIdentity
		}
		if suffix == "" || runtimePoolChildName(runtimePoolResourceName(pool.Namespace, pool.Name), suffix) != intent.Name {
			return nil, workspaceprovider.ErrStaleIdentity
		}
		seen[key] = true
	}
	return intents, nil
}

func (r *RuntimePoolReconciler) saveExternalCredentialIntents(ctx context.Context, pool *corev1alpha1.RuntimePool, intents []externalPrivateCredentialIntent) error {
	encoded, err := json.Marshal(intents)
	if err != nil {
		return err
	}
	before := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	if len(intents) == 0 {
		delete(pool.Annotations, externalPrivateCredentialIntentsAnnotation)
	} else {
		pool.Annotations[externalPrivateCredentialIntentsAnnotation] = string(encoded)
	}
	return r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func externalCredentialSecretMatches(pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, intent externalPrivateCredentialIntent, secret *corev1.Secret) bool {
	if secret.Namespace != intent.Namespace || secret.Name != intent.Name || secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable ||
		(intent.UID != "" && intent.UID != secret.UID) || !externalCoreResourceOwned(pool, cfg, secret) || !runtimePoolManagedCredentialSecret(secret, cfg) ||
		externalCredentialDataDigest(secret.Data) != intent.DataSHA256 {
		return false
	}
	if epoch := secret.Labels[runtimePoolCredentialEpochLabel]; epoch != "" && epoch != strconv.FormatInt(intent.Epoch, 10) {
		return false
	}
	if intent.Role == runtimePoolAuthLabel {
		return secret.Labels[runtimePoolAuthLabel] == booleanTrueValue && secret.Labels[runtimePoolProviderCredentialLabel] == ""
	}
	providerSuffix := runtimePoolProviderSuffixPattern.FindString(secret.Name)
	generation, _, _ := strings.Cut(strings.TrimPrefix(providerSuffix, "provider-e"+strconv.FormatInt(intent.Epoch, 10)+"-g"), "-")
	return secret.Labels[runtimePoolAuthLabel] == "" && secret.Labels[runtimePoolProviderCredentialLabel] == booleanTrueValue &&
		generation == runtimePoolProviderTokenGeneration(secret.Data[runtimePoolProviderTokenKey]) && secret.Labels[runtimePoolProviderGenerationLabel] == generation
}

func (r *RuntimePoolReconciler) bindExternalCredentialUID(ctx context.Context, pool *corev1alpha1.RuntimePool, intents []externalPrivateCredentialIntent, index int, secret *corev1.Secret) error {
	if secret.UID == "" || !intents[index].CreateIssued || (intents[index].UID != "" && intents[index].UID != secret.UID) {
		return workspaceprovider.ErrStaleIdentity
	}
	if intents[index].UID == secret.UID {
		return nil
	}
	intents[index].UID = secret.UID
	return r.saveExternalCredentialIntents(ctx, pool, intents)
}

func (r *RuntimePoolReconciler) recoverExternalCredential(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, intents []externalPrivateCredentialIntent, index int) (*corev1.Secret, error) {
	intent := intents[index]
	secret := &corev1.Secret{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) && !intent.CreateIssued {
			return nil, nil
		}
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("private credential creation or identity is unresolved: %w", workspaceprovider.ErrStaleIdentity)
		}
		return nil, err
	}
	if !intent.CreateIssued {
		// A never-issued intent grants no authority over an existing object.
		return nil, nil
	}
	if !secret.DeletionTimestamp.IsZero() || !externalCredentialSecretMatches(pool, cfg, intent, secret) {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	if err := r.bindExternalCredentialUID(ctx, pool, intents, index, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func externalCredentialCreateRejected(err error) bool {
	return apierrors.IsAlreadyExists(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsBadRequest(err) || apierrors.IsNotFound(err)
}

func (r *RuntimePoolReconciler) createExternalCredential(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, role string) (*corev1.Secret, error) {
	intents, err := externalCredentialIntents(pool)
	if err != nil {
		return nil, err
	}
	suffix, err := r.randomHex(12)
	if err != nil {
		return nil, err
	}
	epoch := strconv.FormatInt(cfg.controllerEpoch, 10)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Labels: mergeStringMap(cloneStringMap(cfg.labels), map[string]string{role: booleanTrueValue, runtimePoolCredentialEpochLabel: epoch})},
		Type: corev1.SecretTypeOpaque, Immutable: new(true), Data: map[string][]byte{}}
	if role == runtimePoolAuthLabel {
		secret.Name = runtimePoolChildName(cfg.baseName, "auth-e"+epoch+"-"+suffix)
		for _, key := range []string{runtimePoolControllerTokenKey, runtimePoolCapabilitySecretKey, runtimePoolBootstrapNonceKey, runtimePoolBootstrapSigningSeedKey} {
			value, err := r.randomSecret(32)
			if err != nil {
				return nil, err
			}
			secret.Data[key] = []byte(value)
		}
	} else {
		secret.Name = runtimePoolChildName(cfg.baseName, "provider-e"+epoch+"-g"+cfg.providerProxy.tokenGeneration+"-"+suffix)
		secret.Labels[runtimePoolProviderGenerationLabel] = cfg.providerProxy.tokenGeneration
		secret.Data[runtimePoolProviderTokenKey] = append([]byte(nil), cfg.providerProxy.token...)
	}
	if err := r.setRuntimePoolControllerReference(pool, secret); err != nil {
		return nil, err
	}
	intents = append(intents, externalPrivateCredentialIntent{Namespace: secret.Namespace, Name: secret.Name, Role: role, Epoch: cfg.controllerEpoch, DataSHA256: externalCredentialDataDigest(secret.Data)})
	index := len(intents) - 1
	if err := r.saveExternalCredentialIntents(ctx, pool, intents); err != nil {
		return nil, err
	}
	intents[index].CreateIssued = true
	if err := r.saveExternalCredentialIntents(ctx, pool, intents); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		if externalCredentialCreateRejected(err) {
			// The API affirmatively rejected this call without creating our
			// object. Persist that no-effect receipt before any retry/reselection.
			intents[index].CreateIssued = false
			if fenceErr := r.saveExternalCredentialIntents(ctx, pool, intents); fenceErr != nil {
				return nil, fenceErr
			}
		}
		return nil, err
	}
	if !externalCredentialSecretMatches(pool, cfg, intents[index], secret) {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	if err := r.bindExternalCredentialUID(ctx, pool, intents, index, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (r *RuntimePoolReconciler) recordKnownExternalAuthCredential(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, secret *corev1.Secret, epoch int64) error {
	intents, err := externalCredentialIntents(pool)
	if err != nil {
		return err
	}
	for index, intent := range intents {
		if intent.Namespace == secret.Namespace && intent.Name == secret.Name {
			if !externalCredentialSecretMatches(pool, cfg, intent, secret) {
				return workspaceprovider.ErrStaleIdentity
			}
			return r.bindExternalCredentialUID(ctx, pool, intents, index, secret)
		}
	}
	intent := externalPrivateCredentialIntent{Namespace: secret.Namespace, Name: secret.Name, Role: runtimePoolAuthLabel, Epoch: epoch, DataSHA256: externalCredentialDataDigest(secret.Data), CreateIssued: true, UID: secret.UID}
	if secret.UID == "" || !externalCredentialSecretMatches(pool, cfg, intent, secret) {
		return workspaceprovider.ErrStaleIdentity
	}
	return r.saveExternalCredentialIntents(ctx, pool, append(intents, intent))
}

// A legacy bootstrap binding may outlive the epoch's name binding during cold
// resume. Its saved Secret UID, rather than any public label, is the authority.
func (r *RuntimePoolReconciler) recordLegacyBootstrapCredential(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) error {
	bootstrap, err := runtimePoolBootstrapInstanceBindingFromAnnotation(pool)
	if err != nil || bootstrap == nil {
		return err
	}
	intents, err := externalCredentialIntents(pool)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.UID == bootstrap.AuthSecretUID {
			return nil
		}
	}
	secrets := &corev1.SecretList{}
	if err := uncachedReader(r.APIReader, r.Client).List(ctx, secrets, client.InNamespace(cfg.namespace)); err != nil {
		return err
	}
	for index := range secrets.Items {
		secret := &secrets.Items[index]
		if secret.UID != bootstrap.AuthSecretUID {
			continue
		}
		suffix := runtimePoolAuthSuffixPattern.FindString(secret.Name)
		epochText, _, _ := strings.Cut(strings.TrimPrefix(suffix, "auth-e"), "-")
		epoch, err := strconv.ParseInt(epochText, 10, 64)
		if err != nil {
			return workspaceprovider.ErrStaleIdentity
		}
		return r.recordKnownExternalAuthCredential(ctx, pool, cfg, secret, epoch)
	}
	return nil
}

func (r *RuntimePoolReconciler) ensureExternalPrivateAuthCredential(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) (*corev1.Secret, error) {
	bindingKey := runtimePoolPrivateAuthSecretBindingAnnotation(cfg.controllerEpoch)
	if pool.Annotations[bindingKey] != "" {
		secret, err := r.boundPrivateWorkspaceRuntimePoolAuthSecret(ctx, pool, cfg, cfg.controllerEpoch)
		if err != nil {
			return nil, err
		}
		if err := r.recordKnownExternalAuthCredential(ctx, pool, cfg, secret, cfg.controllerEpoch); err != nil {
			return nil, err
		}
		return secret, nil
	}
	if err := r.recordLegacyBootstrapCredential(ctx, pool, cfg); err != nil {
		return nil, err
	}
	intents, err := externalCredentialIntents(pool)
	if err != nil {
		return nil, err
	}
	bootstrap, err := runtimePoolBootstrapInstanceBindingFromAnnotation(pool)
	if err != nil {
		return nil, err
	}
	for index, intent := range slices.Backward(intents) {

		if intent.Namespace != cfg.namespace || intent.Role != runtimePoolAuthLabel || intent.Epoch != cfg.controllerEpoch {
			continue
		}
		if bootstrap != nil && intent.UID == bootstrap.AuthSecretUID {
			w, err := r.externalPoolWorkspace(ctx, pool)
			if err != nil {
				return nil, err
			}
			if terminated, err := r.externalObservedInstanceTerminated(ctx, pool, w); err != nil || !terminated {
				return nil, workspaceprovider.ErrStaleIdentity
			}
			if pending, err := r.deleteExternalCredentialIntent(ctx, pool, cfg, intents, index); err != nil || pending {
				return nil, fmt.Errorf("private auth rotation awaits exact credential absence: %w", workspaceprovider.ErrStaleIdentity)
			}
			break
		}
		secret, err := r.recoverExternalCredential(ctx, pool, cfg, intents, index)
		if err != nil {
			return nil, err
		}
		if secret != nil {
			if err := r.bindPrivateRuntimePoolAuthSecret(ctx, pool, bindingKey, secret); err != nil {
				return nil, err
			}
			return secret, nil
		}
		if err := r.saveExternalCredentialIntents(ctx, pool, append(intents[:index:index], intents[index+1:]...)); err != nil {
			return nil, err
		}
		break
	}
	secret, err := r.createExternalCredential(ctx, pool, cfg, runtimePoolAuthLabel)
	if err != nil {
		return nil, err
	}
	if err := r.bindPrivateRuntimePoolAuthSecret(ctx, pool, bindingKey, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (r *RuntimePoolReconciler) ensureExternalPrivateProviderCredential(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) (*corev1.Secret, error) {
	intents, err := externalCredentialIntents(pool)
	if err != nil {
		return nil, err
	}
	digest := externalCredentialDataDigest(map[string][]byte{runtimePoolProviderTokenKey: cfg.providerProxy.token})
	for index, intent := range slices.Backward(intents) {

		if intent.Namespace != cfg.namespace || intent.Role != runtimePoolProviderCredentialLabel || intent.Epoch != cfg.controllerEpoch || intent.DataSHA256 != digest {
			continue
		}
		secret, err := r.recoverExternalCredential(ctx, pool, cfg, intents, index)
		if err != nil || secret != nil {
			return secret, err
		}
		if err := r.saveExternalCredentialIntents(ctx, pool, append(intents[:index:index], intents[index+1:]...)); err != nil {
			return nil, err
		}
		break
	}
	return r.createExternalCredential(ctx, pool, cfg, runtimePoolProviderCredentialLabel)
}

func (r *RuntimePoolReconciler) deleteExternalCredentialIntent(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, intents []externalPrivateCredentialIntent, index int) (bool, error) {
	intent := intents[index]
	secret := &corev1.Secret{}
	err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if intent.CreateIssued && intent.UID == "" && apierrors.IsNotFound(err) {
		return true, nil
	}
	if err == nil && intent.CreateIssued && (intent.UID == "" || secret.UID == intent.UID) {
		if !externalCredentialSecretMatches(pool, cfg, intent, secret) {
			return false, workspaceprovider.ErrStaleIdentity
		}
		if err := r.bindExternalCredentialUID(ctx, pool, intents, index, secret); err != nil {
			return false, err
		}
		pending, err := r.deleteExternalCoreResource(ctx, secret)
		if err != nil || pending {
			return pending, err
		}
	}
	// A rejected/never-issued call or exact recorded UID absence revokes only
	// this receipt, never ownership of a same-name foreign replacement.
	intents = append(intents[:index:index], intents[index+1:]...)
	return false, r.saveExternalCredentialIntents(ctx, pool, intents)
}

func (r *RuntimePoolReconciler) deleteExternalPrivateCredentials(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) (bool, error) {
	// Legacy auth bindings are exact Core UID authority. Legacy provider and
	// unbound auth inventory has no such authority and is left untouched.
	for annotation, binding := range pool.Annotations {
		if !strings.HasPrefix(annotation, runtimePoolPrivateAuthBindingPrefix) || binding == "" {
			continue
		}
		epoch, err := strconv.ParseInt(strings.TrimPrefix(annotation, runtimePoolPrivateAuthBindingPrefix), 10, 64)
		if err != nil || epoch < 1 {
			return false, workspaceprovider.ErrStaleIdentity
		}
		name, uid, err := parseRuntimePoolPrivateSecretBinding(binding)
		if err != nil {
			return false, err
		}
		secret := &corev1.Secret{}
		if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if secret.UID != uid {
			continue
		}
		if err := r.recordKnownExternalAuthCredential(ctx, pool, cfg, secret, epoch); err != nil {
			return false, err
		}
	}
	if err := r.recordLegacyBootstrapCredential(ctx, pool, cfg); err != nil {
		return false, err
	}
	intents, err := externalCredentialIntents(pool)
	if err != nil {
		return false, err
	}
	remaining := false
	for index := range slices.Backward(intents) {
		pending, err := r.deleteExternalCredentialIntent(ctx, pool, cfg, intents, index)
		if err != nil {
			return false, err
		}
		remaining = remaining || pending
		intents, err = externalCredentialIntents(pool)
		if err != nil {
			return false, err
		}
	}
	return remaining, nil
}
