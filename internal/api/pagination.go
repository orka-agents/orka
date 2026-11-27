/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gofiber/fiber/v3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultLimit = 100

	MaxLimit = 500

	cacheContinueUnsupported = "continue-not-supported"
)

type Pagination struct {
	Limit    int64
	Continue string
}

func ParsePagination(limitStr, continueToken string) (*Pagination, error) {
	p := &Pagination{
		Limit:    DefaultLimit,
		Continue: NormalizeListContinue(continueToken),
	}

	if limitStr != "" {
		limit, err := strconv.ParseInt(limitStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid limit parameter: %w", err)
		}
		if limit < 1 {
			return nil, fmt.Errorf("limit must be at least 1")
		}
		if limit > MaxLimit {
			limit = MaxLimit
		}
		p.Limit = limit
	}

	return p, nil
}

func NormalizeListContinue(token string) string {
	if token == cacheContinueUnsupported {
		return ""
	}
	return token
}

func (h *Handlers) listPage(ctx context.Context, list client.ObjectList, opts *client.ListOptions, what string) error {
	if opts == nil {
		opts = &client.ListOptions{}
	}
	switch {
	case opts.Limit <= 0:
		if err := h.client.List(ctx, list, opts); err != nil {
			return listPageError(what, err)
		}
	case h.apiReader != nil:
		if err := h.apiReader.List(ctx, list, opts); err != nil {
			return listPageError(what, err)
		}
	default:
		if opts.Continue != "" {
			return fiber.NewError(fiber.StatusBadRequest, "continue is not supported: list pagination requires an uncached API reader")
		}
		unlimited := *opts
		unlimited.Limit = 0
		unlimited.Continue = ""
		if err := h.client.List(ctx, list, &unlimited); err != nil {
			return listPageError(what, err)
		}
		list.SetContinue("")
		list.SetRemainingItemCount(nil)
	}
	list.SetContinue(NormalizeListContinue(list.GetContinue()))
	list.SetRemainingItemCount(nil)
	return nil
}

func (h *Handlers) listNamespaced(c fiber.Ctx, list client.ObjectList, namespace, what string) error {
	pagination, err := ParsePagination(c.Query("limit", "100"), c.Query("continue", ""))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return h.listPage(c.Context(), list, &client.ListOptions{
		Namespace: namespace,
		Limit:     pagination.Limit,
		Continue:  pagination.Continue,
	}, what)
}

func fetchNamespaced[T client.Object](h *Handlers, c fiber.Ctx, obj T, name, what string) (T, error) {
	var zero T
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return zero, err
	}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return zero, fiber.NewError(fiber.StatusNotFound, what+" not found")
		}
		return zero, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get %s: %v", what, err))
	}
	return obj, nil
}

func listPageError(what string, err error) error {
	if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
		return fiber.NewError(fiber.StatusGone, fmt.Sprintf("continue token for %s has expired; restart the list from the first page", what))
	}
	if apierrors.IsBadRequest(err) {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid continue token for %s; restart the list from the first page", what))
	}
	return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list %s: %v", what, err))
}

const maxAuthorizedListPages = 200

func collectAuthorizedPages[T any](limit int64, start string, fetch func(continueToken string, pageLimit int64) ([]T, string, error)) ([]T, string, error) {
	if limit <= 0 {
		return fetch(start, limit)
	}
	items := []T{}
	continueToken := start
	for page := 1; ; page++ {
		pageItems, next, err := fetch(continueToken, limit-int64(len(items)))
		if err != nil {
			return nil, "", err
		}
		items = append(items, pageItems...)
		if next == "" || int64(len(items)) >= limit {
			return items, next, nil
		}
		if page >= maxAuthorizedListPages {
			return nil, "", fiber.NewError(
				fiber.StatusServiceUnavailable,
				fmt.Sprintf("authorized list scan exceeded the %d-page safety limit before a complete page could be assembled", maxAuthorizedListPages),
			)
		}
		continueToken = next
	}
}
