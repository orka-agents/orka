/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const kagentTestNamespace = "orka-system"

// kagentTestCluster is a fake API server that assigns UIDs, rolls out
// Deployments at once, and reports registrations Ready, as Orka would.
func kagentTestCluster(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	epoch := &corev1alpha1.ControllerEpoch{ObjectMeta: metav1.ObjectMeta{Name: "orka-controller", Namespace: kagentTestNamespace},
		Spec: corev1alpha1.ControllerEpochSpec{Name: kagentControllerEpoch}, Status: corev1alpha1.ControllerEpochStatus{Epoch: 3}}
	assignUID := func(ctx context.Context, c client.WithWatch, obj client.Object) error {
		if obj.GetUID() != "" {
			return nil
		}
		stored := obj.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
			return err
		}
		stored.SetUID(types.UID("uid-" + obj.GetName()))
		if err := c.Update(ctx, stored); err != nil {
			return err
		}
		obj.SetUID(stored.GetUID())
		return nil
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, epoch)...).
		WithStatusSubresource(&corev1alpha1.ControllerEpoch{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				return assignUID(ctx, c, obj)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				return assignUID(ctx, c, obj)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				switch o := obj.(type) {
				case *appsv1.Deployment:
					o.Status = appsv1.DeploymentStatus{ObservedGeneration: o.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
				case *corev1alpha1.AgentRuntime:
					o.Status.Ready, o.Status.ObservedGeneration = true, o.Generation
				}
				return nil
			},
		}).Build()
}

const testKagentPinnedImage = "registry.example/acp-kagent-runtime@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"

// newTestKagentRuntimes deploys bundles into c. The runtime answers the
// capabilities of the profile it was rendered with.
func newTestKagentRuntimes(t *testing.T, c client.Client) (*kagentRuntimes, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	k := &kagentRuntimes{
		c: c, namespace: kagentTestNamespace, out: &out, holder: "test", poll: time.Millisecond,
		options: kagentRuntimeOptions{ControllerNamespace: kagentTestNamespace, ReadyTimeout: time.Second, RetireTimeout: time.Second,
			ModelUpstreamTokenFile: writeTempFile(t, "upstream-token\n")},
		resolve: func(context.Context, string) (string, string, error) {
			return testKagentPinnedImage, testAdapterDigest, nil
		},
	}
	k.capabilities = func(ctx context.Context, namespace, service string) ([]byte, error) {
		deployment := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: service}, deployment); err != nil {
			return nil, err
		}
		digest := ""
		for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST" {
				digest = env.Value
			}
		}
		return json.Marshal(map[string]any{"runtimeProfileDigest": digest, "limits": map[string]any{"maxResidentSessions": 10},
			"supportsDrain": true, "workspaceGovernance": map[string]any{"mode": "strict-governed"}})
	}
	return k, &out
}

// writeTestKagentBundle writes the governed v0 fixture's bundle.
func writeTestKagentBundle(t *testing.T) (string, kagentBundle) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := runKagentImport(t, kagentBundleArgs(writeTempFile(t, governedKagentV0Fixture), dir, "--dry-run")...); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(dir, "probe-gov")
	bundle, _ := readBundle(t, bundleDir)
	return bundleDir, bundle
}

func relayObjectsFor(bundle kagentBundle, serving string) (*corev1.Service, *networkingv1.NetworkPolicy) {
	meta := metav1.ObjectMeta{Name: "probe-gov-relay", Namespace: kagentTestNamespace,
		Labels: map[string]string{kagentAgentLabel: bundle.Agent}, Annotations: map[string]string{kagentSourceAnnotation: kagentSource(bundle)}}
	return &corev1.Service{ObjectMeta: meta, Spec: corev1.ServiceSpec{Selector: map[string]string{kagentRuntimeLabel: serving},
			Ports: []corev1.ServicePort{{Name: "relay", Port: 8092}}}},
		&networkingv1.NetworkPolicy{ObjectMeta: *meta.DeepCopy(), Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"old": "policy"}}}}
}

func TestKagentDeployRegistersAGovernedRuntime(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	c := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	k.unlock()
	secret := &corev1.Secret{}
	if err := c.Get(ctx, k.key(bundle.Runtime+"-auth"), secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["model-upstream-token"]) != "upstream-token" || len(secret.Data["controller-token"]) != 64 ||
		secret.Labels["orka.ai/agent-runtime-auth"] != "true" || secret.Annotations[kagentSourceAnnotation] != kagentSource(bundle) {
		t.Fatalf("auth secret labels %v annotations %v keys %d", secret.Labels, secret.Annotations, len(secret.Data))
	}
	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, k.key(bundle.Runtime+"-config"), configMap); err != nil || len(configMap.BinaryData[kagentBundleConfigFile]) == 0 {
		t.Fatalf("config map = %v, %v", configMap, err)
	}
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, k.key(bundle.Runtime), deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != testKagentPinnedImage ||
		deployment.Annotations[kagentRecoveryAnnotation] != "uid-"+bundle.Runtime {
		t.Fatalf("deployment image %s recovery %q", deployment.Spec.Template.Spec.Containers[0].Image, deployment.Annotations[kagentRecoveryAnnotation])
	}
	relay := &corev1.Service{}
	if err := c.Get(ctx, k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != bundle.Runtime {
		t.Fatalf("relay = %v, %v", relay.Spec.Selector, err)
	}
	if err := c.Get(ctx, k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("relay policy: %v", err)
	}
	registration := &corev1alpha1.AgentRuntime{}
	if err := c.Get(ctx, k.key(bundle.Runtime), registration); err != nil || registration.Spec.Deployment.KubernetesRecovery == nil {
		t.Fatalf("registration = %v, %v", registration.Spec.Deployment, err)
	}
	if uid, err := os.ReadFile(filepath.Join(dir, kagentBundleRegistrationFile)); err != nil || string(uid) != "uid-"+bundle.Runtime+"\n" {
		t.Fatalf("registration.uid = %q, %v", uid, err)
	}
	if err := c.Get(ctx, k.key(bundle.Agent+kagentLockSuffix), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the lock outlived the run: %v", err)
	}
	// Deploying again keeps the runtime's image and secret.
	k.resolve = func(context.Context, string) (string, string, error) {
		return "", "", errors.New("a deployed runtime resolved an image")
	}
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatalf("deploying again: %v", err)
	}
}

// Once a runtime is registered, Orka's recovery owns its controller epoch and
// advances it only after draining; deploying again keeps the epoch the pod
// runs with. A runtime never registered takes the current epoch.
func TestKagentDeployKeepsTheEpochRecoveryOwns(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	c := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	k.unlock()
	epochs := &corev1alpha1.ControllerEpochList{}
	if err := c.List(ctx, epochs); err != nil || len(epochs.Items) != 1 {
		t.Fatalf("controller epochs = %v, %v", epochs.Items, err)
	}
	epochs.Items[0].Status.Epoch = 4
	if err := c.Status().Update(ctx, &epochs.Items[0]); err != nil {
		t.Fatal(err)
	}
	epoch := func() int64 {
		deployment := &appsv1.Deployment{}
		if err := c.Get(ctx, k.key(bundle.Runtime), deployment); err != nil {
			t.Fatal(err)
		}
		value, err := deployedKagentEpoch(deployment)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatalf("deploying again: %v", err)
	}
	k.unlock()
	if got := epoch(); got != 3 {
		t.Fatalf("a registered runtime's epoch = %d, want the 3 recovery set", got)
	}
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, k.key(bundle.Runtime), deployment); err != nil {
		t.Fatal(err)
	}
	delete(deployment.Annotations, kagentRecoveryAnnotation)
	if err := c.Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &corev1alpha1.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Name: bundle.Runtime, Namespace: kagentTestNamespace}}); err != nil {
		t.Fatal(err)
	}
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatalf("deploying an unregistered runtime again: %v", err)
	}
	if got := epoch(); got != 4 {
		t.Fatalf("an unregistered runtime's epoch = %d, want the current 4", got)
	}
}

// Recovery may advance the epoch between deploy's read and its apply; the
// apply then fails instead of writing the old epoch back.
func TestKagentDeployDoesNotOverwriteAnEpochRecoveryAdvanced(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	base := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, base)
	ctx := context.Background()
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	k.unlock()
	recovered := false
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok && patch.Type() == types.ApplyPatchType && !recovered {
				recovered = true
				current := &appsv1.Deployment{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
					return err
				}
				for i := range current.Spec.Template.Spec.Containers {
					for j, env := range current.Spec.Template.Spec.Containers[i].Env {
						if env.Name == kagentEpochEnv {
							current.Spec.Template.Spec.Containers[i].Env[j].Value = "4"
						}
					}
				}
				if err := c.Update(ctx, current); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	k.c = c
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "deploy again") {
		t.Fatalf("deploy over a recovered epoch = %v", err)
	}
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, k.key(bundle.Runtime), deployment); err != nil {
		t.Fatal(err)
	}
	if epoch, err := deployedKagentEpoch(deployment); err != nil || epoch != 4 {
		t.Fatalf("epoch after the failed apply = %d, %v", epoch, err)
	}
}

// A redeploy keeps the Orka API URL the runtime was deployed with, since a
// change restarts its pod without draining; another URL takes a new runtime.
func TestKagentDeployKeepsTheDeployedOrkaAPIURL(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	c := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	k.options.OrkaAPIURL = "http://orka-api.custom.svc:8080"
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	k.unlock()
	k.options.OrkaAPIURL = ""
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatalf("deploying again without --orka-api-url: %v", err)
	}
	k.unlock()
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, k.key(bundle.Runtime), deployment); err != nil {
		t.Fatal(err)
	}
	if url := deployedKagentEnv(deployment, kagentOrkaAPIEnv); url != "http://orka-api.custom.svc:8080" {
		t.Fatalf("Orka API URL after a redeploy = %q", url)
	}
	k.options.OrkaAPIURL = "http://orka-api.other.svc:8080"
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "--orka-api-url") {
		t.Fatalf("redeploy with another URL = %v", err)
	}
}

// A same-named object that is not this runtime's, such as one a person or
// another source's failed deploy made, is never taken over.
func TestKagentDeployRefusesAnUnownedRuntimeObject(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: bundle.Runtime + "-broker", Namespace: kagentTestNamespace,
		Labels: map[string]string{"app": "someone-else"}}}
	c := kagentTestCluster(t, foreign)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "is not runtime") {
		t.Fatalf("deploy over a foreign object = %v", err)
	}
	account := &corev1.ServiceAccount{}
	if err := c.Get(ctx, k.key(bundle.Runtime+"-broker"), account); err != nil || account.Labels[kagentRuntimeLabel] != "" {
		t.Fatalf("the foreign service account was changed: %v, %v", account.Labels, err)
	}
	if err := c.Get(ctx, k.key(bundle.Runtime), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a deployment was applied beside a foreign object: %v", err)
	}
	k.unlock()
	// A foreign Deployment of the runtime's name is refused before anything.
	c = kagentTestCluster(t, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: bundle.Runtime, Namespace: kagentTestNamespace}})
	k, _ = newTestKagentRuntimes(t, c)
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "is not runtime") {
		t.Fatalf("deploy over a foreign deployment = %v", err)
	}
	k.unlock()
	// So is a foreign auth Secret, whose credentials the runtime would mount.
	c = kagentTestCluster(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bundle.Runtime + "-auth", Namespace: kagentTestNamespace},
		Data: map[string][]byte{"controller-token": []byte("someone-else")}})
	k, _ = newTestKagentRuntimes(t, c)
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "is not runtime") {
		t.Fatalf("deploy over a foreign secret = %v", err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, k.key(bundle.Runtime+"-auth"), secret); err != nil || len(secret.Labels) != 0 {
		t.Fatalf("the foreign secret was relabeled: %v, %v", secret.Labels, err)
	}
}

// A claim's StorageClass cannot change, so a redeploy keeps the ledger's
// class when --storage-class is omitted and refuses another.
func TestKagentDeployKeepsTheLedgerStorageClass(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	c := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	k.options.StorageClass = "fast"
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatal(err)
	}
	k.unlock()
	k.options.StorageClass = ""
	if err := k.deploy(ctx, dir); err != nil {
		t.Fatalf("deploying again without --storage-class: %v", err)
	}
	k.unlock()
	claim := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, k.key(bundle.Runtime+"-ledger"), claim); err != nil || claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "fast" {
		t.Fatalf("ledger class after a redeploy = %v, %v", claim.Spec.StorageClassName, err)
	}
	k.options.StorageClass = "slow"
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "--storage-class") {
		t.Fatalf("redeploy with another class = %v", err)
	}
}

// Until the runtime is Ready, a failure moves the relay, and its policy,
// back to what they served before.
func TestKagentDeployMovesTheRelayBackWhenItFails(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, policy := relayObjectsFor(bundle, "probe-gov-kagent-old")
	c := kagentTestCluster(t, service, policy)
	k, out := newTestKagentRuntimes(t, c)
	k.capabilities = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("the runtime does not answer")
	}
	ctx := context.Background()
	if err := k.deploy(ctx, dir); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("deploy = %v", err)
	}
	k.unlock()
	relay := &corev1.Service{}
	if err := c.Get(ctx, k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-old" {
		t.Fatalf("relay after a failed deploy = %v, %v; log %s", relay.Spec.Selector, err, out)
	}
	restored := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, k.key("probe-gov-relay"), restored); err != nil || restored.Spec.PodSelector.MatchLabels["old"] != "policy" {
		t.Fatalf("relay policy after a failed deploy = %v, %v", restored.Spec, err)
	}
}

// A policy the failed deploy created is deleted again.
func TestKagentDeployDeletesARelayPolicyItCreated(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, "probe-gov-kagent-old")
	c := kagentTestCluster(t, service)
	k, _ := newTestKagentRuntimes(t, c)
	k.capabilities = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("the runtime does not answer")
	}
	if err := k.deploy(context.Background(), dir); err == nil {
		t.Fatal("deploy succeeded without capabilities")
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the created policy stayed: %v", err)
	}
}

// A first deploy creates the relay; a failed one deletes it again, with the
// policy it created, so no relay selects a runtime Orka never admitted.
func TestKagentDeployDeletesARelayItCreated(t *testing.T) {
	dir, _ := writeTestKagentBundle(t)
	c := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, c)
	k.capabilities = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("the runtime does not answer")
	}
	if err := k.deploy(context.Background(), dir); err == nil {
		t.Fatal("deploy succeeded without capabilities")
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the created relay stayed: %v", err)
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the created policy stayed: %v", err)
	}
}

// A relay created by a request whose answer was lost is the deploy's own too:
// the rollback finds it by the deploy's nonce and deletes it.
func TestKagentDeployDeletesARelayWhoseCreateAnswerWasLost(t *testing.T) {
	dir, _ := writeTestKagentBundle(t)
	base := kagentTestCluster(t)
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*corev1.Service); ok {
				return errors.New("the connection was reset")
			}
			return nil
		},
	})
	k, _ := newTestKagentRuntimes(t, c)
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "connection was reset") {
		t.Fatalf("deploy = %v", err)
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the relay whose create answer was lost stayed: %v", err)
	}
}

// A deploy that took the relay since a failed one created it relies on the
// relay and its policy, so the failed deploy's rollback leaves both.
func TestKagentDeployLeavesARelayAnotherDeployTookOver(t *testing.T) {
	dir, _ := writeTestKagentBundle(t)
	c := kagentTestCluster(t)
	k, _ := newTestKagentRuntimes(t, c)
	k.capabilities = func(ctx context.Context, _, _ string) ([]byte, error) {
		relay := &corev1.Service{}
		if err := c.Get(ctx, k.key("probe-gov-relay"), relay); err != nil {
			return nil, err
		}
		relay.Spec.Selector = map[string]string{kagentRuntimeLabel: "probe-gov-kagent-next"}
		if err := c.Update(ctx, relay); err != nil {
			return nil, err
		}
		return nil, errors.New("the runtime does not answer")
	}
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("deploy = %v", err)
	}
	relay := &corev1.Service{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-next" {
		t.Fatalf("the relay another deploy took = %v, %v", relay.Spec.Selector, err)
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("the policy the other deploy relies on was deleted: %v", err)
	}
}

// The same holds for an existing policy this deploy changed: the deploy that
// took the relay relies on it as it is.
func TestKagentDeployLeavesAPolicyAnotherDeployTookOver(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, policy := relayObjectsFor(bundle, "probe-gov-kagent-old")
	c := kagentTestCluster(t, service, policy)
	k, _ := newTestKagentRuntimes(t, c)
	k.capabilities = func(ctx context.Context, _, _ string) ([]byte, error) {
		relay := &corev1.Service{}
		if err := c.Get(ctx, k.key("probe-gov-relay"), relay); err != nil {
			return nil, err
		}
		relay.Spec.Selector = map[string]string{kagentRuntimeLabel: "probe-gov-kagent-next"}
		if err := c.Update(ctx, relay); err != nil {
			return nil, err
		}
		return nil, errors.New("the runtime does not answer")
	}
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("deploy = %v", err)
	}
	current := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), current); err != nil || current.Spec.PodSelector.MatchLabels["old"] == "policy" {
		t.Fatalf("the policy the other deploy relies on was restored: %v, %v", current.Spec, err)
	}
}

// A capabilities read that never answers still ends the deploy within the
// ready timeout and moves the relay back.
func TestKagentDeployBoundsTheCapabilitiesRead(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, policy := relayObjectsFor(bundle, "probe-gov-kagent-old")
	c := kagentTestCluster(t, service, policy)
	k, _ := newTestKagentRuntimes(t, c)
	k.capabilities = func(ctx context.Context, _, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := k.deploy(context.Background(), dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deploy = %v", err)
	}
	relay := &corev1.Service{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-old" {
		t.Fatalf("relay after a stalled read = %v, %v", relay.Spec.Selector, err)
	}
}

// A policy patch whose answer was lost may still have committed, so the
// rollback stays armed and restores the policy.
func TestKagentDeployRestoresAPolicyWhosePatchAnswerWasLost(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, policy := relayObjectsFor(bundle, "probe-gov-kagent-old")
	base := kagentTestCluster(t, service, policy)
	lost := false
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok && patch.Type() == types.JSONPatchType && !lost {
				lost = true
				return errors.New("the connection was reset")
			}
			return nil
		},
	})
	k, _ := newTestKagentRuntimes(t, c)
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "connection was reset") {
		t.Fatalf("deploy = %v", err)
	}
	restored := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), restored); err != nil || restored.Spec.PodSelector.MatchLabels["old"] != "policy" {
		t.Fatalf("relay policy after a lost answer = %v, %v", restored.Spec, err)
	}
	relay := &corev1.Service{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-old" {
		t.Fatalf("relay after a lost answer = %v, %v", relay.Spec.Selector, err)
	}
}

// A policy created by a request whose answer was lost is the deploy's own
// too: the rollback finds it by the deploy's nonce and deletes it.
func TestKagentDeployDeletesAPolicyWhoseCreateAnswerWasLost(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, "probe-gov-kagent-old")
	base := kagentTestCluster(t, service)
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
				return errors.New("the connection was reset")
			}
			return nil
		},
	})
	k, _ := newTestKagentRuntimes(t, c)
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "connection was reset") {
		t.Fatalf("deploy = %v", err)
	}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the policy whose create answer was lost stayed: %v", err)
	}
	relay := &corev1.Service{}
	if err := c.Get(context.Background(), k.key("probe-gov-relay"), relay); err != nil || relay.Spec.Selector[kagentRuntimeLabel] != "probe-gov-kagent-old" {
		t.Fatalf("relay after a lost answer = %v, %v", relay.Spec.Selector, err)
	}
}

// A relay of the same Orka Agent name serving another kagent source, of
// another namespace or API version, is not this Agent's.
func TestKagentDeployRefusesAnotherAgentsRelay(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	for _, other := range []string{"kagent.dev/v1alpha2 elsewhere/probe-gov", "api.kagent.dev/v1alpha3 kagent/probe-gov"} {
		service, _ := relayObjectsFor(bundle, "other")
		service.Annotations[kagentSourceAnnotation] = other
		k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, service))
		if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "is not probe-gov's relay") {
			t.Fatalf("deploy onto the relay of %s = %v", other, err)
		}
	}
}

func TestKagentDeployRefusesARotatedCredential(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	// The runtime's own Secret, so only the credential differs.
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bundle.Runtime + "-auth", Namespace: kagentTestNamespace,
		Labels: map[string]string{kagentRuntimeLabel: bundle.Runtime}, Annotations: map[string]string{kagentSourceAnnotation: kagentSource(bundle)}},
		Data: map[string][]byte{"model-upstream-token": []byte("an older token")}}
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, secret))
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "holds another upstream credential") {
		t.Fatalf("deploy with another credential = %v", err)
	}
}

func TestKagentCommandsHoldAPerAgentLock(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	lock := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: bundle.Agent + kagentLockSuffix, Namespace: kagentTestNamespace}}
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, lock))
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "holds ConfigMap probe-gov-kagent-lock") {
		t.Fatalf("deploy while locked = %v", err)
	}
	if err := k.retire(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "holds ConfigMap") {
		t.Fatalf("retire while locked = %v", err)
	}
}

func agentUsing(bundle kagentBundle, name string) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: bundle.Agent, Namespace: kagentTestNamespace},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: name}}}}
}

func runtimeObjects(bundle kagentBundle, name, source string) []client.Object {
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: kagentTestNamespace,
			Labels:      map[string]string{kagentRuntimeLabel: name, kagentAgentLabel: bundle.Agent},
			Annotations: map[string]string{kagentSourceAnnotation: source}}
	}
	return []client.Object{
		&corev1alpha1.AgentRuntime{ObjectMeta: meta(name)},
		&appsv1.Deployment{ObjectMeta: meta(name)},
		&corev1.Service{ObjectMeta: meta(name)},
		&corev1.ConfigMap{ObjectMeta: meta(name + "-config")},
		&corev1.Secret{ObjectMeta: meta(name + "-auth")},
	}
}

// Retire deletes the Agent's other runtimes, and only those of the bundle's
// kagent Agent: a same-named kagent Agent of another namespace maps to the
// same Orka Agent.
func TestKagentRetireDeletesTheAgentsOtherRuntimes(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	objects := append([]client.Object{agentUsing(bundle, bundle.Runtime), service},
		runtimeObjects(bundle, "probe-gov-kagent-old", kagentSource(bundle))...)
	objects = append(objects, runtimeObjects(bundle, "probe-gov-kagent-other", "elsewhere/probe-gov")...)
	// An old runtime whose registration an earlier retire already deleted.
	leftover := runtimeObjects(bundle, "probe-gov-kagent-leftover", kagentSource(bundle))
	objects = append(objects, leftover[1:]...)
	c := kagentTestCluster(t, objects...)
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.retire(ctx, dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"probe-gov-kagent-old", "probe-gov-kagent-leftover"} {
		if err := c.Get(ctx, k.key(name), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s was not retired: %v", name, err)
		}
		if err := c.Get(ctx, k.key(name+"-config"), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s's configuration was not deleted: %v", name, err)
		}
	}
	if err := c.Get(ctx, k.key("probe-gov-kagent-old"), &corev1alpha1.AgentRuntime{}); !apierrors.IsNotFound(err) {
		t.Errorf("the old registration stayed: %v", err)
	}
	if err := c.Get(ctx, k.key("probe-gov-kagent-other"), &appsv1.Deployment{}); err != nil {
		t.Errorf("another kagent Agent's runtime was retired: %v", err)
	}
}

// A v0 runtime's pod reader lives in the kagent Agent's namespace, so retire
// lists it there: an operator may hold RBAC in only that namespace and the
// runtimes'.
func TestKagentRetireListsPodReadersInTheKagentNamespace(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	reader := metav1.ObjectMeta{Name: "probe-gov-kagent-old-pods", Namespace: bundle.KagentNamespace,
		Labels: map[string]string{kagentRuntimeLabel: "probe-gov-kagent-old", kagentRuntimeNamespaceLabel: kagentTestNamespace}}
	objects := append([]client.Object{agentUsing(bundle, bundle.Runtime), service,
		&rbacv1.Role{ObjectMeta: reader}, &rbacv1.RoleBinding{ObjectMeta: *reader.DeepCopy()}},
		runtimeObjects(bundle, "probe-gov-kagent-old", kagentSource(bundle))...)
	base := kagentTestCluster(t, objects...)
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listOptions := (&client.ListOptions{}).ApplyOptions(opts)
			switch list.(type) {
			case *rbacv1.RoleList, *rbacv1.RoleBindingList:
				if listOptions.Namespace != bundle.KagentNamespace {
					return apierrors.NewForbidden(rbacv1.Resource("roles"), "", errors.New("namespace-scoped RBAC"))
				}
			}
			return c.List(ctx, list, opts...)
		},
	})
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.retire(ctx, dir); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(&rbacv1.Role{ObjectMeta: reader})
	if err := c.Get(ctx, key, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Errorf("the pod reader Role stayed: %v", err)
	}
	if err := c.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("the pod reader RoleBinding stayed: %v", err)
	}
}

// A retire interrupted after it deleted an old v0 runtime's own objects
// leaves only its pod reader; the next retire still finds and deletes it, but
// not the reader of a runtime in another Orka namespace.
func TestKagentRetireFindsAnOrphanedPodReader(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	reader := func(runtimeNamespace string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: "probe-gov-kagent-old." + runtimeNamespace + ".agent-pods", Namespace: bundle.KagentNamespace,
			Labels: map[string]string{kagentRuntimeLabel: "probe-gov-kagent-old", kagentAgentLabel: bundle.Agent,
				kagentRuntimeNamespaceLabel: runtimeNamespace},
			Annotations: map[string]string{kagentSourceAnnotation: kagentSource(bundle)}}
	}
	orphan, other := reader(kagentTestNamespace), reader("team-b")
	c := kagentTestCluster(t, agentUsing(bundle, bundle.Runtime), service,
		&rbacv1.Role{ObjectMeta: orphan}, &rbacv1.RoleBinding{ObjectMeta: *orphan.DeepCopy()},
		&rbacv1.Role{ObjectMeta: other}, &rbacv1.RoleBinding{ObjectMeta: *other.DeepCopy()})
	k, _ := newTestKagentRuntimes(t, c)
	ctx := context.Background()
	if err := k.retire(ctx, dir); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(&rbacv1.Role{ObjectMeta: orphan})
	if err := c.Get(ctx, key, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Errorf("the orphaned pod reader Role stayed: %v", err)
	}
	if err := c.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("the orphaned pod reader RoleBinding stayed: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&rbacv1.RoleBinding{ObjectMeta: other}), &rbacv1.RoleBinding{}); err != nil {
		t.Errorf("another Orka namespace's pod reader was deleted: %v", err)
	}
}

// Only v0 runtimes have pod readers, so a v1 retire reads nothing in the
// kagent Agent's namespace, where its operator may hold no RBAC.
func TestKagentRetireOfV1ReadsNoPodReaders(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	path := filepath.Join(dir, kagentBundleRuntimeFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["kagentVersion"] = "v1"
	if raw, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	objects := append([]client.Object{agentUsing(bundle, bundle.Runtime), service},
		runtimeObjects(bundle, "probe-gov-kagent-old", kagentSource(bundle))...)
	base := kagentTestCluster(t, objects...)
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			switch list.(type) {
			case *rbacv1.RoleList, *rbacv1.RoleBindingList:
				return apierrors.NewForbidden(rbacv1.Resource("roles"), "", errors.New("no RBAC in the kagent namespace"))
			}
			return c.List(ctx, list, opts...)
		},
	})
	k, _ := newTestKagentRuntimes(t, c)
	if err := k.retire(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), k.key("probe-gov-kagent-old"), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("the old v1 runtime was not retired: %v", err)
	}
}

func TestKagentRetireRefusesBeforeActivation(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, agentUsing(bundle, "probe-gov-kagent-old")))
	if err := k.retire(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "activate it first") {
		t.Fatalf("retire before activation = %v", err)
	}
	service, _ := relayObjectsFor(bundle, "probe-gov-kagent-old")
	k, _ = newTestKagentRuntimes(t, kagentTestCluster(t, agentUsing(bundle, bundle.Runtime), service))
	if err := k.retire(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "relay probe-gov-relay selects") {
		t.Fatalf("retire while the relay serves the old runtime = %v", err)
	}
}

// Activate previews the import, which must include the bundle's Agent, then
// runs it into the bundle's directory, and requires the Agent to use the
// bundle's runtime afterwards.
func TestKagentActivateSwitchesTheAgent(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	c := kagentTestCluster(t, agentUsing(bundle, "probe-gov-kagent-old"), service)
	k, _ := newTestKagentRuntimes(t, c)
	var calls [][]string
	var input []byte
	k.importer = func(ctx context.Context, args []string, quiet bool) error {
		calls = append(calls, args)
		input, _ = os.ReadFile(args[1])
		bundleDir := args[len(args)-1]
		if quiet {
			if err := os.MkdirAll(filepath.Join(bundleDir, bundle.Agent), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(bundleDir, bundle.Agent, kagentBundleRuntimeFile), []byte("{}"), 0o600)
		}
		return c.Update(ctx, withRuntime(t, c, bundle, bundle.Runtime))
	}
	err := k.activate(context.Background(), dir, []string{"-f", "-", "--allowed-tool", "probe"}, strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.Contains(strings.Join(calls[0], " "), "--dry-run --bundle-dir") ||
		strings.Contains(strings.Join(calls[1], " "), "--dry-run") || calls[1][len(calls[1])-1] != filepath.Dir(dir) {
		t.Fatalf("import calls = %v", calls)
	}
	if string(input) != "input" {
		t.Fatalf("standard input reached the import as %q", input)
	}
	if err := k.activate(context.Background(), dir, []string{"-f", "x", "--dry-run"}, nil); err == nil || !strings.Contains(err.Error(), "without --dry-run") {
		t.Fatalf("activate --dry-run = %v", err)
	}
}

func withRuntime(t *testing.T, c client.Client, bundle kagentBundle, name string) *corev1alpha1.Agent {
	t.Helper()
	agent := &corev1alpha1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: kagentTestNamespace, Name: bundle.Agent}, agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Runtime.RuntimeRef.Name = name
	return agent
}

func TestKagentActivateRefusesAnImportWithoutTheAgent(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	service, _ := relayObjectsFor(bundle, bundle.Runtime)
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, agentUsing(bundle, "old"), service))
	k.importer = func(context.Context, []string, bool) error { return nil }
	if err := k.activate(context.Background(), dir, []string{"-f", "other.yaml"}, nil); err == nil || !strings.Contains(err.Error(), "do not import Agent probe-gov") {
		t.Fatalf("activate with an import that skips the Agent = %v", err)
	}
}

func TestKagentDefaultImageFollowsTheRelease(t *testing.T) {
	if image, err := kagentDefaultImage("v0.4.0"); err != nil || image != "ghcr.io/orka-agents/orka/acp-kagent-runtime:0.4.0" {
		t.Fatalf("release image = %q, %v", image, err)
	}
	for _, build := range []string{"dev", "a49ac96ce-dirty", "v0.4.0-12-gabc"} {
		if _, err := kagentDefaultImage(build); err == nil || !strings.Contains(err.Error(), "pass --image") {
			t.Errorf("%s: err = %v", build, err)
		}
	}
}

// A deploy interrupted together with the reader of its output still moves its
// relay back: a write to the closed pipe fails instead of ending the run.
func TestKagentCommandsSurviveAClosedOutput(t *testing.T) {
	t.Cleanup(func() { signal.Reset(syscall.SIGPIPE) })
	_, stop := kagentSignals(context.Background())
	defer stop()
	if !signal.Ignored(syscall.SIGPIPE) {
		t.Fatal("SIGPIPE would end the run before its rollback")
	}
}

// An empty credential file is not an omitted flag: on a deployed runtime it
// would skip the comparison and keep the old credential silently.
func TestKagentDeployRefusesAnEmptyCredentialFile(t *testing.T) {
	dir, bundle := writeTestKagentBundle(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bundle.Runtime + "-auth", Namespace: kagentTestNamespace},
		Data: map[string][]byte{"model-upstream-token": []byte("the deployed token")}}
	k, _ := newTestKagentRuntimes(t, kagentTestCluster(t, secret))
	k.options.ModelUpstreamTokenFile = writeTempFile(t, "\n")
	if err := k.deploy(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("deploy with an empty credential file = %v", err)
	}
}

// The flags that wrote a bundle name its directory as given, often relative.
func TestKagentActivateAcceptsTheBundleDirectoryAsWritten(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "bundle"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	bundleDir, err := filepath.Abs("bundle")
	if err != nil {
		t.Fatal(err)
	}
	args, err := kagentActivateArgs(context.Background(), []string{"-f", "x.yaml", "--bundle-dir", "bundle", "--bundle-dir=./bundle"}, bundleDir, t.TempDir(), nil)
	if err != nil || strings.Join(args, " ") != "-f x.yaml" {
		t.Fatalf("activate args = %v, %v", args, err)
	}
	if _, err := kagentActivateArgs(context.Background(), []string{"--bundle-dir", "elsewhere"}, bundleDir, t.TempDir(), nil); err == nil {
		t.Fatal("another bundle directory was accepted")
	}
}

// Activate imports into the runtimes' namespace; one named after -- would be
// silently replaced.
func TestKagentActivateRefusesANamespaceAmongTheImportFlags(t *testing.T) {
	for _, flags := range [][]string{{"-f", "x.yaml", "-n", "team-a"}, {"-nteam-a"}, {"--namespace", "team-a"}, {"--namespace=team-a"}} {
		if _, err := kagentActivateArgs(context.Background(), flags, t.TempDir(), t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "before --") {
			t.Errorf("%v: err = %v", flags, err)
		}
	}
}

// Activate reads standard input once, into a file both of its imports read,
// in each form the import accepts.
func TestKagentActivateCachesStandardInput(t *testing.T) {
	for _, flags := range [][]string{{"-f", "-"}, {"-f-"}, {"-f=-"}, {"--file=-"}, {"--file", "-"},
		{"--revision-egress", "-"}, {"--revision-egress=-"}, {"--revision-sources", "-"}, {"--revision-sources=-"}} {
		args, err := kagentActivateArgs(context.Background(), flags, t.TempDir(), t.TempDir(), strings.NewReader("export"))
		if err != nil || len(args) != 2 || !slices.Contains(kagentInputFlags, args[0]) || !kagentReadsStdin(flags) {
			t.Fatalf("%v: args = %v, %v", flags, args, err)
		}
		if data, err := os.ReadFile(args[1]); err != nil || string(data) != "export" {
			t.Errorf("%v: cached input = %q, %v", flags, data, err)
		}
	}
	// Standard input has one reader.
	if _, err := kagentActivateArgs(context.Background(), []string{"-f", "-", "--revision-egress=-"}, t.TempDir(), t.TempDir(), strings.NewReader("export")); err == nil ||
		!strings.Contains(err.Error(), "only one import flag") {
		t.Fatalf("two standard input readers = %v", err)
	}
}

// An interrupt ends activate even while it waits on standard input whose
// writer stays open.
func TestKagentActivateArgsStopsReadingOnInterrupt(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := kagentActivateArgs(ctx, []string{"-f", "-"}, t.TempDir(), t.TempDir(), reader)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("activate args after an interrupt = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading standard input outlived the interrupt")
	}
}

// After --, an Orka API flag would replace the one activate checked against
// --context. The refusal names only the flag: its value may be a token.
func TestKagentActivateRefusesAnOrkaAPIAmongTheImportFlags(t *testing.T) {
	for _, flags := range [][]string{
		{"-f", "x.yaml", "-s", "https://other"}, {"-shttps://other"}, {"--server=https://other"},
		{"-t", "secret-value"}, {"-tsecret-value"}, {"-t=secret-value"}, {"--token", "secret-value"},
		{"--txn-token=secret-value"}, {"--kubeconfig", "other"},
	} {
		_, err := kagentActivateArgs(context.Background(), flags, t.TempDir(), t.TempDir(), nil)
		if err == nil || !strings.Contains(err.Error(), "before --") || strings.Contains(err.Error(), "secret-value") {
			t.Errorf("%v: err = %v", flags, err)
		}
	}
}

// The import finds the Orka API through kubeconfig's current context, so an
// activate in another context needs the API named explicitly.
func TestKagentActivateRefusesAnotherContextWithoutTheAPI(t *testing.T) {
	kubeconfig := writeTempFile(t, `apiVersion: v1
kind: Config
current-context: a
contexts:
- name: a
  context: {cluster: a}
- name: b
  context: {cluster: b}
clusters:
- name: a
  cluster: {server: https://a.example}
- name: b
  cluster: {server: https://b.example}
`)
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	for _, selected := range []string{"", "a"} {
		if err := kagentAPIContextProblem(newRootCmd(), rules, selected); err != nil {
			t.Errorf("--context %q: %v", selected, err)
		}
	}
	root := newRootCmd()
	if err := kagentAPIContextProblem(root, rules, "b"); err == nil || !strings.Contains(err.Error(), "--server and --token") {
		t.Fatalf("--context b without the API = %v", err)
	}
	if err := root.PersistentFlags().Set("server", "https://orka.b.example"); err != nil {
		t.Fatal(err)
	}
	if err := kagentAPIContextProblem(root, rules, "b"); err == nil {
		t.Fatal("--context b with --server alone was accepted")
	}
	if err := root.PersistentFlags().Set("token", "b-token"); err != nil {
		t.Fatal(err)
	}
	if err := kagentAPIContextProblem(root, rules, "b"); err != nil {
		t.Fatalf("--context b with --server and --token = %v", err)
	}
}

// A stalled read cannot outlast the wait's deadline.
func TestKagentWaitBoundsEachCheck(t *testing.T) {
	k := &kagentRuntimes{poll: time.Millisecond}
	start := time.Now()
	err := k.waitFor(context.Background(), 20*time.Millisecond, "a stalled read", func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("wait = %v after %s", err, time.Since(start))
	}
}

// An unreadable transaction token file fails before any lock is taken,
// instead of exiting the process from inside the import.
func TestKagentActivateReadsTheTransactionTokenFirst(t *testing.T) {
	root := newRootCmd()
	if err := root.PersistentFlags().Set("txn-token-file", filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	if _, err := kagentTxnToken(root); err == nil || !strings.Contains(err.Error(), "--txn-token-file") {
		t.Fatalf("unreadable token file = %v", err)
	}
	if err := root.PersistentFlags().Set("txn-token-file", writeTempFile(t, "txn-value\n")); err != nil {
		t.Fatal(err)
	}
	if token, err := kagentTxnToken(root); err != nil || token != "txn-value" {
		t.Fatalf("token = %q, %v", token, err)
	}
	if !kagentReadsStdin([]string{"-f", "-"}) || !kagentReadsStdin([]string{"--file=-"}) || !kagentReadsStdin([]string{"-f-"}) ||
		kagentReadsStdin([]string{"-f", "x.yaml"}) {
		t.Fatal("standard input use was misread")
	}
	// Given after --, the file would be read inside the import, with the locks held.
	for _, flags := range [][]string{{"-f", "x.yaml", "--txn-token-file", "missing"}, {"--txn-token-file=missing"}} {
		if _, err := kagentActivateArgs(context.Background(), flags, t.TempDir(), t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "before --") {
			t.Errorf("%v: err = %v", flags, err)
		}
	}
}
