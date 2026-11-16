/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultTaskWebhookDeliveryWorkers bounds how many Task webhook
	// attempts run at once. Each worker mostly waits on one receiver for at
	// most taskwebhook.DefaultRequestTimeout, so this many hanging receivers
	// delay other notices, never Task reconciliation.
	DefaultTaskWebhookDeliveryWorkers = 8
	// MaxTaskWebhookDeliveryWorkers caps the configurable pool.
	MaxTaskWebhookDeliveryWorkers = 256

	// webhookDeliveryRecheckInterval is how often a Task with a due notice
	// is reconciled again while its delivery is queued or in flight. The
	// dispatcher's status writes reconcile the Task sooner; this only
	// bounds a lost wakeup.
	webhookDeliveryRecheckInterval = time.Minute
	// webhookResultRecordRetries bounds the re-reads that record an
	// attempt's result after a concurrent status write.
	webhookResultRecordRetries = 5
)

// taskWebhookDispatcher sends Task webhook notices (ADR 0042, ADR 0053)
// outside the Task reconcile worker.
//
// The Task reconciler only decides that a notice is due and enqueues the
// Task. A bounded pool of workers then advances its delivery against an
// uncached read, with the same durable claim-before-send lease, attempt
// bound, signing and destination checks as before. The work queue never
// hands one Task to two workers at once, so a Task's notices still go one
// attempt at a time and in stream order, and the persisted lease still
// fences other replicas. A receiver that hangs holds one delivery worker for
// at most the request timeout, never the Task reconcile worker.
type taskWebhookDispatcher struct {
	notifier *WebhookNotifier
	reader   ctrlclient.Reader
	status   taskStatusWriter
	workers  int
	queue    workqueue.TypedRateLimitingInterface[types.NamespacedName]

	mu       sync.Mutex
	requests map[types.NamespacedName]taskWebhookRequest
	sequence uint64
}

// taskWebhookRequest is what the Task reconciler asked to deliver for one
// Task incarnation. Approval notices are owed in every phase; the terminal
// notice only once the reconciler's completion work let it go.
type taskWebhookRequest struct {
	uid      types.UID
	terminal bool
	sequence uint64
}

func newTaskWebhookDispatcher(notifier *WebhookNotifier, reader ctrlclient.Reader, status ctrlclient.SubResourceWriter, workers int) *taskWebhookDispatcher {
	if workers <= 0 {
		workers = DefaultTaskWebhookDeliveryWorkers
	}
	return &taskWebhookDispatcher{
		notifier: notifier,
		reader:   reader,
		status:   &rereadingTaskStatus{writer: status, reader: reader},
		workers:  workers,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[types.NamespacedName](),
			workqueue.TypedRateLimitingQueueConfig[types.NamespacedName]{Name: "task-webhook-delivery"},
		),
		requests: map[types.NamespacedName]taskWebhookRequest{},
	}
}

// NeedLeaderElection keeps delivery on the leader, with the Task controller.
// The persisted attempt lease still fences any other replica.
func (d *taskWebhookDispatcher) NeedLeaderElection() bool { return true }

// Start runs the delivery workers until ctx ends.
func (d *taskWebhookDispatcher) Start(ctx context.Context) error {
	var workers sync.WaitGroup
	for range d.workers {
		workers.Go(func() {
			for d.processNext(ctx) {
			}
		})
	}
	<-ctx.Done()
	d.queue.ShutDown()
	workers.Wait()
	return nil
}

// enqueue asks for the Task's due notices to be advanced. terminal says the
// reconciler's completion work allows the terminal notice.
func (d *taskWebhookDispatcher) enqueue(task *corev1alpha1.Task, terminal bool) {
	key := ctrlclient.ObjectKeyFromObject(task)
	d.mu.Lock()
	request := d.requests[key]
	if request.uid != task.UID {
		request = taskWebhookRequest{uid: task.UID}
	}
	request.terminal = request.terminal || terminal
	d.sequence++
	request.sequence = d.sequence
	d.requests[key] = request
	d.mu.Unlock()
	d.queue.Add(key)
}

// processNext runs one queued delivery pass. It reports false once the
// queue shuts down.
func (d *taskWebhookDispatcher) processNext(ctx context.Context) bool {
	key, shutdown := d.queue.Get()
	if shutdown {
		return false
	}
	defer d.queue.Done(key)
	d.mu.Lock()
	request, ok := d.requests[key]
	d.mu.Unlock()
	if !ok {
		d.queue.Forget(key)
		return true
	}
	log := logf.FromContext(ctx).WithValues("controller", "task-webhook-delivery", "Task", key)
	requeue, err := d.deliver(logf.IntoContext(ctx, log), key, request)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			log.Error(err, "failed to advance Task webhook delivery")
		}
		d.queue.AddRateLimited(key)
	case requeue > 0:
		d.queue.Forget(key)
		d.queue.AddAfter(key, requeue)
	default:
		d.queue.Forget(key)
		d.mu.Lock()
		// A request made during this pass queued the Task again; keep it.
		if current, ok := d.requests[key]; ok && current.sequence == request.sequence {
			delete(d.requests, key)
		}
		d.mu.Unlock()
	}
	return true
}

// deliver advances the Task's approval notices and, when allowed, its
// terminal notice, by at most one attempt each, and returns how long to wait
// before the next pass (zero when nothing remains). A Task that is gone,
// replaced or being deleted drops its notices.
func (d *taskWebhookDispatcher) deliver(ctx context.Context, key types.NamespacedName, request taskWebhookRequest) (time.Duration, error) {
	task := &corev1alpha1.Task{}
	if err := d.reader.Get(ctx, key, task); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	if task.UID != request.uid || !task.DeletionTimestamp.IsZero() {
		return 0, nil
	}
	requeue := time.Duration(0)
	if firstPendingApprovalNotice(task.Status.ApprovalWebhooks) >= 0 {
		next, err := d.notifier.sendApprovalNotices(ctx, d.status, task)
		if err != nil {
			return 0, err
		}
		requeue = next
	}
	if request.terminal && task.Status.Phase.IsTerminal() {
		next, err := d.notifier.reconcileDelivery(ctx, d.status, task)
		if err != nil {
			return 0, err
		}
		requeue = minPositiveDuration(requeue, next)
	}
	return requeue, nil
}

// taskStatusRereader is a status writer that can also refresh a Task from
// an uncached read, so an attempt's result survives a concurrent status
// write on the same Task.
type taskStatusRereader interface {
	taskStatusWriter
	reread(ctx context.Context, task *corev1alpha1.Task) error
}

// rereadingTaskStatus is the dispatcher's status writer.
type rereadingTaskStatus struct {
	writer ctrlclient.SubResourceWriter
	reader ctrlclient.Reader
}

func (s *rereadingTaskStatus) Update(ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
	return s.writer.Update(ctx, obj, opts...)
}

// reread replaces task with its current state. A Task that was replaced
// under the same name is reported as gone.
func (s *rereadingTaskStatus) reread(ctx context.Context, task *corev1alpha1.Task) error {
	current := &corev1alpha1.Task{}
	if err := s.reader.Get(ctx, ctrlclient.ObjectKeyFromObject(task), current); err != nil {
		return err
	}
	if current.UID != task.UID {
		return errors.New("the Task was replaced after its webhook attempt was claimed")
	}
	*task = *current
	return nil
}
