/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// An Agent Host Protocol turn belongs to the person who sent it. People
// signing in to AHP share a namespace, so the Tasks AHP turns create, and the
// Orka Sessions those Tasks share, are shown through the API only to that
// person: anyone else gets 404, as for something that does not exist.
// ServiceAccounts, which operators use, keep the access RBAC grants them.

// ahpNamePrefix starts the name of every Orka Session AHP creates, and so of
// every Task an AHP turn creates.
const ahpNamePrefix = "ahp-"

// ahpSessionName returns a session name as the controller reads it, without
// surrounding space, and whether it is an AHP session's. Checks read names
// so: one with a space would otherwise pass for a different session.
func ahpSessionName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	return name, strings.HasPrefix(name, ahpNamePrefix)
}

// ahpPerson reports a caller signed in as a person rather than a workload.
func ahpPerson(ui *UserInfo) bool {
	return ui != nil && (ui.AuthType == AuthTypeOIDC || ui.AuthType == AuthTypeContextToken)
}

// ahpOwns reports whether the person sent the turn that created the Task.
func ahpOwns(ui *UserInfo, task *corev1alpha1.Task) bool {
	requester := task.Spec.RequestedBy
	return requester != nil && requester.Issuer == ui.Issuer && requester.Subject == ui.Subject
}

// ahpTurnTask reports a Task an AHP turn created: labelled so, or, as Tasks
// from before the label were, named for an AHP session it continues for a
// recorded requester. People cannot create such a Task: the API refuses a
// Task that continues an AHP session not theirs.
func ahpTurnTask(task *corev1alpha1.Task) bool {
	if task.Labels[labels.LabelAHPTurn] == labels.LabelAHPTurnValue {
		return true
	}
	ref := task.Spec.SessionRef
	if ref == nil {
		return false
	}
	_, ahp := ahpSessionName(ref.Name)
	return strings.HasPrefix(task.Name, ahpNamePrefix) && ahp && task.Spec.RequestedBy != nil
}

// ahpTaskHidden reports an AHP turn's Task the caller may not see.
func ahpTaskHidden(ui *UserInfo, task *corev1alpha1.Task) bool {
	return ahpPerson(ui) && ahpTurnTask(task) && !ahpOwns(ui, task)
}

// ahpTaskPrivate guards a /tasks/:id route. A turn's Task that is gone can no
// longer show whose it was, so the records kept under its name stay hidden from
// people.
func (h *Handlers) ahpTaskPrivate(handler fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		if err := h.ahpTaskCheck(c); err != nil {
			return err
		}
		return handler(c)
	}
}

// ahpSessionPrivate guards a /sessions/:id route.
func (h *Handlers) ahpSessionPrivate(handler fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		if err := h.ahpSessionCheck(c); err != nil {
			return err
		}
		return handler(c)
	}
}

func (h *Handlers) ahpTaskCheck(c fiber.Ctx) error {
	ui := GetUserInfo(c)
	if !ahpPerson(ui) {
		return nil
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		// The handler reports the namespace the caller may not use.
		return nil
	}
	name := c.Params("id")
	task := &corev1alpha1.Task{}
	switch err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: name}, task); {
	case err == nil:
		if ahpTaskHidden(ui, task) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
	case apierrors.IsNotFound(err):
		if strings.HasPrefix(name, ahpNamePrefix) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
	default:
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	return nil
}

func (h *Handlers) ahpSessionCheck(c fiber.Ctx) error {
	ui := GetUserInfo(c)
	if _, ahp := ahpSessionName(c.Params("id")); !ahpPerson(ui) || !ahp {
		return nil
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return nil
	}
	hidden, err := ahpSessionHiddenIn(c.Context(), h.client, ui, namespace, c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to look up the session's owner: %v", err))
	}
	if hidden {
		return fiber.NewError(fiber.StatusNotFound, "session not found")
	}
	return nil
}

// ahpSessionOwnersIn returns, for each of the named sessions that is an AHP
// session some turn's Task continues, one such Task, which carries the turn's
// requester. It reads labelled turns first, and scans the namespace's other
// Tasks only for sessions they leave unowned, which turns from before the
// label may own.
func ahpSessionOwnersIn(ctx context.Context, r client.Reader, namespace string, sessions []string) (map[string]*corev1alpha1.Task, error) {
	owners := map[string]*corev1alpha1.Task{}
	wanted := map[string]bool{}
	for _, session := range sessions {
		if name, ahp := ahpSessionName(session); ahp {
			wanted[name] = true
		}
	}
	if len(wanted) == 0 {
		return owners, nil
	}
	collect := func(tasks *corev1alpha1.TaskList) {
		for i := range tasks.Items {
			task := &tasks.Items[i]
			if ref := task.Spec.SessionRef; ref != nil && ahpTurnTask(task) {
				if name, _ := ahpSessionName(ref.Name); wanted[name] && owners[name] == nil {
					owners[name] = task
				}
			}
		}
	}
	var labelled corev1alpha1.TaskList
	if err := r.List(ctx, &labelled, client.InNamespace(namespace), client.MatchingLabels{labels.LabelAHPTurn: labels.LabelAHPTurnValue}); err != nil {
		return nil, err
	}
	collect(&labelled)
	if len(owners) < len(wanted) {
		var all corev1alpha1.TaskList
		if err := r.List(ctx, &all, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		collect(&all)
	}
	return owners, nil
}

// ahpSessionHiddenIn reports an AHP Orka Session the caller may not see: one
// another person's turns use, or one named like AHP's that no turn's Task
// names any more, which nothing can show to be the caller's.
func ahpSessionHiddenIn(ctx context.Context, r client.Reader, ui *UserInfo, namespace, session string) (bool, error) {
	session, ahp := ahpSessionName(session)
	if !ahpPerson(ui) || !ahp {
		return false, nil
	}
	owners, err := ahpSessionOwnersIn(ctx, r, namespace, []string{session})
	if err != nil {
		return false, err
	}
	return ahpSessionHiddenBy(ui, session, owners), nil
}

func ahpSessionHiddenBy(ui *UserInfo, session string, owners map[string]*corev1alpha1.Task) bool {
	session, ahp := ahpSessionName(session)
	if !ahpPerson(ui) || !ahp {
		return false
	}
	owner := owners[session]
	return owner == nil || !ahpOwns(ui, owner)
}
