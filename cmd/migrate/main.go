/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/sozercan/mercan/internal/store"
	"github.com/sozercan/mercan/internal/store/sqlite"
)

func main() {
	var storePath string
	var namespace string
	var cleanup bool
	var dryRun bool
	var kubeconfig string

	flag.StringVar(&storePath, "store-path", "", "Path to SQLite database file (required)")
	flag.StringVar(&namespace, "namespace", "", "Migrate only this namespace (default: all namespaces)")
	flag.BoolVar(&cleanup, "cleanup", false, "Delete migrated ConfigMaps after successful insertion")
	flag.BoolVar(&dryRun, "dry-run", false, "Log what would be migrated without writing")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (default: in-cluster or ~/.kube/config)")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("migrate")

	if storePath == "" {
		fmt.Fprintln(os.Stderr, "Error: --store-path is required")
		flag.Usage()
		os.Exit(1)
	}

	// Build Kubernetes client
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			// Fall back to default kubeconfig for local dev
			loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
			configOverrides := &clientcmd.ConfigOverrides{}
			config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
		}
	}
	if err != nil {
		log.Error(err, "Failed to build Kubernetes config")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Error(err, "Failed to create Kubernetes client")
		os.Exit(1)
	}

	// Open SQLite store (skip in dry-run mode)
	var resultStore store.ResultStore
	var sessionStore store.SessionStore
	if !dryRun {
		db, err := sqlite.NewDB(storePath)
		if err != nil {
			log.Error(err, "Failed to open SQLite database", "path", storePath)
			os.Exit(1)
		}
		defer db.Close() //nolint:errcheck
		s := sqlite.NewStore(db, storePath)
		resultStore = s
		sessionStore = s
	}

	ctx := context.Background()
	var resultsMigrated, sessionsMigrated int
	var hadErrors bool

	// Migrate results
	resultCMs, err := listConfigMaps(ctx, clientset, namespace, "mercan.ai/result=true")
	if err != nil {
		log.Error(err, "Failed to list result ConfigMaps")
		os.Exit(1)
	}

	for i := range resultCMs {
		cm := &resultCMs[i]
		taskName := extractTaskNameFromResult(cm.Name)

		data, ok := cm.Data["result"]
		if !ok {
			log.Info("Skipping result ConfigMap with no 'result' key", "namespace", cm.Namespace, "configmap", cm.Name)
			continue
		}

		if dryRun {
			log.Info("Would migrate result", "namespace", cm.Namespace, "task", taskName)
			resultsMigrated++
			continue
		}

		if err := resultStore.SaveResult(ctx, cm.Namespace, taskName, []byte(data)); err != nil {
			log.Error(err, "Failed to migrate result, skipping", "namespace", cm.Namespace, "task", taskName)
			hadErrors = true
			continue
		}

		log.Info("Migrated result", "namespace", cm.Namespace, "task", taskName)
		resultsMigrated++

		if cleanup {
			if err := deleteConfigMap(ctx, clientset, cm.Namespace, cm.Name); err != nil {
				log.Error(err, "Failed to delete result ConfigMap after migration", "namespace", cm.Namespace, "configmap", cm.Name)
				hadErrors = true
			}
		}
	}

	// Migrate sessions
	sessionCMs, err := listConfigMaps(ctx, clientset, namespace, "mercan.ai/session=true")
	if err != nil {
		log.Error(err, "Failed to list session ConfigMaps")
		os.Exit(1)
	}

	for i := range sessionCMs {
		cm := &sessionCMs[i]
		sessionName := extractSessionName(cm.Name)
		sessionType := "task"
		if cm.Labels["mercan.ai/session-type"] == "chat" {
			sessionType = "chat"
		}

		transcript, ok := cm.Data["transcript.jsonl"]
		if !ok {
			log.Info("Skipping session ConfigMap with no 'transcript.jsonl' key",
				"namespace", cm.Namespace, "configmap", cm.Name)
			continue
		}

		messages, err := parseJSONL(transcript)
		if err != nil {
			log.Error(err, "Failed to parse session transcript, skipping", "namespace", cm.Namespace, "session", sessionName)
			hadErrors = true
			continue
		}

		if dryRun {
			log.Info("Would migrate session", "namespace", cm.Namespace,
				"session", sessionName, "type", sessionType, "messages", len(messages))
			sessionsMigrated++
			continue
		}

		now := time.Now()
		session := &store.SessionRecord{
			Namespace:   cm.Namespace,
			Name:        sessionName,
			SessionType: sessionType,
			CreatedAt:   cm.CreationTimestamp.Time,
			UpdatedAt:   now,
		}

		if err := sessionStore.CreateSession(ctx, session); err != nil {
			// Session may already exist (idempotent migration); try appending messages anyway
			if !strings.Contains(err.Error(), "UNIQUE constraint") {
				log.Error(err, "Failed to create session, skipping", "namespace", cm.Namespace, "session", sessionName)
				hadErrors = true
				continue
			}
			log.Info("Session already exists, skipping creation", "namespace", cm.Namespace, "session", sessionName)
		} else if len(messages) > 0 {
			if err := sessionStore.AppendMessages(ctx, cm.Namespace, sessionName, messages); err != nil {
				log.Error(err, "Failed to append session messages, skipping", "namespace", cm.Namespace, "session", sessionName)
				hadErrors = true
				continue
			}
		}

		log.Info("Migrated session", "namespace", cm.Namespace,
			"session", sessionName, "type", sessionType, "messages", len(messages))
		sessionsMigrated++

		if cleanup {
			if err := deleteConfigMap(ctx, clientset, cm.Namespace, cm.Name); err != nil {
				log.Error(err, "Failed to delete session ConfigMap after migration",
					"namespace", cm.Namespace, "configmap", cm.Name)
				hadErrors = true
			}
		}
	}

	log.Info("Migration complete", "results", resultsMigrated, "sessions", sessionsMigrated)

	if hadErrors {
		os.Exit(1)
	}
}

// listConfigMaps returns ConfigMaps matching the given label selector, either in a
// specific namespace or across all namespaces.
func listConfigMaps(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, labelSelector string,
) ([]corev1.ConfigMap, error) {
	opts := metav1.ListOptions{LabelSelector: labelSelector}
	var allCMs []corev1.ConfigMap

	if namespace != "" {
		list, err := clientset.CoreV1().ConfigMaps(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing ConfigMaps in namespace %s: %w", namespace, err)
		}
		allCMs = list.Items
	} else {
		list, err := clientset.CoreV1().ConfigMaps("").List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing ConfigMaps across all namespaces: %w", err)
		}
		allCMs = list.Items
	}

	return allCMs, nil
}

// deleteConfigMap deletes a single ConfigMap.
func deleteConfigMap(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	return clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// extractTaskNameFromResult derives the task name from a result ConfigMap name.
// Result ConfigMaps are named "{taskName}-result".
func extractTaskNameFromResult(cmName string) string {
	return strings.TrimSuffix(cmName, "-result")
}

// extractSessionName derives the session name from a session ConfigMap name.
// Task sessions: "session-{name}" → "{name}"
// Chat sessions: "chat-session-{id}" → "chat-session-{id}" (kept as-is since that's the session ID)
func extractSessionName(cmName string) string {
	if after, ok := strings.CutPrefix(cmName, "chat-session-"); ok {
		// Chat session: the session name includes the "chat-session-" prefix stripped to just the ID
		return after
	}
	if after, ok := strings.CutPrefix(cmName, "session-"); ok {
		return after
	}
	// Fallback: use full ConfigMap name
	return cmName
}

// parseJSONL parses a JSONL transcript into SessionMessages.
func parseJSONL(data string) ([]store.SessionMessage, error) {
	var messages []store.SessionMessage
	scanner := bufio.NewScanner(strings.NewReader(data))
	// Increase buffer size for potentially large lines
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg store.SessionMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return nil, fmt.Errorf("parsing JSONL line: %w", err)
		}
		messages = append(messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning JSONL: %w", err)
	}
	return messages, nil
}
