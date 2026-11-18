package store

import (
	"context"
	"errors"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// ErrControllerEpochMutationContention marks an exhausted mutation-lock
// acquisition retry window. Errors carrying it also wrap ErrConflict;
// definitive epoch authority failures do not carry this marker.
var ErrControllerEpochMutationContention = errors.New("controller epoch mutation contention")

// MemoryProposalStore adds Orka's proposal application to the provider
// contract's proposal family. The storage client implements
// ApplyMemoryProposal with the contract's resolve and conditional commit, and
// runs the authorization callback in Orka between them.
type MemoryProposalStore interface {
	storagev1.MemoryProposalStore
	ApplyMemoryProposal(ctx context.Context, apply MemoryProposalApply) (*Memory, error)
}

// SecurityStore handles repository security scanning persistence. It adds
// Orka's scan ingestion to the provider contract; the storage client
// implements it with the contract's conditional ingestion command.
type SecurityStore interface {
	storagev1.SecurityStore
	// ApplyScanTaskIngestion atomically applies results, updates the current
	// run, and records the receipt. It skips already ingested Tasks and
	// terminal or cancelled runs. If supplied, validate runs before
	// application and again just before the commit. Neither function runs
	// inside a store transaction. apply must use the supplied store, which
	// records its reads and writes for the commit, and must not perform
	// external mutations. Its writes take effect only when the commit
	// succeeds, and their errors may surface at a later read or at the
	// commit. When another writer changed a record apply read, the
	// ingestion returns ErrPreconditionFailed and changes nothing; the
	// caller repeats it from fresh state rather than re-running apply on
	// state it mutated.
	ApplyScanTaskIngestion(ctx context.Context, ingestion *ScanTaskIngestion, validate func(*ScanRun) error, apply func(SecurityStore, *ScanRun) error) (bool, error)
}

// MessageStore is the inter-agent message store Orka's consumers use. Its
// GetMessages reads or consumes a Task's inbox in one call; the storage client
// maps it onto the provider contract's ListUnreadMessages and ConsumeMessages.
type MessageStore interface {
	// SendMessage stores a new message. Use toTask="*" for broadcast.
	SendMessage(ctx context.Context, msg *Message) error
	// GetMessages returns up to MaxMessageBatch of the oldest unread messages
	// in a Task's inbox, including broadcasts to its siblings, and marks
	// exactly those read when markRead is set. A longer backlog is read over
	// several calls.
	GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]Message, error)
	// DeleteTaskMessages deletes all messages involving a task (sent or received).
	DeleteTaskMessages(ctx context.Context, namespace, taskName string) error
	// DeleteParentMessages deletes all messages for children of a parent task.
	DeleteParentMessages(ctx context.Context, namespace, parentTask string) error
}

// ReadMessages implements MessageStore.GetMessages over a provider: it
// consumes the oldest batch of the inbox when markRead is set and lists it
// otherwise.
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
