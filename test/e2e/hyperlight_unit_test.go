//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/test/utils"
)

func TestHyperlightControllerArgsPreserveFlagsAndReplaceDeviceGroup(t *testing.T) {
	original := []string{
		"--leader-elect", "--hyperlight-device-gid=1", "--health-probe-bind-address=:8081",
		"--hyperlight-device-gid", "2",
	}
	want := []string{"--leader-elect", "--health-probe-bind-address=:8081", "--hyperlight-device-gid=994"}
	got := hyperlightControllerArgs(original, 994)
	if !reflect.DeepEqual(got, want) || original[1] != "--hyperlight-device-gid=1" {
		t.Fatalf("arguments = %v, original = %v", got, original)
	}
	if !reflect.DeepEqual(hyperlightControllerArgs(got, 994), want) {
		t.Fatal("restoring the bundle stacked another group flag")
	}
}

func TestHyperlightControllerPatchClearsAndRestoresBundle(t *testing.T) {
	current := []byte(`{"spec":{"template":{"spec":{"containers":[{
		"name":"manager","args":["--leader-elect"],
		"env":[{"name":"ORKA_HYPERLIGHT_BUNDLE_IMAGE","value":"initial-image"}]
	}]}}}}`)
	for _, bundle := range []string{"", "restored-image"} {
		patch, err := hyperlightControllerPatch([]string{"--leader-elect"}, 994, bundle)
		if err != nil {
			t.Fatal(err)
		}
		current, err = strategicpatch.StrategicMergePatch(current, patch, appsv1.Deployment{})
		if err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(current, &deployment); err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "ORKA_HYPERLIGHT_BUNDLE_IMAGE" {
				found = true
				if env.Value != bundle {
					t.Fatalf("bundle = %q, want %q after strategic merge", env.Value, bundle)
				}
			}
		}
		if !found {
			t.Fatal("bundle environment entry is missing")
		}
	}
}

func TestHyperlightE2EDeviceGroupFailsClosed(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "invalid", "2147483648"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("E2E_HYPERLIGHT_DEVICE_GID", value)
			if _, err := hyperlightE2EDeviceGID(); err == nil {
				t.Fatalf("accepted invalid device group %q", value)
			}
		})
	}
	t.Setenv("E2E_HYPERLIGHT_DEVICE_GID", "994")
	if gid, err := hyperlightE2EDeviceGID(); err != nil || gid != 994 {
		t.Fatalf("group = %d, error = %v", gid, err)
	}
}

func TestHyperlightGuestEvidenceRejectsCannedOrHostResults(t *testing.T) {
	guest := `{"kernel":"5.15.148-Ijiraq","marker":"guest","network_denied":true,"workspace":"/workspace/"}`
	if err := validateHyperlightGuest(guest, "guest", "6.14.0-host", true); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"Hyperlight workspace checks completed.",
		`{"kernel":"6.14.0-host","marker":"guest","network_denied":true,"workspace":"/workspace"}`,
		`{"kernel":"5.15.148-Ijiraq","marker":"wrong","network_denied":true,"workspace":"/workspace"}`,
		`{"kernel":"5.15.148-Ijiraq","marker":"guest","network_denied":false,"workspace":"/workspace"}`,
		`{"kernel":"5.15.148-Ijiraq","marker":"guest","network_denied":true,"workspace":"/tmp"}`,
	} {
		if err := validateHyperlightGuest(output, "guest", "6.14.0-host", true); err == nil {
			t.Fatalf("accepted missing or invalid evidence %s", output)
		}
	}
}

func TestHyperlightSandboxResultAllowsOnlyClaudeBudgetReminder(t *testing.T) {
	const result = `{"exit_code":0,"stdout":"guest\n"}`
	for _, text := range []string{
		result,
		result + "\n\n<system-reminder>\n<total_tokens>15000000 tokens left</total_tokens>\n</system-reminder>",
	} {
		decoded, err := decodeSandboxExecResult(text)
		if err != nil || decoded.Stdout != "guest\n" {
			t.Fatalf("rejected %q: %+v %v", text, decoded, err)
		}
	}
	for _, text := range []string{
		"",
		"Hyperlight workspace checks completed.",
		result + "\nhook-marker",
		result + "\n<system-reminder>\nPreToolUse hook output\n</system-reminder>",
		result + "\n<system-reminder>\n<total_tokens>1 tokens left</total_tokens>\n</system-reminder>\nhook-marker",
	} {
		if _, err := decodeSandboxExecResult(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestHyperlightPodEvidenceRequiresDevicePinningAndRealBundleDelivery(t *testing.T) {
	image := "localhost/worker@sha256:" + strings.Repeat("a", 64)
	bundle := "localhost/bundle@sha256:" + strings.Repeat("b", 64)
	pod := corev1.Pod{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{SupplementalGroups: []int64{994}},
		Containers: []corev1.Container{{
			Image: image,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{controller.DefaultHyperlightDeviceResource: resource.MustParse("1")},
				Limits:   corev1.ResourceList{controller.DefaultHyperlightDeviceResource: resource.MustParse("1")},
			},
			Env: []corev1.EnvVar{
				{Name: workerenv.CodeExecBackend, Value: "hyperlight"}, {Name: workerenv.CodeExecBackendEnforced, Value: "true"},
				{Name: "ORKA_HYPERLIGHT_BINARY", Value: "/opt/orka/hyperlight/bin/hluk"},
				{Name: "ORKA_HYPERLIGHT_ROOTFS_DIR", Value: "/opt/orka/hyperlight/rootfs"},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "cache", MountPath: "/var/cache/orka-hyperlight"},
				{Name: "bundle", MountPath: "/opt/orka/hyperlight", ReadOnly: true},
			},
		}},
		InitContainers: []corev1.Container{{Name: "hyperlight-bundle", Image: bundle}},
	}, Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
		Name: "hyperlight-bundle", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}}}
	if err := validateHyperlightPod(pod, image, bundle, 994, false); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"device": func(p *corev1.Pod) {
			delete(p.Spec.Containers[0].Resources.Requests, controller.DefaultHyperlightDeviceResource)
		},
		"backend":        func(p *corev1.Pod) { p.Spec.Containers[0].Env[0].Value = "in-process" },
		"group":          func(p *corev1.Pod) { p.Spec.SecurityContext.SupplementalGroups = nil },
		"bundle pin":     func(p *corev1.Pod) { p.Spec.InitContainers[0].Image = "bundle:latest" },
		"read-only":      func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[1].ReadOnly = false },
		"separate cache": func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[1].Name = "cache" },
		"init failure":   func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if err := validateHyperlightPod(*changed, image, bundle, 994, false); err == nil {
				t.Fatalf("accepted invalid %s evidence", name)
			}
		})
	}
	unavailable := pod.DeepCopy()
	unavailable.Spec.InitContainers = nil
	unavailable.Spec.Containers[0].VolumeMounts = unavailable.Spec.Containers[0].VolumeMounts[:1]
	if err := validateHyperlightPod(*unavailable, image, "", 994, false); err != nil {
		t.Fatalf("unavailable-bundle control: %v", err)
	}
	if err := validateHyperlightPod(pod, image, "", 994, false); err == nil {
		t.Fatal("negative control accepted a delivered bundle")
	}
	runtimePod := pod.DeepCopy()
	runtimePod.Spec.Containers[0].Env = append(runtimePod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "ORKA_ACP_SANDBOX_EXEC_ENABLED", Value: "true"},
		corev1.EnvVar{Name: "ORKA_HYPERLIGHT_DEVICE_GID", Value: "994"})
	if err := validateHyperlightPod(*runtimePod, image, bundle, 994, true); err != nil {
		t.Fatalf("runtime delivery: %v", err)
	}
}

func TestHyperlightAimockFixturesRequireRealToolCallsAndStatelessWorkspaceSequence(t *testing.T) {
	// BeforeSuite changes cwd to the repository root. The existing suite
	// helper resolves both that location and the normal package cwd, and
	// does not depend on runtime source paths stripped by -trimpath.
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(projectDir, "test", "e2e", "testdata", "aimock", "hyperlight.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Fixtures []struct {
			Match    map[string]any `json:"match"`
			Response struct {
				ToolCalls []struct {
					Name      string `json:"name"`
					ID        string `json:"id"`
					Arguments struct {
						Code, Language string
						Timeout        int
					} `json:"arguments"`
				} `json:"toolCalls"`
			} `json:"response"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	for _, fixture := range document.Fixtures {
		marker, _ := fixture.Match["userMessage"].(string)
		if fixture.Match["sequenceIndex"] != nil {
			t.Fatal("stateful counters break reruns")
		}
		for _, call := range fixture.Response.ToolCalls {
			calls[marker]++
			if call.Arguments.Code == "" {
				continue // Native command rejection and allowed read probes.
			}
			if marker == "[e2e:hyperlight-sandbox-exec]" {
				code := call.Arguments.Code
				switch {
				case strings.Contains(code, "hyperlight-acp-write-refused") && !strings.Contains(code, "errno.EROFS"):
					t.Fatal("the first guest must see every workspace write refused")
				case strings.Contains(code, "'hyperlight-acp-read'") &&
					!strings.Contains(code, "assert not os.path.exists('hyperlight-aimock.txt')"):
					t.Fatal("the second guest must find that the refused write left nothing")
				}
			}
			if call.Arguments.Language != "python" || call.Arguments.Timeout != 30 {
				t.Fatalf("unexpected guest arguments: %+v", call.Arguments)
			}
			if marker != "[e2e:hyperlight-unavailable]" &&
				(!strings.Contains(call.Arguments.Code, "os.uname().release") ||
					!strings.Contains(call.Arguments.Code, "socket.socket")) {
				t.Fatalf("fixture %s lacks guest kernel and socket-policy checks", marker)
			}
		}
	}
	want := map[string]int{
		"[e2e:hyperlight-code-exec]": 1, "[e2e:hyperlight-sandbox-exec]": 3,
		"[e2e:hyperlight-unavailable]": 1,
		"[e2e:hyperlight-claude]":      4,
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("scripted calls = %v, want %v", calls, want)
	}
	if len(document.Fixtures) != 13 ||
		document.Fixtures[2].Match["toolResultContains"] != "hyperlight-acp-read" ||
		document.Fixtures[3].Match["toolResultContains"] != "hyperlight-acp-write-refused" {
		t.Fatal("workspace final/read fixtures must precede the initial write fixture")
	}
}

func TestHyperlightHarnessAgentManifestsDisableNativeCommandsWithoutRemovingRead(t *testing.T) {
	for _, provider := range []string{"claude", "copilot"} {
		t.Run(provider, func(t *testing.T) {
			agent := hyperlightHarnessAgentManifest("test-hyperlight", provider)
			spec := agent["spec"].(map[string]any)
			runtime := spec["runtime"].(map[string]any)
			if runtime["defaultAllowBash"] != false || runtime["type"] != provider ||
				!reflect.DeepEqual(runtime["defaultAllowedTools"], []string{"Read", acp.SandboxExecToolName}) {
				t.Fatalf("unexpected command-isolated policy: %+v", runtime)
			}
			if spec["model"].(map[string]any)["name"] != hyperlightHarness(provider).model {
				t.Fatal("fixture and actual harness model disagree")
			}
		})
	}
}

func TestHyperlightHarnessJournalRequiresActualCatalogRejectionAndNativeRead(t *testing.T) {
	for _, provider := range []string{"claude", "copilot"} {
		t.Run(provider, func(t *testing.T) {
			harness := hyperlightHarness(provider)
			data, err := json.Marshal(map[string]any{
				"path": "/v1" + harness.endpointSuffix,
				"body": map[string]any{
					"tools": []any{
						map[string]any{"function": map[string]string{"name": harness.sandboxTool}},
						map[string]any{"function": map[string]string{"name": harness.readTool}},
					},
					"messages": []any{
						map[string]any{"role": "assistant", "tool_calls": []any{
							map[string]any{"id": "denied", "function": map[string]string{"name": harness.commandTools[0]}}}},
						map[string]string{"role": "tool", "tool_call_id": "denied", "content": "Error: tool is not available"},
						map[string]any{"role": "assistant", "tool_calls": []any{
							map[string]any{"id": "read", "function": map[string]string{"name": harness.readTool}}}},
						map[string]string{"role": "tool", "tool_call_id": "read", "content": "1\t" + liveRuntimeRepoSentinel},
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			load := func() mockLLMRequest {
				t.Helper()
				var request mockLLMRequest
				if err := json.Unmarshal(data, &request); err != nil {
					t.Fatal(err)
				}
				return request
			}
			if err := validateHyperlightHarnessJournal([]mockLLMRequest{load()}, harness); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func(*mockLLMRequest){
				"sandbox missing": func(r *mockLLMRequest) { r.Body.Tools[0].Function.Name = "other" },
				"read missing":    func(r *mockLLMRequest) { r.Body.Tools[1].Function.Name = "other" },
				"not rejected":    func(r *mockLLMRequest) { r.Body.Messages[1].Content = json.RawMessage(`"command succeeded"`) },
				"no native result": func(r *mockLLMRequest) {
					r.Body.Messages[3].Content = json.RawMessage(`"canned final reply"`)
				},
				"wrong protocol": func(r *mockLLMRequest) { r.Path = "/v1/chat/completions" },
				"truncated":      func(r *mockLLMRequest) { r.Body.Truncated = true },
				"unmanaged MCP":  func(r *mockLLMRequest) { r.Body.Tools[1].Function.Name = "hyperlight_unmanaged_exec" },
			} {
				t.Run(name, func(t *testing.T) {
					request := load()
					mutate(&request)
					if err := validateHyperlightHarnessJournal([]mockLLMRequest{request}, harness); err == nil {
						t.Fatalf("accepted invalid %s evidence", name)
					}
				})
			}
			for _, command := range harness.commandTools {
				t.Run("offered "+command, func(t *testing.T) {
					request := load()
					request.Body.Tools[0].Function.Name = command
					if err := validateHyperlightHarnessJournal([]mockLLMRequest{request}, harness); err == nil {
						t.Fatalf("accepted offered command tool %s", command)
					}
				})
			}
			if err := validateHyperlightHarnessJournal(nil, harness); err == nil {
				t.Fatal("accepted an empty journal")
			}
		})
	}
}

func TestHyperlightHarnessFixturesForceExcludedCommandsAndBoundWorkspacePoison(t *testing.T) {
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(projectDir, "test", "e2e", "testdata", "aimock", "hyperlight.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Fixtures []struct {
			Match struct {
				UserMessage        string `json:"userMessage"`
				ToolCallID         string `json:"toolCallId"`
				ToolResultContains string `json:"toolResultContains"`
			} `json:"match"`
			Response struct {
				ToolCalls []struct {
					Name      string         `json:"name"`
					ID        string         `json:"id"`
					Arguments map[string]any `json:"arguments"`
				} `json:"toolCalls"`
			} `json:"response"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"claude"} {
		t.Run(provider, func(t *testing.T) {
			harness := hyperlightHarness(provider)
			prefix := "hyperlight-" + provider
			marker := "[e2e:" + prefix + "]"
			seen := map[string]bool{}
			for _, fixture := range document.Fixtures {
				if fixture.Match.UserMessage != marker {
					continue
				}
				for _, call := range fixture.Response.ToolCalls {
					if seen[call.ID] {
						t.Fatalf("duplicate stateless call ID: %s", call.ID)
					}
					seen[call.ID] = true
					switch call.ID {
					case prefix + "-forbidden":
						command, _ := call.Arguments["command"].(string)
						if call.Name != harness.commandTools[0] || !strings.Contains(command, "HYPERLIGHT_NATIVE_COMMAND_EXECUTED") {
							t.Fatal("fixture does not force an excluded native command with an observable side effect")
						}
					case prefix + "-read":
						if call.Name != harness.readTool || fixture.Match.ToolResultContains != "hyperlight-acp-write-refused" {
							t.Fatal("native read must follow the refused guest writes")
						}
					case prefix + "-guest-write", prefix + "-guest-read":
						code, _ := call.Arguments["code"].(string)
						for _, check := range []string{
							"assert not os.path.exists('HYPERLIGHT_NATIVE_COMMAND_EXECUTED')",
							"assert not os.path.exists('HYPERLIGHT_UNMANAGED_EXECUTED')",
						} {
							if !strings.Contains(code, check) {
								t.Fatalf("%s lacks negative host-execution evidence", call.ID)
							}
						}
						if call.Name != harness.sandboxTool {
							t.Fatal("guest probe used a different MCP tool identity")
						}
						if call.ID == prefix+"-guest-write" {
							if fixture.Match.ToolCallID != prefix+"-forbidden" || !strings.Contains(code, "errno.EROFS") ||
								!strings.Contains(code, "hyperlight_unmanaged") || !strings.Contains(code, "hyperlight-hook.sh") {
								t.Fatal("guest must follow command rejection and find bounded hook/MCP configs refused")
							}
						} else if fixture.Match.ToolCallID != prefix+"-read" ||
							!strings.Contains(code, "assert not os.path.exists(path), path") {
							t.Fatal("guest must follow native read and find no poison files in the read-only workspace")
						}
					default:
						t.Fatalf("unexpected harness fixture call %s", call.ID)
					}
				}
			}
			if len(seen) != 4 {
				t.Fatalf("need rejection, guest write, native read and guest cleanup, got %v", seen)
			}
		})
	}
}

func TestHyperlightNativeRejectionCorrelatesRewrittenCalls(t *testing.T) {
	for _, tc := range []struct {
		name, arguments, output string
		want                    bool
	}{
		{"invalid", `{"tool":"bash"}`,
			"The arguments provided to the tool are invalid: Model tried to call unavailable tool bash", true},
		{"bash", `{}`, "Unknown tool: bash", true},
		{"orka_sandbox_exec", `{}`, "bash arguments provided to the tool are invalid", false},
		{"invalid", `{"tool":"read"}`, "Unknown tool: bash", false},
	} {
		var call mockLLMToolCall
		call.ID = "regenerated-id"
		call.Function.Name, call.Function.Arguments = tc.name, tc.arguments
		var request mockLLMRequest
		text, _ := json.Marshal(tc.output)
		request.Body.Messages = []mockLLMMessage{
			{Role: "assistant", ToolCalls: []mockLLMToolCall{call}},
			{Role: "tool", ToolCallID: call.ID, Content: text},
		}
		if got := hyperlightNativeCommandRejected(request, "bash"); got != tc.want {
			t.Fatalf("%s/%s rejection = %v, want %v", tc.name, tc.arguments, got, tc.want)
		}
	}
}
