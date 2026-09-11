package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

type taskTranscriptSearcher struct {
	authorizer    internalCallerAuthorizer
	sessions      store.SessionStore
	gatewayEvents store.GatewayEventStore
	taskKey       client.ObjectKey
	taskUID       string
}

// NewTaskTranscriptSearcher binds brokered search to an authenticated Task.
// The identity must come from the controller MCP broker, never tool arguments.
// Live Task and session authorization is repeated for each search and cleanup retry.
func NewTaskTranscriptSearcher(reader client.Reader, sessions store.SessionStore, gatewayEvents store.GatewayEventStore, taskKey client.ObjectKey, taskUID string, taskProvenanceProtected bool) tools.TranscriptSearcher {
	return &taskTranscriptSearcher{
		authorizer: internalCallerAuthorizer{k8sReader: reader, taskProvenanceProtected: taskProvenanceProtected},
		sessions:   sessions, gatewayEvents: gatewayEvents, taskKey: taskKey, taskUID: taskUID,
	}
}

func (s *taskTranscriptSearcher) SearchTranscript(ctx context.Context, filter store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error) {
	if s.authorizer.k8sReader == nil || s.taskKey.Namespace == "" || s.taskKey.Name == "" || s.taskUID == "" || filter.Namespace != s.taskKey.Namespace {
		return nil, fiber.NewError(fiber.StatusForbidden, "authenticated task identity required")
	}
	transactions, ok := s.sessions.(store.TaskDataTransactionStore)
	if !ok {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "task data transactions unavailable")
	}
	filter.SessionName = strings.TrimSpace(filter.SessionName)
	filter.ExcludeSessionName = strings.TrimSpace(filter.ExcludeSessionName)
	// Only controller-resolved references may set these authorization filters.
	filter.SessionNames, filter.HistoryBounds = nil, nil
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var results []store.TranscriptSearchResult
	var err error
	for range 3 {
		var task *corev1alpha1.Task
		var allowed map[string][]corev1alpha1.SessionReference
		// Search can read sessions belonging to other Tasks. Fence namespace
		// cleanup, and keep every Kubernetes read outside the SQLite transaction.
		err = transactions.WithAuthorizedTaskDataTransaction(ctx, s.taskKey.Namespace, "", func(authCtx context.Context) error {
			task, allowed, err = s.authorize(authCtx)
			return err
		}, func(txCtx context.Context) error {
			if err := authorizeTaskTranscriptSearch(txCtx, s.sessions, s.gatewayEvents, task, filter.SessionName, allowed); err != nil {
				return err
			}
			results, err = searchAuthorizedTranscriptResults(txCtx, s.sessions, filter, allowed)
			return err
		})
		if !errors.Is(err, store.ErrTaskDataCleanupChanged) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *taskTranscriptSearcher) authorize(ctx context.Context) (*corev1alpha1.Task, map[string][]corev1alpha1.SessionReference, error) {
	task := &corev1alpha1.Task{}
	if err := s.authorizer.k8sReader.Get(ctx, s.taskKey, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, fiber.NewError(fiber.StatusForbidden, "caller task is unavailable")
		}
		return nil, nil, fmt.Errorf("load caller task: %w", err)
	}
	if string(task.UID) != s.taskUID || !activeInternalWorkerTask(task) {
		return nil, nil, fiber.NewError(fiber.StatusForbidden, "caller task identity is no longer active")
	}
	allowed, err := s.authorizer.coordinationTreeSessionReferences(ctx, task)
	return task, allowed, err
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
