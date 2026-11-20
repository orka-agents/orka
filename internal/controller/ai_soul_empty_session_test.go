package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestAINoSoulSessionRevisionSurvivesEmptyTurn(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		append      bool
		appendFails bool
	}{
		{name: "append disabled"},
		{name: "empty appended turn", append: true},
		{name: "transcript write failure", append: true, appendFails: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			r, task, agent, s := newAISoulSessionPinFixture(t, "")
			agent.Spec.Soul = nil
			task.Spec.SessionRef.Append = scenario.append
			if scenario.appendFails {
				task.Spec.Prompt = "first prompt"
				s.appendError = errors.New("temporary transcript failure")
			}
			if err := r.Update(ctx, task); err != nil {
				t.Fatal(err)
			}
			if _, err := r.createTaskJob(ctx, task, agent, nil); err != nil {
				t.Fatal(err)
			}
			if task.Status.Phase != corev1alpha1.TaskPhaseRunning || task.Status.SoulBinding != nil {
				t.Fatal("no-soul Task did not start with legacy prompt semantics")
			}
			state, err := s.ReadSessionSoul(ctx, task.Namespace, "session", task.Name, string(task.UID))
			if err != nil || !state.Established || state.Digest != "" || state.MessageCount != 0 {
				t.Fatalf("no-soul revision was not pinned before execution: %+v, %v", state, err)
			}
			if _, err := r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, ""); err != nil {
				t.Fatal(err)
			}
			session, err := s.GetSession(ctx, task.Namespace, "session")
			if err != nil || session.ActiveTask != "" {
				t.Fatal("first Task did not release its Session lock")
			}
			next := task.DeepCopy()
			next.Name, next.UID, next.ResourceVersion = "next", "next-uid", ""
			next.Spec.SessionRef.Append = true
			next.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}
			if err := r.Create(ctx, next); err != nil {
				t.Fatal(err)
			}
			if err := r.SessionManager.AcquireLock(ctx, next); err != nil {
				t.Fatal(err)
			}
			changed := agent.DeepCopy()
			changed.Generation++
			changed.Spec.Soul = &corev1alpha1.SoulSource{Inline: "new persona"}
			if _, err := r.prepareAISoul(ctx, next, changed); err == nil || !isPermanentAISoulConfigurationError(err) {
				t.Fatalf("empty used Session acquired its first soul: %v", err)
			}
			if next.Status.SoulBinding != nil {
				t.Fatal("rejected persona introduction bound the next Task")
			}
			if prepared, err := r.prepareAISoul(ctx, next, agent); err != nil || prepared != nil {
				t.Fatalf("no-soul continuation failed: %v", err)
			}
			transcript, err := s.LoadTranscript(ctx, task.Namespace, "session", 50)
			if err != nil || len(transcript) != 0 {
				t.Fatal("no-soul identity became visible transcript content")
			}
		})
	}
}

func TestAINoSoulSessionPinFailureRetriesBeforeJobCreation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "write failed"
		if committed {
			name = "commit acknowledgement lost"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, task, agent, s := newAISoulSessionPinFixture(t, "prompt")
			agent.Spec.Soul = nil
			outage := errors.New("temporary Session write failure")
			s.pinError, s.commitBeforeError = outage, committed
			if _, err := r.createTaskJob(ctx, task, agent, nil); !errors.Is(err, outage) || isPermanentAISoulConfigurationError(err) {
				t.Fatalf("pin failure was not retryable: %v", err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
				t.Fatal(err)
			}
			if task.Status.Phase != corev1alpha1.TaskPhasePending || task.Status.Attempts != 0 || task.Status.SoulBinding != nil {
				t.Fatal("no-soul pin failure consumed the Task")
			}
			var jobs batchv1.JobList
			if err := r.List(ctx, &jobs); err != nil || len(jobs.Items) != 0 {
				t.Fatal("Job launched before its no-soul Session pin was confirmed")
			}
			if _, err := r.createTaskJob(ctx, task, agent, nil); err != nil {
				t.Fatal(err)
			}
			state, err := s.ReadSessionSoul(ctx, task.Namespace, "session", task.Name, string(task.UID))
			if err != nil || !state.Established || state.Digest != "" || task.Status.Attempts != 1 {
				t.Fatal("retry did not confirm the no-soul revision before launching once")
			}
		})
	}
}

func TestAINoSoulSessionPinDefersToGatewayOwnership(t *testing.T) {
	ctx := context.Background()
	r, task, _, s := newAISoulSessionPinFixture(t, "prompt")
	s.pinError = errors.New("ordinary pin must not run for Gateway")
	r.SessionManager.SetGatewayEventStore(soulPinGatewayLookup{})
	if err := r.pinAISoulSession(ctx, task, nil); err != nil {
		t.Fatalf("no-soul Gateway pin did not defer to canonical projection: %v", err)
	}
	outage := errors.New("temporary Gateway lookup failure")
	r.SessionManager.SetGatewayEventStore(soulPinGatewayLookup{err: outage})
	if err := r.pinAISoulSession(ctx, task, nil); !errors.Is(err, outage) {
		t.Fatalf("no-soul Gateway lookup failed open: %v", err)
	}
}
