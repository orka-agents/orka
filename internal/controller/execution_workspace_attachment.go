package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
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
	"github.com/orka-agents/orka/internal/labels"
)

const (
	workspaceAttachmentTokenKey          = "token"
	workspaceAttachmentLabel             = labels.LabelWorkspaceAttachment
	workspaceAttachmentTokenEntropyBytes = 24
	workspaceChildNameMaxLength          = 63
	workspaceChildNameHashLength         = 12
	defaultAttachmentLeaseTTL            = 5 * time.Minute
)

var (
	ErrWorkspaceAttachmentLocked           = errors.New("execution workspace attachment is held by another task")
	errWorkspaceAttachmentRotationNotReady = errors.New("expired workspace attachment is not ready for credential rotation")
)

// WorkspaceAttachmentManager owns Lease, epoch, and attachment Secret rotation.
type WorkspaceAttachmentManager struct {
	Client    client.Client
	APIReader client.Reader
	LeaseTTL  time.Duration
	Now       func() time.Time
}

// WorkspaceAttachmentResult contains references safe to pass to a worker Job.
type WorkspaceAttachmentResult struct {
	Epoch         int64
	AttachmentRef workspacev1alpha1.SecretReference
	ExpiresAt     metav1.Time
}

func deleteWorkspaceOwnedAttachmentObject(
	ctx context.Context,
	reader client.Reader,
	writer client.Client,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	obj client.Object,
	kind string,
) error {
	key := client.ObjectKeyFromObject(obj)
	if err := reader.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get workspace attachment %s %s: %w", kind, key, err)
	}
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.UID != workspace.UID {
		return fmt.Errorf(
			"workspace attachment %s %s is not controlled by workspace %s/%s UID %s; refusing deletion",
			kind, key, workspace.Namespace, workspace.Name, workspace.UID,
		)
	}
	if err := writer.Delete(ctx, obj, deleteCurrentObjectPreconditions(obj)...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete workspace attachment %s %s: %w", kind, key, err)
	}
	return nil
}

// Attach rotates authority and writes attachment intent. Bearer token text
// exists only in the created Secret and this function's short-lived buffer.
func (m WorkspaceAttachmentManager) Attach(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) (*WorkspaceAttachmentResult, error) {
	if m.Client == nil || workspace == nil || task == nil {
		return nil, fmt.Errorf("workspace attachment manager, workspace, and task are required")
	}
	if workspace.Namespace != task.Namespace || workspace.UID == "" || task.UID == "" {
		return nil, fmt.Errorf("workspace and task must be persisted in the same namespace")
	}
	now := m.now()
	if err := validateWorkspaceAttachmentAttempt(workspace, task, now); err != nil {
		return nil, err
	}
	ttl := m.LeaseTTL
	if ttl <= 0 {
		ttl = defaultAttachmentLeaseTTL
	}

	bearer, err := newWorkspaceAttachmentBearer()
	if err != nil {
		return nil, err
	}
	defer zeroBytes(bearer)
	tokenDigest := sha256.Sum256(bearer)

	leaseClaimed, err := m.acquireLease(ctx, workspace, task, now, ttl)
	if err != nil {
		return nil, err
	}
	var createdSecret *corev1.Secret
	preserveCreatedSecret := false
	fail := func(cause error) error {
		secret := createdSecret
		if preserveCreatedSecret {
			secret = nil
		}
		cleanupErr := m.cleanupFailedAttachmentAttempt(ctx, workspace, task, secret, leaseClaimed)
		if cleanupErr != nil {
			return errors.Join(cause, cleanupErr)
		}
		return cause
	}

	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := m.Client.Get(ctx, key, current); err != nil {
		return nil, fail(fmt.Errorf("get workspace before attachment: %w", err))
	}
	if current.UID != workspace.UID {
		return nil, fail(fmt.Errorf("workspace %s/%s was replaced before attachment", workspace.Namespace, workspace.Name))
	}
	if err := validateWorkspaceAttachmentAttempt(current, task, now); err != nil {
		return nil, fail(fmt.Errorf("revalidate workspace before attachment: %w", err))
	}

	expiresAt := now.Add(ttl)
	if maxLifetime := current.Spec.Lifecycle.MaxLifetime; maxLifetime != nil && maxLifetime.Duration > 0 {
		maxExpiry := current.CreationTimestamp.Add(maxLifetime.Duration)
		if maxExpiry.Before(expiresAt) {
			expiresAt = maxExpiry
		}
	}
	if !expiresAt.After(now) {
		return nil, fail(fmt.Errorf("workspace attachment would already be expired"))
	}

	epoch, err := nextWorkspaceAttachmentEpoch(current)
	if err != nil {
		return nil, fail(err)
	}
	secretName := attachmentSecretName(current.Name, epoch)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: current.Namespace,
			Labels: map[string]string{
				workspaceAttachmentLabel: string(current.UID),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			workspaceAttachmentTokenKey: append([]byte(nil), bearer...),
			"workspaceUID":              []byte(current.UID),
			"taskUID":                   []byte(task.UID),
			"epoch":                     []byte(strconv.FormatInt(epoch, 10)),
		},
	}
	if err := controllerutil.SetControllerReference(current, secret, m.Client.Scheme()); err != nil {
		return nil, fail(fmt.Errorf("set attachment Secret owner: %w", err))
	}
	if err := m.Client.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fail(fmt.Errorf("create attachment Secret: %w", err))
		}
		recovered, recoveredDigest, recoverErr := m.recoverOrphanedAttachmentSecret(
			ctx, current, epoch, secretName,
		)
		if recoverErr != nil {
			return nil, fail(recoverErr)
		}
		secret = recovered
		tokenDigest = recoveredDigest
	}
	createdSecret = secret

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &workspacev1alpha1.ExecutionWorkspace{}
		if err := m.Client.Get(ctx, key, current); err != nil {
			return err
		}
		if current.UID != workspace.UID {
			return fmt.Errorf("workspace %s/%s was replaced during attachment", workspace.Namespace, workspace.Name)
		}
		if err := validateWorkspaceAttachmentAttempt(current, task, now); err != nil {
			return fmt.Errorf("revalidate workspace attachment intent: %w", err)
		}
		nextEpoch, err := nextWorkspaceAttachmentEpoch(current)
		if err != nil {
			return err
		}
		if nextEpoch != epoch {
			return fmt.Errorf("%w: attachment epoch advanced from %d to %d", ErrWorkspaceAttachmentLocked, epoch, nextEpoch)
		}
		preserveCreatedSecret = true
		if err := m.renewAttachmentLeaseFence(ctx, current, task, ttl); err != nil {
			return err
		}
		preserveCreatedSecret = false

		before := current.DeepCopy()
		attachment := &workspacev1alpha1.ExecutionWorkspaceAttachment{
			TaskRef:     workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID},
			Epoch:       epoch,
			TokenSHA256: "sha256:" + hex.EncodeToString(tokenDigest[:]),
			ExpiresAt:   metav1.NewTime(expiresAt),
		}
		attachment.TokenSecretRef.Name = secretName
		current.Spec.AttachmentEpoch = epoch
		current.Spec.Attachment = attachment
		current.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
		return m.Client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return nil, fail(fmt.Errorf("set workspace attachment intent: %w", err))
	}
	return &WorkspaceAttachmentResult{
		Epoch:         epoch,
		AttachmentRef: workspacev1alpha1.SecretReference{Name: secretName},
		ExpiresAt:     metav1.NewTime(expiresAt),
	}, nil
}

func (m WorkspaceAttachmentManager) renewAttachmentLeaseFence(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	ttl time.Duration,
) error {
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	reader := m.APIReader
	if reader == nil {
		reader = m.Client
	}
	if err := reader.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: attachment Lease %s disappeared before intent publication", ErrWorkspaceAttachmentLocked, key)
		}
		return fmt.Errorf("read attachment Lease before intent publication: %w", err)
	}
	owner := metav1.GetControllerOf(lease)
	if owner == nil || owner.UID != workspace.UID || lease.Spec.HolderIdentity == nil ||
		*lease.Spec.HolderIdentity != string(task.UID) {
		return fmt.Errorf("%w: attachment Lease %s is no longer held by Task %s", ErrWorkspaceAttachmentLocked, key, task.UID)
	}
	now := m.now()
	if leaseExpired(lease, now) {
		return fmt.Errorf("%w: attachment Lease %s expired before intent publication", ErrWorkspaceAttachmentLocked, key)
	}

	before := lease.DeepCopy()
	durationSeconds := max(int32(ttl/time.Second), 1)
	lease.Spec.LeaseDurationSeconds = &durationSeconds
	lease.Spec.RenewTime = &metav1.MicroTime{Time: now}
	if err := m.Client.Patch(ctx, lease, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: attachment Lease %s changed before intent publication", ErrWorkspaceAttachmentLocked, key)
		}
		return fmt.Errorf("renew attachment Lease before intent publication: %w", err)
	}
	return nil
}

func (m WorkspaceAttachmentManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m WorkspaceAttachmentManager) recoverOrphanedAttachmentSecret(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	epoch int64,
	secretName string,
) (*corev1.Secret, [sha256.Size]byte, error) {
	var zeroDigest [sha256.Size]byte
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: secretName}
	if err := m.Client.Get(ctx, key, secret); err != nil {
		return nil, zeroDigest, fmt.Errorf("get existing attachment Secret %s: %w", secretName, err)
	}
	owner := metav1.GetControllerOf(secret)
	bearer := secret.Data[workspaceAttachmentTokenKey]
	metadataMatches := owner != nil && owner.UID == workspace.UID &&
		secret.Type == corev1.SecretTypeOpaque &&
		secret.Labels[workspaceAttachmentLabel] == string(workspace.UID) &&
		string(secret.Data["workspaceUID"]) == string(workspace.UID) &&
		len(secret.Data["taskUID"]) > 0 &&
		string(secret.Data["epoch"]) == strconv.FormatInt(epoch, 10)
	decodedBearer := make([]byte, base64.RawURLEncoding.DecodedLen(len(bearer)))
	decodedLen, decodeErr := base64.RawURLEncoding.Decode(decodedBearer, bearer)
	validBearer := decodeErr == nil && decodedLen == workspaceAttachmentTokenEntropyBytes
	zeroBytes(decodedBearer)
	if !metadataMatches || !validBearer {
		return nil, zeroDigest, fmt.Errorf(
			"attachment Secret %s already exists but is not the exact recoverable workspace attachment; refusing replacement",
			secretName,
		)
	}
	// taskUID records the attempt that created the orphan. It can differ after
	// an expired Lease transfers to a successor Task; the workspace owner,
	// epoch, renewed Lease fence, and optimistic workspace patch decide which
	// Task may publish the recovered authority.
	return secret, sha256.Sum256(bearer), nil
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
		if current.UID != workspace.UID {
			return fmt.Errorf("workspace %s/%s was replaced before attachment revocation", workspace.Namespace, workspace.Name)
		}
		if current.Spec.Attachment != nil && current.Spec.Attachment.Epoch != epoch {
			return fmt.Errorf("attachment epoch %d does not match active intent %d", epoch, current.Spec.Attachment.Epoch)
		}
		highWater := max(current.Spec.AttachmentEpoch, epoch, current.Status.AttachedEpoch)
		if current.Spec.Attachment == nil && current.Spec.AttachmentEpoch == highWater {
			return nil
		}
		before := current.DeepCopy()
		current.Spec.AttachmentEpoch = highWater
		current.Spec.Attachment = nil
		return m.Client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
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
	reader := client.Reader(m.Client)
	if m.APIReader != nil {
		reader = m.APIReader
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		return err
	}
	if current.UID != workspace.UID {
		return fmt.Errorf("workspace %s/%s was replaced before attachment revocation", workspace.Namespace, workspace.Name)
	}
	if current.Status.AttachedEpoch == epoch || workspaceproviderAttached(current, epoch) {
		return fmt.Errorf("attachment epoch %d has not been revoked", epoch)
	}
	if strings.TrimSpace(secretName) != "" {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: workspace.Namespace}}
		if err := deleteWorkspaceOwnedAttachmentObject(ctx, reader, m.Client, current, secret, "Secret"); err != nil {
			return err
		}
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace}}
	if err := deleteWorkspaceOwnedAttachmentObject(ctx, reader, m.Client, current, lease, "Lease"); err != nil {
		return err
	}
	return nil
}

func (m WorkspaceAttachmentManager) acquireLease(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	now time.Time,
	ttl time.Duration,
) (bool, error) {
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	holder := string(task.UID)
	durationSeconds := max(int32(ttl/time.Second), 1)
	claimed := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
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
			if err := m.Client.Create(ctx, lease); err != nil {
				return err
			}
			claimed = true
			return nil
		}
		if err != nil {
			return err
		}

		expired := leaseExpired(lease, now)
		sameHolder := lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder
		if !sameHolder && lease.Spec.HolderIdentity != nil && !expired {
			return ErrWorkspaceAttachmentLocked
		}
		before := lease.DeepCopy()
		lease.Spec.HolderIdentity = &holder
		lease.Spec.LeaseDurationSeconds = &durationSeconds
		lease.Spec.RenewTime = &metav1.MicroTime{Time: now}
		if lease.Spec.AcquireTime == nil || expired {
			lease.Spec.AcquireTime = &metav1.MicroTime{Time: now}
		}
		if err := m.Client.Patch(ctx, lease, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		if !sameHolder || expired {
			claimed = true
		}
		return nil
	})
	return claimed, err
}

func validateWorkspaceAttachmentCandidate(workspace *workspacev1alpha1.ExecutionWorkspace) error {
	if workspace == nil {
		return fmt.Errorf("workspace is required")
	}
	if workspace.Spec.Mode != workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		return fmt.Errorf("only Interactive workspaces may be attached")
	}
	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady, workspacev1alpha1.ExecutionWorkspaceDesiredSuspended:
	default:
		return fmt.Errorf("workspace desired state %q is not reusable", workspace.Spec.DesiredState)
	}
	if !WorkspaceReusable(workspace) {
		return fmt.Errorf("workspace is not reusable")
	}
	return nil
}

// validateWorkspaceAttachmentAttempt admits either a detached reusable
// workspace or an expired, fully enforced attachment held by the same Task.
// The latter is a new attachment attempt: Attach advances the epoch, renews
// the Lease, and rotates the bearer without opening a detached ownership gap.
func validateWorkspaceAttachmentAttempt(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	now time.Time,
) error {
	if workspace == nil || task == nil {
		return fmt.Errorf("workspace and task are required")
	}
	attachment := workspace.Spec.Attachment
	if attachment == nil {
		return validateWorkspaceAttachmentCandidate(workspace)
	}
	if attachment.TaskRef.UID != task.UID || attachment.ExpiresAt.After(now) {
		return ErrWorkspaceAttachmentLocked
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) || workspace.Status.ObservedGeneration != workspace.Generation ||
		!workspace.DeletionTimestamp.IsZero() || workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Spec.AttachmentEpoch != attachment.Epoch || workspace.Status.AttachedEpoch != attachment.Epoch ||
		!workspaceproviderAttached(workspace, attachment.Epoch) {
		return errWorkspaceAttachmentRotationNotReady
	}
	return nil
}

func nextWorkspaceAttachmentEpoch(workspace *workspacev1alpha1.ExecutionWorkspace) (int64, error) {
	highWater := workspace.Spec.AttachmentEpoch
	if workspace.Spec.Attachment != nil {
		highWater = max(highWater, workspace.Spec.Attachment.Epoch)
	}
	highWater = max(highWater, workspace.Status.AttachedEpoch)
	if highWater == math.MaxInt64 {
		return 0, fmt.Errorf("workspace attachment epoch is exhausted")
	}
	return highWater + 1, nil
}

func newWorkspaceAttachmentBearer() ([]byte, error) {
	randomBytes := make([]byte, workspaceAttachmentTokenEntropyBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return nil, fmt.Errorf("generate workspace attachment token: %w", err)
	}
	defer zeroBytes(randomBytes)

	bearer := make([]byte, base64.RawURLEncoding.EncodedLen(len(randomBytes)))
	base64.RawURLEncoding.Encode(bearer, randomBytes)
	return bearer, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func (m WorkspaceAttachmentManager) cleanupFailedAttachmentAttempt(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	secret *corev1.Secret,
	leaseClaimed bool,
) error {
	current := &workspacev1alpha1.ExecutionWorkspace{}
	workspaceErr := m.Client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current)
	if workspaceErr != nil && !apierrors.IsNotFound(workspaceErr) {
		return fmt.Errorf("verify failed workspace attachment attempt: %w", workspaceErr)
	}

	workspaceHasAttachment := workspaceErr == nil && current.UID == workspace.UID && current.Spec.Attachment != nil
	// A handed-off orphan can still race the original paused attempt. Once
	// either Task attaches this exact Secret, cleanup must preserve the bearer;
	// the workspace's optimistic patch decides which attachment won.
	secretIsActive := workspaceHasAttachment && secret != nil &&
		current.Spec.Attachment.TokenSecretRef.Name == secret.Name
	if secretIsActive {
		return nil
	}

	var cleanupErrs []error
	if secret != nil {
		if err := m.Client.Delete(ctx, secret, deleteCurrentObjectPreconditions(secret)...); err != nil && !apierrors.IsNotFound(err) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("clean up failed attachment Secret: %w", err))
		}
	}
	if !leaseClaimed || workspaceHasAttachment {
		return errors.Join(cleanupErrs...)
	}

	lease := &coordinationv1.Lease{}
	leaseKey := types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}
	if err := m.Client.Get(ctx, leaseKey, lease); err != nil {
		if !apierrors.IsNotFound(err) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("get failed attachment Lease for cleanup: %w", err))
		}
		return errors.Join(cleanupErrs...)
	}
	owner := metav1.GetControllerOf(lease)
	if owner == nil || owner.UID != workspace.UID || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(task.UID) {
		return errors.Join(cleanupErrs...)
	}
	if err := m.Client.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !apierrors.IsNotFound(err) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("clean up failed attachment Lease: %w", err))
	}
	return errors.Join(cleanupErrs...)
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
	name := workspaceName + "-" + suffix
	if len(name) <= workspaceChildNameMaxLength {
		return name
	}

	digest := sha256.Sum256([]byte(workspaceName))
	hashSuffix := hex.EncodeToString(digest[:])[:workspaceChildNameHashLength]
	maxPrefix := workspaceChildNameMaxLength - len(hashSuffix) - len(suffix) - 2
	if maxPrefix < 1 {
		maxSuffix := workspaceChildNameMaxLength - workspaceChildNameHashLength - 3
		suffix = strings.TrimLeft(suffix[len(suffix)-maxSuffix:], "-")
		maxPrefix = workspaceChildNameMaxLength - len(hashSuffix) - len(suffix) - 2
	}
	workspaceName = strings.TrimRight(workspaceName[:maxPrefix], "-.")
	if workspaceName == "" {
		workspaceName = "w"
	}
	return workspaceName + "-" + hashSuffix + "-" + suffix
}
