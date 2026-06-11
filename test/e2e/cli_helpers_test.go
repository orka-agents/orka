//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/sozercan/orka/test/utils"
)

var (
	buildOrkaCLIOnce sync.Once
	builtOrkaCLIPath string
	buildOrkaCLIErr  error
)

type cliResult struct {
	Args   []string
	Stdout string
	Stderr string
	Err    error
}

func buildOrkaCLI() string {
	GinkgoHelper()
	buildOrkaCLIOnce.Do(func() {
		projectDir, err := utils.GetProjectDir()
		if err != nil {
			buildOrkaCLIErr = err
			return
		}
		cmd := exec.Command("make", "build-cli")
		cmd.Dir = projectDir
		cmd.Env = append(os.Environ(), "GO111MODULE=on")
		output, err := cmd.CombinedOutput()
		if err != nil {
			buildOrkaCLIErr = fmt.Errorf("make build-cli failed: %w\n%s", err, strings.TrimSpace(string(output)))
			return
		}
		builtOrkaCLIPath = filepath.Join(projectDir, "bin", "orka")
		if st, statErr := os.Stat(builtOrkaCLIPath); statErr != nil {
			buildOrkaCLIErr = statErr
		} else if st.IsDir() {
			buildOrkaCLIErr = fmt.Errorf("%s is a directory", builtOrkaCLIPath)
		}
	})
	Expect(buildOrkaCLIErr).NotTo(HaveOccurred())
	Expect(builtOrkaCLIPath).NotTo(BeEmpty())
	return builtOrkaCLIPath
}

func newIsolatedCLIHome(apiBaseURL, token string) string {
	GinkgoHelper()
	home, err := os.MkdirTemp("", "orka-cli-e2e-home-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_ = os.RemoveAll(home)
	})
	writeCLIConfig(home, apiBaseURL, namespace, token)
	return home
}

func newIsolatedCLIHomeWithNamespace(apiBaseURL, configNamespace, token string) string {
	GinkgoHelper()
	home, err := os.MkdirTemp("", "orka-cli-e2e-home-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_ = os.RemoveAll(home)
	})
	writeCLIConfig(home, apiBaseURL, configNamespace, token)
	return home
}

func writeCLIConfig(home, apiBaseURL, configNamespace, token string) {
	GinkgoHelper()
	configDir := filepath.Join(home, ".orka")
	Expect(os.MkdirAll(configDir, 0o700)).To(Succeed())
	body := fmt.Sprintf("server: %s\nnamespace: %s\ntoken: %s\n", apiBaseURL, configNamespace, token)
	Expect(os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(body), 0o600)).To(Succeed())
}

func runOrka(home string, args ...string) cliResult {
	return runOrkaWithTimeout(5*time.Minute, home, args...)
}

func runOrkaWithTimeout(timeout time.Duration, home string, args ...string) cliResult {
	GinkgoHelper()
	binary := buildOrkaCLI()
	cmd := exec.Command(binary, args...)
	cmd.Env = isolatedCLIEnv(home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := runCommandWithTimeout(cmd, timeout)
	return cliResult{
		Args:   append([]string{}, args...),
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Err:    err,
	}
}

func runCommandWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	GinkgoHelper()
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("command timed out after %s: %w", timeout, err)
			}
		case <-time.After(5 * time.Second):
		}
		return fmt.Errorf("command timed out after %s", timeout)
	}
}

func isolatedCLIEnv(home string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case "HOME":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "HOME="+home, "GO111MODULE=on")
	return env
}

func expectOrkaSuccess(result cliResult, forbidden ...string) {
	GinkgoHelper()
	expectNoSensitiveOutput(result, forbidden...)
	if result.Err != nil {
		Fail(fmt.Sprintf("orka %s failed: %v\n%s", redactedCLIArgs(result, forbidden...), result.Err, redactedCLIOutput(result, forbidden...)), 1)
	}
}

func expectOrkaFailure(result cliResult, forbidden ...string) {
	GinkgoHelper()
	expectNoSensitiveOutput(result, forbidden...)
	if result.Err == nil {
		Fail(fmt.Sprintf("orka %s succeeded unexpectedly\n%s", redactedCLIArgs(result, forbidden...), redactedCLIOutput(result, forbidden...)), 1)
	}
}

func expectNoSensitiveOutput(result cliResult, forbidden ...string) {
	GinkgoHelper()
	combined := result.Stdout + result.Stderr
	for _, secret := range forbidden {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		if strings.Contains(combined, secret) {
			Fail(fmt.Sprintf(
				"orka output contained sensitive value digest=%s\n%s",
				sensitiveDigest(secret),
				redactedCLIOutput(result, forbidden...),
			), 1)
		}
	}
}

func sensitiveDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func redactedCLIArgs(result cliResult, forbidden ...string) string {
	return redactSensitive(strings.Join(result.Args, " "), forbidden...)
}

func redactedCLIOutput(result cliResult, forbidden ...string) string {
	stdout := redactSensitive(result.Stdout, forbidden...)
	stderr := redactSensitive(result.Stderr, forbidden...)
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", strings.TrimSpace(stdout), strings.TrimSpace(stderr))
}

func redactSensitive(value string, forbidden ...string) string {
	redacted := value
	for _, secret := range forbidden {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, secret, "<redacted:"+sensitiveDigest(secret)+">")
	}
	redacted = jwtLikeRegexp.ReplaceAllString(redacted, "<redacted-jwt>")
	return redacted
}

var jwtLikeRegexp = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func expectJSONOutput(output string) any {
	GinkgoHelper()
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		Fail(fmt.Sprintf("expected valid JSON output: %v\n%s", err, truncateForLog(output, 1024)), 1)
	}
	return decoded
}

func expectJSONObject(output string) map[string]any {
	GinkgoHelper()
	decoded := expectJSONOutput(output)
	object, ok := decoded.(map[string]any)
	Expect(ok).To(BeTrue(), "expected JSON object, got %T", decoded)
	return object
}

func expectYAMLOutput(output string) map[string]any {
	GinkgoHelper()
	var decoded map[string]any
	if err := yaml.Unmarshal([]byte(output), &decoded); err != nil {
		Fail(fmt.Sprintf("expected valid YAML output: %v\n%s", err, truncateForLog(output, 1024)), 1)
	}
	return decoded
}

func expectListContainsName(decoded any, name string) {
	GinkgoHelper()
	for _, item := range extractListItems(decoded) {
		if itemName(item) == name {
			return
		}
	}
	Fail(fmt.Sprintf("expected list output to contain resource %q", name), 1)
}

func extractListItems(decoded any) []any {
	switch v := decoded.(type) {
	case []any:
		return v
	case map[string]any:
		for _, key := range []string{"items", "data"} {
			if items, ok := v[key].([]any); ok {
				return items
			}
		}
	}
	return nil
}

func itemName(item any) string {
	m, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	if name, _ := m["name"].(string); name != "" {
		return name
	}
	if id, _ := m["id"].(string); id != "" {
		return id
	}
	metadata, _ := m["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return name
}

func nestedStringFromMap(m map[string]any, path ...string) string {
	var cur any = m
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[key]
	}
	value, _ := cur.(string)
	return value
}

func writeTempManifest(dir, name, body string) string {
	GinkgoHelper()
	path := filepath.Join(dir, name)
	Expect(os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600)).To(Succeed())
	return path
}

func runSuccessfulOrka(home string, forbidden []string, args ...string) cliResult {
	GinkgoHelper()
	result := runOrka(home, args...)
	expectOrkaSuccess(result, forbidden...)
	return result
}

func expectListDoesNotContainName(decoded any, name string) {
	GinkgoHelper()
	for _, item := range extractListItems(decoded) {
		if itemName(item) == name {
			Fail(fmt.Sprintf("expected list output not to contain resource %q", name), 1)
		}
	}
}

func deleteK8sResource(kind, name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	cmd := exec.Command("kubectl", "delete", kind, name, "-n", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

func k8sResourceExists(kind, name string) bool {
	cmd := exec.Command("kubectl", "get", kind, name, "-n", namespace, "--ignore-not-found")
	output, err := utils.Run(cmd)
	return err == nil && strings.TrimSpace(output) != ""
}

func nestedNumberFromMap(m map[string]any, path ...string) float64 {
	var cur any = m
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = obj[key]
	}
	value, _ := cur.(float64)
	return value
}

func nestedBoolFromMap(m map[string]any, path ...string) bool {
	var cur any = m
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur = obj[key]
	}
	value, _ := cur.(bool)
	return value
}
