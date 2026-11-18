package kube

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func validationNetworkPolicy(task *corev1alpha1.Task, jobName string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: task.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"patchverification.orka.ai/job": jobName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		}}
}

func (s *Service) ensureNetworkPolicy(ctx context.Context, task *corev1alpha1.Task, job *batchv1.Job) error {
	policy := validationNetworkPolicy(task, job.Name)
	if err := s.OwnObject(task, policy); err != nil {
		return err
	}
	if err := s.client.Create(ctx, policy); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		stored := &networkingv1.NetworkPolicy{}
		if err := s.reader.Get(ctx, client.ObjectKeyFromObject(policy), stored); err != nil {
			return err
		}
		if !metav1.IsControlledBy(stored, task) || !reflect.DeepEqual(stored.Spec, policy.Spec) {
			return fmt.Errorf("validation NetworkPolicy identity or deny-all specification changed")
		}
	}
	return nil
}

func (s *Service) checkCanary(ctx context.Context) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(s.config.CanaryHost, strconv.Itoa(s.config.CanaryPort)))
	if err != nil {
		return fmt.Errorf("network enforcement canary is unavailable")
	}
	return connection.Close()
}

func (s *Service) OwnObject(task *corev1alpha1.Task, object client.Object) error {
	return controllerutil.SetControllerReference(task, object, s.scheme)
}
