package api

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

type taskMessageStore struct {
	access   brokeredTaskDataAccess
	messages store.MessageStore
}

// NewTaskMessageStore binds brokered messaging to the controller-authenticated
// Task identity. Parent names alone never authorize coordination access.
// guard must come from the authenticated MCP prompt context.
func NewTaskMessageStore(reader client.Reader, messages store.MessageStore, taskKey client.ObjectKey, taskUID string, taskProvenanceProtected bool, guard func(context.Context, func(context.Context) error) error) tools.TaskMessageStore {
	return &taskMessageStore{
		access: brokeredTaskDataAccess{
			authorizer: internalCallerAuthorizer{k8sReader: reader, taskProvenanceProtected: taskProvenanceProtected},
			taskKey:    taskKey, taskUID: taskUID, guard: guard,
		},
		messages: messages,
	}
}

func (s *taskMessageStore) SendMessage(ctx context.Context, message *store.Message) error {
	if message == nil || message.Namespace != s.access.taskKey.Namespace || message.FromTask != s.access.taskKey.Name {
		return fiber.NewError(fiber.StatusForbidden, "authenticated message sender required")
	}
	return s.access.withData(ctx, s.messages, func(authCtx context.Context, task *corev1alpha1.Task) error {
		return s.access.authorizer.verifyTaskMessageScope(authCtx, task, message.ToTask, message.ParentTask)
	}, func(txCtx context.Context) error {
		return s.messages.SendMessage(txCtx, message)
	})
}

func (s *taskMessageStore) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	if namespace != s.access.taskKey.Namespace || taskName != s.access.taskKey.Name {
		return nil, fiber.NewError(fiber.StatusForbidden, "authenticated message recipient required")
	}
	var messages []store.Message
	err := s.access.withData(ctx, s.messages, func(authCtx context.Context, task *corev1alpha1.Task) error {
		_, err := s.access.authorizer.verifiedCoordinationParent(authCtx, task, parentTask)
		return err
	}, func(txCtx context.Context) error {
		var err error
		messages, err = s.messages.GetMessages(txCtx, namespace, taskName, parentTask, markRead)
		return err
	})
	if err != nil {
		return nil, err
	}
	return messages, nil
}
