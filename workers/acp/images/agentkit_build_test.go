package main

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This exercises composition, not executable AgentKit behavior: the scratch
// fixture deliberately has neither a shell nor a runnable agentkit-serve.
func TestAgentKitDockerBuildWithShelllessRuntime(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("Docker integration test requires docker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	probe, stopProbe := context.WithTimeout(ctx, 20*time.Second)
	output, err := exec.CommandContext(probe, "docker", "info").CombinedOutput()
	stopProbe()
	if err != nil {
		t.Skipf("Docker daemon unavailable: %v\n%s", err, output)
	}
	docker := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return strings.TrimSpace(string(output))
	}
	namespace := fmt.Sprintf("orka-agentkit-build-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	registry, container := namespace+"-registry", namespace+"-result"
	result, fixture := namespace+":result", ""
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		commands := [][]string{{"rm", "-fv", container, registry}, {"image", "rm", result}}
		if fixture != "" {
			commands[1] = append(commands[1], fixture)
		}
		for _, args := range commands {
			if output, err := exec.CommandContext(cleanup, "docker", args...).CombinedOutput(); err != nil {
				t.Logf("cleanup docker %s: %v\n%s", strings.Join(args, " "), err, output)
			}
		}
	})
	docker("run", "-d", "--name", registry, "-p", "127.0.0.1::5000",
		"docker.io/library/registry:3@sha256:1be55279f18a2fe1a74edf2664cac61c1bea305b7b4642dab412e7affdcb3e33")
	address := docker("port", registry, "5000/tcp")
	if !strings.HasPrefix(address, "127.0.0.1:") || strings.Contains(address, "\n") {
		t.Fatalf("registry must expose only loopback, got %q", address)
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	ready := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && ctx.Err() == nil; {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/v2/", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == http.StatusOK
		}
		if ready {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("loopback registry did not become ready\n%s", docker("logs", registry))
	}
	fixture = address + "/" + namespace + ":fixture"
	temp := t.TempDir()
	for name, contents := range map[string]string{
		"Dockerfile": "FROM scratch\n" +
			"COPY --chmod=0755 serve /opt/agentkit/bin/agentkit-serve\n" +
			"COPY agent.yaml /agent/agent.yaml\n",
		"serve":      "composition-only placeholder; not an executable runtime\n",
		"agent.yaml": "name: composition-fixture\n",
	} {
		if err := os.WriteFile(filepath.Join(temp, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	docker("build", "--progress=plain", "-t", fixture, temp)
	docker("push", fixture)
	frozen := docker("image", "inspect", "--format", "{{index .RepoDigests 0}}", fixture)
	repository, digest, ok := strings.Cut(frozen, "@")
	if !ok || repository != address+"/"+namespace || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("unexpected loopback fixture RepoDigest %q", frozen)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	docker("build", "--progress=plain", "-f", filepath.Join(root, "workers/acp/images/agentkit/Dockerfile"),
		"--build-arg", "AGENTKIT_RUNTIME_IMAGE="+frozen, "--build-arg", "AGENTKIT_ADAPTER_DIGEST="+digest, "-t", result, root)
	metadata := docker("image", "inspect", "--format", "{{.Config.User}} {{json .Config.Entrypoint}}", result)
	if metadata != `0:0 ["/usr/local/bin/orka-acp-runtime"]` {
		t.Fatalf("unexpected final image user/entrypoint: %s", metadata)
	}
	docker("create", "--name", container, result)
	archive := filepath.Join(temp, "rootfs.tar")
	docker("export", "--output", archive, container)
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close exported rootfs: %v", err)
		}
	}()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(header.Name, "./") == "bin/sh" {
			t.Fatal("composed scratch runtime unexpectedly contains /bin/sh")
		}
	}
}
