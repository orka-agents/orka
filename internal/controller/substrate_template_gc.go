package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// collectHistory removes one unused immutable revision at a time. The durable
// Collecting marker prevents a template writer from reusing the selected name
// between the reference check and the native delete, including after restart.
func (s *nativeSubstrateTemplateStore) collectHistory(ctx context.Context, cm *corev1.ConfigMap, binding *substrateTemplateBinding) error {
	if binding.Pending != nil {
		return nil
	}
	if binding.Collecting == nil {
		for _, revision := range binding.Revisions {
			if revision == binding.Current {
				continue
			}
			used, err := s.historyReferenced(ctx, binding, revision)
			if err != nil {
				return err
			}
			if used {
				continue
			}
			binding.Collecting = &revision
			return s.save(ctx, cm, binding)
		}
		return nil
	}
	revision := *binding.Collecting
	used, err := s.historyReferenced(ctx, binding, revision)
	if err != nil {
		return err
	}
	if used || revision == binding.Current {
		binding.Collecting = nil
		return s.save(ctx, cm, binding)
	}
	api, err := s.r.substrateNativeClient()
	if err != nil {
		return err
	}
	defer api.Close() //nolint:errcheck
	ref := &ateapipb.ObjectRef{Atespace: binding.Atespace, Name: revision.Name}
	native, err := api.Control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: ref})
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}
	if err == nil {
		digest, err := nativeSubstrateTemplateDigest(native)
		if err != nil || digest != revision.Digest || native.GetMetadata().GetUid() != revision.UID {
			return fmt.Errorf("historical Substrate template was replaced; refusing foreign cleanup")
		}
		if _, err := api.Control.DeleteActorTemplate(ctx, &ateapipb.DeleteActorTemplateRequest{ActorTemplate: ref}); err != nil && status.Code(err) != codes.NotFound {
			return err
		}
	}
	binding.Revisions = slices.DeleteFunc(binding.Revisions, func(item substrateNativeTemplateRevision) bool { return item == revision })
	binding.Collecting = nil
	return s.save(ctx, cm, binding)
}

func (s *nativeSubstrateTemplateStore) historyReferenced(ctx context.Context, binding *substrateTemplateBinding, revision substrateNativeTemplateRevision) (bool, error) {
	used, err := s.r.substrateNativeTemplateReferenced(ctx, binding.Atespace, revision, "")
	if err != nil || used {
		return used, err
	}
	list := &corev1.ConfigMapList{}
	if err := s.r.nativeSubstrateReader().List(ctx, list, client.InNamespace(s.r.ControllerNamespace), client.MatchingLabels{runtimePoolUIDLabel: binding.OwnerUID}); err != nil {
		return false, err
	}
	for _, cm := range list.Items {
		data := cm.Data[substrateNativeStateKey]
		if data == "" {
			continue
		}
		record := &substrateNativeState{}
		if json.Unmarshal([]byte(data), record) != nil || record.Schema != substrateNativeStateSchema || record.PoolUID != binding.OwnerUID || record.Atespace != binding.Atespace {
			return false, fmt.Errorf("unreadable native journal during template cleanup")
		}
		if a := record.Attempt; a != nil && (a.Template == revision || a.CreateTemplate == revision) {
			return true, nil
		}
		if record.Checkpoint != nil && record.Checkpoint.Template == revision || record.Pending != nil && record.Pending.TemplateUID == revision.UID {
			return true, nil
		}
		for _, tag := range record.RetiredTags {
			if tag.Template == revision {
				return true, nil
			}
		}
	}
	return false, nil
}
