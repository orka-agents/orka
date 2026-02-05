/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Result represents the execution result
type Result struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration string `json:"duration"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// Get configuration from environment
	taskName := os.Getenv("MERCAN_TASK_NAME")
	taskNamespace := os.Getenv("MERCAN_TASK_NAMESPACE")
	resultConfigMap := os.Getenv("MERCAN_RESULT_CONFIGMAP")

	// Get command from arguments or environment
	var command []string
	if len(os.Args) > 1 {
		command = os.Args[1:]
	} else {
		cmdStr := os.Getenv("MERCAN_COMMAND")
		if cmdStr == "" {
			return fmt.Errorf("no command specified")
		}
		command = strings.Fields(cmdStr)
	}

	if len(command) == 0 {
		return fmt.Errorf("command cannot be empty")
	}

	fmt.Printf("Executing command: %v\n", command)

	// Execute the command
	start := time.Now()
	result := executeCommand(ctx, command)
	result.Duration = time.Since(start).String()

	// Print output
	if result.Stdout != "" {
		fmt.Printf("stdout:\n%s\n", result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprintf(os.Stderr, "stderr:\n%s\n", result.Stderr)
	}

	// Write result to ConfigMap
	if resultConfigMap != "" {
		if err := writeResult(ctx, taskNamespace, resultConfigMap, result); err != nil {
			return fmt.Errorf("failed to write result: %w", err)
		}
	}

	fmt.Printf("Task %s/%s completed with exit code %d\n", taskNamespace, taskName, result.ExitCode)

	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}

	return nil
}

// executeCommand runs the command and captures output
func executeCommand(ctx context.Context, command []string) Result {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()

	err := cmd.Run()

	result := Result{
		ExitCode: 0,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			if result.Stderr == "" {
				result.Stderr = err.Error()
			}
		}
	}

	return result
}

// writeResult writes the result to a ConfigMap
func writeResult(ctx context.Context, namespace, name string, result Result) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Prepare result content
	content := result.Stdout
	if result.ExitCode != 0 && result.Stderr != "" {
		content = fmt.Sprintf("stdout:\n%s\n\nstderr:\n%s\n\nexit_code: %d",
			result.Stdout, result.Stderr, result.ExitCode)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"mercan.ai/result": "true",
			},
		},
		Data: map[string]string{
			"result":    content,
			"exit_code": fmt.Sprintf("%d", result.ExitCode),
			"duration":  result.Duration,
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		// Try update if create fails
		_, err = clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}

	return err
}
