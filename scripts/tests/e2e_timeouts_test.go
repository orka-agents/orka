package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestE2EMakeTimeoutFlags(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, goTimeout, suiteTimeout, gracePeriod string
		arguments                                  []string
	}{
		{name: "local defaults", goTimeout: "50m", suiteTimeout: "45m", gracePeriod: "2m"},
		{name: "CI overrides", goTimeout: "75m", suiteTimeout: "70m", gracePeriod: "2m",
			arguments: []string{"E2E_GO_TEST_TIMEOUT=75m", "E2E_GINKGO_TIMEOUT=70m", "E2E_GINKGO_GRACE_PERIOD=2m"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := make([]string, 0, 15+len(test.arguments))
			args = append(args,
				"--no-print-directory", "-n", "-o", "setup-test-e2e", "-o", "manifests",
				"-o", "generate", "-o", "fmt", "-o", "vet", "test-e2e", "KIND=false", "KUBECTL=false",
			)
			args = append(args, test.arguments...)
			cmd := exec.Command("make", args...)
			cmd.Dir = root
			for _, value := range os.Environ() {
				if strings.HasPrefix(value, "E2E_GO_TEST_TIMEOUT=") || strings.HasPrefix(value, "E2E_GINKGO_TIMEOUT=") ||
					strings.HasPrefix(value, "E2E_GINKGO_GRACE_PERIOD=") || strings.HasPrefix(value, "MAKEFLAGS=") {
					continue
				}
				cmd.Env = append(cmd.Env, value)
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make dry run: %v\n%s", err, output)
			}
			for _, flag := range []string{
				"-timeout " + test.goTimeout, "-ginkgo.timeout=" + test.suiteTimeout,
				"-ginkgo.grace-period=" + test.gracePeriod,
			} {
				if !strings.Contains(string(output), flag) {
					t.Fatalf("E2E invocation does not pass %s", flag)
				}
			}
		})
	}
}

func TestE2EWorkflowTimeoutHeadroom(t *testing.T) {
	for _, test := range []struct{ file, job string }{
		{"test-e2e.yml", "test-e2e"},
		{"gateway-e2e.yml", "gateway-live-e2e"},
	} {
		t.Run(test.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", test.file))
			if err != nil {
				t.Fatal(err)
			}
			var workflow struct {
				Jobs map[string]struct {
					Env      map[string]string `json:"env"`
					Strategy struct {
						Matrix struct {
							Include []struct {
								TestTimeout   string `json:"test_timeout"`
								GinkgoTimeout string `json:"ginkgo_timeout"`
							} `json:"include"`
						} `json:"matrix"`
					} `json:"strategy"`
					Steps []struct {
						Run string `json:"run"`
					} `json:"steps"`
				} `json:"jobs"`
			}
			if err := yaml.Unmarshal(data, &workflow); err != nil {
				t.Fatal(err)
			}
			job, ok := workflow.Jobs[test.job]
			if !ok {
				t.Fatal("E2E job is missing")
			}
			parse := func(value string) time.Duration {
				t.Helper()
				parsed, err := time.ParseDuration(value)
				if err != nil || parsed <= 0 {
					t.Fatalf("invalid E2E deadline %q: %v", value, err)
				}
				return parsed
			}
			grace := parse(job.Env["E2E_GINKGO_GRACE_PERIOD"])
			// Matrix lanes set their own Ginkgo and Go deadlines.
			deadlines := [][2]string{{job.Env["E2E_GINKGO_TIMEOUT"], job.Env["E2E_GO_TEST_TIMEOUT"]}}
			if len(job.Strategy.Matrix.Include) > 0 {
				deadlines = deadlines[:0]
				for _, lane := range job.Strategy.Matrix.Include {
					deadlines = append(deadlines, [2]string{lane.GinkgoTimeout, lane.TestTimeout})
				}
			}
			for _, deadline := range deadlines {
				if parse(deadline[0])+2*grace >= parse(deadline[1]) {
					t.Fatalf("Go timeout %s leaves no headroom for the interrupted spec and suite cleanup", deadline[1])
				}
			}
			if test.job == "gateway-live-e2e" {
				var command string
				for _, step := range job.Steps {
					command += step.Run
				}
				for _, flag := range []string{
					`-ginkgo.timeout "${E2E_GINKGO_TIMEOUT}"`, `-ginkgo.grace-period "${E2E_GINKGO_GRACE_PERIOD}"`,
				} {
					if !strings.Contains(command, flag) {
						t.Fatalf("Gateway E2E does not pass %s", flag)
					}
				}
			}
		})
	}
}
