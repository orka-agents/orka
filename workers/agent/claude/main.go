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
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultMaxTurns   = 50
	workspaceDir      = "/workspace"
	maxResultBytes    = 900 * 1024 // 900 KB to stay under ConfigMap 1MB limit
	defaultClaudePath = "claude"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Set up context with signal handling for graceful shutdown
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGTERM, syscall.SIGINT,
	)
	defer cancel()

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Clone git repo if configured
	if cfg.gitRepo != "" {
		if err := cloneRepo(ctx, cfg); err != nil {
			return fmt.Errorf("git clone failed: %w", err)
		}
	}

	// Execute Claude CLI
	result, err := executeClaude(ctx, cfg)
	if err != nil {
		// On failure, still try to write partial result
		writeErr := writeResult(
			ctx, cfg.taskNamespace, cfg.resultConfigMap,
			fmt.Sprintf("Error: %v\n\n%s", err, result),
		)
		if writeErr != nil {
			fmt.Fprintf(os.Stderr, "failed to write error result: %v\n", writeErr)
		}
		return fmt.Errorf("claude execution failed: %w", err)
	}

	// Write result to ConfigMap
	if err := writeResult(ctx, cfg.taskNamespace, cfg.resultConfigMap, result); err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}

	fmt.Printf("Task %s/%s completed successfully\n", cfg.taskNamespace, cfg.taskName)
	return nil
}

// config holds all worker configuration from environment variables.
type config struct {
	taskName        string
	taskNamespace   string
	resultConfigMap string
	prompt          string
	model           string
	systemPrompt    string
	maxTurns        int
	allowedTools    []string
	disallowedTools []string
	allowBash       bool
	gitRepo         string
	gitBranch       string
	gitRef          string
	subPath         string
	timeoutSeconds  int
}

// loadConfig reads and validates configuration from environment variables.
func loadConfig() (*config, error) {
	cfg := &config{
		taskName:        os.Getenv("MERCAN_TASK_NAME"),
		taskNamespace:   os.Getenv("MERCAN_TASK_NAMESPACE"),
		resultConfigMap: os.Getenv("MERCAN_RESULT_CONFIGMAP"),
		prompt:          os.Getenv("MERCAN_PROMPT"),
		model:           os.Getenv("MERCAN_MODEL"),
		systemPrompt:    os.Getenv("MERCAN_SYSTEM_PROMPT"),
		gitRepo:         os.Getenv("MERCAN_GIT_REPO"),
		gitBranch:       os.Getenv("MERCAN_GIT_BRANCH"),
		gitRef:          os.Getenv("MERCAN_GIT_REF"),
		subPath:         os.Getenv("MERCAN_WORKSPACE_SUBPATH"),
		maxTurns:        defaultMaxTurns,
	}

	if cfg.prompt == "" {
		return nil, fmt.Errorf("MERCAN_PROMPT is required")
	}

	if v := os.Getenv("MERCAN_MAX_TURNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid MERCAN_MAX_TURNS: %w", err)
		}
		cfg.maxTurns = n
	}

	if v := os.Getenv("MERCAN_ALLOWED_TOOLS"); v != "" {
		cfg.allowedTools = strings.Split(v, ",")
	}
	if v := os.Getenv("MERCAN_DISALLOWED_TOOLS"); v != "" {
		cfg.disallowedTools = strings.Split(v, ",")
	}
	cfg.allowBash = os.Getenv("MERCAN_ALLOW_BASH") == "true"

	if v := os.Getenv("MERCAN_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid MERCAN_TIMEOUT_SECONDS: %w", err)
		}
		cfg.timeoutSeconds = n
	}

	return cfg, nil
}

// cloneRepo clones the configured git repository into the workspace.
func cloneRepo(ctx context.Context, cfg *config) error {
	fmt.Printf("Cloning %s into %s\n", cfg.gitRepo, workspaceDir)

	args := []string{"clone"}

	if cfg.gitBranch != "" {
		args = append(args, "--branch", cfg.gitBranch)
	}

	args = append(args, "--single-branch", "--depth=1", cfg.gitRepo, workspaceDir)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Set up git credentials if available
	configureGitAuth(cmd)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	// Checkout specific ref if provided (overrides branch)
	if cfg.gitRef != "" {
		fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin", cfg.gitRef)
		fetchCmd.Dir = workspaceDir
		fetchCmd.Stdout = os.Stdout
		fetchCmd.Stderr = os.Stderr
		if err := fetchCmd.Run(); err != nil {
			return fmt.Errorf("git fetch ref failed: %w", err)
		}

		checkoutCmd := exec.CommandContext(ctx, "git", "checkout", cfg.gitRef)
		checkoutCmd.Dir = workspaceDir
		checkoutCmd.Stdout = os.Stdout
		checkoutCmd.Stderr = os.Stderr
		if err := checkoutCmd.Run(); err != nil {
			return fmt.Errorf("git checkout ref failed: %w", err)
		}
	}

	return nil
}

// configureGitAuth sets up git credential helpers from mounted secrets.
func configureGitAuth(cmd *exec.Cmd) {
	env := os.Environ()

	// Check for git token in mounted secrets
	tokenPaths := []string{
		"/secrets/git/token",
		"/secrets/git/password",
	}
	for _, path := range tokenPaths {
		if data, err := os.ReadFile(path); err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				// Use GIT_ASKPASS to provide the token
				env = append(env,
					fmt.Sprintf("GIT_TOKEN=%s", token),
					"GIT_ASKPASS=/bin/echo-token",
				)
				break
			}
		}
	}

	// Check for username
	if data, err := os.ReadFile("/secrets/git/username"); err == nil {
		username := strings.TrimSpace(string(data))
		if username != "" {
			env = append(env, fmt.Sprintf("GIT_USERNAME=%s", username))
		}
	}

	cmd.Env = env
}

// executeClaude invokes the Claude Code CLI and returns its output.
func executeClaude(ctx context.Context, cfg *config) (string, error) {
	args := buildClaudeArgs(cfg)

	fmt.Printf("Executing: claude %s\n", strings.Join(args, " "))

	// Apply timeout if configured
	execCtx := ctx
	if cfg.timeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		execCtx, timeoutCancel = context.WithTimeout(
			ctx, time.Duration(cfg.timeoutSeconds)*time.Second,
		)
		defer timeoutCancel()
	}

	cmd := exec.CommandContext(execCtx, claudePath(), args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)

	// Set working directory
	dir := workspaceDir
	if cfg.subPath != "" {
		dir = workspaceDir + "/" + cfg.subPath
	}
	cmd.Dir = dir

	// Inherit environment (includes ANTHROPIC_API_KEY from secret mount)
	cmd.Env = os.Environ()

	// Ensure HOME is writable for CLI config/cache
	cmd.Env = append(cmd.Env, "HOME=/home/worker")

	if err := cmd.Run(); err != nil {
		// Include stderr in the error for debugging
		stderrStr := stderr.String()
		if stderrStr != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, stderrStr)
		}
		return stdout.String(), err
	}

	return stdout.String(), nil
}

// buildClaudeArgs constructs the CLI arguments for the claude command.
func buildClaudeArgs(cfg *config) []string {
	args := []string{
		"--print",   // non-interactive: print output and exit
		"--verbose", // verbose output for logging
	}

	// Model
	if cfg.model != "" {
		args = append(args, "--model", cfg.model)
	}

	// System prompt
	if cfg.systemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.systemPrompt)
	}

	// Max turns
	args = append(args, "--max-turns", strconv.Itoa(cfg.maxTurns))

	// Tool permissions
	for _, tool := range cfg.allowedTools {
		args = append(args, "--allowedTools", strings.TrimSpace(tool))
	}
	for _, tool := range cfg.disallowedTools {
		args = append(args, "--disallowedTools", strings.TrimSpace(tool))
	}

	// Prompt (use -p flag to avoid ambiguity with variadic tool flags)
	args = append(args, "-p", cfg.prompt)

	return args
}

// claudePath returns the path to the claude CLI binary.
func claudePath() string {
	if p := os.Getenv("CLAUDE_CLI_PATH"); p != "" {
		return p
	}
	return defaultClaudePath
}

// writeResult writes the task result to a Kubernetes ConfigMap.
func writeResult(ctx context.Context, namespace, name, result string) error {
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Truncate result if too large for ConfigMap
	if len(result) > maxResultBytes {
		truncMsg := "\n\n--- [result truncated: exceeded ConfigMap size limit] ---"
		result = result[:maxResultBytes-len(truncMsg)] + truncMsg
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
			"result": result,
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(
		ctx, cm, metav1.CreateOptions{},
	)
	if err != nil {
		// Try update if create fails (idempotent)
		_, err = clientset.CoreV1().ConfigMaps(namespace).Update(
			ctx, cm, metav1.UpdateOptions{},
		)
	}

	return err
}
