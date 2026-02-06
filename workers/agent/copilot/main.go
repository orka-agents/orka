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
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultMaxTurns    = 50
	workspaceDir       = "/workspace"
	maxResultBytes     = 900 * 1024 // 900 KB to stay under ConfigMap 1MB limit
	defaultCopilotPath = "copilot"
	defaultTimeout     = 10 * time.Minute
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

	// Execute Copilot SDK session
	result, err := executeCopilot(ctx, cfg)
	if err != nil {
		// On failure, still try to write partial result
		writeErr := writeResult(
			ctx, cfg.taskNamespace, cfg.resultConfigMap,
			fmt.Sprintf("Error: %v\n\n%s", err, result),
		)
		if writeErr != nil {
			fmt.Fprintf(os.Stderr, "failed to write error result: %v\n", writeErr)
		}
		return fmt.Errorf("copilot execution failed: %w", err)
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

// buildSessionConfig constructs a Copilot SDK SessionConfig from the worker config.
func buildSessionConfig(cfg *config) *copilot.SessionConfig {
	dir := workspaceDir
	if cfg.subPath != "" {
		dir = workspaceDir + "/" + cfg.subPath
	}

	sessionCfg := &copilot.SessionConfig{
		Model:            cfg.model,
		WorkingDirectory: dir,
		// Auto-approve all permission requests for autonomous operation
		OnPermissionRequest: func(_ copilot.PermissionRequest, _ copilot.PermissionInvocation) (copilot.PermissionRequestResult, error) {
			return copilot.PermissionRequestResult{Kind: "allow"}, nil
		},
	}

	if cfg.systemPrompt != "" {
		sessionCfg.SystemMessage = &copilot.SystemMessageConfig{
			Mode:    "append",
			Content: cfg.systemPrompt,
		}
	}

	if len(cfg.allowedTools) > 0 {
		tools := make([]string, len(cfg.allowedTools))
		for i, t := range cfg.allowedTools {
			tools[i] = strings.TrimSpace(t)
		}
		sessionCfg.AvailableTools = tools
	}

	if len(cfg.disallowedTools) > 0 {
		tools := make([]string, len(cfg.disallowedTools))
		for i, t := range cfg.disallowedTools {
			tools[i] = strings.TrimSpace(t)
		}
		sessionCfg.ExcludedTools = tools
	}

	return sessionCfg
}

// executeCopilot runs the Copilot SDK session and returns the result text.
func executeCopilot(ctx context.Context, cfg *config) (string, error) {
	// Apply timeout if configured
	execCtx := ctx
	if cfg.timeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		execCtx, timeoutCancel = context.WithTimeout(
			ctx, time.Duration(cfg.timeoutSeconds)*time.Second,
		)
		defer timeoutCancel()
	}

	// Create and start the Copilot client
	opts := &copilot.ClientOptions{
		CLIPath: copilotCLIPath(),
		Cwd:     workspaceDir,
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		opts.GithubToken = token
	}
	client := copilot.NewClient(opts)
	if err := client.Start(execCtx); err != nil {
		return "", fmt.Errorf("failed to start copilot client: %w", err)
	}
	defer func() {
		if err := client.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to stop copilot client: %v\n", err)
		}
	}()

	// Build session config and create session
	sessionCfg := buildSessionConfig(cfg)

	fmt.Printf("Creating Copilot session (model=%s, workspace=%s)\n",
		cfg.model, sessionCfg.WorkingDirectory)

	session, err := client.CreateSession(execCtx, sessionCfg)
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}

	// Determine the wait timeout for SendAndWait
	timeout := defaultTimeout
	if cfg.timeoutSeconds > 0 {
		timeout = time.Duration(cfg.timeoutSeconds) * time.Second
	}

	// Send the prompt and wait for completion
	fmt.Printf("Sending prompt (maxTurns=%d, timeout=%s)\n", cfg.maxTurns, timeout)

	response, err := session.SendAndWait(execCtx, copilot.MessageOptions{
		Prompt: cfg.prompt,
	})
	if err != nil {
		return "", fmt.Errorf("send and wait failed: %w", err)
	}

	// Extract result text from the response event
	return extractResult(response), nil
}

// extractResult extracts the text content from a session event response.
func extractResult(event *copilot.SessionEvent) string {
	if event == nil {
		return ""
	}

	// Try Result.Content first (structured result from agent completion)
	if event.Data.Result != nil && event.Data.Result.Content != "" {
		return event.Data.Result.Content
	}

	// Fall back to direct Content field (assistant message)
	if event.Data.Content != nil {
		return *event.Data.Content
	}

	return ""
}

// copilotCLIPath returns the path to the copilot CLI binary.
func copilotCLIPath() string {
	if p := os.Getenv("COPILOT_CLI_PATH"); p != "" {
		return p
	}
	return defaultCopilotPath
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
