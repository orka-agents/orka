package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Native templates have no labels or owner references. Orka keeps the owner
// and immutable revision bindings in its own ConfigMaps, before creating
// provider revisions. These records contain public configuration, never credentials.
// They are not provider CRDs and are never consumed by Substrate.
type substrateTemplateStore interface {
	Get(context.Context, string, string) (*unstructured.Unstructured, error)
	Create(context.Context, *corev1alpha1.RuntimePool, *unstructured.Unstructured) error
	Update(context.Context, *unstructured.Unstructured, *unstructured.Unstructured) error
	Delete(context.Context, *unstructured.Unstructured) error
}

type nativeSubstrateTemplateStore struct{ r *RuntimePoolReconciler }

// +kubebuilder:rbac:groups=ate.dev,resources=workerpools,verbs=get;list;watch

type substrateNativeTemplateRevision struct {
	Name   string `json:"name"`
	UID    string `json:"uid,omitempty"`
	Digest string `json:"digest"`
}

type substrateTemplateBinding struct {
	Retired    bool                              `json:"retired,omitempty"`
	Atespace   string                            `json:"atespace"`
	Name       string                            `json:"name"`
	OwnerUID   string                            `json:"ownerUID"`
	Manifest   json.RawMessage                   `json:"manifest"`
	Current    substrateNativeTemplateRevision   `json:"current"`
	Pending    *substrateNativeTemplateRevision  `json:"pending,omitempty"`
	Collecting *substrateNativeTemplateRevision  `json:"collecting,omitempty"`
	Revisions  []substrateNativeTemplateRevision `json:"revisions"`
}

const substrateTemplateBindingLabel = "orka.ai/substrate-template-binding"

func (r *RuntimePoolReconciler) substrateTemplates() substrateTemplateStore {
	if r.SubstrateTemplates != nil {
		return r.SubstrateTemplates
	}
	return &nativeSubstrateTemplateStore{r: r}
}

func (r *RuntimePoolReconciler) substrateNativeClient() (*workspace.SubstrateNativeClient, error) {
	if r.SubstrateNativeClientFactory != nil {
		return r.SubstrateNativeClientFactory(r.SubstrateConfig)
	}
	return workspace.NewSubstrateNativeClient(r.SubstrateConfig.WorkspaceClientConfig())
}

func (s *nativeSubstrateTemplateStore) key(atespace, name string) (types.NamespacedName, error) {
	if strings.TrimSpace(s.r.ControllerNamespace) == "" {
		return types.NamespacedName{}, fmt.Errorf("controller namespace is required for Substrate template ownership records")
	}
	sum := sha256.Sum256([]byte(atespace + "\x00" + name))
	return types.NamespacedName{Namespace: s.r.ControllerNamespace, Name: "substrate-template-" + hex.EncodeToString(sum[:16])}, nil
}

func (s *nativeSubstrateTemplateStore) read(ctx context.Context, atespace, name string) (*corev1.ConfigMap, *substrateTemplateBinding, error) {
	key, err := s.key(atespace, name)
	if err != nil {
		return nil, nil, err
	}
	cm := &corev1.ConfigMap{}
	reader := s.r.APIReader
	if reader == nil {
		reader = s.r.Client
	}
	if err := reader.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	binding := &substrateTemplateBinding{}
	if err := json.Unmarshal([]byte(cm.Data["binding.json"]), binding); err != nil || binding.Atespace != atespace || binding.Name != name || binding.OwnerUID == "" ||
		cm.Labels[runtimePoolManagedByLabel] != runtimePoolManagedByLabelValue || cm.Labels[runtimePoolUIDLabel] != binding.OwnerUID {
		return nil, nil, fmt.Errorf("substrate template ownership record is invalid or belongs to another owner")
	}
	return cm, binding, nil
}

func (s *nativeSubstrateTemplateStore) save(ctx context.Context, cm *corev1.ConfigMap, binding *substrateTemplateBinding) error {
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if len(data) > 512<<10 {
		return fmt.Errorf("substrate template ownership record exceeds its size limit")
	}
	cm.Data = map[string]string{"binding.json": string(data)}
	if cm.ResourceVersion == "" {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[substrateTemplateBindingLabel] = substrateOwnedLabelValue
		return s.r.Create(ctx, cm)
	}
	return s.r.Update(ctx, cm)
}

func (s *nativeSubstrateTemplateStore) Get(ctx context.Context, atespace, name string) (*unstructured.Unstructured, error) {
	cm, binding, err := s.read(ctx, atespace, name)
	if err != nil {
		return nil, err
	}
	if binding != nil && binding.Current.Name == "" {
		return nil, nil
	}
	if binding != nil && binding.Retired {
		return nil, fmt.Errorf("native Substrate template binding is retired")
	}
	nativeName := name
	if binding != nil {
		nativeName = binding.Current.Name
	}
	api, err := s.r.substrateNativeClient()
	if err != nil {
		return nil, err
	}
	defer api.Close() //nolint:errcheck
	native, err := api.Control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: nativeName}})
	if status.Code(err) == codes.NotFound {
		if binding != nil {
			return nil, fmt.Errorf("bound native Substrate template disappeared; preserving its ownership record")
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read native Substrate ActorTemplate: %w", err)
	}
	if binding == nil {
		return s.infrastructure(ctx, native)
	}
	digest, err := nativeSubstrateTemplateDigest(native)
	if err != nil {
		return nil, err
	}
	if native.GetMetadata().GetUid() != binding.Current.UID || digest != binding.Current.Digest {
		return nil, fmt.Errorf("native Substrate template UID or immutable content no longer matches its ownership record")
	}
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(binding.Manifest, &object.Object); err != nil {
		return nil, err
	}
	if object.GetNamespace() != atespace || object.GetName() != name || object.GetLabels()[runtimePoolUIDLabel] != binding.OwnerUID {
		return nil, fmt.Errorf("substrate template manifest no longer matches its ownership record")
	}
	expected, err := nativeSubstrateRuntimeTemplate(object)
	if err != nil || !proto.Equal(nativeSubstrateTemplateSpec(expected), nativeSubstrateTemplateSpec(native)) {
		return nil, fmt.Errorf("substrate template manifest conflicts with its immutable native revision")
	}
	object.SetUID(types.UID(binding.Current.UID))
	object.SetGeneration(1) // immutable native spec; status-only provider writes are excluded
	object.SetResourceVersion(string(cm.UID) + ":" + cm.ResourceVersion + "/" + strconv.FormatInt(native.GetMetadata().GetVersion(), 10))
	object.Object["provider"] = map[string]any{substrateNativeObjectName: nativeName, substrateNativeObjectUID: binding.Current.UID, "bindingUID": string(cm.UID)}
	return object, nil
}

func (s *nativeSubstrateTemplateStore) infrastructure(ctx context.Context, template *ateapipb.ActorTemplate) (*unstructured.Unstructured, error) {
	raw, err := protojson.Marshal(template)
	if err != nil {
		return nil, err
	}
	spec := map[string]any{}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	delete(spec, "metadata")
	delete(spec, "status")
	// Existing confinement operates on an exact WorkerPool. Resolve the
	// native selector once and freeze that admission with the runtime record.
	match := template.GetWorkerSelector().GetMatchLabels()
	if len(match) == 0 {
		return nil, fmt.Errorf("substrate infrastructure requires an explicit workerSelector")
	}
	pools := &unstructured.UnstructuredList{}
	pools.SetAPIVersion("ate.dev/v1alpha1")
	pools.SetKind("WorkerPoolList")
	// WorkerPools belong to provider namespaces outside the tenant cache.
	if err := s.r.nativeSubstrateReader().List(ctx, pools, client.MatchingLabels(match)); err != nil {
		return nil, fmt.Errorf("resolve native Substrate worker selector: %w", err)
	}
	if len(pools.Items) != 1 {
		return nil, fmt.Errorf("substrate workerSelector must identify exactly one WorkerPool; found %d", len(pools.Items))
	}
	spec["workerPoolRef"] = map[string]any{substrateNativeObjectNamespace: pools.Items[0].GetNamespace(), substrateNativeObjectName: pools.Items[0].GetName()}
	object := &unstructured.Unstructured{Object: map[string]any{substrateObjectSpecField: spec}}
	object.SetGroupVersionKind(substrateActorTemplateGVK)
	object.SetNamespace(template.GetMetadata().GetAtespace())
	object.SetName(template.GetMetadata().GetName())
	object.SetUID(types.UID(template.GetMetadata().GetUid()))
	object.SetResourceVersion(strconv.FormatInt(template.GetMetadata().GetVersion(), 10))
	return object, nil
}

func (s *nativeSubstrateTemplateStore) Create(ctx context.Context, pool *corev1alpha1.RuntimePool, desired *unstructured.Unstructured) error {
	if desired.GetLabels()[runtimePoolUIDLabel] != string(pool.UID) || pool.UID == "" {
		return fmt.Errorf("substrate template requires an exact RuntimePool owner")
	}
	return s.put(ctx, nil, desired)
}

func (s *nativeSubstrateTemplateStore) Update(ctx context.Context, previous, desired *unstructured.Unstructured) error {
	return s.put(ctx, previous, desired)
}

func (s *nativeSubstrateTemplateStore) put(ctx context.Context, previous, desired *unstructured.Unstructured) error {
	native, err := nativeSubstrateRuntimeTemplate(desired)
	if err != nil {
		return err
	}
	digest, err := nativeSubstrateTemplateDigest(native)
	if err != nil {
		return err
	}
	cm, binding, err := s.read(ctx, desired.GetNamespace(), desired.GetName())
	if err != nil {
		return err
	}
	if binding == nil {
		if previous != nil {
			return fmt.Errorf("substrate template ownership record disappeared before update")
		}
		key, err := s.key(desired.GetNamespace(), desired.GetName())
		if err != nil {
			return err
		}
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: cloneStringMap(desired.GetLabels())}}
		binding = &substrateTemplateBinding{Atespace: desired.GetNamespace(), Name: desired.GetName(), OwnerUID: desired.GetLabels()[runtimePoolUIDLabel]}
	} else {
		if binding.Retired || binding.Collecting != nil {
			return fmt.Errorf("substrate template cleanup is in progress")
		}
		if binding.OwnerUID != desired.GetLabels()[runtimePoolUIDLabel] {
			return fmt.Errorf("substrate template binding belongs to a different RuntimePool lifetime")
		}
		if previous != nil && (string(previous.GetUID()) != binding.Current.UID || !strings.HasPrefix(previous.GetResourceVersion(), string(cm.UID)+":"+cm.ResourceVersion+"/")) {
			return fmt.Errorf("substrate template ownership record changed before update")
		}
		if previous == nil && binding.Current.Name != "" {
			return nil
		}
	}
	pending := substrateNativeTemplateRevision{Name: native.GetMetadata().GetName(), Digest: digest}
	if binding.Pending != nil && *binding.Pending != pending {
		return fmt.Errorf("another Substrate template revision is still being created")
	}
	if len(binding.Manifest) == 0 {
		binding.Manifest, err = json.Marshal(desired.Object)
		if err != nil {
			return err
		}
	}
	observed, err := s.materialize(ctx, cm, binding, pending, native)
	if err != nil {
		return err
	}
	if !proto.Equal(nativeSubstrateTemplateSpec(observed), nativeSubstrateTemplateSpec(native)) || observed.GetMetadata().GetUid() == "" {
		return fmt.Errorf("native Substrate template revision conflicts with the controller's immutable content")
	}
	pending.UID = observed.GetMetadata().GetUid()
	manifest := desired.DeepCopy()
	manifest.SetUID("")
	manifest.SetResourceVersion("")
	manifest.SetGeneration(0)
	delete(manifest.Object, "provider")
	binding.Manifest, err = json.Marshal(manifest.Object)
	if err != nil {
		return err
	}
	if binding.Current != pending {
		binding.Revisions = append(binding.Revisions, pending)
	}
	binding.Current, binding.Pending = pending, nil
	return s.save(ctx, cm, binding)
}

// materialize owns the persisted create intent and its one allowed RPC attempt.
func (s *nativeSubstrateTemplateStore) materialize(
	ctx context.Context, cm *corev1.ConfigMap, binding *substrateTemplateBinding,
	pending substrateNativeTemplateRevision, native *ateapipb.ActorTemplate,
) (*ateapipb.ActorTemplate, error) {
	freshIntent := binding.Pending == nil
	api, err := s.r.substrateNativeClient()
	if err != nil {
		return nil, err
	}
	defer api.Close() //nolint:errcheck
	ref := &ateapipb.ObjectRef{Atespace: binding.Atespace, Name: pending.Name}
	observed, err := api.Control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: ref})
	if status.Code(err) == codes.NotFound {
		if !freshIntent {
			// A previous Create may still commit after its client lost the response.
			// Keep its ownership intent until an exact revision can be observed.
			return nil, fmt.Errorf("native Substrate template creation is unresolved; preserving its pending intent")
		}
		binding.Pending = &pending
		if err := s.save(ctx, cm, binding); err != nil {
			return nil, err
		}
		observed, err = api.Control.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{ActorTemplate: native})
		switch status.Code(err) {
		case codes.InvalidArgument, codes.FailedPrecondition, codes.Unauthenticated, codes.PermissionDenied:
			// Upstream rejects authentication and authorization before invoking the create handler
			// and returns these validation errors before persisting a template. This is
			// the sole create attempt for the freshly recorded intent, so corrected
			// credentials or configuration may safely retry or choose another revision.
			binding.Pending = nil
			if saveErr := s.save(ctx, cm, binding); saveErr != nil {
				return nil, errors.Join(fmt.Errorf("create native Substrate template: %w", err), saveErr)
			}
		}
		if status.Code(err) == codes.AlreadyExists {
			observed, err = api.Control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: ref})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("materialize immutable native Substrate template: %w", err)
	}
	return observed, nil
}

func (s *nativeSubstrateTemplateStore) Delete(ctx context.Context, template *unstructured.Unstructured) error {
	cm, binding, err := s.read(ctx, template.GetNamespace(), template.GetName())
	if err != nil || binding == nil {
		return err
	}
	if binding.Current.UID != string(template.GetUID()) {
		return fmt.Errorf("substrate template binding changed before cleanup")
	}
	if !binding.Retired {
		binding.Retired = true
		if err := s.save(ctx, cm, binding); err != nil {
			return err
		}
	}
	api, err := s.r.substrateNativeClient()
	if err != nil {
		return err
	}
	defer api.Close() //nolint:errcheck
	revisions := append([]substrateNativeTemplateRevision(nil), binding.Revisions...)
	if binding.Pending != nil {
		revisions = append(revisions, *binding.Pending)
	}
	retained := false
	for _, revision := range revisions {
		pinned, err := s.r.substrateNativeTemplateReferenced(ctx, binding.Atespace, revision, "")
		if err != nil {
			return err
		}
		if pinned {
			retained = true
			continue
		}
		ref := &ateapipb.ObjectRef{Atespace: binding.Atespace, Name: revision.Name}
		native, err := api.Control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: ref})
		if status.Code(err) == codes.NotFound {
			continue
		}
		if err != nil {
			return err
		}
		digest, err := nativeSubstrateTemplateDigest(native)
		if err != nil {
			return err
		}
		if digest != revision.Digest || (revision.UID != "" && native.GetMetadata().GetUid() != revision.UID) {
			return fmt.Errorf("native Substrate template was replaced; refusing foreign revision cleanup")
		}
		if _, err := api.Control.DeleteActorTemplate(ctx, &ateapipb.DeleteActorTemplateRequest{ActorTemplate: ref}); err != nil && status.Code(err) != codes.NotFound {
			return err
		}
	}
	if retained {
		return nil
	}
	return s.r.Delete(ctx, cm, deleteCurrentObjectPreconditions(cm)...)
}

// Cleanup reads the durable intent even if native creation never completed or
// a provider resource vanished. Delete checks each recorded native UID/content
// before collecting it, including revisions left by an interrupted create.
func (s *nativeSubstrateTemplateStore) GetForCleanup(ctx context.Context, atespace, name string) (*unstructured.Unstructured, error) {
	cm, binding, err := s.read(ctx, atespace, name)
	if err != nil || binding == nil {
		return nil, err
	}
	if binding.Retired {
		return nil, nil
	}
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(binding.Manifest, &object.Object); err != nil {
		return nil, fmt.Errorf("substrate cleanup manifest is unreadable")
	}
	if object.GetNamespace() != atespace || object.GetName() != name || object.GetLabels()[runtimePoolUIDLabel] != binding.OwnerUID {
		return nil, fmt.Errorf("substrate cleanup manifest does not match its owner")
	}
	object.SetUID(types.UID(binding.Current.UID))
	object.SetResourceVersion(string(cm.UID) + ":" + cm.ResourceVersion)
	return object, nil
}
