package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

var _ = Describe("workspace.orka.ai schema validation", func() {
	var namespace string

	BeforeEach(func(ctx context.Context) {
		namespace = fmt.Sprintf("workspace-validation-%d", GinkgoRandomSeed()+int64(GinkgoParallelProcess()))
		ensureNamespace(ctx, namespace)
	})

	It("accepts provider, pool, direct and pooled classes, and interactive workspaces", func(ctx context.Context) {
		provider := validWorkspaceProvider("provider-valid")
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())

		pool := validWorkspacePool(namespace, "pool-valid", provider.Name)
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		direct := validWorkspaceClass(namespace, "direct-valid")
		direct.Spec.ProviderRef = &workspacev1alpha1.ClusterObjectReference{Name: provider.Name}
		direct.Spec.ParametersRef = &workspacev1alpha1.TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakeWorkspaceProfile", Name: "direct"}
		Expect(k8sClient.Create(ctx, direct)).To(Succeed())

		pooled := validWorkspaceClass(namespace, "pooled-valid")
		pooled.Spec.PoolRef = &workspacev1alpha1.LocalObjectReference{Name: pool.Name}
		Expect(k8sClient.Create(ctx, pooled)).To(Succeed())

		workspace := validExecutionWorkspace(namespace, "workspace-valid", direct.Name, provider.Name)
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
	})

	It("rejects invalid class provisioning sources", func(ctx context.Context) {
		class := validWorkspaceClass(namespace, "class-neither")
		Expect(k8sClient.Create(ctx, class)).NotTo(Succeed())

		class = validWorkspaceClass(namespace, "class-empty-pool")
		class.Spec.PoolRef = &workspacev1alpha1.LocalObjectReference{}
		Expect(k8sClient.Create(ctx, class)).NotTo(Succeed())

		class = validWorkspaceClass(namespace, "class-both")
		class.Spec.ProviderRef = &workspacev1alpha1.ClusterObjectReference{Name: "provider"}
		class.Spec.ParametersRef = &workspacev1alpha1.TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakeWorkspaceProfile", Name: "profile"}
		class.Spec.PoolRef = &workspacev1alpha1.LocalObjectReference{Name: "pool"}
		Expect(k8sClient.Create(ctx, class)).NotTo(Succeed())
	})

	It("enforces provider, pool, class, and workspace immutable bindings", func(ctx context.Context) {
		provider := validWorkspaceProvider("provider-immutable")
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		provider.Spec.ControllerName = "other.workspace.orka.ai/v1"
		Expect(k8sClient.Update(ctx, provider)).NotTo(Succeed())

		pool := validWorkspacePool(namespace, "pool-immutable", "provider-immutable")
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		pool.Spec.ProviderRef.Name = "other"
		Expect(k8sClient.Update(ctx, pool)).NotTo(Succeed())

		class := validWorkspaceClass(namespace, "class-immutable")
		class.Spec.PoolRef = &workspacev1alpha1.LocalObjectReference{Name: pool.Name}
		Expect(k8sClient.Create(ctx, class)).To(Succeed())
		class.Spec.RequiredFeatures = append(class.Spec.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureTLS)
		Expect(k8sClient.Update(ctx, class)).NotTo(Succeed())

		workspace := validExecutionWorkspace(namespace, "workspace-immutable", class.Name, provider.Name)
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		workspace.Spec.Slot = "other"
		Expect(k8sClient.Update(ctx, workspace)).NotTo(Succeed())
	})

	It("allows mutable provider lifecycle, pool capacity, desired state, and attachment", func(ctx context.Context) {
		provider := validWorkspaceProvider("provider-mutable")
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
		Expect(k8sClient.Update(ctx, provider)).To(Succeed())

		pool := validWorkspacePool(namespace, "pool-mutable", provider.Name)
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		pool.Spec.Capacity.MaxSize = 20
		Expect(k8sClient.Update(ctx, pool)).To(Succeed())

		class := validWorkspaceClass(namespace, "class-mutable-workspace")
		class.Spec.PoolRef = &workspacev1alpha1.LocalObjectReference{Name: pool.Name}
		Expect(k8sClient.Create(ctx, class)).To(Succeed())

		workspace := validExecutionWorkspace(namespace, "workspace-mutable", class.Name, provider.Name)
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		workspace.Spec.Attachment = workspaceAttachmentForTest(
			"attachment-1", 1, metav1.NewTime(time.Now().Add(time.Minute)),
		)
		Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
	})

	It("rejects Agent-level workspace class selection", func(ctx context.Context) {
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-classref", Namespace: namespace},
			Spec: corev1alpha1.AgentSpec{
				Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
					ClassRef: &corev1alpha1.WorkspaceClassReference{Name: "coding-v1"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, agent)).NotTo(Succeed())
	})

	It("validates class-based Task reuse and legacy field separation", func(ctx context.Context) {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "task-session-required", Namespace: namespace},
			Spec: corev1alpha1.TaskSpec{
				Type:  corev1alpha1.TaskTypeAgent,
				Image: "example.invalid/agent:latest",
				Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
					ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: "coding-v1"},
					ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, task)).NotTo(Succeed())

		task.Name = "task-session-valid"
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session"}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())

		mixed := task.DeepCopy()
		mixed.ResourceVersion = ""
		mixed.UID = ""
		mixed.Name = "task-mixed-workspace"
		mixed.Spec.Execution.Workspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
		Expect(k8sClient.Create(ctx, mixed)).NotTo(Succeed())

		cleanupMixed := task.DeepCopy()
		cleanupMixed.ResourceVersion = ""
		cleanupMixed.UID = ""
		cleanupMixed.Name = "task-mixed-cleanup"
		cleanupMixed.Spec.Execution.Workspace.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyRetain
		Expect(k8sClient.Create(ctx, cleanupMixed)).NotTo(Succeed())
	})

	It("validates provider-neutral MCP workspace selection", func(ctx context.Context) {
		tool := &corev1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "workspace-mcp", Namespace: namespace},
			Spec: corev1alpha1.ToolSpec{
				Description: "workspace hosted MCP server",
				MCP: &corev1alpha1.MCPToolServer{
					Path:      "/mcp",
					Workspace: &corev1alpha1.MCPWorkspace{ClassRef: corev1alpha1.WorkspaceClassReference{Name: "mcp-service-v1"}, Port: 8080},
				},
			},
		}
		Expect(k8sClient.Create(ctx, tool)).To(Succeed())

		mixed := tool.DeepCopy()
		mixed.ResourceVersion = ""
		mixed.UID = ""
		mixed.Name = "workspace-mcp-mixed"
		mixed.Spec.MCP.SubstrateActor = &corev1alpha1.SubstrateMCPActor{TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "legacy"}}
		Expect(k8sClient.Create(ctx, mixed)).NotTo(Succeed())
	})

	It("persists resolved class profile hashes in status", func(ctx context.Context) {
		class := validWorkspaceClass(namespace, "profile-hash-status")
		class.Spec.PoolRef = &workspacev1alpha1.LocalObjectReference{Name: "pool"}
		Expect(k8sClient.Create(ctx, class)).To(Succeed())
		class.Status.ProfileHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		class.Status.ObservedGeneration = class.Generation
		Expect(k8sClient.Status().Update(ctx, class)).To(Succeed())
		stored := &workspacev1alpha1.ExecutionWorkspaceClass{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: class.Name}, stored)).To(Succeed())
		Expect(stored.Status.ProfileHash).To(Equal(class.Status.ProfileHash))
	})

	It("makes Task executionOutcome immutable once recorded", func(ctx context.Context) {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "immutable-outcome", Namespace: namespace},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Image: "busybox:latest"},
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		task.Status.Phase = corev1alpha1.TaskPhaseFinalizing
		task.Status.ExecutionOutcome = &corev1alpha1.TaskExecutionOutcome{
			Phase:      corev1alpha1.TaskPhaseSucceeded,
			Attempt:    1,
			RecordedAt: metav1.Now(),
			Message:    "completed",
		}
		Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())
		task.Status.ExecutionOutcome.Message = "changed"
		Expect(k8sClient.Status().Update(ctx, task)).NotTo(Succeed())
	})

	It("rejects empty immutable binding UIDs", func(ctx context.Context) {
		workspace := validExecutionWorkspace(namespace, "empty-class-uid", "class", "provider")
		workspace.Spec.ClassBinding.UID = ""
		Expect(k8sClient.Create(ctx, workspace)).NotTo(Succeed())

		workspace = validExecutionWorkspace(namespace, "empty-task-uid", "class", "provider")
		workspace.Spec.Attachment = workspaceAttachmentForTest("attachment", 1, metav1.Now())
		workspace.Spec.Attachment.TaskRef.UID = ""
		Expect(k8sClient.Create(ctx, workspace)).NotTo(Succeed())
	})

	It("rejects credential-bearing workspace status endpoints", func(ctx context.Context) {
		workspace := validExecutionWorkspace(namespace, "credential-endpoint", "class", "provider")
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		workspace.Status.Endpoints = []workspacev1alpha1.ExecutionWorkspaceEndpoint{{
			Name: "data-plane", URL: "https://user:password@example.test/mcp", Protocol: "HTTPS",
		}}
		Expect(k8sClient.Status().Update(ctx, workspace)).NotTo(Succeed())
	})

	It("rejects mode-incompatible attachment and service fields", func(ctx context.Context) {
		interactive := validExecutionWorkspace(namespace, "interactive-service", "class", "provider")
		interactive.Spec.Service = &workspacev1alpha1.ExecutionWorkspaceServiceSpec{Ports: []workspacev1alpha1.ExecutionWorkspaceServicePort{{Name: "mcp", Port: 8080, Protocol: "HTTP"}}}
		Expect(k8sClient.Create(ctx, interactive)).NotTo(Succeed())

		service := validExecutionWorkspace(namespace, "service-attachment", "class", "provider")
		service.Spec.Mode = workspacev1alpha1.ExecutionWorkspaceModeService
		service.Spec.Service = &workspacev1alpha1.ExecutionWorkspaceServiceSpec{Ports: []workspacev1alpha1.ExecutionWorkspaceServicePort{{Name: "mcp", Port: 8080, Protocol: "HTTP"}}}
		service.Spec.Attachment = workspaceAttachmentForTest("attachment-1", 1, metav1.Now())
		Expect(k8sClient.Create(ctx, service)).NotTo(Succeed())
	})
})

func ensureNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

func validWorkspaceProvider(name string) *workspacev1alpha1.ExecutionWorkspaceProvider {
	return &workspacev1alpha1.ExecutionWorkspaceProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{
			ControllerName:    "fake.workspace.orka.ai/v1",
			ParametersRef:     workspacev1alpha1.TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakeProviderConfig", Name: "default"},
			LifecycleState:    workspacev1alpha1.ExecutionWorkspaceProviderActive,
			RequiredContracts: []string{workspacev1alpha1.ContractVersionV1},
		},
	}
}

func validWorkspacePool(namespace, name, provider string) *workspacev1alpha1.ExecutionWorkspacePool {
	return &workspacev1alpha1.ExecutionWorkspacePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: workspacev1alpha1.ExecutionWorkspacePoolSpec{
			ProviderRef:   workspacev1alpha1.ClusterObjectReference{Name: provider},
			ParametersRef: workspacev1alpha1.TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakePoolParameters", Name: "default"},
			Capacity:      workspacev1alpha1.ExecutionWorkspacePoolCapacity{MinReady: 1, MaxSize: 10},
		},
	}
}

func validWorkspaceClass(namespace, name string) *workspacev1alpha1.ExecutionWorkspaceClass {
	return &workspacev1alpha1.ExecutionWorkspaceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: workspacev1alpha1.ExecutionWorkspaceClassSpec{
			Mode:               workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			RequiredFeatures:   []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureExec},
			AllowedReuseScopes: []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeNone, workspacev1alpha1.WorkspaceReuseScopeSession},
			Lifecycle:          validWorkspaceLifecycle(),
		},
	}
}

func validExecutionWorkspace(namespace, name, class, provider string) *workspacev1alpha1.ExecutionWorkspace {
	return &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode:            workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			ClassBinding:    workspacev1alpha1.ImmutableObjectBinding{Name: class, UID: types.UID("class-uid"), Generation: 1, ProfileHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{Name: provider, UID: types.UID("provider-uid"), Generation: 1},
			Slot:            "default",
			DesiredState:    workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle:       validWorkspaceLifecycle(),
		},
	}
}

func validWorkspaceLifecycle() workspacev1alpha1.ExecutionWorkspaceLifecycle {
	return workspacev1alpha1.ExecutionWorkspaceLifecycle{
		DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachSuspend,
		AllowedOnDetach: []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete},
		DetachTimeout:   metav1.Duration{Duration: 2 * time.Minute},
		DeletionPolicy: workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
			ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete,
			PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionRetain,
			Checkpoints:       workspacev1alpha1.WorkspaceDeletionActionDelete,
		},
	}
}
