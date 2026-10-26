package common

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/orka-agents/orka/internal/enforcement"
	"github.com/orka-agents/orka/internal/workerenv"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// WaitForEnforcementFromEnv precedes model input, custom tools and MCP startup.
// The distinct executable path makes an older worker image fail closed: it
// cannot ignore an unknown environment variable or an unparsed command flag.
func WaitForEnforcementFromEnv(ctx context.Context, required bool) error {
	digest := os.Getenv(workerenv.EnforcementRequirementDigest)
	if digest == "" && !required {
		return nil
	}
	podName, podUID := os.Getenv(workerenv.WorkerPodName), os.Getenv(workerenv.WorkerPodUID)
	namespace := os.Getenv(workerenv.TaskNamespace)
	if digest == "" || podName == "" || podUID == "" || namespace == "" {
		return errors.New("enforced worker requires frozen requirement and physical Pod identity")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("configure authenticated enforcement admission: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("configure enforcement Pod reader: %w", err)
	}
	return WaitForPodEnforcement(ctx, client, namespace, podName, podUID, digest)
}

func WaitForPodEnforcement(ctx context.Context, client kubernetes.Interface, namespace, name, uid, digest string) error {
	pods := client.CoreV1().Pods(namespace)
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = selector
			return pods.List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = selector
			return pods.Watch(ctx, options)
		},
	}
	check := func(object any) (bool, error) {
		pod, ok := object.(*corev1.Pod)
		if !ok {
			return false, errors.New("unexpected enforcement admission object")
		}
		return enforcement.PodApproved(pod, uid, "worker", digest)
	}
	_, err := watchtools.UntilWithSync(ctx, cache.ToListWatcherWithWatchListSemantics(lw, client), &corev1.Pod{}, func(store cache.Store) (bool, error) {
		object, exists, err := store.GetByKey(namespace + "/" + name)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, errors.New("physical worker Pod no longer exists")
		}
		return check(object)
	}, func(event watch.Event) (bool, error) {
		if event.Type == watch.Deleted {
			return false, errors.New("physical worker Pod was deleted before admission")
		}
		return check(event.Object)
	})
	if err != nil {
		return fmt.Errorf("wait for current container enforcement: %w", err)
	}
	return ctx.Err()
}
