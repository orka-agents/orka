package store

import (
	"context"
	"errors"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var ErrControllerEpochMutationContention = errors.New("controller epoch mutation contention")

type MemoryProposalStore interface {
	storagev1.MemoryProposalStore
	ApplyMemoryProposal(ctx context.Context, apply MemoryProposalApply) (*Memory, error)
}

type SecurityStore interface {
	storagev1.SecurityStore
	ApplyScanTaskIngestion(ctx context.Context, ingestion *ScanTaskIngestion, validate func(*ScanRun) error, apply func(SecurityStore, *ScanRun) error) (bool, error)
}

type MessageStore interface {
	SendMessage(ctx context.Context, msg *Message) error
	GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]Message, error)
	DeleteTaskMessages(ctx context.Context, namespace, taskName string) error
	DeleteParentMessages(ctx context.Context, namespace, parentTask string) error
}

func ReadMessages(
	ctx context.Context, provider storagev1.MessageStore, namespace, taskName, parentTask string, markRead bool,
) ([]Message, error) {
	if markRead {
		return provider.ConsumeMessages(ctx, ConsumeMessagesRequest{
			Namespace: namespace, TaskName: taskName, ParentTask: parentTask, Limit: MaxMessageBatch,
		})
	}
	return provider.ListUnreadMessages(ctx, namespace, taskName, parentTask, MaxMessageBatch)
}
