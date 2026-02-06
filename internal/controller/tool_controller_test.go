/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

var _ = Describe("Tool Controller", func() {
	Context("When reconciling a tool with a reachable endpoint", func() {
		const resourceName = "test-tool-reachable"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		var server *httptest.Server

		BeforeEach(func() {
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			tool := &corev1alpha1.Tool{}
			err := k8sClient.Get(ctx, typeNamespacedName, tool)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "A test tool with reachable endpoint",
						HTTP: corev1alpha1.HTTPExecution{
							URL:    server.URL,
							Method: "POST",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			server.Close()
			resource := &corev1alpha1.Tool{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Available condition to true and status.available to true", func() {
			controllerReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(toolHealthCheckInterval))

			tool := &corev1alpha1.Tool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tool)).To(Succeed())
			Expect(tool.Status.Available).To(BeTrue())
			Expect(tool.Status.Error).To(BeEmpty())
			Expect(tool.Status.LastCheck).NotTo(BeNil())

			availCond := meta.FindStatusCondition(tool.Status.Conditions, "Available")
			Expect(availCond).NotTo(BeNil())
			Expect(availCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(availCond.Reason).To(Equal("EndpointReachable"))
		})
	})

	Context("When reconciling a tool with an unreachable endpoint", func() {
		const resourceName = "test-tool-unreachable"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			tool := &corev1alpha1.Tool{}
			err := k8sClient.Get(ctx, typeNamespacedName, tool)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "A test tool with unreachable endpoint",
						HTTP: corev1alpha1.HTTPExecution{
							URL:    "http://127.0.0.1:19999/nonexistent",
							Method: "POST",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Tool{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Available condition to false", func() {
			controllerReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				HTTPClient: &http.Client{
					Timeout: 1 * time.Second,
				},
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(toolHealthCheckInterval))

			tool := &corev1alpha1.Tool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tool)).To(Succeed())
			Expect(tool.Status.Available).To(BeFalse())
			Expect(tool.Status.Error).NotTo(BeEmpty())

			availCond := meta.FindStatusCondition(tool.Status.Conditions, "Available")
			Expect(availCond).NotTo(BeNil())
			Expect(availCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(availCond.Reason).To(Equal("EndpointUnreachable"))
		})
	})

	Context("When reconciling a tool with a missing auth secret", func() {
		const resourceName = "test-tool-missing-secret"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			tool := &corev1alpha1.Tool{}
			err := k8sClient.Get(ctx, typeNamespacedName, tool)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "A tool with missing auth secret",
						HTTP: corev1alpha1.HTTPExecution{
							URL:    "http://localhost:8080/test",
							Method: "POST",
							AuthSecretRef: &corev1alpha1.SecretKeySelector{
								Name: "nonexistent-secret",
								Key:  "token",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Tool{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Available condition to false with secret error", func() {
			controllerReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			tool := &corev1alpha1.Tool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tool)).To(Succeed())
			Expect(tool.Status.Available).To(BeFalse())
			Expect(tool.Status.Error).To(ContainSubstring("nonexistent-secret"))
		})
	})

	Context("When reconciling a tool with auth secret but missing key", func() {
		const resourceName = "test-tool-bad-key"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			// Create the secret with a different key
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tool-secret-bad-key",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"other-key": []byte("some-value"),
				},
			}
			secretNN := types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}
			existing := &corev1.Secret{}
			if err := k8sClient.Get(ctx, secretNN, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			}

			tool := &corev1alpha1.Tool{}
			err := k8sClient.Get(ctx, typeNamespacedName, tool)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "A tool with wrong key in auth secret",
						HTTP: corev1alpha1.HTTPExecution{
							URL:    "http://localhost:8080/test",
							Method: "POST",
							AuthSecretRef: &corev1alpha1.SecretKeySelector{
								Name: "tool-secret-bad-key",
								Key:  "token",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Tool{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
			secret := &corev1.Secret{}
			secretNN := types.NamespacedName{Name: "tool-secret-bad-key", Namespace: "default"}
			if err := k8sClient.Get(ctx, secretNN, secret); err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		})

		It("should set Available condition to false with key-not-found error", func() {
			controllerReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			tool := &corev1alpha1.Tool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tool)).To(Succeed())
			Expect(tool.Status.Available).To(BeFalse())
			Expect(tool.Status.Error).To(ContainSubstring("key \"token\" not found"))
		})
	})

	Context("When reconciling a tool with invalid URL", func() {
		const resourceName = "test-tool-bad-url"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			tool := &corev1alpha1.Tool{}
			err := k8sClient.Get(ctx, typeNamespacedName, tool)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "A tool with an invalid URL",
						HTTP: corev1alpha1.HTTPExecution{
							URL:    "not-a-url",
							Method: "POST",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Tool{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Available condition to false with URL validation error", func() {
			controllerReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			tool := &corev1alpha1.Tool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tool)).To(Succeed())
			Expect(tool.Status.Available).To(BeFalse())
			Expect(tool.Status.Error).To(ContainSubstring("invalid http.url"))
		})
	})

	Context("When reconciling a tool with authInject=body but no authBodyKey", func() {
		const resourceName = "test-tool-body-no-key"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			tool := &corev1alpha1.Tool{}
			err := k8sClient.Get(ctx, typeNamespacedName, tool)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "A tool with body inject but no key",
						HTTP: corev1alpha1.HTTPExecution{
							URL:        "http://localhost:8080/test",
							Method:     "POST",
							AuthInject: "body",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.Tool{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Available condition to false with authBodyKey error", func() {
			controllerReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			tool := &corev1alpha1.Tool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tool)).To(Succeed())
			Expect(tool.Status.Available).To(BeFalse())
			Expect(tool.Status.Error).To(ContainSubstring("authBodyKey is required"))
		})
	})

	Context("When reconciling a deleted tool", func() {
		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      "deleted-tool",
			Namespace: "default",
		}

		It("should not return an error", func() {
			controllerReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
