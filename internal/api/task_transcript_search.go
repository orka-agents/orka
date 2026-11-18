package api

import (
	"context"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

type taskTranscriptSearcher struct {
	access        brokeredTaskDataAccess
	sessions      store.SessionStore
	gatewayEvents store.GatewayEventStore
}

// NewTaskTranscriptSearcher binds brokered search to an authenticated Task.
// The identity must come from the controller MCP broker, never tool arguments.
// Live Task and session authorization is repeated for each search and cleanup retry.
// guard must come from the authenticated MCP prompt context.
func NewTaskTranscriptSearcher(reader client.Reader, sessions store.SessionStore, gatewayEvents store.GatewayEventStore, taskKey client.ObjectKey, taskUID string, taskProvenanceProtected bool, guard func(context.Context, func(context.Context) error) error) tools.TranscriptSearcher {
	return &taskTranscriptSearcher{
		access: brokeredTaskDataAccess{
			authorizer: internalCallerAuthorizer{k8sReader: reader, taskProvenanceProtected: taskProvenanceProtected},
			taskKey:    taskKey, taskUID: taskUID, guard: guard,
		},
		sessions: sessions, gatewayEvents: gatewayEvents,
	}
}

func (s *taskTranscriptSearcher) SearchTranscript(ctx context.Context, filter store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error) {
	if filter.Namespace != s.access.taskKey.Namespace {
		return nil, fiber.NewError(fiber.StatusForbidden, "authenticated task identity required")
	}
	filter.SessionName = strings.TrimSpace(filter.SessionName)
	filter.ExcludeSessionName = strings.TrimSpace(filter.ExcludeSessionName)
	// Only controller-resolved references may set these authorization filters.
	filter.SessionNames, filter.HistoryBounds = nil, nil
	var results []store.TranscriptSearchResult
	var task *corev1alpha1.Task
	var allowed map[string][]corev1alpha1.SessionReference
	err := s.access.withData(ctx, s.sessions, func(authCtx context.Context, caller *corev1alpha1.Task) error {
		task = caller
		var err error
		allowed, err = s.access.authorizer.coordinationTreeSessionReferences(authCtx, task)
		return err
	}, func(txCtx context.Context) error {
		if err := authorizeTaskTranscriptSearch(txCtx, s.sessions, s.gatewayEvents, task, filter.SessionName, allowed); err != nil {
			return err
		}
		var err error
		results, err = searchAuthorizedTranscriptResults(txCtx, s.sessions, filter, allowed)
		return err
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func authorizeTaskTranscriptSearch(ctx context.Context, sessions store.SessionStore, gatewayEvents store.GatewayEventStore, task *corev1alpha1.Task, sessionName string, allowed map[string][]corev1alpha1.SessionReference) error {
	if gatewayEvents != nil {
		_, err := gatewayEvents.GetGatewayEventForTask(ctx, task.Namespace, task.Name, string(task.UID))
		switch {
		case err == nil:
			// Gateway turns require an event-specific cutoff rather than the
			// Task session references used for ordinary transcript search.
			return fiber.NewError(fiber.StatusForbidden, "gateway session transcript search is unavailable")
		case errors.Is(err, store.ErrNotFound):
		default:
			return fiber.NewError(fiber.StatusInternalServerError, "failed to load gateway transcript ownership")
		}
	}
	if sessionName == "" {
		return nil
	}
	if _, ok := allowed[sessionName]; !ok {
		return fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this session")
	}
	sessionType, err := transcriptSessionType(ctx, sessions, task.Namespace, sessionName)
	switch {
	case errors.Is(err, store.ErrNotFound), sessionType == store.SessionTypeGateway:
		return fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this session")
	case err != nil:
		return fiber.NewError(fiber.StatusInternalServerError, "failed to load session transcript policy")
	default:
		return nil
	}
}
