// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type externalCleanupReviewClient struct {
	client.Client
	role, stage        string
	lost, fired        bool
	createError        error
	creates            map[string]int
	deletes            map[types.NamespacedName]client.DeleteOptions
	lostDelete         bool
	replaceAfterDelete bool
}

func (c *externalCleanupReviewClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if object.GetUID() == "" {
		object.SetUID(types.UID("review-" + object.GetNamespace() + "-" + object.GetName()))
	}
	secret, private := object.(*corev1.Secret)
	fail := private && c.stage == "create" && secret.Labels[c.role] == booleanTrueValue && !c.fired
	if fail && !c.lost {
		c.fired = true
		return c.createError
	}
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if private {
		for _, role := range []string{runtimePoolAuthLabel, runtimePoolProviderCredentialLabel} {
			if secret.Labels[role] == booleanTrueValue {
				c.creates[role]++
			}
		}
	}
	if fail {
		c.fired = true
		return errors.New("accepted credential CREATE response lost")
	}
	return nil
}

func (c *externalCleanupReviewClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	fail := false
	if pool, ok := object.(*corev1alpha1.RuntimePool); ok && !c.fired {
		intents, _ := externalCredentialIntents(pool)
		for _, intent := range intents {
			if intent.Role != c.role {
				continue
			}
			fail = fail || c.stage == "intent" && !intent.CreateIssued || c.stage == "issued" && intent.CreateIssued && intent.UID == "" || c.stage == "uid" && intent.UID != ""
		}
		fail = fail || c.stage == "binding" && pool.Annotations[runtimePoolPrivateAuthSecretBindingAnnotation(7)] != ""
		if c.stage == "prune" {
			old := &corev1alpha1.RuntimePool{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), old); err != nil {
				return err
			}
			previous, _ := externalCredentialIntents(old)
			for _, prior := range previous {
				if prior.Role != c.role {
					continue
				}
				found := false
				for _, current := range intents {
					found = found || current.Name == prior.Name && current.Namespace == prior.Namespace
				}
				fail = fail || !found
			}
		}
	}
	if fail && !c.lost {
		c.fired = true
		return errors.New("credential metadata write rejected")
	}
	if err := c.Client.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	if fail {
		c.fired = true
		return errors.New("accepted credential metadata response lost")
	}
	return nil
}

func (c *externalCleanupReviewClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	c.deletes[client.ObjectKeyFromObject(object)] = *(&client.DeleteOptions{}).ApplyOptions(options)
	if err := c.Client.Delete(ctx, object, options...); err != nil {
		return err
	}
	if c.replaceAfterDelete {
		c.replaceAfterDelete = false
		foreign := object.DeepCopyObject().(client.Object)
		foreign.SetUID("foreign-replacement")
		foreign.SetResourceVersion("")
		foreign.SetOwnerReferences(nil)
		if err := c.Client.Create(ctx, foreign); err != nil {
			return err
		}
	}
	if c.lostDelete {
		c.lostDelete = false
		return errors.New("accepted exact DELETE response lost")
	}
	return nil
}

func externalCleanupReviewFixture(t *testing.T) (*externalRuntimePoolFixture, *externalCleanupReviewClient) {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	c := &externalCleanupReviewClient{Client: f.r.Client, creates: map[string]int{}, deletes: map[types.NamespacedName]client.DeleteOptions{}}
	f.r.APIReader = c.Client
	f.r.Client = c
	return f, c
}

func externalCleanupReviewDelete(t *testing.T, f *externalRuntimePoolFixture) {
	t.Helper()
	ctx := context.Background()
	w := f.currentWorkspace(t)
	w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	if err := f.r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateDeleted
	w.Status.ObservedGeneration = w.Generation
	w.Status.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked, EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionNotApplicable, PersistentVolumes: workspacev1alpha1.DispositionNotApplicable, Checkpoints: workspacev1alpha1.DispositionNotApplicable, ProviderResources: workspacev1alpha1.DispositionNotApplicable}
	if err := f.r.Status().Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.Delete(ctx, &pool); err != nil {
		t.Fatal(err)
	}
}

func externalCleanupReviewGone(t *testing.T, f *externalRuntimePoolFixture) {
	t.Helper()
	ctx := context.Background()
	for range 8 {
		_, _ = f.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)})
		if apierrors.IsNotFound(f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{})) {
			return
		}
	}
	t.Fatal("normal exact provider termination did not release the RuntimePool")
}

func TestExternalCredentialReceiptsRecoverMetadataAndCreateResponsesBeforePublication(t *testing.T) {
	for _, role := range []string{runtimePoolAuthLabel, runtimePoolProviderCredentialLabel} {
		for _, stage := range []string{"intent", "issued", "uid", "create", "binding"} {
			if stage == "binding" && role != runtimePoolAuthLabel {
				continue
			}
			for _, lost := range []bool{false, true} {
				if stage == "create" && !lost || stage == "issued" && lost {
					continue
				}
				t.Run(role+"/"+stage+map[bool]string{false: "/rejected", true: "/lost"}[lost], func(t *testing.T) {
					f, c := externalCleanupReviewFixture(t)
					c.role, c.stage, c.lost = role, stage, lost
					for range 4 {
						runtimePoolReconcile(t, f.r, f.pool)
						if c.fired {
							break
						}
					}
					if !c.fired || f.currentWorkspace(t).Spec.Workload != nil || f.seeds != 0 {
						t.Fatal("uncertain credential write reached publication/bootstrap")
					}
					f.publish(t)
					pool := runtimePoolTestGetPool(t, f.r, f.pool)
					intents, err := externalCredentialIntents(&pool)
					if err != nil || len(intents) != 2 {
						t.Fatalf("receipt count/validity = %d/%v", len(intents), err)
					}
					for _, intent := range intents {
						secret := &corev1.Secret{}
						if err := c.Get(context.Background(), types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}, secret); err != nil {
							t.Fatal(err)
						}
						if !intent.CreateIssued || intent.UID == "" || intent.UID != secret.UID || intent.DataSHA256 != externalCredentialDataDigest(secret.Data) {
							t.Fatal("published credential lacks exact private receipt")
						}
					}
					if c.creates[role] != 1 {
						t.Fatalf("accepted credential CREATE count = %d", c.creates[role])
					}
				})
			}
		}
	}
}

func TestExternalCredentialIssuedUnknownUIDAbsenceNeverReplaysOrFinalizes(t *testing.T) {
	for _, role := range []string{runtimePoolAuthLabel, runtimePoolProviderCredentialLabel} {
		for _, stage := range []string{"create", "issued"} {
			t.Run(role+"/"+stage, func(t *testing.T) {
				f, c := externalCleanupReviewFixture(t)
				c.role, c.stage, c.lost, c.createError = role, stage, stage == "issued", errors.New("uncertain CREATE transport outcome")
				for range 8 {
					runtimePoolReconcile(t, f.r, f.pool)
				}
				if !c.fired || f.currentWorkspace(t).Spec.Workload != nil || c.creates[role] != 0 || f.seeds != 0 {
					t.Fatal("uncertain absence selected/published a replacement")
				}
				externalCleanupReviewDelete(t, f)
				for range 4 {
					runtimePoolReconcile(t, f.r, f.pool)
				}
				pool := runtimePoolTestGetPool(t, f.r, f.pool)
				if !strings.Contains(strings.Join(pool.Finalizers, ","), runtimePoolFinalizer) {
					t.Fatal("ambiguous CREATE lost cleanup protection")
				}
			})
		}
	}
}

func TestExternalCredentialDefinitiveCreateRejectionAllowsNormalCleanup(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	c.role, c.stage = runtimePoolAuthLabel, "create"
	c.createError = apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "private", errors.New("admission rejected"))
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
		if c.fired {
			break
		}
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	intents, err := externalCredentialIntents(&pool)
	if err != nil || len(intents) != 1 || intents[0].CreateIssued {
		t.Fatal("definitive rejection retained an ambiguous creation claim")
	}
	externalCleanupReviewDelete(t, f)
	externalCleanupReviewGone(t, f)
}

func TestExternalCoreCleanupPreservesSpoofedForeignResourcesAndDeletesExactChildren(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	f.r.EnablePDB = true
	f.publish(t)
	ctx := context.Background()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	foreign := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: runtimePoolChildName(cfg.baseName, "auth-e7-"+strings.Repeat("f", 24)), Labels: mergeStringMap(cloneStringMap(cfg.labels), map[string]string{runtimePoolAuthLabel: booleanTrueValue, runtimePoolCredentialEpochLabel: "7"})}, Type: corev1.SecretTypeOpaque, Immutable: new(true), Data: auth.DeepCopy().Data},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: "foreign-service", Labels: cloneStringMap(cfg.labels)}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: "foreign-pdb", Labels: cloneStringMap(cfg.labels)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "foreign-namespace", Name: cfg.baseName, Labels: cloneStringMap(cfg.labels)}},
	}
	for _, object := range foreign {
		if err := c.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	externalCleanupReviewDelete(t, f)
	c.lostDelete = true
	externalCleanupReviewGone(t, f)
	for _, object := range foreign {
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopyObject().(client.Object)); err != nil {
			t.Fatal("foreign label inventory was changed", err)
		}
		if _, deleted := c.deletes[client.ObjectKeyFromObject(object)]; deleted {
			t.Fatal("foreign object received DELETE")
		}
	}
	for key, options := range c.deletes {
		if key.Name == f.pool.Name {
			continue
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID == "" || options.Preconditions.ResourceVersion == nil {
			t.Fatal("child DELETE lacks exact UID/RV fence")
		}
	}
	for _, object := range []client.Object{&corev1.Secret{ObjectMeta: auth.ObjectMeta}, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: cfg.baseName}}, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: runtimePoolChildName(cfg.baseName, "pdb")}}} {
		if !apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(object), object)) {
			t.Fatal("exact Core child survived authorized cleanup")
		}
	}
}

func TestExternalCoreCleanupPreservesSameKeyForeignDiscoveryShapes(t *testing.T) {
	for _, kind := range []string{"service", "pdb"} {
		for _, drift := range []string{"owner", "key", "spec"} {
			t.Run(kind+"/"+drift, func(t *testing.T) {
				f, c := externalCleanupReviewFixture(t)
				f.r.EnablePDB = true
				f.publish(t)
				cfg, err := f.r.runtimePoolConfig(f.pool)
				if err != nil {
					t.Fatal(err)
				}
				var object client.Object = &corev1.Service{}
				name := cfg.baseName
				if kind == "pdb" {
					object = &policyv1.PodDisruptionBudget{}
					name = runtimePoolChildName(cfg.baseName, "pdb")
				}
				key := types.NamespacedName{Namespace: cfg.namespace, Name: name}
				if err := c.Get(context.Background(), key, object); err != nil {
					t.Fatal(err)
				}
				switch drift {
				case "owner":
					object.SetOwnerReferences(nil)
				case "key":
					object.GetLabels()[runtimePoolKeyLabel] = "foreign"
				case "spec":
					if service, ok := object.(*corev1.Service); ok {
						service.Spec.Selector = map[string]string{"foreign": "true"}
					} else {
						object.(*policyv1.PodDisruptionBudget).Spec.MinAvailable = new(intstr.FromInt32(1))
					}
				}
				if err := c.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
				externalCleanupReviewDelete(t, f)
				externalCleanupReviewGone(t, f)
				if _, deleted := c.deletes[key]; deleted {
					t.Fatal("same-key foreign discovery object was deleted")
				}
				if err := c.Get(context.Background(), key, object); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestExternalCoreCleanupAcceptsAPIDefaultsAndObservesReplacementUID(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	f.r.EnablePDB = true
	f.publish(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	service := &corev1.Service{}
	key := types.NamespacedName{Namespace: cfg.namespace, Name: cfg.baseName}
	if err := c.Get(context.Background(), key, service); err != nil {
		t.Fatal(err)
	}
	service.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
	service.Spec.IPFamilyPolicy = new(corev1.IPFamilyPolicySingleStack)
	service.Spec.InternalTrafficPolicy = new(corev1.ServiceInternalTrafficPolicyCluster)
	service.Spec.SessionAffinity = corev1.ServiceAffinityNone
	if err := c.Update(context.Background(), service); err != nil {
		t.Fatal(err)
	}
	// The actual deleted UID disappears; a same-name replacement is foreign
	// and cannot be deleted or hold up this Core allocation's finalizer.
	c.replaceAfterDelete = true
	remaining, err := f.r.deleteExternalCoreDiscoveryResources(context.Background(), &pool, cfg)
	if err != nil || remaining {
		t.Fatalf("API defaults/replacement = %v/%v", remaining, err)
	}
	if err := c.Get(context.Background(), key, service); err != nil || service.UID != "foreign-replacement" {
		t.Fatal("same-name replacement was not preserved", err)
	}
	options := c.deletes[key]
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
		t.Fatal("discovery delete was not fenced")
	}
	externalCleanupReviewDelete(t, f)
	externalCleanupReviewGone(t, f)
	if err := c.Get(context.Background(), key, service); err != nil {
		t.Fatal("retry deleted foreign replacement", err)
	}
}

type externalCleanupUnavailableReader struct{ client.Reader }

func (r externalCleanupUnavailableReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, service := object.(*corev1.Service); service {
		return errors.New("authoritative Service read unavailable")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestExternalCoreCleanupReadOutageKeepsFinalizerAndRetries(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	f.publish(t)
	externalCleanupReviewDelete(t, f)
	f.r.APIReader = externalCleanupUnavailableReader{c.Client}
	for range 3 {
		_, _ = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)})
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if !strings.Contains(strings.Join(pool.Finalizers, ","), runtimePoolFinalizer) {
		t.Fatal("read uncertainty released finalizer")
	}
	f.r.APIReader = c.Client
	externalCleanupReviewGone(t, f)
}

func TestExternalCredentialReceiptPruningRecoversRejectedAndLostResponse(t *testing.T) {
	for _, role := range []string{runtimePoolAuthLabel, runtimePoolProviderCredentialLabel} {
		for _, lost := range []bool{false, true} {
			t.Run(role+map[bool]string{false: "/rejected", true: "/lost"}[lost], func(t *testing.T) {
				f, c := externalCleanupReviewFixture(t)
				f.publish(t)
				pool := runtimePoolTestGetPool(t, f.r, f.pool)
				intents, err := externalCredentialIntents(&pool)
				if err != nil {
					t.Fatal(err)
				}
				c.role, c.stage, c.lost = role, "prune", lost
				externalCleanupReviewDelete(t, f)
				externalCleanupReviewGone(t, f)
				if !c.fired {
					t.Fatal("receipt pruning fault not exercised")
				}
				for _, intent := range intents {
					key := types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}
					if !apierrors.IsNotFound(c.Get(context.Background(), key, &corev1.Secret{})) {
						t.Fatal("accepted deletion was not recovered")
					}
				}
			})
		}
	}
}

func TestExternalCredentialSameNameForeignUIDIsNeverSeededOrDeleted(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	f.publish(t)
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	if err := c.Client.Delete(context.Background(), &auth); err != nil {
		t.Fatal(err)
	}
	auth.UID = "foreign-same-name"
	auth.ResourceVersion = ""
	if err := c.Client.Create(context.Background(), &auth); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if f.seeds != 0 || f.supervisor.probeCalls != 0 {
		t.Fatal("same-name replacement was seeded")
	}
	externalCleanupReviewDelete(t, f)
	externalCleanupReviewGone(t, f)
	if _, deleted := c.deletes[client.ObjectKeyFromObject(&auth)]; deleted {
		t.Fatal("same-name replacement received DELETE")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(&auth), &corev1.Secret{}); err != nil {
		t.Fatal(err)
	}
}

func TestExternalCredentialForeignPreseedsAreNotAdoptedOrCollected(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	cfg, err := f.r.runtimePoolConfig(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	foreign := []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: runtimePoolChildName(cfg.baseName, "auth-e7-"+strings.Repeat("f", 24)), Labels: mergeStringMap(cloneStringMap(cfg.labels), map[string]string{runtimePoolAuthLabel: booleanTrueValue, runtimePoolCredentialEpochLabel: "7"})}, Type: corev1.SecretTypeOpaque, Immutable: new(true), Data: map[string][]byte{runtimePoolControllerTokenKey: []byte("foreign-controller"), runtimePoolCapabilitySecretKey: []byte("foreign-capability")}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.namespace, Name: runtimePoolChildName(cfg.baseName, "provider-e7-g"+cfg.providerProxy.tokenGeneration+"-"+strings.Repeat("f", 24)), Labels: mergeStringMap(cloneStringMap(cfg.labels), map[string]string{runtimePoolProviderCredentialLabel: booleanTrueValue, runtimePoolCredentialEpochLabel: "7", runtimePoolProviderGenerationLabel: cfg.providerProxy.tokenGeneration})}, Type: corev1.SecretTypeOpaque, Immutable: new(true), Data: map[string][]byte{runtimePoolProviderTokenKey: []byte("foreign-provider")}},
	}
	for _, secret := range foreign {
		if err := f.r.setRuntimePoolControllerReference(f.pool, secret); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
	}
	f.publish(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	intents, err := externalCredentialIntents(&pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range intents {
		for _, secret := range foreign {
			if intent.Name == secret.Name {
				t.Fatal("foreign preseed acquired a trusted receipt")
			}
		}
	}
	externalCleanupReviewDelete(t, f)
	externalCleanupReviewGone(t, f)
	for _, secret := range foreign {
		if _, deleted := c.deletes[client.ObjectKeyFromObject(secret)]; deleted {
			t.Fatal("foreign private-shaped preseed was deleted")
		}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExternalCoreCleanupUsesFrozenRuntimeNamespaceAndPrivateReceipts(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	f.r.EnablePDB = true
	f.r.RuntimeNamespace = "separate-runtime"
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.RuntimeNamespace = "separate-runtime"
	if err := c.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	f.publish(t)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	intents, err := externalCredentialIntents(&pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range intents {
		secret := &corev1.Secret{}
		if intent.Namespace != "separate-runtime" || c.Get(context.Background(), types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}, secret) != nil || len(secret.OwnerReferences) != 0 {
			t.Fatal("split-namespace credential was not created as intended")
		}
	}
	f.r.RuntimeNamespace = "changed-controller-default"
	externalCleanupReviewDelete(t, f)
	externalCleanupReviewGone(t, f)
	for _, intent := range intents {
		if !apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}, &corev1.Secret{})) {
			t.Fatal("frozen cross-namespace credential survived")
		}
	}
	for key := range c.deletes {
		if key.Name != pool.Name && key.Namespace != "separate-runtime" {
			t.Fatal("cleanup escaped the frozen runtime namespace")
		}
	}
}

func TestExternalCredentialCleanupUsesReceiptsAcrossEpochAndTokenRotation(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	f.publish(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	old, err := externalCredentialIntents(&pool)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	cfg.controllerEpoch++
	cfg.providerProxy.token = []byte("different-private-operator-token")
	cfg.providerProxy.tokenGeneration = runtimePoolProviderTokenGeneration(cfg.providerProxy.token)
	if _, _, err := f.r.ensurePrivateWorkspaceRuntimePoolSecrets(context.Background(), &pool, cfg); err != nil {
		t.Fatal(err)
	}
	all, err := externalCredentialIntents(&pool)
	if err != nil || len(all) != 4 {
		t.Fatalf("rotation history = %d/%v", len(all), err)
	}
	externalCleanupReviewDelete(t, f)
	externalCleanupReviewGone(t, f)
	for _, intent := range append(old, all...) {
		if !apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}, &corev1.Secret{})) {
			t.Fatal("saved historical credential survived")
		}
	}
}

func TestExternalCredentialLegacyExactAuthUIDCleanupLeavesUnknownProvider(t *testing.T) {
	for _, bootstrapOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "name-and-uid", true: "bootstrap-uid"}[bootstrapOnly], func(t *testing.T) {
			f, c := externalCleanupReviewFixture(t)
			f.publish(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			intents, err := externalCredentialIntents(&pool)
			if err != nil {
				t.Fatal(err)
			}
			auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
			delete(pool.Annotations, externalPrivateCredentialIntentsAnnotation)
			if bootstrapOnly {
				delete(pool.Annotations, runtimePoolPrivateAuthSecretBindingAnnotation(7))
				encoded, _ := json.Marshal(runtimePoolBootstrapInstanceBinding{AuthSecretUID: auth.UID, WorkloadUID: "never-current-runtime"})
				pool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] = string(encoded)
				// Public no-allocation closeout rightly rejects consumed bootstrap;
				// this exact authorized resource cleanup seam proves legacy UID recovery.
				if err := c.Update(context.Background(), &pool); err != nil {
					t.Fatal(err)
				}
				cfg, err := f.r.runtimePoolConfig(&pool)
				if err != nil {
					t.Fatal(err)
				}
				if remaining, err := f.r.deleteExternalRuntimePoolCoreResources(context.Background(), &pool, cfg); err != nil || remaining {
					t.Fatalf("legacy cleanup = %v/%v", remaining, err)
				}
			} else {
				if err := c.Update(context.Background(), &pool); err != nil {
					t.Fatal(err)
				}
				externalCleanupReviewDelete(t, f)
				externalCleanupReviewGone(t, f)
			}
			for _, intent := range intents {
				err := c.Get(context.Background(), types.NamespacedName{Namespace: intent.Namespace, Name: intent.Name}, &corev1.Secret{})
				if intent.Role == runtimePoolAuthLabel && !apierrors.IsNotFound(err) {
					t.Fatal("exact legacy auth UID survived")
				}
				if intent.Role == runtimePoolProviderCredentialLabel && err != nil {
					t.Fatal("unreceipted legacy provider was collected")
				}
			}
		})
	}
}

func TestExternalCredentialPrivateDataDriftFailsClosedBeforeBootstrapAndCleanup(t *testing.T) {
	f, c := externalCleanupReviewFixture(t)
	f.materialize(t)
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	auth.Data[runtimePoolControllerTokenKey] = []byte("foreign-private-token")
	if err := c.Update(context.Background(), &auth); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if f.seeds != 0 || f.supervisor.probeCalls != 0 {
		t.Fatal("credential drift was seeded")
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.deleteExternalPrivateCredentials(context.Background(), &pool, cfg); err == nil {
		t.Fatal("changed private Data authorized DELETE")
	}
	if _, deleted := c.deletes[client.ObjectKeyFromObject(&auth)]; deleted {
		t.Fatal("changed private Data was deleted")
	}
}
