/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package utils

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

const (
	defaultKindBinary  = "kind"
	defaultKindCluster = "orka-test-e2e"
)

// ConfigureAndValidateE2EKindTarget activates the isolated kubeconfig prepared
// by scripts/e2e-kind-cluster.sh and verifies its complete helper state before
// any e2e setup can mutate Kubernetes resources.
func ConfigureAndValidateE2EKindTarget() error {
	cluster := defaultKindCluster
	if value, ok := os.LookupEnv("KIND_CLUSTER"); ok && value != "" {
		cluster = value
	}
	expected := "kind-" + cluster

	projectDir, err := GetProjectDir()
	if err != nil {
		return err
	}
	stateDir := os.Getenv("E2E_CLUSTER_STATE_DIR")
	if stateDir == "" {
		stateDir = filepath.Join(projectDir, "bin", "e2e-kind-state", cluster)
	} else if !filepath.IsAbs(stateDir) {
		stateDir = filepath.Join(projectDir, stateDir)
	}
	stateDir = filepath.Clean(stateDir)
	state, err := loadE2EKindState(stateDir, cluster, expected)
	if err != nil {
		return err
	}
	if err := validateE2ERunClaim(cluster, expected, stateDir); err != nil {
		return err
	}
	if err := os.Setenv("KUBECONFIG", state.kubeconfig); err != nil {
		return fmt.Errorf("set isolated KUBECONFIG: %w", err)
	}
	if _, err := os.Stat(state.kubeconfig); err != nil {
		return fmt.Errorf("inspect isolated kubeconfig %s: %w", state.kubeconfig, err)
	}

	kubectlBinary := "kubectl"
	if value, ok := os.LookupEnv("KUBECTL"); ok && value != "" {
		kubectlBinary = value
	}
	return validateE2EKubeconfig(kubectlBinary, state.kubeconfig, expected, state.fingerprint)
}

func validateE2ERunClaim(cluster, expected, stateDir string) error {
	if os.Getenv("E2E_KIND_TARGET_READY") != "1" {
		return fmt.Errorf("e2e run is not inside a validated helper operation")
	}
	if os.Getenv("E2E_KIND_EXPECTED_CONTEXT") != expected || os.Getenv("E2E_KIND_EXPECTED_CLUSTER") != expected {
		return fmt.Errorf("e2e helper target claim does not match %q", expected)
	}
	lockRoot := os.Getenv("E2E_CLUSTER_LOCK_ROOT")
	operationToken := os.Getenv("E2E_KIND_OPERATION_TOKEN")
	if lockRoot == "" || operationToken == "" || !filepath.IsAbs(lockRoot) {
		return fmt.Errorf("e2e helper operation claim is incomplete")
	}
	cleanLockRoot := filepath.Clean(lockRoot)
	operationDir := filepath.Join(cleanLockRoot, "kind-"+cluster+".operation")
	tokenPath := filepath.Join(operationDir, "token")
	if err := requireRegularFile(tokenPath, "e2e helper operation token"); err != nil {
		return err
	}
	activeToken, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("read e2e helper operation token %s: %w", tokenPath, err)
	}
	if strings.TrimSpace(string(activeToken)) != operationToken {
		return fmt.Errorf("e2e helper operation token does not match the active cluster lock")
	}
	for _, claimPath := range []string{
		filepath.Join(operationDir, "state_dir"),
		filepath.Join(cleanLockRoot, "kind-"+cluster+".lease", "state_dir"),
	} {
		if err := requireRegularFile(claimPath, "e2e helper state claim"); err != nil {
			return err
		}
		claimedState, readErr := os.ReadFile(claimPath)
		if readErr != nil {
			return fmt.Errorf("read e2e helper state claim %s: %w", claimPath, readErr)
		}
		if filepath.Clean(strings.TrimSpace(string(claimedState))) != stateDir {
			return fmt.Errorf("e2e helper operation is bound to a different state directory")
		}
	}
	return nil
}

func requireRegularFile(path, description string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s %s: %w", description, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file: %s", description, path)
	}
	return nil
}

type e2eKindState struct {
	kubeconfig  string
	fingerprint string
}

func loadE2EKindState(stateDir, cluster, expected string) (e2eKindState, error) {
	if strings.ContainsRune(stateDir, os.PathListSeparator) {
		return e2eKindState{}, fmt.Errorf("e2e cluster state path contains the KUBECONFIG path-list separator")
	}
	info, err := os.Stat(stateDir)
	if os.IsNotExist(err) {
		return e2eKindState{}, fmt.Errorf("no isolated e2e cluster state exists at %s", stateDir)
	}
	if err != nil {
		return e2eKindState{}, fmt.Errorf("inspect e2e cluster state %s: %w", stateDir, err)
	}
	if !info.IsDir() {
		return e2eKindState{}, fmt.Errorf("e2e cluster state path is not a directory: %s", stateDir)
	}

	values := make(map[string]string, 7)
	for _, name := range []string{"version", "status", "cluster", "context", "kubeconfig", "created", "fingerprint"} {
		value, readErr := readStateValue(stateDir, name)
		if readErr != nil {
			return e2eKindState{}, readErr
		}
		values[name] = value
	}
	if values["version"] != "1" {
		return e2eKindState{}, fmt.Errorf("unsupported e2e cluster state version %q", values["version"])
	}
	if values["status"] != "ready" {
		return e2eKindState{}, fmt.Errorf("e2e cluster state is not ready: %s", values["status"])
	}
	if values["cluster"] != cluster {
		return e2eKindState{}, fmt.Errorf(
			"e2e cluster state targets %q, not requested cluster %q", values["cluster"], cluster,
		)
	}
	if values["context"] != expected {
		return e2eKindState{}, fmt.Errorf(
			"e2e cluster state context %q does not equal target %q", values["context"], expected,
		)
	}
	if values["created"] != "0" && values["created"] != "1" {
		return e2eKindState{}, fmt.Errorf("invalid e2e cluster ownership value %q", values["created"])
	}
	canonicalKubeconfig := filepath.Join(stateDir, "target.kubeconfig")
	if values["kubeconfig"] != canonicalKubeconfig {
		return e2eKindState{}, fmt.Errorf(
			"e2e cluster state kubeconfig path %q is not canonical %q",
			values["kubeconfig"], canonicalKubeconfig,
		)
	}
	kubeconfigInfo, err := os.Lstat(canonicalKubeconfig)
	if err != nil {
		return e2eKindState{}, fmt.Errorf("inspect helper-owned kubeconfig %s: %w", canonicalKubeconfig, err)
	}
	if !kubeconfigInfo.Mode().IsRegular() {
		return e2eKindState{}, fmt.Errorf("helper-owned kubeconfig is not a regular file: %s", canonicalKubeconfig)
	}
	if values["fingerprint"] == "" {
		return e2eKindState{}, fmt.Errorf("e2e cluster state has an empty fingerprint: %s", stateDir)
	}

	return e2eKindState{
		kubeconfig:  values["kubeconfig"],
		fingerprint: values["fingerprint"],
	}, nil
}

func validateE2EKubeconfig(binary, kubeconfig, expected, fingerprint string) error {
	currentContext, err := kubectlConfigOutput(binary, kubeconfig, "config", "current-context")
	if err != nil {
		return err
	}
	currentCluster, err := kubectlConfigOutput(
		binary,
		kubeconfig,
		"config", "view", "--minify", "-o", "jsonpath={.contexts[0].context.cluster}",
	)
	if err != nil {
		return err
	}
	if currentContext != expected {
		return fmt.Errorf("refusing e2e run: kubeconfig context %q does not equal target %q", currentContext, expected)
	}
	if currentCluster != expected {
		return fmt.Errorf("refusing e2e run: kubeconfig cluster %q does not equal target %q", currentCluster, expected)
	}

	identityMaterial, err := kubectlConfigOutput(
		binary,
		kubeconfig,
		"config", "view", "--raw", "--minify", "-o",
		`jsonpath={.clusters[0].cluster.server}{"\n"}{.clusters[0].cluster.certificate-authority-data}`,
	)
	if err != nil {
		return err
	}
	identityParts := strings.SplitN(identityMaterial, "\n", 2)
	if len(identityParts) != 2 || identityParts[0] == "" || identityParts[1] == "" {
		return fmt.Errorf("isolated kubeconfig cluster identity is incomplete")
	}
	actualFingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(identityMaterial)))
	if actualFingerprint != fingerprint {
		return fmt.Errorf("refusing e2e run: kubeconfig identity does not match helper state")
	}
	return nil
}

func readStateValue(stateDir, name string) (string, error) {
	path := filepath.Join(stateDir, name)
	value, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read e2e cluster state %s: %w", path, err)
	}
	return strings.TrimSpace(string(value)), nil
}

func kubectlConfigOutput(binary, kubeconfig string, args ...string) (string, error) {
	commandArgs := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.Command(binary, commandArgs...)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("kubectl config validation failed: %s: %w", stderr, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}
