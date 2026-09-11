package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	substrateCatalogLabel     = "orka.ai/substrate-checkpoint-catalog"
	substrateCatalogFinalizer = "orka.ai/substrate-checkpoint-data"
	substrateCatalogKey       = "checkpoint.json"
)

// References are committed by Kubernetes CAS before a source releases its
// reference. The deleting marker closes acquisition before native GC starts.
// This is an Orka ownership protocol, not native lifecycle fencing.
type substrateCheckpointArtifact struct {
	Schema          string                                    `json:"schema"`
	Atespace        string                                    `json:"atespace"`
	Checkpoint      substrateNativeCheckpoint                 `json:"checkpoint"`
	Namespace       string                                    `json:"namespace"`
	SourceWorkspace workspacev1alpha1.ObjectIdentityReference `json:"sourceWorkspace"`
	SourcePool      workspacev1alpha1.ObjectIdentityReference `json:"sourcePool"`
	ClassBinding    workspacev1alpha1.ImmutableObjectBinding  `json:"classBinding"`
	ProviderBinding workspacev1alpha1.ImmutableObjectBinding  `json:"providerBinding"`
	Runtime         corev1alpha1.RuntimePoolRuntimeSpec       `json:"runtime"`
	Manifest        json.RawMessage                           `json:"manifest"`
	Owners          map[string]bool                           `json:"owners"`
	Deleting        bool                                      `json:"deleting,omitempty"`
}

func substratePoolCheckpointOwner(pool *corev1alpha1.RuntimePool) string {
	return "pool:" + pool.Namespace + "/" + pool.Name + ":" + string(pool.UID)
}

func substratePublicCheckpointOwner(checkpoint *workspacev1alpha1.ExecutionWorkspaceCheckpoint) string {
	return "checkpoint:" + checkpoint.Namespace + "/" + checkpoint.Name + ":" + string(checkpoint.UID)
}

func (r *RuntimePoolReconciler) nativeSubstrateReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *RuntimePoolReconciler) readSubstrateCheckpointArtifact(ctx context.Context, digest string) (*corev1.ConfigMap, *substrateCheckpointArtifact, error) {
	if !validSHA256Digest(digest) {
		return nil, nil, fmt.Errorf("checkpoint digest is invalid")
	}
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: controllerNamespaceForRuntimePool(r.ControllerNamespace), Name: "substrate-data-" + strings.TrimPrefix(digest, "sha256:")[:40]}
	if err := r.nativeSubstrateReader().Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}, nil, nil
		}
		return nil, nil, err
	}
	artifact := &substrateCheckpointArtifact{}
	if err := json.Unmarshal([]byte(cm.Data[substrateCatalogKey]), artifact); err != nil || artifact.Schema != "orka.substrate-checkpoint.v1" ||
		artifact.Checkpoint.Digest != digest || artifact.Atespace == "" || artifact.Namespace == "" || artifact.Checkpoint.Template.UID == "" ||
		cm.Labels[substrateCatalogLabel] != substrateOwnedLabelValue || cm.Labels[runtimePoolManagedByLabel] != runtimePoolManagedByLabelValue {
		return nil, nil, fmt.Errorf("checkpoint catalog identity or schema is invalid")
	}
	return cm, artifact, nil
}

func (r *RuntimePoolReconciler) saveSubstrateCheckpointArtifact(ctx context.Context, cm *corev1.ConfigMap, artifact *substrateCheckpointArtifact) error {
	data, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	if len(data) > 512<<10 {
		return fmt.Errorf("checkpoint catalog exceeds its record limit")
	}
	cm.Data = map[string]string{substrateCatalogKey: string(data)}
	if cm.ResourceVersion == "" {
		cm.Labels = map[string]string{substrateCatalogLabel: substrateOwnedLabelValue, runtimePoolManagedByLabel: runtimePoolManagedByLabelValue}
		cm.Finalizers = []string{substrateCatalogFinalizer}
		return r.Create(ctx, cm)
	}
	return r.Update(ctx, cm)
}

func (r *RuntimePoolReconciler) registerSubstrateCheckpointArtifact(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, record *substrateNativeState, checkpoint *substrateNativeCheckpoint) error {
	cm, artifact, err := r.readSubstrateCheckpointArtifact(ctx, checkpoint.Digest)
	if err != nil {
		return err
	}
	owner := substratePoolCheckpointOwner(pool)
	if artifact != nil {
		if checkpoint.CreatedAt.IsZero() {
			// A catalog commit can precede a failed journal write. Recover its
			// original completion time, including captures from older journals.
			checkpoint.CreatedAt = artifact.Checkpoint.CreatedAt
		}
		if artifact.Deleting || !cm.DeletionTimestamp.IsZero() || artifact.Atespace != record.Atespace || !reflect.DeepEqual(artifact.Checkpoint, *checkpoint) || !artifact.Owners[owner] {
			return fmt.Errorf("checkpoint catalog conflicts with its capture intent")
		}
		return nil
	}
	_, binding, err := (&nativeSubstrateTemplateStore{r: r}).read(ctx, record.Atespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return err
	}
	if binding == nil || binding.OwnerUID != string(pool.UID) || binding.Current != checkpoint.Template {
		return fmt.Errorf("checkpoint source template binding is not available")
	}
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = metav1.NewTime(r.now())
	}
	artifact = &substrateCheckpointArtifact{Schema: "orka.substrate-checkpoint.v1", Atespace: record.Atespace, Namespace: pool.Namespace, Checkpoint: *checkpoint, Runtime: pool.Spec.Runtime, Manifest: binding.Manifest, Owners: map[string]bool{owner: true}}
	artifact.SourcePool = workspacev1alpha1.ObjectIdentityReference{Name: pool.Name, UID: pool.UID}
	if name := pool.Labels[acpExecutionWorkspaceLinkLabel]; name != "" {
		ws := &workspacev1alpha1.ExecutionWorkspace{}
		if err := r.nativeSubstrateReader().Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: name}, ws); err != nil {
			return err
		}
		if string(ws.UID) != pool.Annotations[acpExecutionWorkspaceUIDAnnotation] {
			return fmt.Errorf("checkpoint source workspace lifetime changed")
		}
		artifact.SourceWorkspace = workspacev1alpha1.ObjectIdentityReference{Name: ws.Name, UID: ws.UID}
		artifact.ClassBinding, artifact.ProviderBinding = ws.Spec.ClassBinding, ws.Spec.ProviderBinding
	}
	return r.saveSubstrateCheckpointArtifact(ctx, cm, artifact)
}

func (r *RuntimePoolReconciler) acquireSubstrateCheckpointArtifact(ctx context.Context, cm *corev1.ConfigMap, artifact *substrateCheckpointArtifact, fromOwner, owner string) error {
	if artifact == nil || artifact.Deleting || !cm.DeletionTimestamp.IsZero() {
		return fmt.Errorf("checkpoint is being collected")
	}
	if artifact.Owners[owner] {
		return nil
	}
	if !artifact.Owners[fromOwner] {
		return fmt.Errorf("checkpoint source released its data before acquisition")
	}
	if len(artifact.Owners) >= 2048 {
		return fmt.Errorf("checkpoint has reached its reference limit")
	}
	artifact.Owners[owner] = true
	return r.saveSubstrateCheckpointArtifact(ctx, cm, artifact)
}

func (r *RuntimePoolReconciler) releaseSubstrateCheckpointArtifact(ctx context.Context, digest, owner string) error {
	cm, artifact, err := r.readSubstrateCheckpointArtifact(ctx, digest)
	if err != nil || artifact == nil {
		return err
	}
	if !artifact.Owners[owner] {
		return nil
	}
	delete(artifact.Owners, owner)
	if len(artifact.Owners) == 0 {
		artifact.Deleting = true
	}
	return r.saveSubstrateCheckpointArtifact(ctx, cm, artifact)
}

func (r *RuntimePoolReconciler) listSubstrateCheckpointArtifacts(ctx context.Context) ([]corev1.ConfigMap, error) {
	list := &corev1.ConfigMapList{}
	err := r.nativeSubstrateReader().List(ctx, list, client.InNamespace(controllerNamespaceForRuntimePool(r.ControllerNamespace)), client.MatchingLabels{substrateCatalogLabel: substrateOwnedLabelValue})
	return list.Items, err
}

func (r *RuntimePoolReconciler) substrateNativeTemplateReferenced(ctx context.Context, atespace string, revision substrateNativeTemplateRevision, excluding string) (bool, error) {
	items, err := r.listSubstrateCheckpointArtifacts(ctx)
	if err != nil {
		return false, err
	}
	for _, cm := range items {
		if cm.Name == excluding {
			continue
		}
		artifact := &substrateCheckpointArtifact{}
		if err := json.Unmarshal([]byte(cm.Data[substrateCatalogKey]), artifact); err != nil {
			return false, fmt.Errorf("unreadable checkpoint reference during template cleanup")
		}
		if artifact.Atespace == atespace && artifact.Checkpoint.Template.UID == revision.UID {
			return true, nil
		}
	}
	return false, nil
}

// Native cleanup can resume after either deletion call loses its response.
// Do not accept new owners once Deleting is persisted.
func (r *RuntimePoolReconciler) collectSubstrateCheckpointArtifact(ctx context.Context, cm *corev1.ConfigMap, artifact *substrateCheckpointArtifact) error {
	if len(artifact.Owners) != 0 {
		return nil
	}
	if !artifact.Deleting {
		artifact.Deleting = true
		return r.saveSubstrateCheckpointArtifact(ctx, cm, artifact)
	}
	api, err := r.substrateNativeClient()
	if err != nil {
		return err
	}
	defer api.Close() //nolint:errcheck
	err = verifiedNativeSubstrateCheckpoint(ctx, api.Control, artifact.Atespace, &artifact.Checkpoint)
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}
	if err == nil {
		_, err = api.Control.DeleteTag(ctx, &ateapipb.DeleteTagRequest{Tag: &ateapipb.ObjectRef{Atespace: artifact.Atespace, Name: artifact.Checkpoint.Name}})
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
	}
	// Template ownership records outlive the source pool until the final
	// catalog reference disappears. Their separate collector retries on restart.
	controllerutil.RemoveFinalizer(cm, substrateCatalogFinalizer)
	if err := r.Update(ctx, cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	return client.IgnoreNotFound(r.Delete(ctx, cm, deleteCurrentObjectPreconditions(cm)...))
}
