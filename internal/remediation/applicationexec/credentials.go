package applicationexec

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type credentialReceipt struct {
	PodUID       types.UID `json:"podUID"`
	ActorsDigest string    `json:"actorsDigest"`
	Attempted    bool      `json:"attempted"`
	SecretKey    string    `json:"secretKey"`
}

func actorSecretName(role string) string { return jobName(role) + "-actors" }

// Bound TokenRequests are required: projecting the observer's own SA token
// repeatedly under different file names would not create alternate identities.
// Tokens are delivered only through a protected-namespace Secret bound to this
// exact observer Pod. They never enter Task specs, control ledgers or reports.
func (b *Backend) ensureActorCredentials(ctx context.Context, r *run, side, role string,
	pod *corev1.Pod, actors []pv.ApplicationActorIdentity) error {
	if len(actors) == 0 {
		return nil
	}
	protected := observerSide(r, side)
	key := resourceKey(protected, role+"-actors")
	content, _ := json.Marshal(actors)
	actorsDigest := pv.Digest(content)
	if previous, found := r.state.Credentials[key]; found {
		return b.checkActorCredentials(ctx, r, key, pod, actors, actorsDigest, previous)
	}
	if pod.UID == "" || pod.Namespace != namespaceName(r, protected) || !pod.DeletionTimestamp.IsZero() {
		return contract.ErrConflict
	}
	if err := b.authorizeEffect(ctx, r); err != nil {
		return err
	}
	if err := b.receiptFinalizer(ctx, pod, true); err != nil {
		return err
	}
	r.state.Credentials[key] = credentialReceipt{PodUID: pod.UID, ActorsDigest: actorsDigest, Attempted: true, SecretKey: key}
	if err := b.save(ctx, r); err != nil {
		return err
	}
	data := make(map[string][]byte, len(actors))
	for _, actor := range actors {
		if actor.Namespace != pod.Namespace {
			return contract.ErrConflict
		}
		account := &corev1.ServiceAccount{}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: actor.Namespace, Name: actor.ServiceAccountName}, account); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		if string(account.UID) != actor.ServiceAccountUID || !account.DeletionTimestamp.IsZero() {
			return contract.ErrConflict
		}
		issued, err := b.targetKube.CoreV1().ServiceAccounts(actor.Namespace).CreateToken(ctx, actor.ServiceAccountName,
			&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(600)),
				BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: podKind, Name: pod.Name, UID: pod.UID}}},
			metav1.CreateOptions{})
		if err != nil {
			return safeError(ErrUncertain, err)
		}
		if issued.Status.Token == "" || len(issued.Status.Token) > 16<<10 ||
			!issued.Status.ExpirationTimestamp.After(time.Now()) ||
			issued.Status.ExpirationTimestamp.After(time.Now().Add(11*time.Minute)) {
			return contract.ErrConflict
		}
		data[actor.ID] = []byte(issued.Status.Token)
	}
	current := &corev1.Pod{}
	if err := b.targetReader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if current.UID != pod.UID || !current.DeletionTimestamp.IsZero() || !reflect.DeepEqual(current.Spec, pod.Spec) {
		return contract.ErrConflict
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: actorSecretName(role), Namespace: pod.Namespace,
		Labels: map[string]string{roleLabel: "actor-credentials"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: podKind, Name: pod.Name, UID: pod.UID,
			Controller: new(true), BlockOwnerDeletion: new(true)}}},
		Immutable: new(true), Type: corev1.SecretTypeOpaque, Data: data}
	_, err := b.ensureObject(ctx, r, key, secret)
	return err
}

func (b *Backend) checkActorCredentials(ctx context.Context, r *run, key string, pod *corev1.Pod,
	actors []pv.ApplicationActorIdentity, digest string, receipt credentialReceipt) error {
	if receipt.PodUID != pod.UID || receipt.ActorsDigest != digest || receipt.SecretKey != key {
		return contract.ErrConflict
	}
	if r.effect(key) == nil {
		return ErrUncertain
	}
	object, err := b.boundObject(ctx, r, key)
	if err != nil {
		return err
	}
	secret := object.(*corev1.Secret)
	if !ptr.Deref(secret.Immutable, false) || !targetOwnerMatches(r, secret) || len(secret.Data) != len(actors) {
		return contract.ErrConflict
	}
	for _, actor := range actors {
		if len(secret.Data[actor.ID]) == 0 {
			return contract.ErrConflict
		}
	}
	return nil
}

func targetOwnerMatches(r *run, object client.Object) bool {
	secret, ok := object.(*corev1.Secret)
	if !ok || secret.Labels[roleLabel] != "actor-credentials" || len(secret.OwnerReferences) != 1 {
		return false
	}
	owner := secret.OwnerReferences[0]
	if owner.APIVersion != "v1" || owner.Kind != podKind || owner.Controller == nil || !*owner.Controller || owner.UID == "" {
		return false
	}
	for _, receipt := range r.state.Pods {
		if receipt.Namespace == secret.Namespace && receipt.Identity.PodName == owner.Name &&
			receipt.Identity.PodUID == string(owner.UID) && receipt.Identity.ContainerName == workerModeObserve &&
			receipt.Identity.ServiceAccountName == observerAccountName {
			return true
		}
	}
	return false
}
