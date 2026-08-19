/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type statusUpdateCountingClient struct {
	client.Client
	statusUpdateCount  int
	conflictsRemaining int
}

func (c *statusUpdateCountingClient) Status() client.SubResourceWriter {
	return &statusUpdateCountingWriter{
		SubResourceWriter:  c.Client.Status(),
		statusUpdateCount:  &c.statusUpdateCount,
		conflictsRemaining: &c.conflictsRemaining,
	}
}

type statusUpdateCountingWriter struct {
	client.SubResourceWriter
	statusUpdateCount  *int
	conflictsRemaining *int
}

func (w *statusUpdateCountingWriter) Update(
	ctx context.Context,
	obj client.Object,
	opts ...client.SubResourceUpdateOption,
) error {
	(*w.statusUpdateCount)++
	if *w.conflictsRemaining > 0 {
		(*w.conflictsRemaining)--
		return apierrors.NewConflict(
			schema.GroupResource{Group: "core.orka.ai", Resource: "status"},
			obj.GetName(),
			errors.New("injected status conflict"),
		)
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}
