package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/protobuf/proto"
)

//nolint:gocyclo // Keep ordered durable transitions and their failure boundaries visible together.
func (r *RuntimePoolReconciler) importNativeSubstrateCheckpoint(ctx context.Context, pool *corev1alpha1.RuntimePool, cm *corev1.ConfigMap, record *substrateNativeState) error {
	ref := pool.Spec.ExecutionWorkspace.Substrate.RestoreFrom
	if ref == nil || record.OriginDigest != "" {
		return nil
	}
	if !substrateRuntimePoolSuspendCapable(pool) || record.Attempt != nil || record.Checkpoint != nil {
		return fmt.Errorf("checkpoint restore can only seed a fresh Substrate DataOnly workspace")
	}
	artifactCM, artifact, err := r.readSubstrateCheckpointArtifact(ctx, ref.Digest)
	if err != nil {
		return err
	}
	if artifact == nil || artifact.Namespace != pool.Namespace || artifact.Atespace != record.Atespace || !reflect.DeepEqual(artifact.Runtime, pool.Spec.Runtime) {
		return fmt.Errorf("checkpoint is unavailable or differs from the namespace, infrastructure, or runtime profile")
	}
	ws := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.nativeSubstrateReader().Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Labels[acpExecutionWorkspaceLinkLabel]}, ws); err != nil {
		return err
	}
	if ws.UID == artifact.SourceWorkspace.UID || string(ws.UID) != pool.Annotations[acpExecutionWorkspaceUIDAnnotation] ||
		ws.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceControllerLabelValue ||
		ws.Spec.ClassBinding != artifact.ClassBinding || ws.Spec.ProviderBinding != artifact.ProviderBinding {
		return fmt.Errorf("checkpoint restore requires a new workspace with the exact source class and provider revisions")
	}
	owner := substratePoolCheckpointOwner(pool)
	if !artifact.Owners[owner] {
		checkpoint := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{}
		if err := r.nativeSubstrateReader().Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: ref.Name}, checkpoint); err != nil {
			return err
		}
		ready := meta.FindStatusCondition(checkpoint.Status.Conditions, substrateNativeReady)
		if string(checkpoint.UID) != ref.UID || checkpoint.Status.Digest != ref.Digest || !checkpoint.DeletionTimestamp.IsZero() ||
			checkpoint.Status.Phase != substrateNativeReady || ready == nil || ready.Status != "True" || ready.ObservedGeneration != checkpoint.Generation ||
			checkpoint.Spec.WorkspaceRef.UID == ws.UID || !reflect.DeepEqual(checkpoint.Status.ClassBinding, &artifact.ClassBinding) {
			return fmt.Errorf("restoreFrom does not identify a Ready checkpoint at the accepted UID and digest")
		}
		// A recovery export can retain imported data with older provenance.
		// Its acquired catalog owner binds this public checkpoint to that data.
		if err := r.acquireSubstrateCheckpointArtifact(ctx, artifactCM, artifact, substratePublicCheckpointOwner(checkpoint), owner); err != nil {
			return err
		}
	}
	api, err := r.substrateNativeClient()
	if err != nil {
		return err
	}
	defer api.Close() //nolint:errcheck
	if err := verifiedNativeSubstrateCheckpoint(ctx, api.Control, artifact.Atespace, &artifact.Checkpoint); err != nil {
		return err
	}
	if err := verifyNativeSubstrateTemplate(ctx, api.Control, artifact.Atespace, artifact.Checkpoint.Template); err != nil {
		return err
	}
	record.OriginDigest = ref.Digest
	checkpoint := artifact.Checkpoint
	record.Checkpoint = &checkpoint
	return r.saveNativeSubstrateState(ctx, cm, record)
}

func (r *RuntimePoolReconciler) verifyNativeSubstrateRestoreLayout(ctx context.Context, pool *corev1alpha1.RuntimePool, record *substrateNativeState, desired *unstructured.Unstructured) error {
	if record.OriginDigest == "" || record.Checkpoint == nil {
		return fmt.Errorf("checkpoint has no committed import intent")
	}
	_, artifact, err := r.readSubstrateCheckpointArtifact(ctx, record.Checkpoint.Digest)
	if err != nil {
		return err
	}
	if artifact == nil || !artifact.Owners[substratePoolCheckpointOwner(pool)] || artifact.Atespace != record.Atespace {
		return fmt.Errorf("checkpoint restore has no retained Data reference")
	}
	source := &unstructured.Unstructured{}
	if err := json.Unmarshal(artifact.Manifest, &source.Object); err != nil {
		return err
	}
	oldTemplate, err := nativeSubstrateRuntimeTemplate(source)
	if err != nil {
		return err
	}
	newTemplate, err := nativeSubstrateRuntimeTemplate(desired)
	if err != nil {
		return err
	}
	// Pool/Session identifiers and public bootstrap keys necessarily change on
	// a fork. Runtime image/profile, sandbox, volumes, mounts, resources, and
	// process security must be identical. Native UpdateActor adds its own CAS
	// and compatible-data-restore validation before the new Actor can boot.
	normalize := func(template *ateapipb.ActorTemplate) *ateapipb.ActorTemplate {
		out := nativeSubstrateTemplateSpec(template)
		out.Metadata = nil
		for _, container := range out.Containers {
			container.Env = nil
		}
		return out
	}
	if !proto.Equal(normalize(oldTemplate), normalize(newTemplate)) {
		return fmt.Errorf("checkpoint restore would change the admitted runtime or durable volume layout")
	}
	return nil
}
