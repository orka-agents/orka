package applicationexec

import (
	"context"
	"errors"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errDataFinalizationBlocked = errors.New("owned application data finalization did not complete before its cleanup deadline")

type sandboxDataObject struct {
	index  int
	object client.Object
}

func (b *Backend) finalizeSandboxData(ctx context.Context, r *run) (bool, error) {
	if r.state.DataFinalizationDeadline == nil {
		deadline := time.Now().UTC().Add(min(30*time.Second, b.config.CleanupTimeout/2))
		r.state.DataFinalizationDeadline = &deadline
		if err := b.save(ctx, r); err != nil {
			return false, err
		}
	}
	data, err := b.sandboxData(ctx, r)
	if err != nil || len(data) == 0 {
		return err == nil, err
	}
	finalizersFirst := false
	for _, current := range data {
		finalizersFirst = finalizersFirst || len(current.object.GetFinalizers()) != 0
	}
	for _, current := range data {
		if finalizersFirst && len(current.object.GetFinalizers()) == 0 {
			continue
		}
		if _, err := b.deleteEffect(ctx, r, current.index); err != nil {
			return false, err
		}
	}
	if time.Now().Before(*r.state.DataFinalizationDeadline) {
		return false, nil
	}
	remaining, err := b.sandboxData(ctx, r)
	if err != nil || len(remaining) == 0 {
		return err == nil, err
	}
	// Finalizer ownership cannot be inferred from a client-chosen name. Never
	// strip operator, storage, or namespace finalizers to manufacture settlement.
	if !r.state.DataFinalizationBlocked {
		r.state.DataFinalizationBlocked = true
		if r.state.Failed == "" {
			r.state.Failed = "application-data-finalization-blocked"
		}
		if err := b.save(ctx, r); err != nil {
			return false, err
		}
	}
	return false, errDataFinalizationBlocked
}

func (b *Backend) sandboxData(ctx context.Context, r *run) ([]sandboxDataObject, error) {
	plan := r.state.Request.Checks.Application
	if plan == nil {
		return nil, nil
	}
	definitions, err := admittedResources(plan, r.state.Request.Checks.Checks, r.state.Request.ApplicationPolicy)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool)
	for _, definition := range definitions {
		if !sandboxDataKind(definition.GetAPIVersion(), definition.GetKind(), r.state.Request.ApplicationPolicy) {
			continue
		}
		for _, side := range pv.ActionSides(r.state.Request.Action) {
			allowed[dataIdentity(definition.GetAPIVersion(), definition.GetKind(), namespaceName(r, side), definition.GetName())] = true
		}
	}
	var data []sandboxDataObject
	for index, entry := range r.state.Effects {
		if !allowed[dataIdentity(entry.APIVersion, entry.Kind, entry.Namespace, entry.Name)] ||
			entry.Deleted || entry.Rejected || entry.GeneratedBy != "" {
			continue
		}
		if entry.UID == "" {
			return nil, ErrUncertain
		}
		if _, err := b.cleanupNamespace(ctx, r, entry.Namespace); err != nil {
			return nil, err
		}
		object, err := b.objectFor(entry)
		if err != nil {
			return nil, err
		}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, safeError(contract.ErrUnavailable, err)
		}
		if object.GetUID() != entry.UID {
			return nil, contract.ErrConflict
		}
		data = append(data, sandboxDataObject{index: index, object: object})
	}
	return data, nil
}

func dataIdentity(apiVersion, kind, namespace, name string) string {
	return apiVersion + "\x00" + kind + "\x00" + namespace + "\x00" + name
}

func sandboxDataKind(apiVersion, kind string, policy *pv.ApplicationPolicy) bool {
	switch apiVersion + "/" + kind {
	case "v1/ConfigMap", "v1/Service", "networking.k8s.io/v1/Ingress":
		return true
	}
	if policy != nil {
		for _, rule := range policy.Resources {
			if rule.APIVersion == apiVersion && rule.Kind == kind && rule.Namespaced && rule.CRD != nil {
				return true
			}
		}
	}
	return false
}
