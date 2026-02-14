/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/store/sqlite"
)

// newReconciler creates a TaskReconciler with all dependencies for testing.
func newReconciler() *TaskReconciler {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		panic(err)
	}
	ss := sqlite.NewStore(db, ":memory:")
	return &TaskReconciler{
		Client:          k8sClient,
		Scheme:          k8sClient.Scheme(),
		JobBuilder:      NewJobBuilder(k8sClient),
		SessionManager:  NewSessionManager(ss),
		WebhookNotifier: NewWebhookNotifier(),
		Recorder:        record.NewFakeRecorder(100),
		ResultStore:     ss,
		SessionStore:    ss,
		PlanStore:       ss,
	}
}

// cleanupTask removes the task and waits for deletion.
func cleanupTask(ctx context.Context, nn types.NamespacedName) {
	task := &corev1alpha1.Task{}
	if err := k8sClient.Get(ctx, nn, task); err == nil {
		// Remove finalizer first so delete can proceed
		if controllerutil.ContainsFinalizer(task, TaskFinalizer) {
			controllerutil.RemoveFinalizer(task, TaskFinalizer)
			_ = k8sClient.Update(ctx, task)
		}
		_ = k8sClient.Delete(ctx, task)
	}
}

var _ = Describe("Task Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: defaultNS,
		}
		task := &corev1alpha1.Task{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Task")
			err := k8sClient.Get(ctx, typeNamespacedName, task)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: defaultNS,
					},
					Spec: corev1alpha1.TaskSpec{
						Type:    corev1alpha1.TaskTypeContainer,
						Image:   "alpine:latest",
						Command: []string{"echo", "hello"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Task{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Task")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := newReconciler()

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("handleDeletion", func() {
		It("should clean up result ConfigMap and remove finalizer", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-handle-deletion"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}

			// Save result to store
			Expect(r.ResultStore.SaveResult(ctx, ns, taskName, []byte("some-result"))).To(Succeed())

			// Create the task with finalizer
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo", "hello"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Set result ref in status
			task.Status.ResultRef = &corev1alpha1.ResultReference{
				Available: true,
			}
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			// Delete the task (sets DeletionTimestamp)
			Expect(k8sClient.Delete(ctx, task)).To(Succeed())

			// Re-fetch to get DeletionTimestamp
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.DeletionTimestamp.IsZero()).To(BeFalse())

			// Reconcile – should call handleDeletion
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Result should be deleted from store
			_, err = r.ResultStore.GetResult(ctx, ns, taskName)
			Expect(err).To(HaveOccurred())

			// Task should be gone (finalizer removed → deletion completes)
			err = k8sClient.Get(ctx, nn, &corev1alpha1.Task{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should clean up associated Job during deletion", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-deletion-job-cleanup"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}

			// Create job
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName + "-job",
					Namespace: ns,
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{
								{Name: "worker", Image: "alpine:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			// Create task with finalizer
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Set job name in status
			task.Status.JobName = taskName + "-job"
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			// Delete the task
			Expect(k8sClient.Delete(ctx, task)).To(Succeed())
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())

			// Reconcile to handle deletion
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Job should be deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: taskName + "-job", Namespace: ns}, &batchv1.Job{})
				return errors.IsNotFound(err)
			}, 5*time.Second, 200*time.Millisecond).Should(BeTrue())
		})

		It("should handle deletion when no result ConfigMap exists", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-deletion-no-result"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			Expect(k8sClient.Delete(ctx, task)).To(Succeed())
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("handlePending", func() {
		It("should create a Job and transition to Running", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-handle-pending"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			// Create task
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo", "hello"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// First reconcile: add finalizer
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile: initialize status to Pending
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Third reconcile: handlePending → create Job, set Running
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Verify task is now Running
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))
			Expect(task.Status.JobName).NotTo(BeEmpty())
			Expect(task.Status.StartTime).NotTo(BeNil())
			Expect(task.Status.Attempts).To(Equal(int32(1)))

			// Verify JobCreated condition
			cond := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			// Verify Job exists
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: ns}, job)).To(Succeed())
		})

		It("should fail when agent ref does not exist", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-pending-bad-agent"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "test prompt",
					AgentRef: &corev1alpha1.AgentReference{
						Name: "nonexistent-agent",
					},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Reconcile through finalizer and status init
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})

			// handlePending should fail the task because agent doesn't exist
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(task.Status.Message).To(ContainSubstring("failed to get agent"))
		})
	})

	Context("handleRunning", func() {
		It("should complete task when Job succeeds", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-running-success"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			// Create task and reconcile to Running
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo", "hello"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Reconcile to Running
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn}) // finalizer
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn}) // status init
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn}) // handlePending→Running

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))

			// Simulate Job success by updating Job status
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: ns}, job)).To(Succeed())
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Reconcile handleRunning
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseSucceeded))
			Expect(task.Status.Message).To(Equal("task completed successfully"))
			Expect(task.Status.CompletionTime).NotTo(BeNil())

			// Verify Complete condition
			cond := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeComplete)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("TaskSucceeded"))
		})

		It("should fail task when Job fails and no retry policy", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-running-fail"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"false"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())

			// Simulate Job failure
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: ns}, job)).To(Succeed())
			job.Status.Failed = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(task.Status.Message).To(Equal("job failed"))

			cond := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeComplete)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TaskFailed"))
		})

		It("should fail task when Job is not found", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-running-job-missing"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Set status to Running with a non-existent job name
			now := metav1.Now()
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			task.Status.JobName = "nonexistent-job"
			task.Status.StartTime = &now
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(task.Status.Message).To(Equal("job not found"))
		})

		It("should fail task on timeout", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-running-timeout"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			timeout := metav1.Duration{Duration: 1 * time.Millisecond}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"sleep", "100"},
					Timeout: &timeout,
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Set status to Running with a start time far in the past
			pastTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			task.Status.JobName = "some-job"
			task.Status.StartTime = &pastTime
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(task.Status.Message).To(Equal("task timed out"))
		})

		It("should requeue when Job is still running", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-running-still-running"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"sleep", "100"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Reconcile to Running
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))

			// Job status: no Succeeded, no Failed (still running)
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))

			// Phase should still be Running
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))
		})
	})

	Context("handleCompleted", func() {
		It("should be a no-op for completed task without webhook", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-completed-no-webhook"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("completeTask / failTask", func() {
		It("should set Succeeded phase with correct condition", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-complete-success"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			result, err := r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "all good")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseSucceeded))
			Expect(task.Status.Message).To(Equal("all good"))
			Expect(task.Status.CompletionTime).NotTo(BeNil())

			cond := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeComplete)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("TaskSucceeded"))
		})

		It("should set Failed phase via failTask", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-fail-task"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			result, err := r.failTask(ctx, task, "something broke")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(task.Status.Message).To(Equal("something broke"))

			cond := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeComplete)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TaskFailed"))
		})
	})

	Context("shouldRetry", func() {
		It("should return false when no retry policy", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{RetryPolicy: nil},
			}
			Expect(r.shouldRetry(task)).To(BeFalse())
		})

		It("should return true when attempts < maxRetries", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
				},
				Status: corev1alpha1.TaskStatus{Attempts: 1},
			}
			Expect(r.shouldRetry(task)).To(BeTrue())
		})

		It("should return false when attempts >= maxRetries", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
				},
				Status: corev1alpha1.TaskStatus{Attempts: 3},
			}
			Expect(r.shouldRetry(task)).To(BeFalse())
		})

		It("should return false when attempts equal maxRetries", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 2},
				},
				Status: corev1alpha1.TaskStatus{Attempts: 2},
			}
			Expect(r.shouldRetry(task)).To(BeFalse())
		})
	})

	Context("retryTask", func() {
		It("should reset task to Pending and schedule retry with delay", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-retry-task"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"false"},
					RetryPolicy: &corev1alpha1.RetryPolicy{
						MaxRetries: 3,
					},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Set status to Running
			now := metav1.Now()
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			task.Status.JobName = ""
			task.Status.StartTime = &now
			task.Status.Attempts = 1
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			result, err := r.retryTask(ctx, task)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Task should be back to Pending
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhasePending))
			Expect(task.Status.JobName).To(BeEmpty())
		})

		It("should retry task when Job fails with retry policy", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-running-retry"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"false"},
					RetryPolicy: &corev1alpha1.RetryPolicy{
						MaxRetries: 3,
					},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Reconcile to Running
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))
			Expect(task.Status.Attempts).To(Equal(int32(1)))

			// Simulate Job failure
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: ns}, job)).To(Succeed())
			job.Status.Failed = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Reconcile – should trigger retry (attempts=1 < maxRetries=3)
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Task should be back to Pending for retry
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhasePending))
		})
	})

	Context("calculateRetryDelay", func() {
		It("should return default delay when no retry policy", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec:   corev1alpha1.TaskSpec{RetryPolicy: nil},
				Status: corev1alpha1.TaskStatus{Attempts: 1},
			}
			Expect(r.calculateRetryDelay(task)).To(Equal(10 * time.Second))
		})

		It("should return default delay when no initial delay set", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
				},
				Status: corev1alpha1.TaskStatus{Attempts: 1},
			}
			Expect(r.calculateRetryDelay(task)).To(Equal(10 * time.Second))
		})

		It("should calculate exponential backoff with default multiplier", func() {
			r := newReconciler()
			initialDelay := metav1.Duration{Duration: 5 * time.Second}
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					RetryPolicy: &corev1alpha1.RetryPolicy{
						MaxRetries:   5,
						InitialDelay: &initialDelay,
					},
				},
				Status: corev1alpha1.TaskStatus{Attempts: 1},
			}
			// attempt=1 → delay = 5s (no multiplication)
			Expect(r.calculateRetryDelay(task)).To(Equal(5 * time.Second))

			// attempt=2 → delay = 5s * 2 = 10s
			task.Status.Attempts = 2
			Expect(r.calculateRetryDelay(task)).To(Equal(10 * time.Second))

			// attempt=3 → delay = 5s * 2 * 2 = 20s
			task.Status.Attempts = 3
			Expect(r.calculateRetryDelay(task)).To(Equal(20 * time.Second))
		})

		It("should use custom backoff multiplier", func() {
			r := newReconciler()
			initialDelay := metav1.Duration{Duration: 2 * time.Second}
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					RetryPolicy: &corev1alpha1.RetryPolicy{
						MaxRetries:        5,
						InitialDelay:      &initialDelay,
						BackoffMultiplier: 3,
					},
				},
				Status: corev1alpha1.TaskStatus{Attempts: 2},
			}
			// attempt=2 → delay = 2s * 3 = 6s
			Expect(r.calculateRetryDelay(task)).To(Equal(6 * time.Second))
		})

		It("should cap delay at 5 minutes", func() {
			r := newReconciler()
			initialDelay := metav1.Duration{Duration: 1 * time.Minute}
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					RetryPolicy: &corev1alpha1.RetryPolicy{
						MaxRetries:   10,
						InitialDelay: &initialDelay,
					},
				},
				Status: corev1alpha1.TaskStatus{Attempts: 10},
			}
			Expect(r.calculateRetryDelay(task)).To(Equal(5 * time.Minute))
		})
	})

	Context("collectResult", func() {
		It("should set ResultRef when result exists in store", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-collect-result"
			ns := defaultNS

			// Save result to store
			Expect(r.ResultStore.SaveResult(ctx, ns, taskName, []byte("hello world"))).To(Succeed())

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
			}

			err := r.collectResult(ctx, task)
			Expect(err).NotTo(HaveOccurred())
			Expect(task.Status.ResultRef).NotTo(BeNil())
			Expect(task.Status.ResultRef.Available).To(BeTrue())
		})

		It("should return nil when result does not exist in store", func() {
			ctx := context.Background()
			r := newReconciler()

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nonexistent-task",
					Namespace: defaultNS,
				},
			}

			err := r.collectResult(ctx, task)
			Expect(err).NotTo(HaveOccurred())
			Expect(task.Status.ResultRef).To(BeNil())
		})
	})

	Context("resolveProviderRef", func() {
		It("should return nil for agent type tasks", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAgent,
					AI: &corev1alpha1.AISpec{
						ProviderRef: &corev1alpha1.ProviderReference{Name: "test-provider"},
					},
				},
			}
			Expect(r.resolveProviderRef(task, nil)).To(BeNil())
		})

		It("should return task-level provider ref when set", func() {
			r := newReconciler()
			taskProviderRef := &corev1alpha1.ProviderReference{
				Name:      "task-provider",
				Namespace: "prod",
			}
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
					AI: &corev1alpha1.AISpec{
						ProviderRef: taskProviderRef,
					},
				},
			}
			agent := &corev1alpha1.Agent{
				Spec: corev1alpha1.AgentSpec{
					ProviderRef: &corev1alpha1.ProviderReference{Name: "agent-provider"},
				},
			}

			ref := r.resolveProviderRef(task, agent)
			Expect(ref).NotTo(BeNil())
			Expect(ref.Name).To(Equal("task-provider"))
		})

		It("should fall back to agent-level provider ref", func() {
			r := newReconciler()
			agentProviderRef := &corev1alpha1.ProviderReference{
				Name: "agent-provider",
			}
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
				},
			}
			agent := &corev1alpha1.Agent{
				Spec: corev1alpha1.AgentSpec{
					ProviderRef: agentProviderRef,
				},
			}

			ref := r.resolveProviderRef(task, agent)
			Expect(ref).NotTo(BeNil())
			Expect(ref.Name).To(Equal("agent-provider"))
		})

		It("should return nil when no provider ref is set", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
				},
			}
			Expect(r.resolveProviderRef(task, nil)).To(BeNil())
		})

		It("should return nil for container type tasks with no AI spec", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeContainer,
				},
			}
			Expect(r.resolveProviderRef(task, nil)).To(BeNil())
		})
	})

	Context("validateTaskAgentCompatibility", func() {
		It("should succeed for container tasks without agent", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
			}
			Expect(r.validateTaskAgentCompatibility(task, nil)).To(Succeed())
		})

		It("should succeed for AI tasks without agent", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			}
			Expect(r.validateTaskAgentCompatibility(task, nil)).To(Succeed())
		})

		It("should succeed for AI tasks with agent without runtime", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			}
			agent := &corev1alpha1.Agent{
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
				},
			}
			Expect(r.validateTaskAgentCompatibility(task, agent)).To(Succeed())
		})

		It("should fail for agent tasks without agentRef", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
			}
			err := r.validateTaskAgentCompatibility(task, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("agent tasks require an agentRef"))
		})

		It("should fail for agent tasks when agent has no runtime", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAgent,
					Prompt: "test",
				},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
				Spec:       corev1alpha1.AgentSpec{},
			}
			err := r.validateTaskAgentCompatibility(task, agent)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not have a runtime configured"))
		})

		It("should fail for agent tasks when agent has both runtime and providerRef", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAgent,
					Prompt: "test",
				},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
				Spec: corev1alpha1.AgentSpec{
					Runtime:     &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
					ProviderRef: &corev1alpha1.ProviderReference{Name: "p"},
				},
			}
			err := r.validateTaskAgentCompatibility(task, agent)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("both runtime and providerRef set"))
		})

		It("should fail for agent tasks when agent has runtime and model.provider", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAgent,
					Prompt: "test",
				},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot},
					Model:   &corev1alpha1.ModelConfig{Provider: "openai"},
				},
			}
			err := r.validateTaskAgentCompatibility(task, agent)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("both runtime and model.provider set"))
		})

		It("should fail for agent tasks without prompt", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAgent,
				},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
				},
			}
			err := r.validateTaskAgentCompatibility(task, agent)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("prompt is required"))
		})

		It("should succeed for valid agent tasks", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAgent,
					Prompt: "fix this bug",
				},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot},
				},
			}
			Expect(r.validateTaskAgentCompatibility(task, agent)).To(Succeed())
		})

		It("should fail for AI tasks with agent that has runtime", func() {
			r := newReconciler()
			task := &corev1alpha1.Task{
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
				},
			}
			err := r.validateTaskAgentCompatibility(task, agent)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("use type: agent instead of type: ai"))
		})
	})

	Context("SetupWithManager", func() {
		It("should set up the controller without error", func() {
			r := newReconciler()
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme: k8sClient.Scheme(),
			})
			Expect(err).NotTo(HaveOccurred())

			err = r.SetupWithManager(mgr)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Reconcile edge cases", func() {
		It("should return no error when task is not found (deleted)", func() {
			ctx := context.Background()
			r := newReconciler()

			nn := types.NamespacedName{Name: "nonexistent-task", Namespace: defaultNS}
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should add finalizer on first reconcile", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-add-finalizer"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(task, TaskFinalizer)).To(BeTrue())
		})

		It("should initialize status to Pending on second reconcile", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-init-status"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// First reconcile: add finalizer
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})

			// Second reconcile: initialize status
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhasePending))
		})
	})

	Context("Scheduled Tasks", func() {
		It("should transition from Pending to Scheduled when schedule is set", func() {
			ctx := context.Background()
			r := newReconciler()

			taskName := "test-scheduled-pending"
			nn := types.NamespacedName{Name: taskName, Namespace: defaultNS}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: defaultNS,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:     corev1alpha1.TaskTypeContainer,
					Image:    "alpine:latest",
					Command:  []string{"echo", "hello"},
					Schedule: "*/5 * * * *",
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// First reconcile: add finalizer
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile: initialize status to Pending
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Third reconcile: should transition to Scheduled
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseScheduled))
			Expect(task.Status.NextScheduleTime).NotTo(BeNil())
		})

		It("should fail with invalid cron expression", func() {
			ctx := context.Background()
			r := newReconciler()

			taskName := "test-scheduled-invalid-cron"
			nn := types.NamespacedName{Name: taskName, Namespace: defaultNS}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: defaultNS,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:     corev1alpha1.TaskTypeContainer,
					Image:    "alpine:latest",
					Schedule: "not-a-cron",
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// First reconcile: add finalizer
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile: initialize to Pending
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Third reconcile: should fail with invalid cron
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(task.Status.Message).To(ContainSubstring("invalid cron expression"))
		})

		It("should create child task when schedule time is reached", func() {
			ctx := context.Background()
			r := newReconciler()

			taskName := "test-scheduled-child"
			nn := types.NamespacedName{Name: taskName, Namespace: defaultNS}
			defer cleanupTask(ctx, nn)

			deadline := int64(300)
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: defaultNS,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:                    corev1alpha1.TaskTypeContainer,
					Image:                   "alpine:latest",
					Command:                 []string{"echo", "hello"},
					Schedule:                "*/1 * * * *",
					StartingDeadlineSeconds: &deadline,
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Reconcile through: finalizer → Pending → Scheduled
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Simulate time passing: set LastScheduleTime to the past
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseScheduled))
			pastTime := metav1.NewTime(time.Now().Add(-2 * time.Minute))
			task.Status.LastScheduleTime = &pastTime
			task.Status.NextScheduleTime = &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			// Reconcile should create a child task
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Verify child task was created
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.LastScheduleTime).NotTo(BeNil())

			// List child tasks
			var childList corev1alpha1.TaskList
			Expect(k8sClient.List(ctx, &childList,
				client.InNamespace(defaultNS),
				client.MatchingLabels{"mercan.ai/parent-task": taskName},
			)).To(Succeed())
			Expect(childList.Items).NotTo(BeEmpty())

			// Verify child has correct properties
			child := &childList.Items[0]
			Expect(child.Spec.Schedule).To(BeEmpty(), "child should not have schedule")
			Expect(child.Spec.Image).To(Equal("alpine:latest"))
			Expect(child.Labels["mercan.ai/scheduled-run"]).To(Equal("true"))

			// Clean up child
			for i := range childList.Items {
				cleanupTask(ctx, types.NamespacedName{Name: childList.Items[i].Name, Namespace: defaultNS})
			}
		})

		It("should skip run when Forbid policy and active child exists", func() {
			ctx := context.Background()
			r := newReconciler()

			taskName := "test-scheduled-forbid"
			nn := types.NamespacedName{Name: taskName, Namespace: defaultNS}
			defer cleanupTask(ctx, nn)

			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: defaultNS,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:              corev1alpha1.TaskTypeContainer,
					Image:             "alpine:latest",
					Schedule:          "*/1 * * * *",
					ConcurrencyPolicy: corev1alpha1.ForbidConcurrent,
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Create an active child task
			activeChild := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName + "-active-child",
					Namespace: defaultNS,
					Labels: map[string]string{
						"mercan.ai/parent-task":   taskName,
						"mercan.ai/scheduled-run": "true",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:  corev1alpha1.TaskTypeContainer,
					Image: "alpine:latest",
				},
			}
			Expect(k8sClient.Create(ctx, activeChild)).To(Succeed())
			defer cleanupTask(ctx, types.NamespacedName{Name: activeChild.Name, Namespace: defaultNS})

			// Set child to Running
			activeChild.Status.Phase = corev1alpha1.TaskPhaseRunning
			Expect(k8sClient.Status().Update(ctx, activeChild)).To(Succeed())

			// Reconcile parent through to Scheduled
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Set LastScheduleTime in the past to trigger a run
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			pastTime := metav1.NewTime(time.Now().Add(-2 * time.Minute))
			task.Status.LastScheduleTime = &pastTime
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			// Reconcile should NOT create a new child (Forbid)
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Verify no additional child was created
			var childList corev1alpha1.TaskList
			Expect(k8sClient.List(ctx, &childList,
				client.InNamespace(defaultNS),
				client.MatchingLabels{"mercan.ai/parent-task": taskName},
			)).To(Succeed())
			Expect(childList.Items).To(HaveLen(1), "should only have the original active child")
		})

		It("should not create runs when suspended", func() {
			ctx := context.Background()
			r := newReconciler()

			taskName := "test-scheduled-suspend"
			nn := types.NamespacedName{Name: taskName, Namespace: defaultNS}
			defer cleanupTask(ctx, nn)

			suspend := true
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: defaultNS,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:     corev1alpha1.TaskTypeContainer,
					Image:    "alpine:latest",
					Schedule: "*/1 * * * *",
					Suspend:  &suspend,
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Reconcile through to Scheduled
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Set LastScheduleTime in the past
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			pastTime := metav1.NewTime(time.Now().Add(-2 * time.Minute))
			task.Status.LastScheduleTime = &pastTime
			Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())

			// Reconcile — should skip because suspended
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			// No children should be created
			var childList corev1alpha1.TaskList
			Expect(k8sClient.List(ctx, &childList,
				client.InNamespace(defaultNS),
				client.MatchingLabels{"mercan.ai/parent-task": taskName},
			)).To(Succeed())
			Expect(childList.Items).To(BeEmpty())
		})
	})

	Context("coordination enforcement", func() {
		It("should fail child task when depth exceeds maxDepth", func() {
			ctx := context.Background()
			r := newReconciler()
			ns := defaultNS

			// Create parent agent with coordination enabled, maxDepth=1
			parentAgent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "coord-parent-agent-depth",
					Namespace: ns,
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
					Coordination: &corev1alpha1.CoordinationConfig{
						Enabled:  true,
						MaxDepth: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentAgent)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, parentAgent) }()

			// Create parent task referencing the agent
			parentTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "coord-parent-task-depth",
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "parent task",
					AgentRef: &corev1alpha1.AgentReference{
						Name: "coord-parent-agent-depth",
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentTask)).To(Succeed())
			defer cleanupTask(ctx, types.NamespacedName{Name: parentTask.Name, Namespace: ns})

			// Create child task with depth=2 (exceeds maxDepth=1)
			childName := "coord-child-depth-exceeded"
			childNN := types.NamespacedName{Name: childName, Namespace: ns}
			childTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      childName,
					Namespace: ns,
					Labels: map[string]string{
						"mercan.ai/parent-task": "coord-parent-task-depth",
					},
					Annotations: map[string]string{
						"mercan.ai/coordination-depth": "2",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo", "hello"},
				},
			}
			Expect(k8sClient.Create(ctx, childTask)).To(Succeed())
			defer cleanupTask(ctx, childNN)

			// Reconcile through: finalizer → status init → handlePending
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, childNN, childTask)).To(Succeed())
			Expect(childTask.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(childTask.Status.Message).To(ContainSubstring("depth"))
			Expect(childTask.Status.Message).To(ContainSubstring("exceeds max"))
		})

		It("should fail child task when agent is not in allowedAgents", func() {
			ctx := context.Background()
			r := newReconciler()
			ns := defaultNS

			// Create parent agent with allowedAgents=[allowed-agent]
			parentAgent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "coord-parent-agent-allow",
					Namespace: ns,
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
					Coordination: &corev1alpha1.CoordinationConfig{
						Enabled: true,
						AllowedAgents: []corev1alpha1.AllowedAgent{
							{Name: "allowed-agent"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentAgent)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, parentAgent) }()

			// Create the disallowed agent so the child's agentRef resolves
			disallowedAgent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "disallowed-agent",
					Namespace: ns,
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
				},
			}
			Expect(k8sClient.Create(ctx, disallowedAgent)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, disallowedAgent) }()

			// Create parent task
			parentTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "coord-parent-task-allow",
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "parent task",
					AgentRef: &corev1alpha1.AgentReference{
						Name: "coord-parent-agent-allow",
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentTask)).To(Succeed())
			defer cleanupTask(ctx, types.NamespacedName{Name: parentTask.Name, Namespace: ns})

			// Create child task referencing disallowed-agent
			childName := "coord-child-not-allowed"
			childNN := types.NamespacedName{Name: childName, Namespace: ns}
			childTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      childName,
					Namespace: ns,
					Labels: map[string]string{
						"mercan.ai/parent-task": "coord-parent-task-allow",
					},
					Annotations: map[string]string{
						"mercan.ai/coordination-depth": "1",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "child task",
					AgentRef: &corev1alpha1.AgentReference{
						Name: "disallowed-agent",
					},
				},
			}
			Expect(k8sClient.Create(ctx, childTask)).To(Succeed())
			defer cleanupTask(ctx, childNN)

			// Reconcile through: finalizer → status init → handlePending
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, childNN, childTask)).To(Succeed())
			Expect(childTask.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
			Expect(childTask.Status.Message).To(ContainSubstring("not in parent's allowedAgents"))
		})

		It("should requeue child task when max concurrent children reached", func() {
			ctx := context.Background()
			r := newReconciler()
			ns := defaultNS

			// Create parent agent with maxConcurrentChildren=1
			parentAgent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "coord-parent-agent-conc",
					Namespace: ns,
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
					Coordination: &corev1alpha1.CoordinationConfig{
						Enabled:               true,
						MaxConcurrentChildren: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentAgent)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, parentAgent) }()

			// Create parent task
			parentTaskName := "coord-parent-task-conc"
			parentTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      parentTaskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "parent task",
					AgentRef: &corev1alpha1.AgentReference{
						Name: "coord-parent-agent-conc",
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentTask)).To(Succeed())
			defer cleanupTask(ctx, types.NamespacedName{Name: parentTaskName, Namespace: ns})

			// Create a running sibling child task
			siblingName := "coord-sibling-running"
			siblingNN := types.NamespacedName{Name: siblingName, Namespace: ns}
			sibling := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      siblingName,
					Namespace: ns,
					Labels: map[string]string{
						"mercan.ai/parent-task": parentTaskName,
					},
					Annotations: map[string]string{
						"mercan.ai/coordination-depth": "1",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo", "sibling"},
				},
			}
			Expect(k8sClient.Create(ctx, sibling)).To(Succeed())
			defer cleanupTask(ctx, siblingNN)

			// Set sibling to Running phase
			sibling.Status.Phase = corev1alpha1.TaskPhaseRunning
			Expect(k8sClient.Status().Update(ctx, sibling)).To(Succeed())

			// Create the new child task that should be requeued
			childName := "coord-child-conc-pending"
			childNN := types.NamespacedName{Name: childName, Namespace: ns}
			childTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      childName,
					Namespace: ns,
					Labels: map[string]string{
						"mercan.ai/parent-task": parentTaskName,
					},
					Annotations: map[string]string{
						"mercan.ai/coordination-depth": "1",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo", "child"},
				},
			}
			Expect(k8sClient.Create(ctx, childTask)).To(Succeed())
			defer cleanupTask(ctx, childNN)

			// Reconcile through: finalizer → status init
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})

			// Third reconcile: handlePending → should requeue due to concurrency
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Task should still be Pending, not Failed
			Expect(k8sClient.Get(ctx, childNN, childTask)).To(Succeed())
			Expect(childTask.Status.Phase).To(Equal(corev1alpha1.TaskPhasePending))
		})

		It("should pass coordination checks for valid child task", func() {
			ctx := context.Background()
			r := newReconciler()
			ns := defaultNS

			// Create parent agent with coordination allowing "child-agent"
			parentAgent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "coord-parent-agent-valid",
					Namespace: ns,
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
					Coordination: &corev1alpha1.CoordinationConfig{
						Enabled:  true,
						MaxDepth: 3,
						AllowedAgents: []corev1alpha1.AllowedAgent{
							{Name: "child-agent-valid"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentAgent)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, parentAgent) }()

			// Create the child agent so agentRef resolves
			childAgent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-agent-valid",
					Namespace: ns,
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude"},
				},
			}
			Expect(k8sClient.Create(ctx, childAgent)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, childAgent) }()

			// Create parent task
			parentTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "coord-parent-task-valid",
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "parent task",
					AgentRef: &corev1alpha1.AgentReference{
						Name: "coord-parent-agent-valid",
					},
				},
			}
			Expect(k8sClient.Create(ctx, parentTask)).To(Succeed())
			defer cleanupTask(ctx, types.NamespacedName{Name: parentTask.Name, Namespace: ns})

			// Create child task with valid depth and allowed agent
			childName := "coord-child-valid"
			childNN := types.NamespacedName{Name: childName, Namespace: ns}
			childTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      childName,
					Namespace: ns,
					Labels: map[string]string{
						"mercan.ai/parent-task": "coord-parent-task-valid",
					},
					Annotations: map[string]string{
						"mercan.ai/coordination-depth": "1",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "child task",
					AgentRef: &corev1alpha1.AgentReference{
						Name: "child-agent-valid",
					},
				},
			}
			Expect(k8sClient.Create(ctx, childTask)).To(Succeed())
			defer cleanupTask(ctx, childNN)

			// Reconcile through: finalizer → status init → handlePending
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: childNN})
			Expect(err).NotTo(HaveOccurred())

			// Task should NOT have failed with coordination errors
			// It may fail later due to provider/job creation, but not coordination
			Expect(k8sClient.Get(ctx, childNN, childTask)).To(Succeed())
			if childTask.Status.Phase == corev1alpha1.TaskPhaseFailed {
				Expect(childTask.Status.Message).NotTo(ContainSubstring("depth"))
				Expect(childTask.Status.Message).NotTo(ContainSubstring("allowedAgents"))
				Expect(childTask.Status.Message).NotTo(ContainSubstring("coordination"))
			}
		})
	})

	Context("collectResult with existing result", func() {
		It("should set ResultRef for completeTask flow", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := fmt.Sprintf("test-complete-with-result-%d", time.Now().UnixNano())
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			// Save result to store
			Expect(r.ResultStore.SaveResult(ctx, ns, taskName, []byte("task output"))).To(Succeed())

			// Create task
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       taskName,
					Namespace:  ns,
					Finalizers: []string{TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeContainer,
					Image:   "alpine:latest",
					Command: []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Complete the task – collectResult should pick up the store result
			_, err := r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "done")
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.ResultRef).NotTo(BeNil())
			Expect(task.Status.ResultRef.Available).To(BeTrue())
		})
	})

	Context("autonomous mode", func() {
		It("should re-create Job and increment iteration when autonomous task Job succeeds", func() {
			ctx := context.Background()
			r := newReconciler()
			taskName := "test-autonomous-loop"
			agentName := "test-autonomous-agent"
			ns := defaultNS
			nn := types.NamespacedName{Name: taskName, Namespace: ns}
			agentNN := types.NamespacedName{Name: agentName, Namespace: ns}
			defer cleanupTask(ctx, nn)

			// Create Agent with Coordination.Autonomous = true
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      agentName,
					Namespace: ns,
				},
				Spec: corev1alpha1.AgentSpec{
					Coordination: &corev1alpha1.CoordinationConfig{
						Enabled:       true,
						Autonomous:    true,
						MaxIterations: 10,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
				})
			}()

			// Verify the agent was created
			Expect(k8sClient.Get(ctx, agentNN, agent)).To(Succeed())

			// Create Task referencing the autonomous agent
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      taskName,
					Namespace: ns,
				},
				Spec: corev1alpha1.TaskSpec{
					Type:   corev1alpha1.TaskTypeAI,
					Prompt: "autonomous test prompt",
					AgentRef: &corev1alpha1.AgentReference{
						Name: agentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())

			// Reconcile through: finalizer → status init → handlePending→Running
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})

			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))
			Expect(task.Status.JobName).NotTo(BeEmpty())
			oldJobName := task.Status.JobName

			// Simulate Job success
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: oldJobName, Namespace: ns}, job)).To(Succeed())
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Reconcile handleRunning — autonomous path should reset to Pending
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Verify: task is NOT Succeeded, phase is reset to Pending
			Expect(k8sClient.Get(ctx, nn, task)).To(Succeed())
			Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhasePending))
			Expect(task.Status.Iteration).To(Equal(int32(1)))
			Expect(task.Status.JobName).To(BeEmpty())
			Expect(task.Status.Message).To(ContainSubstring("autonomous iteration"))

			// Old Job should be deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: oldJobName, Namespace: ns}, &batchv1.Job{})
				return errors.IsNotFound(err)
			}, 5*time.Second, 200*time.Millisecond).Should(BeTrue())
		})
	})
})
