package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type faultingSoulSessionStore struct {
	*sqlite.Store
	pinError          error
	commitBeforeError bool
	appendError       error
	pinCalls          int
}

func (s *faultingSoulSessionStore) EnsureSessionSoulWithLock(ctx context.Context, namespace, name, owner, uid, digest string) error {
	s.pinCalls++
	if s.pinError != nil {
		err := s.pinError
		s.pinError = nil
		if s.commitBeforeError {
			if commitErr := s.Store.EnsureSessionSoulWithLock(ctx, namespace, name, owner, uid, digest); commitErr != nil {
				return commitErr
			}
		}
		return err
	}
	return s.Store.EnsureSessionSoulWithLock(ctx, namespace, name, owner, uid, digest)
}

func (s *faultingSoulSessionStore) AppendMessagesWithLock(ctx context.Context, namespace, name, owner, uid string, messages []store.SessionMessage) error {
	if s.appendError != nil {
		return s.appendError
	}
	return s.Store.AppendMessagesWithLock(ctx, namespace, name, owner, uid, messages)
}

func newAISoulSessionPinFixture(t *testing.T, prompt string) (*TaskReconciler, *corev1alpha1.Task, *corev1alpha1.Agent, *faultingSoulSessionStore) {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "default", UID: "first-uid", Generation: 1},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: prompt,
			SessionRef: &corev1alpha1.SessionReference{Name: "session", Create: true, Append: true}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec:       corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "original persona"}},
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	s := &faultingSoulSessionStore{Store: r.SessionManager.store.(*sqlite.Store)}
	r.SessionManager = NewSessionManager(s)
	if err := r.SessionManager.AcquireLock(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return r, task, agent, s
}

func TestAISoulSessionPinRetriesBeforeExecution(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "failed before commit"
		if committed {
			name = "commit acknowledgement lost"
		}
		t.Run(name, func(t *testing.T) {
			r, task, agent, s := newAISoulSessionPinFixture(t, "prompt")
			ctx := context.Background()
			outage := errors.New("temporary Session write failure")
			s.pinError, s.commitBeforeError = outage, committed
			if _, err := r.prepareAISoul(ctx, task, agent); !errors.Is(err, outage) || isPermanentAISoulConfigurationError(err) {
				t.Fatalf("pin failure did not remain retryable: %v", err)
			}
			if task.Status.SoulBinding == nil || task.Status.Attempts != 0 || task.Status.Phase != corev1alpha1.TaskPhasePending {
				t.Fatal("pin failure lost the Task binding or consumed its attempt")
			}
			for range 2 {
				if _, err := r.prepareAISoul(ctx, task, agent); err != nil {
					t.Fatal(err)
				}
			}
			state, err := s.ReadSessionSoul(ctx, task.Namespace, "session", task.Name, string(task.UID))
			if err != nil || !state.Established || state.Digest != agentcontext.SessionDigest(task.Status.SoulBinding) || state.MessageCount != 0 {
				t.Fatal("retry failed to establish stable digest-only identity")
			}
		})
	}
}

func TestAISoulSessionPinSurvivesFailedOrEmptyFinalTranscript(t *testing.T) {
	for _, prompt := range []string{"first prompt", ""} {
		t.Run(prompt, func(t *testing.T) {
			r, task, agent, s := newAISoulSessionPinFixture(t, prompt)
			ctx := context.Background()
			if _, err := r.prepareAISoul(ctx, task, agent); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
				t.Fatal(err)
			}
			task.Status.Attempts = 1
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			if err := r.Status().Update(ctx, task); err != nil {
				t.Fatal(err)
			}
			s.appendError = errors.New("transient transcript outage")
			if _, err := r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "done"); err != nil {
				t.Fatal(err)
			}
			next := task.DeepCopy()
			next.Name, next.UID, next.ResourceVersion = "next", "next-uid", ""
			next.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}
			if err := r.Create(ctx, next); err != nil {
				t.Fatal(err)
			}
			if err := r.SessionManager.AcquireLock(ctx, next); err != nil {
				t.Fatal(err)
			}
			changed := agent.DeepCopy()
			changed.Generation++
			changed.Spec.Soul.Inline = "different persona"
			if _, err := r.prepareAISoul(ctx, next, changed); err == nil || !isPermanentAISoulConfigurationError(err) {
				t.Fatalf("lost transcript allowed Session persona drift: %v", err)
			}
			if _, err := r.prepareAISoul(ctx, next, agent); err != nil {
				t.Fatalf("same persona continuation failed: %v", err)
			}
			transcript, err := s.LoadTranscript(ctx, task.Namespace, "session", 50)
			if err != nil || len(transcript) != 0 {
				t.Fatal("control-only revision pin became visible conversation content")
			}
		})
	}
}

type soulPinGatewayLookup struct {
	store.GatewayEventStore
	err error
}

func (s soulPinGatewayLookup) GetGatewayEventForTask(context.Context, string, string, string) (*store.GatewayEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &store.GatewayEvent{ID: "gateway-event", SessionName: "session"}, nil
}

func TestAISoulSessionPinDefersToGatewayOwnership(t *testing.T) {
	r, task, agent, s := newAISoulSessionPinFixture(t, "prompt")
	ctx := context.Background()
	prepared, err := resolveAISoul(ctx, r.Client, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	s.pinError = errors.New("ordinary pin must not run")
	r.SessionManager.SetGatewayEventStore(soulPinGatewayLookup{})
	if err := r.pinAISoulSession(ctx, task, prepared); err != nil || s.pinError == nil {
		t.Fatal("ordinary pinning intercepted a Gateway-owned Task")
	}
	outage := errors.New("temporary Gateway lookup failure")
	r.SessionManager.SetGatewayEventStore(soulPinGatewayLookup{err: outage})
	if err := r.pinAISoulSession(ctx, task, prepared); !errors.Is(err, outage) || s.pinError == nil {
		t.Fatal("Gateway lookup failure fell through to ordinary pinning")
	}
}

type interruptedSoulStatusClient struct {
	client.Client
	interrupt error
	commit    bool
}

func (c *interruptedSoulStatusClient) Status() client.SubResourceWriter {
	return interruptedSoulStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type interruptedSoulStatusWriter struct {
	client.SubResourceWriter
	owner *interruptedSoulStatusClient
}

func (s interruptedSoulStatusWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	if s.owner.interrupt != nil {
		err := s.owner.interrupt
		s.owner.interrupt = nil
		if s.owner.commit {
			if commitErr := s.SubResourceWriter.Patch(ctx, object, patch, options...); commitErr != nil {
				return commitErr
			}
		}
		return fmt.Errorf("request interrupted: %w", err)
	}
	return s.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func TestAISoulInterruptedStatusWriteCannotPinSession(t *testing.T) {
	for _, interruption := range []error{context.DeadlineExceeded, context.Canceled} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/committed=%v", interruption, committed), func(t *testing.T) {
				r, task, agent, s := newAISoulSessionPinFixture(t, "prompt")
				ctx := context.Background()
				baseClient := r.Client
				r.Client = &interruptedSoulStatusClient{Client: baseClient, interrupt: interruption, commit: committed}
				if _, err := r.prepareAISoul(ctx, task, agent); !errors.Is(err, interruption) || isPermanentAISoulConfigurationError(err) {
					t.Fatalf("interrupted status write appeared successful/permanent: %v", err)
				}
				if s.pinCalls != 0 {
					t.Fatal("Session pin was attempted before a confirmed Task binding")
				}
				if err := baseClient.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
					t.Fatal(err)
				}
				if task.Status.Phase != corev1alpha1.TaskPhasePending || task.Status.Attempts != 0 {
					t.Fatal("interrupted preparation consumed an execution attempt")
				}
				if _, err := r.prepareAISoul(ctx, task, agent); err != nil {
					t.Fatal(err)
				}
				if s.pinCalls != 1 {
					t.Fatal("fresh retry did not establish the Session pin exactly once")
				}
			})
		}
	}
}
