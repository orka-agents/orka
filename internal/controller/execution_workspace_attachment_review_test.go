package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	current.Status.Conditions = []metav1.Condition{{
		Type:   string(workspacev1alpha1.ConditionWorkspaceAttached),
		Status: metav1.ConditionTrue,
		Reason: "Attached",
	}}
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
	current.Status.Conditions = []metav1.Condition{{
		Type:   string(workspacev1alpha1.ConditionWorkspaceAttached),
		Status: metav1.ConditionFalse,
		Reason: "Revoked",
	}}
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
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatalf("get second attached workspace: %v", err)
	}
	if current.Spec.AttachmentEpoch != 2 || current.Spec.Attachment == nil || current.Spec.Attachment.Epoch != 2 {
		t.Fatalf("second attachment state = %#v", current.Spec)
	}
}

func TestWorkspaceAttachmentManagerBootstrapsLegacyActiveEpochOnRevocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := testBoundWorkspace(t, "attachment-review", "legacy-workspace", "class", "provider")
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
		Client:   c,
		LeaseTTL: time.Minute,
		Now: func() time.Time {
			return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
		},
	}
}
