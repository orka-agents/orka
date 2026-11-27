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
	DefaultTaskWebhookDeliveryWorkers = 8
	MaxTaskWebhookDeliveryWorkers     = 256

	webhookDeliveryRecheckInterval = time.Minute
	webhookResultRecordRetries     = 5
)

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

func (d *taskWebhookDispatcher) NeedLeaderElection() bool { return true }

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
		if current, ok := d.requests[key]; ok && current.sequence == request.sequence {
			delete(d.requests, key)
		}
		d.mu.Unlock()
	}
	return true
}

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
	if request.terminal && task.Status.Phase.IsTerminal() && task.DeletionTimestamp.IsZero() {
		next, err := d.notifier.reconcileDelivery(ctx, d.status, task)
		if err != nil {
			return 0, err
		}
		requeue = minPositiveDuration(requeue, next)
	}
	return requeue, nil
}

type taskStatusRereader interface {
	taskStatusWriter
	reread(ctx context.Context, task *corev1alpha1.Task) error
}

type rereadingTaskStatus struct {
	writer ctrlclient.SubResourceWriter
	reader ctrlclient.Reader
}

func (s *rereadingTaskStatus) Update(ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
	return s.writer.Update(ctx, obj, opts...)
}

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
