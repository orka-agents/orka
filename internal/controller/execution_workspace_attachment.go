package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

const (
	workspaceAttachmentTokenKey = "token"
	workspaceAttachmentLabel    = "workspace.orka.ai/attachment-for"
	defaultAttachmentLeaseTTL   = 5 * time.Minute
)

var ErrWorkspaceAttachmentLocked = errors.New("execution workspace attachment is held by another task")

// WorkspaceAttachmentManager owns Lease, epoch, and attachment Secret rotation.
type WorkspaceAttachmentManager struct {
	Client   client.Client
	LeaseTTL time.Duration
	Now      func() time.Time
}

// WorkspaceAttachmentResult contains references safe to pass to a worker Job.
type WorkspaceAttachmentResult struct {
	Epoch         int64
	AttachmentRef workspacev1alpha1.SecretReference
	ExpiresAt     metav1.Time
}

// Attach rotates authority and writes attachment intent. Raw token bytes exist
// only in the created Secret and this function's short-lived local buffer.
func (m WorkspaceAttachmentManager) Attach(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) (*WorkspaceAttachmentResult, error) {
	if m.Client == nil || workspace == nil || task == nil {
		return nil, fmt.Errorf("workspace attachment manager, workspace, and task are required")
	}
	if workspace.Spec.Mode != workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		return nil, fmt.Errorf("only Interactive workspaces may be attached")
	}
	if workspace.Namespace != task.Namespace || workspace.UID == "" || task.UID == "" {
		return nil, fmt.Errorf("workspace and task must be persisted in the same namespace")
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined || !WorkspaceReusable(workspace) {
		return nil, fmt.Errorf("workspace is not reusable")
	}

	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	ttl := m.LeaseTTL
	if ttl <= 0 {
		ttl = defaultAttachmentLeaseTTL
	}
	expiresAt := now.Add(ttl)
	if maxLifetime := workspace.Spec.Lifecycle.MaxLifetime; maxLifetime != nil && maxLifetime.Duration > 0 {
		maxExpiry := workspace.CreationTimestamp.Add(maxLifetime.Duration)
		if maxExpiry.Before(expiresAt) {
			expiresAt = maxExpiry
		}
	}
	if !expiresAt.After(now) {
		return nil, fmt.Errorf("workspace attachment would already be expired")
	}

	if err := m.acquireLease(ctx, workspace, task, now, ttl); err != nil {
		return nil, err
	}

	epoch := workspace.Status.AttachedEpoch + 1
	if workspace.Spec.Attachment != nil && workspace.Spec.Attachment.Epoch >= epoch {
		epoch = workspace.Spec.Attachment.Epoch + 1
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("generate workspace attachment token: %w", err)
	}
	tokenDigest := sha256.Sum256(token[:])
	secretName := attachmentSecretName(workspace.Name, epoch)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: workspace.Namespace,
			Labels: map[string]string{
				workspaceAttachmentLabel: string(workspace.UID),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			workspaceAttachmentTokenKey: append([]byte(nil), token[:]...),
			"workspaceUID":              []byte(workspace.UID),
			"taskUID":                   []byte(task.UID),
			"epoch":                     []byte(strconv.FormatInt(epoch, 10)),
		},
	}
	if err := controllerutil.SetControllerReference(workspace, secret, m.Client.Scheme()); err != nil {
		return nil, fmt.Errorf("set attachment Secret owner: %w", err)
	}
	if err := m.Client.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create attachment Secret: %w", err)
		}
		return nil, fmt.Errorf("attachment Secret %s already exists", secretName)
	}

	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &workspacev1alpha1.ExecutionWorkspace{}
		if err := m.Client.Get(ctx, key, current); err != nil {
			return err
		}
		if current.Spec.Attachment != nil {
			return ErrWorkspaceAttachmentLocked
		}
		before := current.DeepCopy()
		attachment := &workspacev1alpha1.ExecutionWorkspaceAttachment{
			TaskRef:     workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID},
			Epoch:       epoch,
			TokenSHA256: "sha256:" + hex.EncodeToString(tokenDigest[:]),
			ExpiresAt:   metav1.NewTime(expiresAt),
		}
		attachment.TokenSecretRef.Name = secretName
		current.Spec.Attachment = attachment
		current.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
		return m.Client.Patch(ctx, current, client.MergeFrom(before))
	})
	for i := range token {
		token[i] = 0
	}
	if err != nil {
		_ = m.Client.Delete(ctx, secret)
		return nil, fmt.Errorf("set workspace attachment intent: %w", err)
	}
	return &WorkspaceAttachmentResult{
		Epoch:         epoch,
		AttachmentRef: workspacev1alpha1.SecretReference{Name: secretName},
		ExpiresAt:     metav1.NewTime(expiresAt),
	}, nil
}

// BeginRevocation clears attachment intent. The adapter must revoke the active
// epoch before FinalizeRevocation removes the Secret and Lease.
func (m WorkspaceAttachmentManager) BeginRevocation(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	epoch int64,
) error {
	if m.Client == nil || workspace == nil || epoch <= 0 {
		return fmt.Errorf("workspace and positive epoch are required")
	}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &workspacev1alpha1.ExecutionWorkspace{}
		if err := m.Client.Get(ctx, key, current); err != nil {
			return err
		}
		if current.Spec.Attachment == nil {
			return nil
		}
		if current.Spec.Attachment.Epoch != epoch {
			return fmt.Errorf("attachment epoch %d does not match active intent %d", epoch, current.Spec.Attachment.Epoch)
		}
		before := current.DeepCopy()
		current.Spec.Attachment = nil
		return m.Client.Patch(ctx, current, client.MergeFrom(before))
	})
}

// FinalizeRevocation deletes the token Secret and Lease only after provider
// status no longer reports the epoch as attached.
func (m WorkspaceAttachmentManager) FinalizeRevocation(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	epoch int64,
	secretName string,
) error {
	if m.Client == nil || workspace == nil || epoch <= 0 {
		return fmt.Errorf("workspace and positive epoch are required")
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		return err
	}
	if current.Status.AttachedEpoch == epoch || workspaceproviderAttached(current, epoch) {
		return fmt.Errorf("attachment epoch %d has not been revoked", epoch)
	}
	if strings.TrimSpace(secretName) != "" {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: workspace.Namespace}}
		if err := m.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete attachment Secret: %w", err)
		}
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace}}
	if err := m.Client.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete attachment Lease: %w", err)
	}
	return nil
}

func (m WorkspaceAttachmentManager) acquireLease(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	now time.Time,
	ttl time.Duration,
) error {
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	holder := string(task.UID)
	durationSeconds := max(int32(ttl/time.Second), 1)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		lease := &coordinationv1.Lease{}
		err := m.Client.Get(ctx, key, lease)
		if apierrors.IsNotFound(err) {
			lease = &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &holder,
					LeaseDurationSeconds: &durationSeconds,
					AcquireTime:          &metav1.MicroTime{Time: now},
					RenewTime:            &metav1.MicroTime{Time: now},
				},
			}
			if err := controllerutil.SetControllerReference(workspace, lease, m.Client.Scheme()); err != nil {
				return err
			}
			return m.Client.Create(ctx, lease)
		}
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != holder && !leaseExpired(lease, now) {
			return ErrWorkspaceAttachmentLocked
		}
		before := lease.DeepCopy()
		lease.Spec.HolderIdentity = &holder
		lease.Spec.LeaseDurationSeconds = &durationSeconds
		lease.Spec.RenewTime = &metav1.MicroTime{Time: now}
		if lease.Spec.AcquireTime == nil || leaseExpired(lease, now) {
			lease.Spec.AcquireTime = &metav1.MicroTime{Time: now}
		}
		return m.Client.Patch(ctx, lease, client.MergeFrom(before))
	})
}

func leaseExpired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease == nil || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return !lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).After(now)
}

func workspaceproviderAttached(workspace *workspacev1alpha1.ExecutionWorkspace, epoch int64) bool {
	for _, condition := range workspace.Status.Conditions {
		if condition.Type == string(workspacev1alpha1.ConditionWorkspaceAttached) && condition.Status == metav1.ConditionTrue {
			return workspace.Status.AttachedEpoch == epoch
		}
	}
	return false
}

func attachmentSecretName(workspaceName string, epoch int64) string {
	return boundedWorkspaceChildName(workspaceName, "attachment-"+strconv.FormatInt(epoch, 10))
}

func attachmentLeaseName(workspaceName string) string {
	return boundedWorkspaceChildName(workspaceName, "attachment")
}

func boundedWorkspaceChildName(workspaceName, suffix string) string {
	workspaceName = strings.Trim(strings.ToLower(strings.TrimSpace(workspaceName)), "-")
	if workspaceName == "" {
		workspaceName = "workspace"
	}
	maxPrefix := max(63-len(suffix)-1, 1)
	if len(workspaceName) > maxPrefix {
		workspaceName = strings.TrimRight(workspaceName[:maxPrefix], "-")
	}
	return workspaceName + "-" + suffix
}
