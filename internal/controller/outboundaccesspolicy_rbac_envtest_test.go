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
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

var _ = Describe("OutboundAccessPolicy TokenRequest RBAC", func() {
	It("authorizes only the exact resolved ServiceAccount names and revokes before updates and deletion", func() {
		testContext := context.Background()
		namespace := fmt.Sprintf("oap-rbac-%d", time.Now().UnixNano())
		Expect(k8sClient.Create(testContext, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
		})

		const (
			aiWorker           = "scoped-ai-worker"
			initialSubject     = "initial-subject"
			replacementSubject = "replacement-subject"
			unreferenced       = "unreferenced-subject"
		)
		for _, name := range []string{aiWorker, initialSubject, replacementSubject, unreferenced} {
			Expect(k8sClient.Create(testContext, &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			})).To(Succeed())
		}

		policy := &corev1alpha1.OutboundAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "resource-token", Namespace: namespace},
			Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
				Grant:         corev1alpha1.OutboundGrantTokenExchange,
				TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/oauth/token"},
				Subject: corev1alpha1.OutboundTokenSource{
					Source:            corev1alpha1.OutboundTokenSourceServiceAccount,
					ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{Name: initialSubject},
				},
				ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
			}},
		}
		Expect(k8sClient.Create(testContext, policy)).To(Succeed())
		request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: policy.Name}}
		reconciler := &OutboundAccessPolicyReconciler{
			Client:                     k8sClient,
			APIReader:                  k8sClient,
			Scheme:                     k8sClient.Scheme(),
			AIWorkerServiceAccountName: aiWorker,
		}
		for range 3 {
			_, err := reconciler.Reconcile(testContext, request)
			Expect(err).NotTo(HaveOccurred())
		}

		clientset, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		allowed := func(resourceNamespace, resourceName string) bool {
			review, reviewErr := clientset.AuthorizationV1().SubjectAccessReviews().Create(testContext, &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User: fmt.Sprintf("system:serviceaccount:%s:%s", namespace, aiWorker),
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace:   resourceNamespace,
						Verb:        "create",
						Group:       "",
						Resource:    "serviceaccounts",
						Subresource: "token",
						Name:        resourceName,
					},
				},
			}, metav1.CreateOptions{})
			Expect(reviewErr).NotTo(HaveOccurred())
			return review.Status.Allowed
		}

		Eventually(func() bool { return allowed(namespace, initialSubject) }).Should(BeTrue())
		Consistently(func() bool { return allowed(namespace, unreferenced) }, time.Second, 100*time.Millisecond).Should(BeFalse())
		Consistently(func() bool { return allowed("default", initialSubject) }, time.Second, 100*time.Millisecond).Should(BeFalse())

		Expect(k8sClient.Get(testContext, request.NamespacedName, policy)).To(Succeed())
		policy.Spec.Direct.Subject.ServiceAccountRef.Name = replacementSubject
		Expect(k8sClient.Update(testContext, policy)).To(Succeed())
		Eventually(func() bool {
			_, reconcileErr := reconciler.Reconcile(testContext, request)
			Expect(reconcileErr).NotTo(HaveOccurred())
			return !allowed(namespace, initialSubject) && allowed(namespace, replacementSubject)
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())

		Expect(k8sClient.Get(testContext, request.NamespacedName, policy)).To(Succeed())
		Expect(k8sClient.Delete(testContext, policy)).To(Succeed())
		Eventually(func() bool {
			_, reconcileErr := reconciler.Reconcile(testContext, request)
			Expect(reconcileErr).NotTo(HaveOccurred())
			getErr := k8sClient.Get(testContext, request.NamespacedName, &corev1alpha1.OutboundAccessPolicy{})
			return apierrors.IsNotFound(getErr) && !allowed(namespace, replacementSubject)
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
	})
})
