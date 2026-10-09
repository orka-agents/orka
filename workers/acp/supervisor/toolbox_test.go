package supervisor

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const supervisorTestToolboxDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func supervisorTestToolboxes() []harnessv2.RuntimeToolbox {
	return []harnessv2.RuntimeToolbox{
		{Image: "registry.example.com/tools/yq@" + supervisorTestToolboxDigest, MountPath: "/opt/yq-jq", PathEntries: []string{"bin"}},
		{Image: "registry.example.com/tools/brew@" + supervisorTestToolboxDigest, MountPath: harnessv2.RuntimeToolboxHomebrewMountPath, PathEntries: []string{"bin", "sbin"}},
	}
}

func setLoadConfigEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	controllerToken := filepath.Join(dir, "controller-token")
	capabilitySecret := filepath.Join(dir, "capability-secret")
	providerToken := filepath.Join(dir, "provider-token")
	for path, value := range map[string]string{
		controllerToken: strings.Repeat("t", 32), capabilitySecret: strings.Repeat("s", 32), providerToken: "provider-capability",
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		EnvPodUID: "pod-uid", EnvSupervisorBootID: "boot", EnvControllerEpoch: "1", EnvRuntimePoolUID: "pool-uid",
		EnvRuntimePoolGeneration: "1", EnvProvider: providerKindCodex, EnvModel: "gpt-test", EnvWorkspaceIntent: "read",
		EnvAgentConfigurationDigest: testDigest("agent"), EnvToolPolicyDigest: testDigest("tool"),
		EnvApprovalPolicyDigest: testDigest("approval"), EnvMCPConfigurationDigest: testDigest("mcp"),
		EnvProxyCredentialRole: "provider", EnvProxyCredentialScope: "model:gpt-test", EnvResourceClass: "standard",
		EnvControllerTokenFile: controllerToken, EnvCapabilitySecretFile: capabilitySecret, EnvProviderTokenFile: providerToken,
		EnvMCPBrokerURL: "http://orka-controller.orka-system.svc:8080", EnvTrustNamespace: "default",
		EnvSessionBaseDir: filepath.Join(dir, "sessions"), EnvFirstSessionUID: "20000", EnvLastSessionUID: "20010", EnvSessionGID: "20000",
		EnvToolboxes: "", EnvToolboxMountMethod: "",
	} {
		t.Setenv(name, value)
	}
}

func TestLoadConfigFromEnvToolboxesJoinTheProfileDigest(t *testing.T) {
	setLoadConfigEnv(t)
	without, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(without.Toolboxes) != 0 {
		t.Fatalf("unexpected toolboxes %#v", without.Toolboxes)
	}
	toolboxes := supervisorTestToolboxes()
	encoded := `[{"image":"` + toolboxes[0].Image + `","mountPath":"/opt/yq-jq","pathEntries":["bin"]},` +
		`{"image":"` + toolboxes[1].Image + `","mountPath":"` + harnessv2.RuntimeToolboxHomebrewMountPath + `","pathEntries":["bin","sbin"]}]`
	t.Setenv(EnvToolboxes, encoded)
	t.Setenv(EnvToolboxMountMethod, "imageVolume")
	with, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(with.Toolboxes) != 2 || with.Toolboxes[1].PathEntries[1] != "sbin" || with.ToolboxMountMethod != "imageVolume" {
		t.Fatalf("toolboxes not loaded: %#v method=%q", with.Toolboxes, with.ToolboxMountMethod)
	}
	if with.Fence.RuntimeProfileDigest == without.Fence.RuntimeProfileDigest {
		t.Fatal("toolboxes must change the supervisor's runtime profile digest")
	}
	// The supervisor must compute exactly the digest the controller computes
	// for the same profile with the same toolbox list.
	expected := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: acp.BuiltInRuntimeAdapterDigests(providerKindCodex),
		ProviderKind: providerKindCodex, Model: "gpt-test",
		AgentConfigurationDigest: testDigest("agent"), ToolPolicyDigest: testDigest("tool"),
		ApprovalPolicyDigest: testDigest("approval"), MCPConfigurationDigest: testDigest("mcp"),
		WorkspaceIntent: harnessv2.WorkspaceIntentRead, ProxyCredentialRole: "provider", ProxyCredentialScope: "model:gpt-test",
		ResourceClass: "standard", Toolboxes: toolboxes,
	}
	expectedDigest, err := harnessv2.CanonicalProfileDigest(expected)
	if err != nil {
		t.Fatal(err)
	}
	if with.Fence.RuntimeProfileDigest != expectedDigest {
		t.Fatalf("supervisor digest %s != controller digest %s", with.Fence.RuntimeProfileDigest, expectedDigest)
	}
	t.Setenv(EnvToolboxes, `[{"image":"registry.example.com/tools/yq@`+supervisorTestToolboxDigest+`","mountPath":"/opt/codex"}]`)
	if _, err := LoadConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvToolboxes) {
		t.Fatalf("reserved mount path must be rejected: %v", err)
	}
	t.Setenv(EnvToolboxes, "[]")
	empty, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Fence.RuntimeProfileDigest != without.Fence.RuntimeProfileDigest {
		t.Fatal("an empty toolbox list must digest exactly like an absent one")
	}
}

func TestChildPathAppendsToolboxFolders(t *testing.T) {
	got := harnessv2.RuntimeToolboxChildPath(acp.DefaultChildPath, supervisorTestToolboxes())
	want := acp.DefaultChildPath + ":/opt/yq-jq/bin:/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin"
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, acp.DefaultChildPath+":") {
		t.Fatal("toolbox folders must come after the system PATH")
	}
}

func fakeELFHeader(machine uint16) []byte {
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(header[18:20], machine)
	return header
}

func TestVerifyToolboxesChecksMountsWithLstatOnly(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(root, "opt", "yq")
	if err := os.MkdirAll(filepath.Join(mount, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Toolboxes: []harnessv2.RuntimeToolbox{{
		Image: "registry.example.com/tools/yq@" + supervisorTestToolboxDigest, MountPath: mount, PathEntries: []string{"bin"},
	}}}
	if err := VerifyToolboxes(cfg); err != nil {
		t.Fatalf("mounted toolbox: %v", err)
	}
	if err := VerifyToolboxes(Config{}); err != nil {
		t.Fatalf("no toolboxes: %v", err)
	}
	missing := cfg
	missing.Toolboxes = []harnessv2.RuntimeToolbox{{Image: cfg.Toolboxes[0].Image, MountPath: filepath.Join(root, "opt", "missing")}}
	err = VerifyToolboxes(missing)
	var failure *toolbox.Failure
	if !errors.As(err, &failure) || failure.Reason != toolbox.ReasonMissingMount {
		t.Fatalf("missing mount: %v", err)
	}
	entry := cfg
	entry.Toolboxes = []harnessv2.RuntimeToolbox{{Image: cfg.Toolboxes[0].Image, MountPath: mount, PathEntries: []string{"sbin"}}}
	if err := VerifyToolboxes(entry); !errors.As(err, &failure) || failure.Reason != toolbox.ReasonMissingPathEntry {
		t.Fatalf("missing path entry: %v", err)
	}
	link := filepath.Join(root, "opt", "link")
	if err := os.Symlink(mount, link); err != nil {
		t.Fatal(err)
	}
	linked := cfg
	linked.Toolboxes = []harnessv2.RuntimeToolbox{{Image: cfg.Toolboxes[0].Image, MountPath: link}}
	if err := VerifyToolboxes(linked); !errors.As(err, &failure) || failure.Reason != toolbox.ReasonMissingMount {
		t.Fatalf("symlinked mount must be rejected: %v", err)
	}
}

func TestVerifyToolboxesRunsUnprivilegedArchCheckForImageVolumes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The test binary stands in for the supervisor binary; TestMain-free
	// delegation: the subcommand is implemented by the toolbox package, so
	// route the exec through a tiny helper process below.
	helper := filepath.Join(t.TempDir(), "orka-acp-runtime")
	// The supervisor launches the check with a minimal environment, so the
	// helper is selected by an argument marker rather than an env variable.
	script := "#!/bin/sh\nexec " + self + " -test.run=TestToolboxCheckHelperProcess -- orka-toolbox-helper \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := toolboxCheckCommand
	toolboxCheckCommand = helper
	t.Cleanup(func() { toolboxCheckCommand = previous })

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(root, "opt", "yq")
	if err := os.MkdirAll(filepath.Join(mount, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	machine := uint16(0x3e)
	if runtime.GOARCH == "arm64" {
		machine = 0xb7
	}
	if err := os.WriteFile(filepath.Join(mount, "bin", "yq"), fakeELFHeader(machine), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ToolboxMountMethod: "imageVolume", Toolboxes: []harnessv2.RuntimeToolbox{{
		Image: "registry.example.com/tools/yq@" + supervisorTestToolboxDigest, MountPath: mount, PathEntries: []string{"bin"},
	}}}
	if err := VerifyToolboxes(cfg); err != nil {
		t.Fatalf("matching architecture: %v", err)
	}
	wrong := uint16(0xb7)
	if runtime.GOARCH == "arm64" {
		wrong = 0x3e
	}
	if err := os.WriteFile(filepath.Join(mount, "bin", "yq"), fakeELFHeader(wrong), 0o755); err != nil {
		t.Fatal(err)
	}
	err = VerifyToolboxes(cfg)
	var failure *toolbox.Failure
	if !errors.As(err, &failure) || failure.Reason != toolbox.ReasonArchMismatch {
		t.Fatalf("wrong architecture: %v", err)
	}
	cfg.ToolboxMountMethod = "copy"
	if err := VerifyToolboxes(cfg); err != nil {
		t.Fatalf("copy mode must not re-run the architecture check: %v", err)
	}
}

// TestToolboxCheckHelperProcess is the exec target for the unprivileged check
// test above. It behaves like the supervisor binary's toolbox subcommands.
func TestToolboxCheckHelperProcess(t *testing.T) {
	var args []string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) && os.Args[i+1] == "orka-toolbox-helper" {
			args = os.Args[i+2:]
			break
		}
	}
	if args == nil {
		t.Skip("helper process only")
	}
	if len(args) == 0 || !toolbox.IsSubcommand(args[0]) {
		os.Exit(2)
	}
	os.Exit(toolbox.Run(args[0], args[1:], os.Stdout, os.Stderr))
}
