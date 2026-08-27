package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func TestWorkspaceAttachmentManagerPersistsEpochAcrossReattach(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	firstTask := attachmentReviewTask(workspace.Namespace, "first-task")
	secondTask := attachmentReviewTask(workspace.Namespace, "second-task")
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, firstTask, secondTask).
		Build()
	manager := attachmentReviewManager(c)

	first, err := manager.Attach(ctx, workspace.DeepCopy(), firstTask)
	if err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if first.Epoch != 1 {
		t.Fatalf("first epoch = %d, want 1", first.Epoch)
	}
	leaseKey := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get first attachment Lease: %v", err)
	}
	if got := lease.Annotations[workspaceAttachmentLeaseEpochAnnotation]; got != "1" {
		t.Fatalf("first attachment Lease epoch = %q, want 1", got)
	}

	current := &workspacev1alpha1.ExecutionWorkspace{}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatalf("get first attached workspace: %v", err)
	}
	if current.Spec.AttachmentEpoch != 1 || current.Spec.Attachment == nil || current.Spec.Attachment.Epoch != 1 {
		t.Fatalf("first attachment state = %#v", current.Spec)
	}
	current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	current.Status.AttachedEpoch = first.Epoch
	apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:   string(workspacev1alpha1.ConditionWorkspaceAttached),
		Status: metav1.ConditionTrue,
		Reason: "Attached",
	})
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatalf("mark first attachment active: %v", err)
	}

	if err := manager.BeginRevocation(ctx, current, first.Epoch); err != nil {
		t.Fatalf("BeginRevocation: %v", err)
	}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatalf("get workspace after begin revocation: %v", err)
	}
	if current.Spec.Attachment != nil || current.Spec.AttachmentEpoch != first.Epoch {
		t.Fatalf("revoking attachment state = %#v, want cleared intent with epoch high-water %d", current.Spec, first.Epoch)
	}
	current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	current.Status.AttachedEpoch = 0
	apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:   string(workspacev1alpha1.ConditionWorkspaceAttached),
		Status: metav1.ConditionFalse,
		Reason: "Revoked",
	})
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatalf("mark first attachment revoked: %v", err)
	}
	if err := manager.FinalizeRevocation(ctx, current, first.Epoch, first.AttachmentRef.Name); err != nil {
		t.Fatalf("FinalizeRevocation: %v", err)
	}

	detached := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, key, detached); err != nil {
		t.Fatalf("get detached workspace: %v", err)
	}
	second, err := manager.Attach(ctx, detached, secondTask)
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}
	if second.Epoch != 2 {
		t.Fatalf("second epoch = %d, want 2", second.Epoch)
	}
	if second.AttachmentRef.Name == first.AttachmentRef.Name {
		t.Fatalf("attachment Secret name was reused: %q", second.AttachmentRef.Name)
	}
	if err := c.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get second attachment Lease: %v", err)
	}
	if got := lease.Annotations[workspaceAttachmentLeaseEpochAnnotation]; got != "2" {
		t.Fatalf("second attachment Lease epoch = %q, want 2", got)
	}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatalf("get second attached workspace: %v", err)
	}
	if current.Spec.AttachmentEpoch != 2 || current.Spec.Attachment == nil || current.Spec.Attachment.Epoch != 2 {
		t.Fatalf("second attachment state = %#v", current.Spec)
	}
}

func TestWorkspaceAttachmentManagerRejectsForeignLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "foreign-lease-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	task := attachmentReviewTask(workspace.Namespace, "foreign-lease-task")
	controller := true
	foreignLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workspace.Namespace,
			Name:      attachmentLeaseName(workspace.Name),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       "foreign-owner",
				UID:        types.UID("foreign-owner-uid"),
				Controller: &controller,
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task, foreignLease).
		Build()
	manager := attachmentReviewManager(c)

	result, err := manager.Attach(ctx, workspace.DeepCopy(), task)
	if result != nil || err == nil || !strings.Contains(err.Error(), "not controlled by workspace") {
		t.Fatalf("Attach = (%#v, %v), want foreign Lease ownership rejection", result, err)
	}
	currentLease := &coordinationv1.Lease{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreignLease), currentLease); err != nil {
		t.Fatalf("foreign Lease was deleted: %v", err)
	}
	owner := metav1.GetControllerOf(currentLease)
	if owner == nil || owner.UID != types.UID("foreign-owner-uid") || currentLease.Spec.HolderIdentity != nil {
		t.Fatalf("foreign Lease was mutated: %#v", currentLease)
	}
	currentWorkspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), currentWorkspace); err != nil {
		t.Fatalf("get workspace after rejected attachment: %v", err)
	}
	if currentWorkspace.Spec.Attachment != nil || currentWorkspace.Spec.AttachmentEpoch != 0 {
		t.Fatalf("foreign Lease admitted attachment intent: %#v", currentWorkspace.Spec)
	}
	secrets := &corev1.SecretList{}
	if err := c.List(ctx, secrets, client.InNamespace(workspace.Namespace)); err != nil {
		t.Fatalf("list attachment Secrets after rejected attachment: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("rejected attachment created Secrets: %#v", secrets.Items)
	}
}

func TestWorkspaceAttachmentManagerFinalizeRevocationUsesAPIReader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "finalize-reader-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	task := attachmentReviewTask(workspace.Namespace, "finalize-reader-task")
	baseClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task).
		Build()
	setupManager := attachmentReviewManager(baseClient)

	result, err := setupManager.Attach(ctx, workspace.DeepCopy(), task)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	workspaceKey := client.ObjectKeyFromObject(workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := baseClient.Get(ctx, workspaceKey, current); err != nil {
		t.Fatalf("get attached workspace: %v", err)
	}
	if err := setupManager.BeginRevocation(ctx, current, result.Epoch); err != nil {
		t.Fatalf("BeginRevocation: %v", err)
	}
	if err := baseClient.Get(ctx, workspaceKey, current); err != nil {
		t.Fatalf("get revoking workspace: %v", err)
	}
	current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	current.Status.AttachedEpoch = result.Epoch
	current.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue, Reason: "Attached",
	}}
	if err := baseClient.Status().Update(ctx, current); err != nil {
		t.Fatalf("mark attachment active: %v", err)
	}

	staleWorkspace := current.DeepCopy()
	staleWorkspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	staleWorkspace.Status.AttachedEpoch = 0
	staleWorkspace.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse, Reason: "Revoked",
	}}
	secretKey := types.NamespacedName{Namespace: workspace.Namespace, Name: result.AttachmentRef.Name}
	leaseKey := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	staleClient := &attachmentFinalizeStaleClient{
		Client:    baseClient,
		workspace: staleWorkspace,
		secretKey: secretKey,
		leaseKey:  leaseKey,
	}
	manager := WorkspaceAttachmentManager{Client: staleClient, APIReader: baseClient}

	if err := manager.FinalizeRevocation(ctx, staleWorkspace, result.Epoch, result.AttachmentRef.Name); err == nil {
		t.Fatal("FinalizeRevocation ignored authoritative attached status")
	}
	if err := baseClient.Get(ctx, secretKey, &corev1.Secret{}); err != nil {
		t.Fatalf("active attachment Secret was deleted: %v", err)
	}

	if err := baseClient.Get(ctx, workspaceKey, current); err != nil {
		t.Fatalf("get workspace before final revocation: %v", err)
	}
	current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	current.Status.AttachedEpoch = 0
	current.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse, Reason: "Revoked",
	}}
	if err := baseClient.Status().Update(ctx, current); err != nil {
		t.Fatalf("mark attachment revoked: %v", err)
	}
	legacyLease := &coordinationv1.Lease{}
	if err := baseClient.Get(ctx, leaseKey, legacyLease); err != nil {
		t.Fatalf("get attachment Lease before legacy migration: %v", err)
	}
	delete(legacyLease.Annotations, workspaceAttachmentLeaseEpochAnnotation)
	if err := baseClient.Update(ctx, legacyLease); err != nil {
		t.Fatalf("remove attachment Lease epoch marker: %v", err)
	}
	if err := manager.FinalizeRevocation(ctx, staleWorkspace, result.Epoch, result.AttachmentRef.Name); err != nil {
		t.Fatalf("FinalizeRevocation: %v", err)
	}
	if err := baseClient.Get(ctx, secretKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("attachment Secret still exists: %v", err)
	}
	if err := baseClient.Get(ctx, leaseKey, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("attachment Lease still exists: %v", err)
	}
}

func TestWorkspaceAttachmentManagerFinalizeRevocationPreservesNewerLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "finalize-race-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	firstTask := attachmentReviewTask(workspace.Namespace, "finalize-race-first-task")
	successorTask := attachmentReviewTask(workspace.Namespace, "finalize-race-successor-task")
	baseClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, firstTask, successorTask).
		Build()
	setupManager := attachmentReviewManager(baseClient)

	result, err := setupManager.Attach(ctx, workspace.DeepCopy(), firstTask)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	workspaceKey := client.ObjectKeyFromObject(workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := baseClient.Get(ctx, workspaceKey, current); err != nil {
		t.Fatalf("get attached workspace: %v", err)
	}
	if err := setupManager.BeginRevocation(ctx, current, result.Epoch); err != nil {
		t.Fatalf("BeginRevocation: %v", err)
	}
	if err := baseClient.Get(ctx, workspaceKey, current); err != nil {
		t.Fatalf("get revoking workspace: %v", err)
	}
	current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	current.Status.AttachedEpoch = 0
	current.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse, Reason: "Revoked",
	}}
	if err := baseClient.Status().Update(ctx, current); err != nil {
		t.Fatalf("mark attachment revoked: %v", err)
	}

	leaseKey := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	mutatingReader := &attachmentLeaseMutatingReader{
		Reader: baseClient,
		key:    leaseKey,
		mutate: func(ctx context.Context) error {
			lease := &coordinationv1.Lease{}
			if err := baseClient.Get(ctx, leaseKey, lease); err != nil {
				return err
			}
			holder := string(successorTask.UID)
			lease.Spec.HolderIdentity = &holder
			lease.Annotations[workspaceAttachmentLeaseEpochAnnotation] = "2"
			if err := baseClient.Update(ctx, lease); err != nil {
				return err
			}
			latest := &workspacev1alpha1.ExecutionWorkspace{}
			if err := baseClient.Get(ctx, workspaceKey, latest); err != nil {
				return err
			}
			latest.Spec.AttachmentEpoch = 2
			latest.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
				TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: successorTask.Name, UID: successorTask.UID},
				Epoch:   2,
			}
			return baseClient.Update(ctx, latest)
		},
	}
	manager := WorkspaceAttachmentManager{Client: baseClient, APIReader: mutatingReader}
	if err := manager.FinalizeRevocation(ctx, current, result.Epoch, result.AttachmentRef.Name); err == nil ||
		!strings.Contains(err.Error(), "belongs to epoch") {
		t.Fatalf("FinalizeRevocation with newer Lease = %v, want epoch-fence rejection", err)
	}
	if mutatingReader.mutationErr != nil {
		t.Fatalf("publish successor attachment during finalization: %v", mutatingReader.mutationErr)
	}

	lease := &coordinationv1.Lease{}
	if err := baseClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("newer attachment Lease was deleted: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(successorTask.UID) ||
		lease.Annotations[workspaceAttachmentLeaseEpochAnnotation] != "2" {
		t.Fatalf("newer attachment Lease = %#v, want successor epoch 2", lease)
	}
	if err := baseClient.Get(ctx, workspaceKey, current); err != nil {
		t.Fatalf("get successor workspace: %v", err)
	}
	if current.Spec.Attachment == nil || current.Spec.Attachment.Epoch != 2 ||
		current.Spec.Attachment.TaskRef.UID != successorTask.UID {
		t.Fatalf("successor attachment = %#v, want epoch 2 Task %q", current.Spec.Attachment, successorTask.UID)
	}
}

func TestWorkspaceAttachmentManagerBootstrapsLegacyActiveEpochOnRevocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "legacy-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	workspace.Status.AttachedEpoch = 7
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef:   workspacev1alpha1.ObjectIdentityReference{Name: "legacy-task", UID: types.UID("legacy-task-uid")},
		Epoch:     7,
		ExpiresAt: metav1.NewTime(time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)),
	}
	newTask := attachmentReviewTask(workspace.Namespace, "post-upgrade-task")
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, newTask).
		Build()
	manager := attachmentReviewManager(c)

	if err := manager.BeginRevocation(ctx, workspace.DeepCopy(), 7); err != nil {
		t.Fatalf("BeginRevocation: %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatalf("get legacy workspace after revocation began: %v", err)
	}
	if current.Spec.Attachment != nil || current.Spec.AttachmentEpoch != 7 {
		t.Fatalf("legacy revocation state = %#v, want epoch high-water 7 with no attachment", current.Spec)
	}
	current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	current.Status.AttachedEpoch = 0
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatalf("mark legacy attachment revoked: %v", err)
	}

	result, err := manager.Attach(ctx, current, newTask)
	if err != nil {
		t.Fatalf("Attach after legacy revocation: %v", err)
	}
	if result.Epoch != 8 {
		t.Fatalf("post-upgrade epoch = %d, want 8", result.Epoch)
	}
}

func TestWorkspaceAttachmentManagerStoresHeaderSafeBearerText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "token-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	task := attachmentReviewTask(workspace.Namespace, "token-task")
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task).
		Build()
	manager := attachmentReviewManager(c)

	result, err := manager.Attach(ctx, workspace.DeepCopy(), task)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: result.AttachmentRef.Name}, secret); err != nil {
		t.Fatalf("get attachment Secret: %v", err)
	}
	bearer := secret.Data[workspaceAttachmentTokenKey]
	decoded, err := base64.RawURLEncoding.DecodeString(string(bearer))
	if err != nil {
		t.Fatalf("bearer is not unpadded base64url text: %v", err)
	}
	if len(decoded) != workspaceAttachmentTokenEntropyBytes {
		t.Fatalf("decoded bearer entropy = %d bytes, want %d", len(decoded), workspaceAttachmentTokenEntropyBytes)
	}
	request, err := http.NewRequest(http.MethodGet, "https://workspace.invalid/v1/operations", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+string(bearer))
	if err := request.Write(io.Discard); err != nil {
		t.Fatalf("write request with generated bearer: %v", err)
	}

	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get attached workspace: %v", err)
	}
	digest := sha256.Sum256(bearer)
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if current.Spec.Attachment == nil || current.Spec.Attachment.TokenSHA256 != wantDigest {
		t.Fatalf("attachment digest = %#v, want %q", current.Spec.Attachment, wantDigest)
	}
	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.UID != workspace.UID {
		t.Fatalf("attachment Secret controller = %#v, want workspace UID %q", owner, workspace.UID)
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease); err != nil {
		t.Fatalf("get attachment Lease: %v", err)
	}
	owner = metav1.GetControllerOf(lease)
	if owner == nil || owner.UID != workspace.UID {
		t.Fatalf("attachment Lease controller = %#v, want workspace UID %q", owner, workspace.UID)
	}
}

// A controller restart after Secret creation but before the workspace patch
// leaves an exact next-epoch orphan. Once the old holder's Lease expires, a
// successor adopts that credential instead of failing forever on AlreadyExists.
func TestWorkspaceAttachmentManagerRecoversOwnedOrphanAfterLeaseHandoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "orphan-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	task := attachmentReviewTask(workspace.Namespace, "successor-task")
	previousTaskUID := types.UID("orphaned-task-uid")
	bearer := []byte(base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("o", workspaceAttachmentTokenEntropyBytes))))
	owner := *metav1.NewControllerRef(workspace, workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace"))
	orphan := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workspace.Namespace,
			Name:      attachmentSecretName(workspace.Name, 1),
			Labels:    map[string]string{workspaceAttachmentLabel: string(workspace.UID)},
			OwnerReferences: []metav1.OwnerReference{
				owner,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			workspaceAttachmentTokenKey: bearer,
			"workspaceUID":              []byte(workspace.UID),
			"taskUID":                   []byte(previousTaskUID),
			"epoch":                     []byte("1"),
		},
	}
	previousHolder := string(previousTaskUID)
	durationSeconds := int32(60)
	renewedAt := metav1.NewMicroTime(time.Date(2026, 7, 25, 11, 58, 0, 0, time.UTC))
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workspace.Namespace,
			Name:      attachmentLeaseName(workspace.Name),
			OwnerReferences: []metav1.OwnerReference{
				owner,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &previousHolder,
			LeaseDurationSeconds: &durationSeconds,
			RenewTime:            &renewedAt,
		},
	}
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task, orphan, lease).
		Build()
	manager := attachmentReviewManager(c)

	result, err := manager.Attach(ctx, workspace.DeepCopy(), task)
	if err != nil {
		t.Fatalf("Attach with owned orphan: %v", err)
	}
	if result.Epoch != 1 || result.AttachmentRef.Name != orphan.Name {
		t.Fatalf("recovered attachment = %#v, want orphan %q at epoch 1", result, orphan.Name)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("get recovered workspace: %v", err)
	}
	digest := sha256.Sum256(bearer)
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if current.Spec.Attachment == nil || current.Spec.Attachment.TaskRef.UID != task.UID ||
		current.Spec.Attachment.TokenSHA256 != wantDigest {
		t.Fatalf("recovered attachment intent = %#v, want digest %q", current.Spec.Attachment, wantDigest)
	}
	gotSecret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(orphan), gotSecret); err != nil {
		t.Fatalf("get recovered Secret: %v", err)
	}
	if string(gotSecret.Data[workspaceAttachmentTokenKey]) != string(bearer) {
		t.Fatal("recovery replaced the orphaned bearer")
	}
	gotLease := &coordinationv1.Lease{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(lease), gotLease); err != nil {
		t.Fatalf("get handed-off Lease: %v", err)
	}
	if gotLease.Spec.HolderIdentity == nil || *gotLease.Spec.HolderIdentity != string(task.UID) {
		t.Fatalf("Lease holder = %#v, want successor Task %q", gotLease.Spec.HolderIdentity, task.UID)
	}
}

func TestWorkspaceAttachmentManagerFencesPausedHolderAfterLeaseHandoff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workspace := testBoundWorkspace(t, "attachment-review", "paused-handoff-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	originalTask := attachmentReviewTask(workspace.Namespace, "paused-original-task")
	successorTask := attachmentReviewTask(workspace.Namespace, "paused-successor-task")
	baseClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, originalTask, successorTask).
		Build()

	startTime := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	currentTime := startTime
	var clockMu sync.RWMutex
	now := func() time.Time {
		clockMu.RLock()
		defer clockMu.RUnlock()
		return currentTime
	}
	setNow := func(value time.Time) {
		clockMu.Lock()
		defer clockMu.Unlock()
		currentTime = value
	}
	leaseKey := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	originalReached := make(chan struct{})
	originalRelease := make(chan struct{}, 1)
	successorReached := make(chan struct{})
	successorRelease := make(chan struct{}, 1)
	defer func() {
		select {
		case originalRelease <- struct{}{}:
		default:
		}
		select {
		case successorRelease <- struct{}{}:
		default:
		}
	}()

	type attachOutcome struct {
		result *WorkspaceAttachmentResult
		err    error
	}
	originalOutcomes := make(chan attachOutcome, 1)
	originalManager := WorkspaceAttachmentManager{
		Client: baseClient,
		APIReader: &attachmentLeaseBarrierReader{
			Reader:  baseClient,
			key:     leaseKey,
			reached: originalReached,
			release: originalRelease,
		},
		LeaseTTL: time.Minute,
		Now:      now,
	}
	go func() {
		result, err := originalManager.Attach(ctx, workspace.DeepCopy(), originalTask)
		originalOutcomes <- attachOutcome{result: result, err: err}
	}()
	waitForAttachmentBarrier(t, ctx, originalReached, "original Lease fence")

	setNow(startTime.Add(2 * time.Minute))
	successorOutcomes := make(chan attachOutcome, 1)
	successorManager := WorkspaceAttachmentManager{
		Client: baseClient,
		APIReader: &attachmentLeaseBarrierReader{
			Reader:  baseClient,
			key:     leaseKey,
			reached: successorReached,
			release: successorRelease,
		},
		LeaseTTL: time.Minute,
		Now:      now,
	}
	go func() {
		result, err := successorManager.Attach(ctx, workspace.DeepCopy(), successorTask)
		successorOutcomes <- attachOutcome{result: result, err: err}
	}()
	waitForAttachmentBarrier(t, ctx, successorReached, "successor Lease fence")

	liveLease := &coordinationv1.Lease{}
	if err := baseClient.Get(ctx, leaseKey, liveLease); err != nil {
		t.Fatalf("get handed-off Lease: %v", err)
	}
	if liveLease.Spec.HolderIdentity == nil || *liveLease.Spec.HolderIdentity != string(successorTask.UID) {
		t.Fatalf("Lease holder before publication = %#v, want successor Task %q", liveLease.Spec.HolderIdentity, successorTask.UID)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := baseClient.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("get workspace before resumed publication: %v", err)
	}
	if current.Spec.Attachment != nil {
		t.Fatalf("successor published attachment before its fence was released: %#v", current.Spec.Attachment)
	}

	originalRelease <- struct{}{}
	var original attachOutcome
	select {
	case original = <-originalOutcomes:
	case <-ctx.Done():
		t.Fatal("timed out waiting for paused original attachment attempt")
	}
	if original.result != nil || !errors.Is(original.err, ErrWorkspaceAttachmentLocked) {
		t.Fatalf("resumed original Attach = (%#v, %v), want Lease ownership rejection", original.result, original.err)
	}
	orphan := &corev1.Secret{}
	orphanKey := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentSecretName(workspace.Name, 1)}
	if err := baseClient.Get(ctx, orphanKey, orphan); err != nil {
		t.Fatalf("resumed original removed the handed-off orphan: %v", err)
	}

	successorRelease <- struct{}{}
	var successor attachOutcome
	select {
	case successor = <-successorOutcomes:
	case <-ctx.Done():
		t.Fatal("timed out waiting for successor attachment attempt")
	}
	if successor.err != nil || successor.result == nil {
		t.Fatalf("successor Attach = (%#v, %v), want success", successor.result, successor.err)
	}
	if err := baseClient.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("get successor attachment: %v", err)
	}
	if current.Spec.Attachment == nil || current.Spec.Attachment.TaskRef.UID != successorTask.UID {
		t.Fatalf("published attachment = %#v, want successor Task %q", current.Spec.Attachment, successorTask.UID)
	}
	if err := baseClient.Get(ctx, leaseKey, liveLease); err != nil {
		t.Fatalf("reload successor Lease: %v", err)
	}
	if liveLease.Spec.HolderIdentity == nil || *liveLease.Spec.HolderIdentity != string(successorTask.UID) {
		t.Fatalf("final Lease holder = %#v, want successor Task %q", liveLease.Spec.HolderIdentity, successorTask.UID)
	}
}

func TestWorkspaceAttachmentManagerRefusesForeignOrphanedSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "foreign-orphan-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	task := attachmentReviewTask(workspace.Namespace, "foreign-orphan-task")
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: workspace.Namespace,
		Name:      attachmentSecretName(workspace.Name, 1),
	}}
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task, foreign).
		Build()
	manager := attachmentReviewManager(c)

	if result, err := manager.Attach(ctx, workspace.DeepCopy(), task); err == nil ||
		!strings.Contains(err.Error(), "not the exact recoverable workspace attachment") {
		t.Fatalf("Attach with foreign orphan = (%#v, %v), want ownership rejection", result, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{}); err != nil {
		t.Fatalf("foreign Secret must be preserved: %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Spec.Attachment != nil || current.Spec.AttachmentEpoch != 0 {
		t.Fatalf("foreign collision mutated attachment intent: %#v", current.Spec)
	}
}

func TestBoundedWorkspaceChildNameHashesTruncatedWorkspaceNames(t *testing.T) {
	t.Parallel()
	sharedPrefix := strings.Repeat("a", 50) + "." + strings.Repeat("b", 40)
	workspaceNames := []string{sharedPrefix + "-first", sharedPrefix + "-second"}

	for _, nameFor := range []struct {
		name   string
		create func(string) string
		suffix string
	}{
		{name: "Secret", create: func(workspaceName string) string { return attachmentSecretName(workspaceName, 17) }, suffix: "-attachment-17"},
		{name: "Lease", create: attachmentLeaseName, suffix: "-attachment"},
	} {
		t.Run(nameFor.name, func(t *testing.T) {
			first := nameFor.create(workspaceNames[0])
			second := nameFor.create(workspaceNames[1])
			if first == second {
				t.Fatalf("truncated child names collided: %q", first)
			}
			for _, name := range []string{first, second} {
				if len(name) > workspaceChildNameMaxLength {
					t.Fatalf("child name length = %d, want <= %d: %q", len(name), workspaceChildNameMaxLength, name)
				}
				if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
					t.Fatalf("child name %q is not DNS-1123: %v", name, problems)
				}
				if !strings.HasSuffix(name, nameFor.suffix) {
					t.Fatalf("child name %q does not preserve suffix %q", name, nameFor.suffix)
				}
			}
		})
	}

	for _, suffix := range []string{"attachment", "attachment-17"} {
		maxPrefix := workspaceChildNameMaxLength - workspaceChildNameHashLength - len(suffix) - 2
		workspaceName := strings.Repeat("a", maxPrefix-1) + "." + strings.Repeat("b", 40)
		name := boundedWorkspaceChildName(workspaceName, suffix)
		if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
			t.Fatalf("child name with a dot at the hash boundary %q is not DNS-1123: %v", name, problems)
		}
	}
}

func TestWorkspaceAttachmentManagerRevalidatesReusableStateBeforeIntentPatch(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		mutate func(context.Context, client.Client, *workspacev1alpha1.ExecutionWorkspace) error
		verify func(*testing.T, *workspacev1alpha1.ExecutionWorkspace)
	}{
		{
			name: "desired deleted",
			mutate: func(ctx context.Context, c client.Client, workspace *workspacev1alpha1.ExecutionWorkspace) error {
				workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
				return c.Update(ctx, workspace)
			},
			verify: func(t *testing.T, workspace *workspacev1alpha1.ExecutionWorkspace) {
				t.Helper()
				if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
					t.Fatalf("desired state = %q, want Deleted", workspace.Spec.DesiredState)
				}
			},
		},
		{
			name: "desired quarantined",
			mutate: func(ctx context.Context, c client.Client, workspace *workspacev1alpha1.ExecutionWorkspace) error {
				workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
				return c.Update(ctx, workspace)
			},
			verify: func(t *testing.T, workspace *workspacev1alpha1.ExecutionWorkspace) {
				t.Helper()
				if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
					t.Fatalf("desired state = %q, want Quarantined", workspace.Spec.DesiredState)
				}
			},
		},
		{
			name: "observed deleting",
			mutate: func(ctx context.Context, c client.Client, workspace *workspacev1alpha1.ExecutionWorkspace) error {
				workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateDeleting
				return c.Status().Update(ctx, workspace)
			},
			verify: func(t *testing.T, workspace *workspacev1alpha1.ExecutionWorkspace) {
				t.Helper()
				if workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateDeleting {
					t.Fatalf("observed state = %q, want Deleting", workspace.Status.State)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace := testBoundWorkspace(t, "attachment-review", "state-"+strings.ReplaceAll(testCase.name, " ", "-"), "class", "provider")
			markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
			workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
			task := attachmentReviewTask(workspace.Namespace, "state-task-"+strings.ReplaceAll(testCase.name, " ", "-"))
			baseClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(workspace).
				WithObjects(workspace, task).
				Build()
			mutatingClient := &attachmentReviewMutatingClient{
				Client: baseClient,
				workspaceKey: types.NamespacedName{
					Namespace: workspace.Namespace,
					Name:      workspace.Name,
				},
				mutate: testCase.mutate,
			}
			manager := attachmentReviewManager(mutatingClient)

			if result, err := manager.Attach(ctx, workspace.DeepCopy(), task); err == nil {
				t.Fatalf("Attach = %#v, want revalidation error", result)
			}
			if mutatingClient.mutationErr != nil {
				t.Fatalf("mutate workspace during attachment: %v", mutatingClient.mutationErr)
			}

			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := baseClient.Get(ctx, mutatingClient.workspaceKey, current); err != nil {
				t.Fatalf("get rejected workspace: %v", err)
			}
			testCase.verify(t, current)
			if current.Spec.Attachment != nil || current.Spec.AttachmentEpoch != 0 {
				t.Fatalf("rejected attachment mutated workspace intent: %#v", current.Spec)
			}
			secrets := &corev1.SecretList{}
			if err := baseClient.List(ctx, secrets, client.InNamespace(workspace.Namespace)); err != nil {
				t.Fatalf("list Secrets after rejected attachment: %v", err)
			}
			for i := range secrets.Items {
				if secrets.Items[i].Labels[workspaceAttachmentLabel] == string(workspace.UID) {
					t.Fatalf("attachment Secret %q remained after rejected attachment", secrets.Items[i].Name)
				}
			}
			lease := &coordinationv1.Lease{}
			err := baseClient.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("attachment Lease remained after rejected attachment: %v", err)
			}
		})
	}
}

func TestWorkspaceAttachmentManagerUsesOptimisticLockForIntentRevalidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "optimistic-lock-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	task := attachmentReviewTask(workspace.Namespace, "optimistic-lock-task")
	baseClient := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task).
		Build()
	mutatingClient := &attachmentReviewPatchMutatingClient{
		Client: baseClient,
		workspaceKey: types.NamespacedName{
			Namespace: workspace.Namespace,
			Name:      workspace.Name,
		},
	}
	manager := attachmentReviewManager(mutatingClient)

	if result, err := manager.Attach(ctx, workspace.DeepCopy(), task); err == nil {
		t.Fatalf("Attach = %#v, want concurrent desired-state revalidation error", result)
	}
	if mutatingClient.mutationErr != nil {
		t.Fatalf("mutate workspace immediately before intent patch: %v", mutatingClient.mutationErr)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := baseClient.Get(ctx, mutatingClient.workspaceKey, current); err != nil {
		t.Fatalf("get concurrently changed workspace: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		t.Fatalf("desired state = %q, want Deleted", current.Spec.DesiredState)
	}
	if current.Spec.Attachment != nil || current.Spec.AttachmentEpoch != 0 {
		t.Fatalf("concurrent desired-state change admitted attachment intent: %#v", current.Spec)
	}
	secretList := &corev1.SecretList{}
	if err := baseClient.List(ctx, secretList, client.InNamespace(workspace.Namespace)); err != nil {
		t.Fatalf("list Secrets after optimistic-lock rejection: %v", err)
	}
	for i := range secretList.Items {
		if secretList.Items[i].Labels[workspaceAttachmentLabel] == string(workspace.UID) {
			t.Fatalf("attachment Secret %q remained after optimistic-lock rejection", secretList.Items[i].Name)
		}
	}
	lease := &coordinationv1.Lease{}
	err := baseClient.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("attachment Lease remained after optimistic-lock rejection: %v", err)
	}
}

type attachmentReviewMutatingClient struct {
	client.Client
	workspaceKey types.NamespacedName
	mutate       func(context.Context, client.Client, *workspacev1alpha1.ExecutionWorkspace) error
	once         sync.Once
	mutationErr  error
}

type attachmentFinalizeStaleClient struct {
	client.Client
	workspace *workspacev1alpha1.ExecutionWorkspace
	secretKey types.NamespacedName
	leaseKey  types.NamespacedName
}

func (c *attachmentFinalizeStaleClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	switch object := object.(type) {
	case *workspacev1alpha1.ExecutionWorkspace:
		if c.workspace != nil && key == client.ObjectKeyFromObject(c.workspace) {
			c.workspace.DeepCopyInto(object)
			return nil
		}
	case *corev1.Secret:
		if key == c.secretKey {
			return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
		}
	case *coordinationv1.Lease:
		if key == c.leaseKey {
			return apierrors.NewNotFound(coordinationv1.Resource("leases"), key.Name)
		}
	}
	return c.Client.Get(ctx, key, object, opts...)
}

type attachmentLeaseMutatingReader struct {
	client.Reader
	key         types.NamespacedName
	mutate      func(context.Context) error
	once        sync.Once
	mutationErr error
}

func (r *attachmentLeaseMutatingReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	if _, ok := object.(*coordinationv1.Lease); ok && key == r.key {
		r.once.Do(func() {
			if r.mutate != nil {
				r.mutationErr = r.mutate(ctx)
			}
		})
		if r.mutationErr != nil {
			return r.mutationErr
		}
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

type attachmentLeaseBarrierReader struct {
	client.Reader
	key     types.NamespacedName
	reached chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *attachmentLeaseBarrierReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	shouldBlock := false
	if _, ok := object.(*coordinationv1.Lease); ok && key == r.key {
		r.once.Do(func() { shouldBlock = true })
	}
	if shouldBlock {
		close(r.reached)
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func waitForAttachmentBarrier(t *testing.T, ctx context.Context, reached <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", name)
	}
}

type attachmentReviewPatchMutatingClient struct {
	client.Client
	workspaceKey types.NamespacedName
	once         sync.Once
	mutationErr  error
}

func (c *attachmentReviewPatchMutatingClient) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	workspace, ok := object.(*workspacev1alpha1.ExecutionWorkspace)
	if ok && workspace.Spec.Attachment != nil {
		c.once.Do(func() {
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, c.workspaceKey, current); err != nil {
				c.mutationErr = err
				return
			}
			current.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
			c.mutationErr = c.Update(ctx, current)
		})
	}
	return c.Client.Patch(ctx, object, patch, opts...)
}

func (c *attachmentReviewMutatingClient) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	if err := c.Client.Create(ctx, object, opts...); err != nil {
		return err
	}
	secret, ok := object.(*corev1.Secret)
	if !ok || secret.Data[workspaceAttachmentTokenKey] == nil {
		return nil
	}
	c.once.Do(func() {
		workspace := &workspacev1alpha1.ExecutionWorkspace{}
		if err := c.Get(ctx, c.workspaceKey, workspace); err != nil {
			c.mutationErr = err
			return
		}
		c.mutationErr = c.mutate(ctx, c.Client, workspace)
	})
	return nil
}

func attachmentReviewTask(namespace, name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		UID:       types.UID(name + "-uid"),
	}}
}

func attachmentReviewManager(c client.Client) WorkspaceAttachmentManager {
	return WorkspaceAttachmentManager{
		Client:    c,
		APIReader: c,
		LeaseTTL:  time.Minute,
		Now: func() time.Time {
			return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
		},
	}
}
