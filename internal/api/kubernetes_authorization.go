/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const taskResourceGroup = "core.orka.ai"

func (h *Handlers) authorizeKubernetesTaskAccess(c fiber.Ctx, verb, namespace, name string) error {
	ui := GetUserInfo(c)
	if ui == nil || h.client == nil {
		return nil
	}
	if ui.AuthType != AuthTypeTokenReview && ui.AuthType != AuthTypeOIDC {
		return nil
	}

	allowed, reason, err := h.subjectAccessReview(c.Context(), ui, authorizationv1.ResourceAttributes{
		Group:     taskResourceGroup,
		Resource:  "tasks",
		Verb:      verb,
		Namespace: namespace,
		Name:      name,
	})
	if err != nil {
		log.Error(err, "kubernetes authorization check failed", "username", ui.Username, "verb", verb, "namespace", namespace, "task", name)
		return fiber.NewError(fiber.StatusForbidden, "task access denied")
	}
	if !allowed {
		log.Info("kubernetes authorization denied task access", "username", ui.Username, "verb", verb, "namespace", namespace, "task", name, "reason", reason)
		return fiber.NewError(fiber.StatusForbidden, "task access denied")
	}

	return nil
}

func (h *Handlers) subjectAccessReview(ctx context.Context, ui *UserInfo, attrs authorizationv1.ResourceAttributes) (bool, string, error) {
	sar := &authorizationv1.SubjectAccessReview{
		ObjectMeta: metav1.ObjectMeta{Name: "orka-api-authorization"},
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               ui.Username,
			Groups:             ui.Groups,
			Extra:              authorizationExtra(ui.Extra),
			ResourceAttributes: &attrs,
		},
	}
	if err := h.client.Create(ctx, sar); err != nil {
		return false, "", fmt.Errorf("create SubjectAccessReview: %w", err)
	}
	return sar.Status.Allowed, sar.Status.Reason, nil
}

func authorizationExtra(extra map[string]authenticationv1.ExtraValue) map[string]authorizationv1.ExtraValue {
	if len(extra) == 0 {
		return nil
	}
	copied := make(map[string]authorizationv1.ExtraValue, len(extra))
	for k, v := range extra {
		values := make(authorizationv1.ExtraValue, len(v))
		for i, value := range v {
			values[i] = value
		}
		copied[k] = values
	}
	return copied
}
