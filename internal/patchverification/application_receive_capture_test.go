package patchverification

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

type applicationReceiverTriggerActor struct {
	applicationTestActor
	trigger func(context.Context) error
}

func (actor applicationReceiverTriggerActor) Apply(ctx context.Context, namespace string, action ApplicationAction,
	target ApplicationActionTarget) (ApplicationActionObservation, error) {
	if err := actor.trigger(ctx); err != nil {
		return ApplicationActionObservation{}, err
	}
	return actor.applicationTestActor.Apply(ctx, namespace, action, target)
}

func applicationReceiverObserverInput(t *testing.T, state *applicationReceiverTestState) ApplicationWorkerInput {
	t.Helper()
	manifest, _, _ := applicationReceiverTestEvidence(t)
	for index := range manifest.Checks {
		check := &manifest.Checks[index]
		check.Application.SettleSeconds = 2
		check.Application.TCP.Port = state.receiver.check.Port
		check.Application.TCP.Terminator = []byte("\n")
		check.Application.TCP.Response = []byte("ack\n")
	}
	for index := range manifest.Application.Fixtures {
		fixture := &manifest.Application.Fixtures[index]
		if fixture.ID == "receiver" {
			fixture.Port = state.receiver.check.Port
		}
	}
	binding, evidence := applicationTestEvidence(t, manifest)
	input := state.receiver.input
	input.Version, input.Manifest, input.Binding = ApplicationProtocolVersion, manifest, binding
	input.Side, input.CheckID, input.TaskUID = Original, manifest.Checks[0].ID, "observer-task"
	input.Target = evidence[0].Application.Target
	input.Artifact, input.Subject = &evidence[0].Application.Artifact, &evidence[0].Application.Subject
	input.Worker.ImageID = manifest.Environment.Dependencies[ApplicationObserverImageIDKey]
	input.Actors = []ApplicationActorIdentity{{ID: "author", Namespace: input.Worker.Namespace,
		NamespaceUID: input.Worker.NamespaceUID, ServiceAccountName: "actor-author", ServiceAccountUID: "actor-author-uid"}}
	input.ActionTargets = []ApplicationActionTarget{{Index: 0, APIVersion: "testing.example/v1", Resource: "widgets",
		Name: "case-unsafe", MustBeAbsent: true, OwnerUID: "run-ledger"}}
	input.MarkerRef = &ApplicationProtectedMarkerRef{
		Namespace: input.Worker.Namespace, NamespaceUID: input.Worker.NamespaceUID, Name: "protected-value",
		UID: "protected-marker-uid", Key: "marker", Digest: input.MarkerDigest,
	}
	state.pod.Status.ContainerStatuses[0].ImageID = input.Worker.ImageID
	state.update(t)
	subject := applicationLiveTestPod(input.Target.Namespace, *input.Subject, manifest.Application.Runtime.Image, true)
	subject.Status.PodIP = "127.0.0.2"
	require.NoError(t, state.client.Tracker().Add(subject))
	require.NoError(t, state.client.Tracker().Add(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: input.Subject.ServiceAccountName, Namespace: input.Target.Namespace, UID: types.UID(input.Subject.ServiceAccountUID)}}))
	require.NoError(t, ValidateApplicationWorkerInput(input))
	require.NoError(t, state.receiver.listener.Close())
	return input
}

func dialApplicationReceiverSubject(ctx context.Context, address *net.TCPAddr) (net.Conn, error) {
	return (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).DialContext(ctx, "tcp4", address.String())
}

func applicationReceiverSubjectResponse(ctx context.Context, address *net.TCPAddr, marker []byte, sent chan<- struct{}) error {
	defer close(sent)
	connection, err := dialApplicationReceiverSubject(ctx, address)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	stop := applicationCloseOnCancel(ctx, connection)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := connection.Write(append(append([]byte{}, marker...), '\n')); err != nil {
		return err
	}
	sent <- struct{}{}
	response := make([]byte, 4)
	if _, err := io.ReadFull(connection, response); err != nil {
		return err
	}
	if string(response) != "ack\n" {
		return errors.New("subject did not receive the frozen response")
	}
	return nil
}

func TestApplicationReceiverAnswersActorTriggeredSubjectDuringLongSettle(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	address := state.receiver.listener.Addr().(*net.TCPAddr)
	input := applicationReceiverObserverInput(t, &state)
	done := make(chan error, 1)
	actor := applicationReceiverTriggerActor{
		applicationTestActor: applicationTestActor{identity: input.Actors[0], status: 201},
		trigger: func(ctx context.Context) error {
			sent := make(chan struct{}, 1)
			go func() {
				responseContext, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				done <- applicationReceiverSubjectResponse(responseContext, address, state.receiver.marker, sent)
			}()
			select {
			case <-sent:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	receipts := 0
	started := time.Now()
	proof, err := ObserveApplicationWithActors(t.Context(), input, state.client,
		dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), state.receiver.marker,
		map[string]ApplicationActorClient{"author": actor},
		func(ApplicationActionObservation) error { receipts++; return nil })
	require.NoError(t, err)
	require.NoError(t, <-done, "the subject's response deadline is shorter than settling, not a test-harness assertion")
	require.Equal(t, 1, receipts)
	require.GreaterOrEqual(t, time.Since(started), 3*time.Second,
		"capture during settling must not shorten the additional frozen observation window")
	tcp := proof.Observation.Application.TCP
	require.True(t, tcp.Result.MarkerMatched)
	require.Equal(t, 1, tcp.Connections)
	require.Equal(t, 49, tcp.Bytes, "the qualified nonce is never included in subject traffic")
	require.NoError(t, ValidateObservation(input.Manifest, input.Binding, proof.Observation))
}

func TestApplicationReceiverActorFailureClosesPendingSubject(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	address := state.receiver.listener.Addr().(*net.TCPAddr)
	input := applicationReceiverObserverInput(t, &state)
	var connection net.Conn
	actorFailure := errors.New("actor operation did not complete")
	actor := applicationReceiverTriggerActor{
		applicationTestActor: applicationTestActor{identity: input.Actors[0], status: 201},
		trigger: func(ctx context.Context) error {
			var err error
			connection, err = dialApplicationReceiverSubject(ctx, address)
			if err != nil {
				return err
			}
			_, err = connection.Write([]byte("unfinished frame"))
			if err != nil {
				return err
			}
			return actorFailure
		},
	}
	proof, err := ObserveApplicationWithActors(t.Context(), input, state.client,
		dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), state.receiver.marker,
		map[string]ApplicationActorClient{"author": actor}, func(ApplicationActionObservation) error { return nil })
	require.ErrorIs(t, err, actorFailure)
	require.Nil(t, proof.Observation.Application)
	require.NotNil(t, connection)
	defer func() { _ = connection.Close() }()
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = connection.Read(make([]byte, 1))
	require.Error(t, err)
	var networkError net.Error
	require.False(t, errors.As(err, &networkError) && networkError.Timeout(),
		"actor-error return must stop the receiver, not leave its subject blocked until its own timeout")
}

func startApplicationReceiverTestCapture(t *testing.T, state applicationReceiverTestState, ctx context.Context) {
	t.Helper()
	require.NoError(t, state.receiver.verifyService(ctx))
	require.NoError(t, state.receiver.verifyServiceDataPlane(ctx))
	require.NoError(t, state.receiver.startCapture(ctx))
}

func waitApplicationReceiverCapture(t *testing.T, capture *applicationReceiverCapture, predicate func(*applicationReceiverCapture) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		capture.lock.Lock()
		defer capture.lock.Unlock()
		return predicate(capture)
	}, time.Second, time.Millisecond)
}

func TestApplicationReceiverCancellationStopsAndJoinsPartialFrames(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	startApplicationReceiverTestCapture(t, state, ctx)
	connection, err := dialApplicationReceiverSubject(ctx, state.receiver.listener.Addr().(*net.TCPAddr))
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	_, err = connection.Write([]byte("partial"))
	require.NoError(t, err)
	waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool { return len(c.active) == 1 })
	cancel()
	state.receiver.close()
	state.receiver.close()
	select {
	case <-state.receiver.capture.done:
	default:
		t.Fatal("receiver close returned before its collector and frame workers were joined")
	}
	waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
		return len(c.active) == 0 && !c.completed
	})
	proof, content, err := state.receiver.observe(ctx)
	require.Error(t, err)
	require.Nil(t, proof)
	require.Nil(t, content)
}

func TestApplicationReceiverCaptureNeverSubstitutesForACompleteWindow(t *testing.T) {
	for _, timeout := range []time.Duration{150 * time.Millisecond, 3 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			state := newApplicationReceiverTestState(t)
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			startApplicationReceiverTestCapture(t, state, ctx)
			if timeout > time.Second {
				cancel()
			}
			proof, content, err := state.receiver.observe(ctx)
			require.Error(t, err)
			require.Nil(t, proof)
			require.Nil(t, content)
			select {
			case <-state.receiver.capture.done:
			default:
				t.Fatal("failed window retained collector workers")
			}
		})
	}
}

func sendApplicationReceiverFrame(t *testing.T, state applicationReceiverTestState, content string) {
	t.Helper()
	connection, err := net.DialTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.2")},
		state.receiver.listener.Addr().(*net.TCPAddr))
	require.NoError(t, err)
	_, err = connection.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, connection.Close())
}

func TestApplicationReceiverTrafficLimitsSpanCaptureAndFinalWindow(t *testing.T) {
	for _, limit := range []string{"connections", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			state := newApplicationReceiverTestState(t)
			state.receiver.check.MaxBytes = 64
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			startApplicationReceiverTestCapture(t, state, ctx)
			count, first := 8, ""
			if limit == "bytes" {
				count, first = 1, strings.Repeat("x", 32)
			}
			for index := range count {
				sendApplicationReceiverFrame(t, state, first)
				waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
					return c.totalConnections == index+1 && len(c.active) == 0
				})
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := state.receiver.observe(ctx)
				done <- err
			}()
			waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
				return !c.windowEnd.IsZero()
			})
			connection, err := net.DialTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.2")},
				state.receiver.listener.Addr().(*net.TCPAddr))
			require.NoError(t, err)
			_, _ = connection.Write([]byte(strings.Repeat("y", 33)))
			_ = connection.Close()
			require.Error(t, <-done, "arming a window must not reset earlier traffic budgets")
			state.receiver.close()
			waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
				return len(c.active) == 0 && !c.completed
			})
		})
	}
}

func TestApplicationReceiverFinalWindowBoundsAlreadyAcceptedPartialFrame(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	startApplicationReceiverTestCapture(t, state, ctx)
	connection, err := dialApplicationReceiverSubject(ctx, state.receiver.listener.Addr().(*net.TCPAddr))
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	_, err = connection.Write([]byte("incomplete"))
	require.NoError(t, err)
	waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool { return len(c.active) == 1 })
	started := time.Now()
	proof, content, err := state.receiver.observe(ctx)
	require.Error(t, err)
	require.Nil(t, proof)
	require.Nil(t, content)
	require.Less(t, time.Since(started), 3*time.Second,
		"arming must shorten an already accepted frame's deadline, not wait for the whole-check deadline")
	waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
		return len(c.active) == 0 && !c.completed
	})
}

func TestApplicationReceiverUnarmedCaptureCannotOutliveCheckDeadline(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	startApplicationReceiverTestCapture(t, state, ctx)
	select {
	case <-state.receiver.capture.done:
	case <-time.After(2 * time.Second):
		t.Fatal("capture survived its frozen check deadline before a window was armed")
	}
	waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
		return c.err != nil && !c.completed && len(c.active) == 0
	})
}

func TestApplicationReceiverActiveProbeLimitStopsAndJoinsWorkers(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	startApplicationReceiverTestCapture(t, state, ctx)
	connections := make([]*net.TCPConn, 0, 17)
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	for index := range 17 {
		connection, err := net.DialTCP("tcp4", nil, state.receiver.listener.Addr().(*net.TCPAddr))
		require.NoError(t, err)
		connections = append(connections, connection)
		if index < 16 {
			waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
				return len(c.active) == index+1
			})
		}
	}
	select {
	case <-state.receiver.capture.done:
	case <-ctx.Done():
		t.Fatal("active connection bound failed to stop the receiver promptly")
	}
	state.receiver.capture.lock.Lock()
	captureErr, active := state.receiver.capture.err, len(state.receiver.capture.active)
	state.receiver.capture.lock.Unlock()
	require.ErrorIs(t, captureErr, ErrLimit)
	require.Zero(t, active)
}

func TestApplicationReceiverTotalProbeLimitDoesNotResetAtWindowStart(t *testing.T) {
	state := newApplicationReceiverTestState(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	startApplicationReceiverTestCapture(t, state, ctx)
	for index := range 256 {
		connection, err := net.DialTCP("tcp4", nil, state.receiver.listener.Addr().(*net.TCPAddr))
		require.NoError(t, err)
		require.NoError(t, connection.Close())
		waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
			return c.totalConnections == index+1 && len(c.active) == 0
		})
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := state.receiver.observe(ctx)
		done <- err
	}()
	waitApplicationReceiverCapture(t, state.receiver.capture, func(c *applicationReceiverCapture) bool {
		return !c.windowEnd.IsZero()
	})
	connection, err := net.DialTCP("tcp4", nil, state.receiver.listener.Addr().(*net.TCPAddr))
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	require.ErrorIs(t, <-done, ErrLimit)
	state.receiver.close()
}
