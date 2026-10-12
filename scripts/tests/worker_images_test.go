package scriptstest

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestE2ERegistryPinnedWorkerImages(t *testing.T) {
	fixture := `
set -Eeuo pipefail
manager_image=controller:test
worker_image=worker:test
general_worker_image=worker:test
fake_runtime_image=runtime:test
kind_cluster=test-cluster
fixture_image=connectors-fixture:test
publisher_image=publisher:test
cluster=test-cluster
log() { :; }
docker() {
  if [[ "$1" == build && "$2" == -t && "$3" == worker:test ]]; then
    touch "$TEST_DIR/worker-built"
  fi
}
run() { "$@"; }
kind() { :; }
orka_e2e_bootstrap_admission_tls() { :; }
build_fake_runtime() { :; }
make() {
  case "$1" in
    docker-build-ai-worker) touch "$TEST_DIR/worker-built" ;;
    deploy) printf '%s\n' "$@" ;;
  esac
}
orka_kind_registry_push() {
  if [[ "$2" == "orka/$TEST_WORKER_NAME" ]]; then
    [[ "$1" == "$worker_image" && -f "$TEST_DIR/worker-built" ]] || return 1
    [[ "$FAIL_WORKER_PUSH" != 1 ]] || return 77
  fi
  printf 'registry.test:5000/%s@sha256:%s\n' "$2" "$TEST_DIGEST"
}
`
	for _, e2e := range []struct {
		name, script, start, end, workerName, input string
		wrap                                        bool
	}{
		{
			name: "connectors", script: "live-connectors-e2e.sh",
			start: `log "Building and loading images"`, end: "\nkubectl wait --for=condition=Established",
			workerName: "ai-worker", input: "AI_WORKER_IMG",
		},
		{
			name: "security scan", script: "security-scan-e2e.sh",
			start:      `  log "Building general worker image ${general_worker_image}"`,
			end:        `  log "Bootstrapping test-only admission TLS"`,
			workerName: "general-worker", input: "GENERAL_WORKER_IMG", wrap: true,
		},
	} {
		source := workspaceScript(t, e2e.script)
		setup := sourceSection(t, source, e2e.start, e2e.end)
		if e2e.wrap {
			setup = "setup_images() {\n" + setup + "\n}\nsetup_images\n" +
				"printf 'GENERAL_WORKER_IMG=%s\\n' \"$general_worker_image\"\n"
		}
		for _, failPush := range []bool{false, true} {
			name := e2e.name + "/published worker"
			if failPush {
				name = e2e.name + "/failed worker publication"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, "bash", "-c", fixture+setup)
				fail := "0"
				if failPush {
					fail = "1"
				}
				digest := strings.Repeat("a", 64)
				command.Env = append(os.Environ(), "TEST_DIR="+t.TempDir(), "TEST_DIGEST="+digest,
					"FAIL_WORKER_PUSH="+fail, "TEST_WORKER_NAME="+e2e.workerName)
				output, err := command.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatal("image setup fixture timed out")
				}
				arguments := strings.Fields(string(output))
				if failPush {
					if err == nil || strings.Contains(string(output), e2e.input+"=") {
						t.Fatalf("failed image publication must prevent deployment: %v, %s", err, output)
					}
					return
				}
				if err != nil {
					t.Fatalf("image setup failed: %v, %s", err, output)
				}
				want := e2e.input + "=registry.test:5000/orka/" + e2e.workerName + "@sha256:" + digest
				if !slices.Contains(arguments, want) {
					t.Fatalf("deployment did not receive the published worker digest: %s", output)
				}
			})
		}
	}
}
