/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfiguredImage = "registry.example/kagent-configured@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
const testRuntimeImage = "registry.example/kagent-runtime@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"

func kagentBundleArgs(path, dir string, extra ...string) []string {
	return append([]string{"-f", path, "--bundle-dir", dir, "--control-plane-url", "http://kagent-controller.kagent.svc:8083",
		"--relay-url", governedRelay, "--model-upstream-url", "http://vekil.vekil-system.svc:8080/v1", "--model-upstream-model", "gpt-4.1"}, extra...)
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readBundle(t *testing.T, dir string) (kagentBundle, []byte) {
	t.Helper()
	bundle, config, err := readKagentBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return bundle, config
}

// A bundle names its runtime after the pinned configuration, so a changed
// pin gets a new runtime beside the old one, and the same pin the same one.
func TestAgentImportKagentCmdWritesABundlePinnedToItsConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := writeTempFile(t, governedKagentV0Fixture)
	out, err := runKagentImport(t, kagentBundleArgs(path, dir, "--dry-run", "--approval-required-tool", "probe-orka",
		"--unreachable", "kagent-postgresql.kagent.svc:5432", "--accept-kagent-commit", "0123456", "--max-output-tokens", "2048")...)
	if err != nil {
		t.Fatal(err)
	}
	bundle, config := readBundle(t, filepath.Join(dir, "probe-gov"))
	if !strings.HasPrefix(bundle.Runtime, "probe-gov-kagent-") || len(bundle.Runtime) != len("probe-gov-kagent-")+kagentBundleDigestChars ||
		bundle.KagentVersion != kagentVersionV0 || !bundle.Governed || bundle.Model != "kagent/kagent/probe-gov" ||
		strings.Join(bundle.AllowedTools, ",") != "probe-orka" || strings.Join(bundle.ApprovalRequiredTools, ",") != "probe-orka" {
		t.Fatalf("bundle = %+v", bundle)
	}
	if !strings.Contains(out, "name: "+bundle.Runtime) {
		t.Fatalf("the Orka Agent does not reference the pinned runtime:\n%s", out)
	}
	var wrapper kagentWrapperConfig
	if err := json.Unmarshal(config, &wrapper); err != nil {
		t.Fatal(err)
	}
	if wrapper.Kagent.ExpectedImageDigest != governedV0Image || strings.Join(wrapper.Unreachable, ",") != "kagent-postgresql.kagent.svc:5432" ||
		strings.Join(wrapper.Kagent.AcceptedCommits, ",") != "0123456" || wrapper.Governed == nil || wrapper.Governed.MaxOutputTokens != 2048 {
		t.Fatalf("bundle configuration = %s", config)
	}
	again := t.TempDir()
	if _, err := runKagentImport(t, kagentBundleArgs(path, again, "--dry-run", "--approval-required-tool", "probe-orka",
		"--unreachable", "kagent-postgresql.kagent.svc:5432", "--accept-kagent-commit", "0123456", "--max-output-tokens", "2048")...); err != nil {
		t.Fatal(err)
	}
	if same, _ := readBundle(t, filepath.Join(again, "probe-gov")); same.Runtime != bundle.Runtime {
		t.Fatalf("the same pin named runtime %s, then %s", bundle.Runtime, same.Runtime)
	}
	if _, err := runKagentImport(t, kagentBundleArgs(path, again, "--dry-run")...); err != nil {
		t.Fatal(err)
	}
	if changed, _ := readBundle(t, filepath.Join(again, "probe-gov")); changed.Runtime == bundle.Runtime {
		t.Fatal("a changed configuration kept its runtime name")
	}
}

func TestAgentImportKagentCmdRefusesBadWrapperFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeTempFile(t, governedKagentV0Fixture)
	for name, test := range map[string]struct {
		args []string
		want string
	}{
		"no config destination":   {[]string{"-f", path, "--dry-run", "--unreachable", "router:8080"}, "pass --wrapper-config-dir or --bundle-dir"},
		"bad unreachable":         {kagentBundleArgs(path, t.TempDir(), "--dry-run", "--unreachable", "router"), "must be a host:port"},
		"bad commit":              {kagentBundleArgs(path, t.TempDir(), "--dry-run", "--accept-kagent-commit", "1EB89FB"), "lowercase hex"},
		"unknown approval tool":   {kagentBundleArgs(path, t.TempDir(), "--dry-run", "--approval-required-tool", "nope"), "not an allowed tool"},
		"negative output":         {kagentBundleArgs(path, t.TempDir(), "--dry-run", "--max-output-tokens", "-1"), "must not be negative"},
		"two config destinations": {append(kagentBundleArgs(path, t.TempDir(), "--dry-run"), "--wrapper-config-dir", t.TempDir()), "are exclusive"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runKagentImport(t, test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

// The image digest comes from the Agent's only pod, which must run the
// pinned config hash.
func TestKagentV0ImageDigestComesFromTheAgentsPod(t *testing.T) {
	for name, test := range map[string]struct {
		input, want string
	}{
		"no pod":          {strings.Replace(governedKagentV0Fixture, "kind: Pod\n", "kind: ConfigMap\n", 1), "the Agent's pod is not in the input"},
		"another hash":    {strings.Replace(governedKagentV0Fixture, `annotations: {kagent.dev/config-hash: "`+governedV0Hash+`"}`+"\nspec:\n  containers:\n  - name: kagent\n    image", `annotations: {kagent.dev/config-hash: "7"}`+"\nspec:\n  containers:\n  - name: kagent\n    image", 1), `runs config hash "7"`},
		"no image digest": {strings.Replace(governedKagentV0Fixture, "golang-adk@"+governedV0Image, "golang-adk:0.10.3", 1), "reports no image digest"},
		"not running":     {strings.Replace(governedKagentV0Fixture, "phase: Running", "phase: Pending", 1), "is not running"},
	} {
		t.Run(name, func(t *testing.T) {
			mode := classifyV0Fixture(t, test.input, governedRelay)
			if mode.ImageDigest != "" || !strings.Contains(mode.ImageProblem, test.want) {
				t.Fatalf("mode = %+v, want %q", mode, test.want)
			}
		})
	}
	if mode := classifyV0Fixture(t, governedKagentV0Fixture, governedRelay); mode.ImageDigest != governedV0Image || strings.Join(mode.Tools, ",") != "probe-orka" {
		t.Fatalf("mode = %+v", mode)
	}
}

// testRuntimeUID is the UID of the AgentRuntime a test bundle was deployed as.
const testRuntimeUID = "6f1c2b9e-0000-4000-8000-000000000002"

// recordRegistration does what deploy does once the bundle is registered.
func recordRegistration(t *testing.T, bundleDir, uid string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bundleDir, kagentBundleRegistrationFile), []byte(uid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// kagentRuntimeState is an AgentRuntime as Orka's API returns it, which has
// observed generation 1.
func kagentRuntimeState(ready bool, generation int, deleting bool) map[string]any {
	status := "False"
	if ready {
		status = "True"
	}
	metadata := map[string]any{"uid": testRuntimeUID, "generation": float64(generation)}
	if deleting {
		metadata["deletionTimestamp"] = "2026-10-19T00:00:00Z"
	}
	return map[string]any{"metadata": metadata, "status": map[string]any{"ready": ready, "observedGeneration": float64(1),
		"conditions": []any{map[string]any{"type": "Ready", "status": status}}}}
}

// Activating a bundle points the Orka Agent at its runtime only once Orka
// would admit Tasks to that runtime: Ready for its current spec, and not being
// deleted.
func TestAgentImportKagentCmdActivatesABundleOnlyOnAReadyRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := writeTempFile(t, governedKagentV0Fixture)
	if _, err := runKagentImport(t, kagentBundleArgs(path, dir, "--dry-run")...); err != nil {
		t.Fatal(err)
	}
	bundle, _ := readBundle(t, filepath.Join(dir, "probe-gov"))
	api := &kagentImportAPI{agents: map[string]map[string]any{}, runtimes: map[string]map[string]any{}}
	srv := kagentImportAPIServer(t, api)
	args := kagentBundleArgs(path, dir, "--server", srv.URL, "-n", "orka-system")
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), "records no registration") || api.creates != 0 {
		t.Fatalf("activation of an undeployed bundle = %v (%d creates)", err, api.creates)
	}
	recordRegistration(t, filepath.Join(dir, "probe-gov"), testRuntimeUID)
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), "is not registered") || api.creates != 0 {
		t.Fatalf("activation before registration = %v (%d creates)", err, api.creates)
	}
	api.mu.Lock()
	api.runtimes[bundle.Runtime] = kagentRuntimeState(false, 1, false)
	api.mu.Unlock()
	// With two Agents, one unready runtime keeps both where they are.
	two := writeTempFile(t, governedKagentV0Fixture+"---\n"+strings.ReplaceAll(governedKagentV0Fixture, "probe-gov", "probe-two"))
	twoDir := t.TempDir()
	if _, err := runKagentImport(t, kagentBundleArgs(two, twoDir, "--dry-run")...); err != nil {
		t.Fatal(err)
	}
	other, _ := readBundle(t, filepath.Join(twoDir, "probe-two"))
	recordRegistration(t, filepath.Join(twoDir, "probe-gov"), testRuntimeUID)
	recordRegistration(t, filepath.Join(twoDir, "probe-two"), testRuntimeUID)
	api.mu.Lock()
	api.runtimes[other.Runtime] = kagentRuntimeState(true, 1, false)
	api.mu.Unlock()
	if _, err := runKagentImport(t, kagentBundleArgs(two, twoDir, "--server", srv.URL, "-n", "orka-system")...); err == nil || api.creates != 0 {
		t.Fatalf("a partial activation = %v (%d creates)", err, api.creates)
	}
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), "is not Ready") || api.creates != 0 {
		t.Fatalf("activation on an unready runtime = %v (%d creates)", err, api.creates)
	}
	for state, want := range map[string]map[string]any{
		"has not observed its current spec": kagentRuntimeState(true, 2, false),
		"is being deleted":                  kagentRuntimeState(true, 1, true),
	} {
		api.mu.Lock()
		api.runtimes[bundle.Runtime] = want
		api.mu.Unlock()
		if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), state) || api.creates != 0 {
			t.Fatalf("activation on a runtime that %s = %v (%d creates)", state, err, api.creates)
		}
	}
	// A registration of the same name that the deploy did not create, such
	// as one in another cluster than the deploy targeted, is refused.
	elsewhere := kagentRuntimeState(true, 1, false)
	elsewhere["metadata"].(map[string]any)["uid"] = "6f1c2b9e-0000-4000-8000-0000000000ff"
	api.mu.Lock()
	api.runtimes[bundle.Runtime] = elsewhere
	api.mu.Unlock()
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), "the registration the bundle was deployed as") || api.creates != 0 {
		t.Fatalf("activation on another registration = %v (%d creates)", err, api.creates)
	}
	api.mu.Lock()
	api.runtimes[bundle.Runtime] = kagentRuntimeState(true, 1, false)
	api.mu.Unlock()
	// Activation switches the Agents to the bundles that were deployed, not
	// to what other arguments would pin.
	if _, err := runKagentImport(t, append(args, "--runtime-revision", "4")...); err == nil ||
		!strings.Contains(err.Error(), "is not the one these arguments pin") || api.creates != 0 {
		t.Fatalf("activation with other arguments = %v (%d creates)", err, api.creates)
	}
	if _, err := runKagentImport(t, kagentBundleArgs(path, t.TempDir(), "--server", srv.URL, "-n", "orka-system")...); err == nil || api.creates != 0 {
		t.Fatalf("activation without bundles = %v (%d creates)", err, api.creates)
	}
	if _, err := runKagentImport(t, args...); err != nil || api.creates != 1 {
		t.Fatalf("activation on a ready runtime = %v (%d creates)", err, api.creates)
	}
	api.mu.Lock()
	ref := kagentString(api.agents["probe-gov"], "spec", "runtime", "runtimeRef", "name")
	api.mu.Unlock()
	if ref != bundle.Runtime {
		t.Fatalf("activated Agent references %q, want %q", ref, bundle.Runtime)
	}
}

func renderBundle(t *testing.T, dir string, options kagentRenderOptions) []map[string]any {
	t.Helper()
	list, err := renderKagentRuntime(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := list["items"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		raw, _ := json.Marshal(item)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		items = append(items, obj)
	}
	return items
}

func findKind(items []map[string]any, kind string) map[string]any {
	for _, item := range items {
		if item["kind"] == kind {
			return item
		}
	}
	return nil
}

func TestRenderKagentRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := writeTempFile(t, governedKagentV0Fixture)
	if _, err := runKagentImport(t, kagentBundleArgs(path, dir, "--dry-run", "--approval-required-tool", "probe-orka")...); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(dir, "probe-gov")
	bundle, config := readBundle(t, bundleDir)
	options := kagentRenderOptions{ConfiguredImage: testConfiguredImage, RuntimeImage: testRuntimeImage, ControllerEpoch: 3,
		Namespace: "orka-system", Part: kagentPartWorkload}
	workload := renderBundle(t, bundleDir, options)
	deployment := findKind(workload, "Deployment")
	pod := kagentMap(deployment, "spec", "template", "spec")
	containers, _ := pod["containers"].([]any)
	if kagentString(deployment, "metadata", "name") != bundle.Runtime || len(containers) != 2 ||
		pod["automountServiceAccountToken"] != false || kagentString(deployment, "spec", "strategy", "type") != "Recreate" {
		t.Fatalf("deployment = %v", deployment)
	}
	labels := kagentMap(deployment, "spec", "template", "metadata", "labels")
	if labels[kagentBrokerLabel] != "true" || labels[kagentRuntimeLabel] != bundle.Runtime || labels[kagentRelayLabel] != "probe-gov-relay" {
		t.Fatalf("pod labels = %v", labels)
	}
	// Every object names the kagent Agent the runtime wraps, which retirement
	// scopes its inventory to.
	for _, item := range workload {
		if kagentString(item, "metadata", "annotations", kagentSourceAnnotation) != bundle.Source || bundle.Source != "kagent.dev/v1alpha2 kagent/probe-gov" {
			t.Fatalf("%v has no kagent source", item["kind"])
		}
	}
	// The same bundle may run in several Orka namespaces, which share the
	// Agent's namespace.
	for _, kind := range []string{"Role", "RoleBinding"} {
		reader := findKind(workload, kind)
		if reader == nil || kagentString(reader, "metadata", "namespace") != "kagent" ||
			kagentString(reader, "metadata", "name") != bundle.Runtime+".orka-system.agent-pods" ||
			kagentMap(reader, "metadata", "labels")[kagentRuntimeNamespaceLabel] != "orka-system" {
			t.Fatalf("a v0 runtime needs its own read access to the Agent's pods: %v", reader)
		}
	}
	if !bytes.Contains(mustJSON(t, workload), []byte(`"value":"3"`)) || !bytes.Contains(mustJSON(t, workload), []byte(bundle.ConfigDigest)) {
		t.Fatal("the supervisor is missing the controller epoch or configuration digest")
	}
	if _, err := renderKagentRuntime(bundleDir, kagentRenderOptions{ConfiguredImage: "registry.example/kagent:latest", RuntimeImage: testRuntimeImage,
		ControllerEpoch: 3, Namespace: "orka-system", Part: kagentPartWorkload}); err == nil || !strings.Contains(err.Error(), "pinned by digest") {
		t.Fatalf("a tag-only image = %v", err)
	}

	checkRenderedRelay(t, bundleDir, bundle, options)
	checkRenderedRegistration(t, bundleDir, bundle, config, options)
	options.Part, options.DeploymentUID = kagentPartRegistration, "6f1c2b9e-0000-4000-8000-000000000001"
	options.Capabilities = writeCapabilities(t, "sha256:"+strings.Repeat("9", 64))

	// A bundle edited after the import no longer matches its pin.
	if err := os.WriteFile(filepath.Join(bundleDir, kagentBundleConfigFile), append(config, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := renderKagentRuntime(bundleDir, options); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("an edited bundle = %v", err)
	}
}

// The wrapper reads a missing or null governed stanza as observed mode, which
// brokers no Orka tools.
func TestOnlyAGovernedBundleListsTools(t *testing.T) {
	adapter := "sha256:" + strings.Repeat("a", 64)
	for _, config := range []string{`{"model":"m"}`, `{"model":"m","governed":null}`} {
		bundle := kagentBundle{Model: "m", AllowedTools: []string{"probe"}, ConfigDigest: "sha256:" + strings.Repeat("b", 64)}
		_, err := kagentRuntimeProfileFor(bundle, []byte(config), adapter)
		if err == nil || !strings.Contains(err.Error(), "only a governed kagent configuration") {
			t.Errorf("%s: err = %v", config, err)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func toStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

func TestKagentRelayService(t *testing.T) {
	for origin, want := range map[string]string{
		"http://probe-kagent-relay.orka-system.svc:8092":               "probe-kagent-relay orka-system 8092",
		"http://probe-kagent-relay.orka-system.svc.cluster.local:8092": "probe-kagent-relay orka-system 8092",
		"http://probe-kagent-relay.orka-system:9000":                   "probe-kagent-relay orka-system 9000",
	} {
		service, namespace, port, err := kagentRelayService(origin)
		if got := fmt.Sprintf("%s %s %d", service, namespace, port); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", origin, got, err, want)
		}
	}
	for _, origin := range []string{"http://relay.orka-system.svc", "https://relay.orka-system.svc:8092", "http://relay:8092", "http://10.0.0.1:8092", "http://a.b.c.svc:8092"} {
		if _, _, _, err := kagentRelayService(origin); err == nil {
			t.Errorf("%s was accepted", origin)
		}
	}
}

func checkRenderedRelay(t *testing.T, bundleDir string, bundle kagentBundle, options kagentRenderOptions) {
	t.Helper()
	options.Part = kagentPartRelay
	service := findKind(renderBundle(t, bundleDir, options), "Service")
	// The relay Service is the one the kagent Agent's pinned config names.
	ports, _ := kagentMap(service, "spec")["ports"].([]any)
	if kagentString(service, "metadata", "name") != "probe-gov-relay" || kagentMap(service, "spec", "selector")[kagentRuntimeLabel] != bundle.Runtime ||
		len(ports) != 1 || ports[0].(map[string]any)["port"] != float64(8092) {
		t.Fatalf("relay service = %v", service)
	}
	// The policy selects only the runtimes of this relay: another kagent
	// Agent's runtimes of the same Orka Agent name carry their own relay's
	// name.
	policy := findKind(renderBundle(t, bundleDir, options), "NetworkPolicy")
	if selector := kagentMap(policy, "spec", "podSelector", "matchLabels"); len(selector) != 1 || selector[kagentRelayLabel] != "probe-gov-relay" {
		t.Fatalf("relay policy selects %v", selector)
	}
	// Both relay objects name the kagent Agent they serve.
	for _, kind := range []string{"Service", "NetworkPolicy"} {
		if source := kagentString(findKind(renderBundle(t, bundleDir, options), kind), "metadata", "annotations", kagentSourceAnnotation); source != bundle.Source {
			t.Fatalf("relay %s source = %q", kind, source)
		}
	}
	options.Namespace = "team"
	if _, err := renderKagentRuntime(bundleDir, options); err == nil || !strings.Contains(err.Error(), `in namespace "orka-system"`) {
		t.Fatalf("a relay rendered outside its namespace = %v", err)
	}
}

func writeCapabilities(t *testing.T, digest string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caps.json")
	raw, _ := json.Marshal(map[string]any{"runtimeProfileDigest": digest, "limits": map[string]any{"maxResidentSessions": 10},
		"supportsDrain": true, "supportsPublicationFinalization": true, "workspaceGovernance": map[string]any{"mode": "strict-governed"}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func checkRenderedRegistration(t *testing.T, bundleDir string, bundle kagentBundle, config []byte, options kagentRenderOptions) {
	t.Helper()
	profile, err := kagentRuntimeProfileFor(bundle, config, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	options.Part, options.Capabilities = kagentPartRegistration, writeCapabilities(t, profile.Profile.Digest)
	if _, err := renderKagentRuntime(bundleDir, options); err == nil || !strings.Contains(err.Error(), "--deployment-uid") {
		t.Fatalf("a registration without the Deployment = %v", err)
	}
	options.DeploymentUID = "6f1c2b9e-0000-4000-8000-000000000001"
	registration := findKind(renderBundle(t, bundleDir, options), "AgentRuntime")
	if kagentString(registration, "metadata", "annotations", kagentSourceAnnotation) != bundle.Source {
		t.Fatalf("registration has no kagent source: %v", registration["metadata"])
	}
	// Orka restarts the supervisor with its new epoch after it restarts.
	recovery := kagentMap(registration, "spec", "deployment", "kubernetesRecovery")
	if recovery["deploymentName"] != bundle.Runtime || recovery["deploymentUID"] != options.DeploymentUID || recovery["containerName"] != "supervisor" {
		t.Fatalf("registration recovery = %v", recovery)
	}
	if kagentString(registration, "spec", "capabilities", "profile", "digest") != profile.Profile.Digest ||
		kagentString(registration, "spec", "clientAuth", "controllerBearerTokenSecretRef", "name") != bundle.Runtime+"-auth" ||
		kagentMap(registration, "spec", "capabilities", "workspaceGovernance")["trusted"] != false ||
		strings.Join(toStrings(kagentMap(registration, "spec", "capabilities", "mcpPolicy")["approvalRequiredTools"]), ",") != "probe-orka" {
		t.Fatalf("registration = %v", registration)
	}
	options.Capabilities = writeCapabilities(t, "sha256:"+strings.Repeat("9", 64))
	if _, err := renderKagentRuntime(bundleDir, options); err == nil || !strings.Contains(err.Error(), "not the rendered") {
		t.Fatalf("a runtime serving another profile = %v", err)
	}
}

// --allowed-tool adds to an Agent's named tools, so naming the tools of one
// Agent whose binding names none keeps another Agent's own.
func TestKagentBundleAllowedToolsAddToNamedTools(t *testing.T) {
	named := kagentWrappedAgent{Mode: kagentMode{Mode: kagentModeGoverned, Tools: []string{"probe-orka"}}}
	open := kagentWrappedAgent{Mode: kagentMode{Mode: kagentModeGoverned, AnyTool: true}}
	options := kagentWrapperOptions{AllowedTools: []string{"other"}, ApprovalRequiredTools: []string{"probe-orka"}}
	if allowed, approvals := options.kagentBundlePolicy(named); strings.Join(allowed, ",") != "other,probe-orka" || strings.Join(approvals, ",") != "probe-orka" {
		t.Fatalf("named Agent policy = %v, %v", allowed, approvals)
	}
	if allowed, approvals := options.kagentBundlePolicy(open); strings.Join(allowed, ",") != "other" || len(approvals) != 0 {
		t.Fatalf("open Agent policy = %v, %v", allowed, approvals)
	}
}

// A relay Service selects one runtime, so governed Agents cannot share one.
func TestKagentBundleRefusesASharedRelay(t *testing.T) {
	agent := func(name, relay string) kagentWrappedAgent {
		return kagentWrappedAgent{Source: "kagent/" + name, Manifest: map[string]any{"metadata": map[string]any{"name": name}},
			Mode: kagentMode{Mode: kagentModeGoverned, RelayOrigin: relay}}
	}
	options := kagentWrapperOptions{BundleDir: t.TempDir()}
	if err := options.validateBundle([]kagentWrappedAgent{agent("a", "http://a-relay.orka-system.svc:8092"),
		agent("b", "http://b-relay.orka-system.svc:8092")}); err != nil {
		t.Fatal(err)
	}
	err := options.validateBundle([]kagentWrappedAgent{agent("a", "http://shared.orka-system.svc:8092"),
		agent("b", "http://shared.orka-system.svc.cluster.local:9000")})
	if err == nil || !strings.Contains(err.Error(), "share relay Service orka-system/shared") {
		t.Fatalf("a shared relay = %v", err)
	}
}

// A governed Agent with no tools registers explicit empty lists: Orka refuses
// a null tool policy.
func TestKagentBundlePolicyListsAreNeverNull(t *testing.T) {
	allowed, approvalRequired := kagentWrapperOptions{}.kagentBundlePolicy(kagentWrappedAgent{Mode: kagentMode{Mode: kagentModeGoverned}})
	if allowed == nil || approvalRequired == nil || len(allowed) != 0 || len(approvalRequired) != 0 {
		t.Fatalf("policy = %#v, %#v", allowed, approvalRequired)
	}
}

// A governed binding may name no tools, so kagent calls whatever Orka's MCP
// server lists; --allowed-tool then names them.
func TestKagentBundleAllowedToolsComeFromTheFlagFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unnamed := strings.Replace(governedKagentV0Fixture, ", toolNames: [probe-orka]", "", 1)
	dir := t.TempDir()
	// An empty policy would refuse every call such a binding makes.
	if _, err := runKagentImport(t, kagentBundleArgs(writeTempFile(t, unnamed), dir, "--dry-run")...); err == nil || !strings.Contains(err.Error(), "--allowed-tool") {
		t.Fatalf("a binding without tool names and no --allowed-tool = %v", err)
	}
	if _, err := runKagentImport(t, kagentBundleArgs(writeTempFile(t, unnamed), dir, "--dry-run", "--approval-required-tool", "probe-orka")...); err == nil {
		t.Fatal("an approval for a tool no binding names was accepted")
	}
	if _, err := runKagentImport(t, kagentBundleArgs(writeTempFile(t, unnamed), dir, "--dry-run",
		"--allowed-tool", "probe-orka", "--allowed-tool", "other", "--approval-required-tool", "probe-orka")...); err != nil {
		t.Fatal(err)
	}
	bundle, _ := readBundle(t, filepath.Join(dir, "probe-gov"))
	if strings.Join(bundle.AllowedTools, ",") != "other,probe-orka" || strings.Join(bundle.ApprovalRequiredTools, ",") != "probe-orka" {
		t.Fatalf("bundle tools = %+v", bundle)
	}
}

// The Orka tool policy is part of the runtime's profile, so changing only the
// approvals still names a new runtime; a revision names one for new images.
func TestKagentBundleRuntimeNameCoversTheToolPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeTempFile(t, governedKagentV0Fixture)
	names := map[string]bool{}
	for _, extra := range [][]string{nil, {"--approval-required-tool", "probe-orka"}, {"--allowed-tool", "probe-orka", "--allowed-tool", "other"}, {"--runtime-revision", "1"}} {
		dir := t.TempDir()
		if _, err := runKagentImport(t, kagentBundleArgs(path, dir, append([]string{"--dry-run"}, extra...)...)...); err != nil {
			t.Fatal(err)
		}
		bundle, _ := readBundle(t, filepath.Join(dir, "probe-gov"))
		names[bundle.Runtime] = true
	}
	if len(names) != 4 {
		t.Fatalf("runtime names = %v, want one per tool policy and revision", names)
	}
}

// An Orka Agent name fits a bundle when its runtime's name, which is the
// runtime Service's name and a label value, fits a DNS label.
func TestKagentBundleNamesFitTheRuntimeService(t *testing.T) {
	o := kagentWrapperOptions{BundleDir: t.TempDir()}
	for length, fits := range map[int]bool{45: true, 46: false} {
		name := "a" + strings.Repeat("b", length-1)
		agent := kagentWrappedAgent{Source: "kagent/" + name, Manifest: map[string]any{"metadata": map[string]any{"name": name}}}
		err := o.validateBundle([]kagentWrappedAgent{agent})
		if fits != (err == nil) || (err != nil && !strings.Contains(err.Error(), "at most 45 characters")) {
			t.Errorf("%d characters: err = %v", length, err)
		}
	}
}

// kagent Agents of one namespace and name but different API versions are
// different sources, so neither takes the other's runtimes or relay.
func TestKagentBundleSourceNamesTheAPIVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := runKagentImport(t, kagentBundleArgs(writeTempFile(t, governedKagentV0Fixture), dir, "--dry-run")...); err != nil {
		t.Fatal(err)
	}
	bundle, _ := readBundle(t, filepath.Join(dir, "probe-gov"))
	if bundle.Source != "kagent.dev/v1alpha2 kagent/probe-gov" {
		t.Fatalf("bundle source = %q", bundle.Source)
	}
}
