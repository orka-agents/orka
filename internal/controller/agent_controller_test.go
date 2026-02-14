/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

var _ = Describe("Agent Controller", func() {
	Context("When reconciling a valid agent with model config", func() {
		const resourceName = "test-agent-model"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						Model: &corev1alpha1.ModelConfig{
							Provider: "anthropic",
							Name:     "claude-sonnet-4-20250514",
						},
						SystemPrompt: &corev1alpha1.PromptSource{
							Inline: "You are a helpful assistant.",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready condition to true", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			readyCond := meta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal("ValidationSucceeded"))
		})

		It("should set activeTasks to 0 when no tasks exist", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			Expect(agent.Status.ActiveTasks).To(Equal(int32(0)))
		})
	})

	Context("When reconciling an agent with a missing provider ref", func() {
		const resourceName = "test-agent-missing-provider"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						ProviderRef: &corev1alpha1.ProviderReference{
							Name: "nonexistent-provider",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready condition to false", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			readyCond := meta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("ValidationFailed"))
			Expect(readyCond.Message).To(ContainSubstring("nonexistent-provider"))
		})
	})

	Context("When reconciling an agent with runtime and providerRef (mutually exclusive)", func() {
		const resourceName = "test-agent-mutex"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						ProviderRef: &corev1alpha1.ProviderReference{
							Name: "some-provider",
						},
						Runtime: &corev1alpha1.AgentCLIRuntime{
							Type: "claude",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready condition to false with mutual exclusion error", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			readyCond := meta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Message).To(ContainSubstring("mutually exclusive"))
		})
	})

	Context("When reconciling an agent with no model config or providerRef or runtime", func() {
		const resourceName = "test-agent-no-config"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready condition to false requiring provider config", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			readyCond := meta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Message).To(ContainSubstring("providerRef or model.provider"))
		})
	})

	Context("When reconciling an agent with a valid runtime config", func() {
		const resourceName = "test-agent-runtime"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						Runtime: &corev1alpha1.AgentCLIRuntime{
							Type: "claude",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready condition to true", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			readyCond := meta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("When reconciling an agent with a missing secret ref", func() {
		const resourceName = "test-agent-missing-secret"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						Model: &corev1alpha1.ModelConfig{
							Provider: "anthropic",
							Name:     "claude-sonnet-4-20250514",
						},
						SecretRef: &corev1.LocalObjectReference{
							Name: "nonexistent-secret",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready condition to false for missing secret", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			readyCond := meta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Message).To(ContainSubstring("nonexistent-secret"))
		})
	})

	Context("When reconciling a missing systemPrompt ConfigMap", func() {
		const resourceName = "test-agent-missing-prompt-cm"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						Model: &corev1alpha1.ModelConfig{
							Provider: "anthropic",
							Name:     "claude-sonnet-4-20250514",
						},
						SystemPrompt: &corev1alpha1.PromptSource{
							ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{
								Name: "nonexistent-cm",
								Key:  "prompt",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready condition to false for missing ConfigMap", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			readyCond := meta.FindStatusCondition(agent.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Message).To(ContainSubstring("nonexistent-cm"))
		})
	})

	Context("When reconciling a deleted agent", func() {
		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      "deleted-agent",
			Namespace: "default",
		}

		It("should not return an error", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When counting active tasks", func() {
		const agentName = "test-agent-task-count"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      agentName,
			Namespace: "default",
		}

		BeforeEach(func() {
			// Create the agent
			agent := &corev1alpha1.Agent{}
			err := k8sClient.Get(ctx, typeNamespacedName, agent)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name:      agentName,
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						Model: &corev1alpha1.ModelConfig{
							Provider: "anthropic",
							Name:     "claude-sonnet-4-20250514",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			// Create a running task referencing this agent
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "task-for-agent-count",
					Namespace: "default",
				},
				Spec: corev1alpha1.TaskSpec{
					Type:    corev1alpha1.TaskTypeAI,
					Image:   "alpine:latest",
					Command: []string{"echo", "hello"},
					AgentRef: &corev1alpha1.AgentReference{
						Name: agentName,
					},
				},
			}
			taskNN := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
			existing := &corev1alpha1.Task{}
			if err := k8sClient.Get(ctx, taskNN, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, task)).To(Succeed())
				// Set phase to Running
				Expect(k8sClient.Get(ctx, taskNN, task)).To(Succeed())
				task.Status.Phase = corev1alpha1.TaskPhaseRunning
				Expect(k8sClient.Status().Update(ctx, task)).To(Succeed())
			}
		})

		AfterEach(func() {
			// Cleanup task
			task := &corev1alpha1.Task{}
			taskNN := types.NamespacedName{Name: "task-for-agent-count", Namespace: "default"}
			if err := k8sClient.Get(ctx, taskNN, task); err == nil {
				Expect(k8sClient.Delete(ctx, task)).To(Succeed())
			}

			// Cleanup agent
			resource := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should count active tasks correctly", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			Expect(agent.Status.ActiveTasks).To(Equal(int32(1)))
		})

		It("should set LastUsed when active tasks exist", func() {
			controllerReconciler := &AgentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, agent)).To(Succeed())
			Expect(agent.Status.LastUsed).NotTo(BeNil())
		})
	})

	Context("TTL-based agent cleanup", func() {
		ctx := context.Background()

		It("should delete an agent whose TTL has expired", func() {
			expiredName := "test-agent-ttl-expired"
			ttlNN := types.NamespacedName{Name: expiredName, Namespace: "default"}
			ttl := metav1.Duration{Duration: 1 * time.Second}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      expiredName,
					Namespace: "default",
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{
						Provider: "anthropic",
						Name:     "claude-sonnet-4-20250514",
					},
					TTLAfterLastTask: &ttl,
				},
			}
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())

			// Set LastUsed to the past
			Expect(k8sClient.Get(ctx, ttlNN, agent)).To(Succeed())
			past := metav1.NewTime(time.Now().Add(-10 * time.Second))
			agent.Status.LastUsed = &past
			Expect(k8sClient.Status().Update(ctx, agent)).To(Succeed())

			// Reconcile — should delete the agent
			r := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ttlNN})
			Expect(err).NotTo(HaveOccurred())

			// Agent should be gone
			err = k8sClient.Get(ctx, ttlNN, &corev1alpha1.Agent{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should NOT delete an agent without TTL", func() {
			noTTLName := "test-agent-no-ttl"
			noTTLNN := types.NamespacedName{Name: noTTLName, Namespace: "default"}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      noTTLName,
					Namespace: "default",
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{
						Provider: "anthropic",
						Name:     "claude-sonnet-4-20250514",
					},
				},
			}
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())

			r := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: noTTLNN})
			Expect(err).NotTo(HaveOccurred())

			// Agent should still exist
			Expect(k8sClient.Get(ctx, noTTLNN, agent)).To(Succeed())
			Expect(k8sClient.Delete(ctx, agent)).To(Succeed())
		})

		It("should requeue when TTL has not expired yet", func() {
			futureName := "test-agent-ttl-future"
			futureNN := types.NamespacedName{Name: futureName, Namespace: "default"}
			ttl := metav1.Duration{Duration: 1 * time.Hour}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      futureName,
					Namespace: "default",
				},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{
						Provider: "anthropic",
						Name:     "claude-sonnet-4-20250514",
					},
					TTLAfterLastTask: &ttl,
				},
			}
			Expect(k8sClient.Create(ctx, agent)).To(Succeed())

			// Set LastUsed to now (TTL not expired)
			Expect(k8sClient.Get(ctx, futureNN, agent)).To(Succeed())
			now := metav1.Now()
			agent.Status.LastUsed = &now
			Expect(k8sClient.Status().Update(ctx, agent)).To(Succeed())

			r := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: futureNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Agent should still exist
			Expect(k8sClient.Get(ctx, futureNN, agent)).To(Succeed())
			Expect(k8sClient.Delete(ctx, agent)).To(Succeed())
		})
	})
})
