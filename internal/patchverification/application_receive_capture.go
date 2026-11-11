package patchverification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

type applicationReceiverCapture struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	lock             sync.Mutex
	active           map[*net.TCPConn]struct{}
	deadline         time.Time
	windowEnd        time.Time
	totalConnections int
	proof            ApplicationTCPIdentity
	completed        bool
	err              error
}

func applicationCloseOnCancel(ctx context.Context, closer io.Closer) func() {
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = closer.Close()
		close(closed)
	})
	var joined sync.Once
	return func() {
		joined.Do(func() {
			if !stop() {
				<-closed
			}
		})
	}
}

// Capture begins after route/nonce qualification, before any actor can trigger
// the subject. The later observation call arms an additional full window; it
// never resets traffic counters or discards actions/settling-period frames.
func (receiver *applicationReceiver) startCapture(ctx context.Context) error {
	deadline, bounded := ctx.Deadline()
	if receiver.route == nil || receiver.capture != nil || ctx.Err() != nil || !bounded ||
		receiver.check.WindowSeconds < 1 || receiver.check.WindowSeconds > 120 {
		return ErrBinding
	}
	if err := receiver.listener.SetDeadline(deadline); err != nil {
		return ErrEvidence
	}
	if receiver.stop != nil {
		receiver.stop()
	}
	ctx, cancel := context.WithCancel(ctx)
	route := *receiver.route
	capture := &applicationReceiverCapture{
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		active: make(map[*net.TCPConn]struct{}, 16), deadline: deadline,
		proof: ApplicationTCPIdentity{
			Endpoint: ApplicationEndpoint{ID: receiver.check.Target, Address: receiver.selfIP,
				Port: receiver.check.Port, Pod: receiver.input.Worker},
			MarkerID: receiver.check.MarkerID, MarkerDigest: Digest(receiver.marker), MarkerBytes: len(receiver.marker),
			Mode: ApplicationTCPReceive, WindowSeconds: receiver.check.WindowSeconds, ReceiverRoute: &route,
		},
	}
	receiver.capture = capture
	receiver.stop = applicationCloseOnCancel(ctx, receiver.listener)
	go receiver.collect(capture)
	return nil
}

func (capture *applicationReceiverCapture) fail(err error) {
	capture.lock.Lock()
	if capture.err == nil {
		capture.err = err
	}
	capture.lock.Unlock()
	capture.cancel()
}

func (receiver *applicationReceiver) collect(capture *applicationReceiverCapture) {
	defer close(capture.done)
	var workers sync.WaitGroup
	windowCompleted := false
	for {
		connection, err := receiver.listener.AcceptTCP()
		if err != nil {
			windowCompleted = capture.windowElapsed(err)
			if !windowCompleted {
				capture.fail(errors.New("application receiver listener failed before completing the window"))
			}
			break
		}
		subject, err := receiver.admitConnection(capture, connection)
		if err != nil {
			_ = connection.Close()
			capture.fail(err)
			break
		}
		workers.Go(func() { receiver.collectFrame(capture, connection, subject) })
	}
	workers.Wait()
	capture.lock.Lock()
	defer capture.lock.Unlock()
	capture.completed = windowCompleted && capture.err == nil && capture.ctx.Err() == nil
	if !capture.completed && capture.err == nil {
		capture.err = ErrEvidence
	}
}

func (capture *applicationReceiverCapture) windowElapsed(err error) bool {
	var networkError net.Error
	capture.lock.Lock()
	defer capture.lock.Unlock()
	return errors.As(err, &networkError) && networkError.Timeout() && !capture.windowEnd.IsZero() &&
		!time.Now().Before(capture.windowEnd) && capture.ctx.Err() == nil
}

func (receiver *applicationReceiver) admitConnection(capture *applicationReceiverCapture, connection *net.TCPConn) (bool, error) {
	remote, ok := connection.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false, ErrEvidence
	}
	subject := remote.IP.Equal(receiver.subjectIP)
	capture.lock.Lock()
	defer capture.lock.Unlock()
	if capture.err != nil || capture.ctx.Err() != nil {
		return false, ErrEvidence
	}
	if len(capture.active) >= 16 || capture.totalConnections >= 256 ||
		subject && capture.proof.Connections >= 8 {
		return false, ErrLimit
	}
	if err := connection.SetDeadline(capture.deadline); err != nil {
		return false, ErrEvidence
	}
	capture.totalConnections++
	if subject {
		capture.proof.Connections++
	}
	capture.active[connection] = struct{}{}
	return subject, nil
}

func (receiver *applicationReceiver) collectFrame(capture *applicationReceiverCapture, connection *net.TCPConn, subject bool) {
	defer func() {
		capture.lock.Lock()
		delete(capture.active, connection)
		capture.lock.Unlock()
		_ = connection.Close()
	}()
	stop := applicationCloseOnCancel(capture.ctx, connection)
	defer stop()
	content, err := receiver.readConnectionFrame(connection, subject)
	capture.lock.Lock()
	if err != nil || capture.ctx.Err() != nil || len(content) > receiver.check.MaxBytes-capture.proof.Bytes {
		capture.lock.Unlock()
		capture.fail(errors.New("application receiver frame was incomplete or oversized"))
		return
	}
	if subject {
		capture.proof.Bytes += len(content)
		capture.proof.Result.MarkerMatched = capture.proof.Result.MarkerMatched || bytes.Contains(content, receiver.marker)
	}
	capture.lock.Unlock()
}

func (receiver *applicationReceiver) armWindow(ctx context.Context) error {
	capture := receiver.capture
	windowEnd := time.Now().Add(time.Duration(receiver.check.WindowSeconds) * time.Second)
	capture.lock.Lock()
	defer capture.lock.Unlock()
	if capture.err != nil {
		return capture.err
	}
	if capture.ctx.Err() != nil || ctx.Err() != nil || !capture.windowEnd.IsZero() {
		return ErrEvidence
	}
	if deadline, bounded := ctx.Deadline(); bounded && !windowEnd.Before(deadline) {
		return errors.New("application receiver has insufficient remaining time for a complete window")
	}
	if !windowEnd.Before(capture.deadline) {
		return errors.New("application receiver has insufficient remaining time for a complete window")
	}
	capture.windowEnd, capture.deadline = windowEnd, windowEnd
	if err := receiver.listener.SetDeadline(windowEnd); err != nil {
		return ErrEvidence
	}
	for connection := range capture.active {
		if err := connection.SetDeadline(windowEnd); err != nil {
			return ErrEvidence
		}
	}
	return nil
}

func (receiver *applicationReceiver) observe(ctx context.Context) (*ApplicationTCPIdentity, []byte, error) {
	if receiver.capture == nil {
		return nil, nil, ErrBinding
	}
	if err := receiver.armWindow(ctx); err != nil {
		receiver.close()
		return nil, nil, err
	}
	select {
	case <-receiver.capture.done:
	case <-ctx.Done():
		receiver.close()
		return nil, nil, ErrEvidence
	}
	capture := receiver.capture
	capture.lock.Lock()
	completed, captureErr, proof := capture.completed, capture.err, capture.proof
	capture.lock.Unlock()
	if captureErr != nil {
		receiver.close()
		return nil, nil, captureErr
	}
	if !completed || ctx.Err() != nil || capture.ctx.Err() != nil {
		receiver.close()
		return nil, nil, ErrEvidence
	}
	if err := receiver.verifyService(ctx); err != nil {
		receiver.close()
		return nil, nil, err
	}
	proof.Result.Connected = proof.Connections > 0
	content, _ := json.Marshal(proof.Result)
	return &proof, content, nil
}
