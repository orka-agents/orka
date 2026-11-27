package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/orka-agents/orka/internal/store"
	"golang.org/x/sync/semaphore"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrSessionTurnStoreNotConfigured = errors.New("session turn persistence is not configured")

var ErrOutboxStoreNotConfigured = errors.New("outbox persistence is not configured")

type SQLitePersistence interface {
	store.SessionTurnPersistenceStore
	store.OutboxPersistenceStore
	store.SessionCleanupPersistenceStore
}

type Option func(*Store) error

func WithAPIReader(reader client.Reader) Option {
	return func(s *Store) error {
		if reader == nil {
			return store.ValidationErrorf("Kubernetes API reader must not be nil")
		}
		s.reader = reader
		return nil
	}
}

func WithWatchNamespace(namespace string) Option {
	return func(s *Store) error {
		namespace = strings.TrimSpace(namespace)
		if err := validateKubernetesNamespace(namespace); err != nil {
			return err
		}
		s.watchNamespace = namespace
		return nil
	}
}

func WithSessionRuntimeCleanup(cleanup store.SessionRuntimeCleanupFunc) Option {
	return func(s *Store) error {
		if cleanup == nil {
			return store.ValidationErrorf("Session runtime cleanup must not be nil")
		}
		s.sessionRuntimeCleanup = cleanup
		return nil
	}
}

type Store struct {
	client                client.Client
	reader                client.Reader
	controlNamespace      string
	watchNamespace        string
	sessionTurns          store.SessionTurnPersistenceStore
	outbox                store.OutboxPersistenceStore
	sessionCleanup        store.SessionCleanupPersistenceStore
	sessionRuntimeCleanup store.SessionRuntimeCleanupFunc
	sessionCleanupLocks   sessionCleanupLockSet
	epochMutations        *semaphore.Weighted
}

func NewComposite(kubeClient client.Client, controlNamespace string, persistence SQLitePersistence, options ...Option) (*Store, error) {
	if persistence == nil {
		return nil, store.ValidationErrorf("SQLite persistence store is required")
	}
	result, err := New(kubeClient, controlNamespace, options...)
	if err != nil {
		return nil, err
	}
	result.sessionTurns = persistence
	result.outbox = persistence
	result.sessionCleanup = persistence
	return result, nil
}

func New(kubeClient client.Client, controlNamespace string, options ...Option) (*Store, error) {
	if kubeClient == nil {
		return nil, store.ValidationErrorf("Kubernetes client is required")
	}
	controlNamespace = strings.TrimSpace(controlNamespace)
	if err := validateKubernetesNamespace(controlNamespace); err != nil {
		return nil, err
	}
	result := &Store{
		client: kubeClient, controlNamespace: controlNamespace,
		epochMutations: semaphore.NewWeighted(1),
	}
	for _, option := range options {
		if option == nil {
			return nil, store.ValidationErrorf("Kubernetes control-store option must not be nil")
		}
		if err := option(result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) GetSessionTurn(ctx context.Context, id string) (*store.SessionTurn, error) {
	if s.sessionTurns == nil {
		return nil, ErrSessionTurnStoreNotConfigured
	}
	return s.sessionTurns.GetSessionTurn(ctx, id)
}

func (s *Store) readClient() client.Reader {
	if s != nil && s.reader != nil {
		return s.reader
	}
	if s == nil {
		return nil
	}
	return s.client
}

func (s *Store) namespacedListOptions(options ...client.ListOption) []client.ListOption {
	if s == nil || s.watchNamespace == "" {
		return options
	}
	return append(options, client.InNamespace(s.watchNamespace))
}

func (s *Store) requireClient() error {
	if s == nil || s.client == nil {
		return fmt.Errorf("kubernetes control store is not initialized")
	}
	return nil
}

var (
	_ store.ControllerEpochStore        = (*Store)(nil)
	_ store.PromptAttemptStore          = (*Store)(nil)
	_ store.SessionControlStore         = (*Store)(nil)
	_ store.SessionCleanupStore         = (*Store)(nil)
	_ store.SessionCleanupRecoveryStore = (*Store)(nil)
	_ store.BranchClaimStore            = (*Store)(nil)
	_ store.PublicationStore            = (*Store)(nil)
	_ store.ExternalEffectStore         = (*Store)(nil)
	_ store.OutboxProjectionStore       = (*Store)(nil)
	_ store.DurableControlStore         = (*Store)(nil)
)
