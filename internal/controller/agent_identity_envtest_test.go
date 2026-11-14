// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

var _ = Describe("Agent identity claim grant time", func() {
	It("is recorded from the API server's write time on the pass after the grant", func() {
		ctx := context.Background()
		agent := identityBoundTestAgent("grant-time", time.Now())
		agent.Namespace = "default"
		agent.UID, agent.Generation, agent.CreationTimestamp = "", 0, metav1.Time{}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })
		r := &AgentReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Identity: testIdentityConfig()}
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)}

		_, err := r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		granted := &corev1alpha1.Agent{}
		Expect(k8sClient.Get(ctx, request.NamespacedName, granted)).To(Succeed())
		Expect(granted.Status.IdentityClaim).NotTo(BeNil())
		Expect(granted.Status.IdentityClaim.GrantedAt).To(BeNil(), "the granting pass leaves the grant time to the API server")
		Expect(granted.Status.Ready).To(BeFalse())
		grantWrite := apiServerWriteTime(granted)
		Expect(grantWrite).NotTo(BeNil(), "the API server records the granting write's time")

		_, err = r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		stamped := &corev1alpha1.Agent{}
		Expect(k8sClient.Get(ctx, request.NamespacedName, stamped)).To(Succeed())
		Expect(stamped.Status.IdentityClaim).NotTo(BeNil())
		Expect(stamped.Status.IdentityClaim.GrantedAt).NotTo(BeNil())
		Expect(stamped.Status.IdentityClaim.GrantedAt.Equal(grantWrite)).To(BeTrue(),
			"grant time %s, want the API server's write time %s", stamped.Status.IdentityClaim.GrantedAt, grantWrite)
	})
})
