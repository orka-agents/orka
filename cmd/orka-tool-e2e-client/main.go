/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	namespace := strings.TrimSpace(os.Getenv("ORKA_TOOL_NAMESPACE"))
	if namespace == "" {
		namespace = "default"
	}
	name := strings.TrimSpace(os.Getenv("ORKA_TOOL_NAME"))
	if name == "" {
		return fmt.Errorf("ORKA_TOOL_NAME is required")
	}
	args := strings.TrimSpace(os.Getenv("ORKA_TOOL_ARGS"))
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return fmt.Errorf("ORKA_TOOL_ARGS must be valid JSON")
	}

	if os.Getenv(workerenv.TaskNamespace) == "" {
		if err := os.Setenv(workerenv.TaskNamespace, namespace); err != nil {
			return err
		}
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add Orka scheme: %w", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add core scheme: %w", err)
	}
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	tool := &corev1alpha1.Tool{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, tool); err != nil {
		return fmt.Errorf("get tool %s/%s: %w", namespace, name, err)
	}
	if !tool.Status.Available {
		return fmt.Errorf("tool %s/%s is not available: %s", namespace, name, tool.Status.Error)
	}

	execCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := worker.NewToolExecutor().Execute(execCtx, tool, json.RawMessage(args))
	if err != nil {
		return fmt.Errorf("execute tool %s/%s: %w", namespace, name, err)
	}
	fmt.Println(result)

	if expected := strings.TrimSpace(os.Getenv("ORKA_TOOL_EXPECT_RESULT")); expected != "" && result != expected {
		return fmt.Errorf("tool result = %q, want %q", result, expected)
	}
	return nil
}
