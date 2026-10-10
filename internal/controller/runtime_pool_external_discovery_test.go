// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func externalDiscoveryTestFixture(t *testing.T) (*externalRuntimePoolFixture, *externalCleanupReviewClient, *corev1alpha1.RuntimePool, runtimePoolConfig) {
	t.Helper()
	f, c := externalCleanupReviewFixture(t)
	f.r.EnablePDB = true
	f.r.RuntimeNamespace = "separate-runtime"
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.RuntimeNamespace = f.r.RuntimeNamespace
	if err := c.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	return f, c, &pool, cfg
}

func TestExternalDiscoveryPreservesSameKeyForeignObjects(t *testing.T) {
	for _, kind := range []string{"Service", "PodDisruptionBudget"} {
		for _, recorded := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/preseed", true: "/replacement"}[recorded], func(t *testing.T) {
				f, c, pool, cfg := externalDiscoveryTestFixture(t)
				object := externalDiscoveryObject(cfg, kind)
				if recorded {
					if err := f.r.ensureExternalDiscoveryResource(t.Context(), pool, cfg, kind); err != nil {
						t.Fatal(err)
					}
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
						t.Fatal(err)
					}
					if err := c.Client.Delete(t.Context(), object); err != nil {
						t.Fatal(err)
					}
					object.SetResourceVersion("")
				}
				object.SetUID("foreign-discovery-uid")
				if err := c.Create(t.Context(), object); err != nil {
					t.Fatal(err)
				}
				before := object.DeepCopyObject().(client.Object)
				if err := f.r.ensureExternalDiscoveryResource(t.Context(), pool, cfg, kind); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
					t.Fatalf("foreign exact-spec discovery object admitted: %v", err)
				}
				if kind == "PodDisruptionBudget" {
					f.r.EnablePDB = false
					if err := f.r.ensureRuntimePoolPDB(t.Context(), pool, cfg); err != nil {
						t.Fatal(err)
					}
				}
				if remaining, err := f.r.deleteExternalCoreDiscoveryResources(t.Context(), pool, cfg); err != nil || remaining {
					t.Fatalf("foreign discovery object blocked unrelated cleanup: %v, %v", remaining, err)
				}
				current := object.DeepCopyObject().(client.Object)
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(object), current); err != nil || !reflect.DeepEqual(current, before) {
					t.Fatalf("foreign discovery resource was modified or collected: %v", err)
				}
				if _, deleted := c.deletes[client.ObjectKeyFromObject(object)]; deleted {
					t.Fatal("foreign discovery object received a DELETE")
				}
			})
		}
	}
}

type externalDiscoveryWriteFailureClient struct {
	client.Client
	kind, stage     string
	accepted, fired bool
	creates         int
}

func (c *externalDiscoveryWriteFailureClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if reflect.TypeOf(object) != reflect.TypeOf(externalDiscoveryObject(runtimePoolConfig{}, c.kind)) {
		return c.Client.Create(ctx, object, options...)
	}
	c.creates++
	if c.stage == "create" && !c.fired && !c.accepted {
		c.fired = true
		return apierrors.NewForbidden(schema.GroupResource{Resource: "objects"}, object.GetName(), errors.New("create denied"))
	}
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if c.stage == "create" && !c.fired {
		c.fired = true
		return errors.New("accepted discovery create response lost")
	}
	return nil
}

func (c *externalDiscoveryWriteFailureClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	pool, ok := object.(*corev1alpha1.RuntimePool)
	fail := false
	if ok && !c.fired {
		intents, _ := externalDiscoveryIntents(pool)
		for _, intent := range intents {
			if intent.Kind == c.kind {
				fail = fail || c.stage == "intent" && !intent.CreateIssued || c.stage == "issued" && intent.CreateIssued && intent.UID == "" || c.stage == "uid" && intent.UID != ""
			}
		}
	}
	if fail && !c.accepted {
		c.fired = true
		return errors.New("discovery receipt write rejected")
	}
	if err := c.Client.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	if fail {
		c.fired = true
		return errors.New("accepted discovery receipt response lost")
	}
	return nil
}

func TestExternalDiscoveryReceiptLossNeverAdoptsUnknownUID(t *testing.T) {
	for _, kind := range []string{"Service", "PodDisruptionBudget"} {
		for _, stage := range []string{"intent", "issued", "create", "uid"} {
			for _, accepted := range []bool{false, true} {
				t.Run(kind+"/"+stage+map[bool]string{false: "/rejected", true: "/accepted"}[accepted], func(t *testing.T) {
					f, c, pool, cfg := externalDiscoveryTestFixture(t)
					failed := &externalDiscoveryWriteFailureClient{Client: c, kind: kind, stage: stage, accepted: accepted}
					f.r.Client = failed
					if err := f.r.ensureExternalDiscoveryResource(t.Context(), pool, cfg, kind); err == nil || !failed.fired {
						t.Fatalf("write failure not exercised: %v", err)
					}
					currentPool := runtimePoolTestGetPool(t, f.r, f.pool)
					unknown := stage == "issued" && accepted || stage == "create" && accepted || stage == "uid" && !accepted
					if unknown {
						for range 3 {
							if err := f.r.ensureExternalDiscoveryResource(t.Context(), &currentPool, cfg, kind); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
								t.Fatalf("unknown UID received admission: %v", err)
							}
							if _, err := f.r.deleteExternalCoreDiscoveryResources(t.Context(), &currentPool, cfg); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
								t.Fatalf("unknown UID allowed finalization: %v", err)
							}
						}
						wantCreates := 1
						if stage == "issued" {
							wantCreates = 0
						}
						if failed.creates != wantCreates {
							t.Fatal("unknown issued create was replayed")
						}
						if len(c.deletes) != 0 {
							t.Fatal("unknown UID resource was deleted")
						}
						return
					}
					if err := f.r.ensureExternalDiscoveryResource(t.Context(), &currentPool, cfg, kind); err != nil {
						t.Fatalf("known create outcome failed retry: %v", err)
					}
					intents, err := externalDiscoveryIntents(&currentPool)
					if err != nil || len(intents) != 1 || intents[0].UID == "" {
						t.Fatalf("acknowledged UID receipt absent: %+v %v", intents, err)
					}
					if remaining, err := f.r.deleteExternalCoreDiscoveryResources(t.Context(), &currentPool, cfg); err != nil || remaining {
						t.Fatalf("exact child deletion: %v %v", remaining, err)
					}
					object := externalDiscoveryObject(cfg, kind)
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
						t.Fatalf("exact discovery object remains: %v", err)
					}
					options := c.deletes[client.ObjectKeyFromObject(object)]
					if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != intents[0].UID || options.Preconditions.ResourceVersion == nil {
						t.Fatal("child DELETE lacked exact UID/RV preconditions")
					}
				})
			}
		}
	}
}

func TestExternalDiscoveryCleanupRejectsCorruptedReceipts(t *testing.T) {
	f, _, pool, cfg := externalDiscoveryTestFixture(t)
	pool.Annotations[externalDiscoveryIntentsAnnotation] = "not-json"
	if _, err := f.r.deleteExternalCoreDiscoveryResources(t.Context(), pool, cfg); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
		t.Fatalf("corrupt receipt allowed finalization: %v", err)
	}
	pool.Annotations[externalDiscoveryIntentsAnnotation] = `[{"namespace":"separate-runtime","name":"foreign","kind":"Service","createIssued":true,"uid":"foreign"}]`
	if _, err := externalDiscoveryIntents(pool); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
		t.Fatalf("foreign key receipt accepted: %v", err)
	}
}
