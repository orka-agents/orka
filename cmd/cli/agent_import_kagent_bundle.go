/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/cli/client"
)

// A deployment bundle holds what one wrapped Agent's runtime is built and
// registered from. The runtime's name carries its configuration digest, so a
// re-pin makes a new runtime beside the old one: Orka cannot change a
// registered AgentRuntime's profile in place, and the old runtime keeps
// serving its sessions until it is deleted.
const (
	kagentBundleConfigFile  = "kagent.json"
	kagentBundleRuntimeFile = "runtime.json"
	// kagentBundleRegistrationFile records the UID of the AgentRuntime the
	// bundle was deployed and registered as.
	kagentBundleRegistrationFile = "registration.uid"
	// kagentBundleDigestChars of the configuration digest name each runtime.
	kagentBundleDigestChars = 10
	kagentRelayScheme       = "http"
)

// kagentBundle is runtime.json: what the runtime renderer reads.
type kagentBundle struct {
	// Agent is the Orka Agent, and Runtime the AgentRuntime that serves it.
	Agent   string `json:"agent"`
	Runtime string `json:"runtime"`
	// KagentNamespace and KagentName name the wrapped kagent Agent.
	KagentNamespace string `json:"kagentNamespace"`
	KagentName      string `json:"kagentName"`
	// KagentVersion is "v0" or "v1".
	KagentVersion string `json:"kagentVersion"`
	// Source identifies the wrapped kagent Agent as the Orka Agent's
	// orka.ai/kagent-source annotation does: "<apiVersion> <namespace>/<name>".
	// Every object of the runtime carries it, and only objects that do serve
	// this kagent Agent, so kagent Agents of one name in different namespaces
	// or API versions never share a runtime or relay.
	Source   string `json:"source"`
	Governed bool   `json:"governed"`
	// Relay is the origin a governed Agent's model and tools point at. Its
	// Service name is part of the kagent Agent's pinned config, so it stays
	// the same for every runtime of the Agent.
	Relay string `json:"relay,omitempty"`
	Model string `json:"model"`
	// ConfigDigest is the sha256 of kagent.json, the profile's agent
	// configuration digest.
	ConfigDigest string `json:"configDigest"`
	// Revision distinguishes runtimes of one configuration built from new
	// images (--runtime-revision).
	Revision int `json:"revision,omitempty"`
	// AllowedTools and ApprovalRequiredTools are a governed Agent's Orka tools.
	AllowedTools          []string `json:"allowedTools"`
	ApprovalRequiredTools []string `json:"approvalRequiredTools"`
}

// kagentBundleRuntimeName names the runtime for one configuration and Orka
// tool policy, both part of its immutable profile. The images are too, but
// they are built after the name is chosen, so a runtime keeps the images it
// was first deployed with; revision, when positive, names a new runtime for
// new images.
func kagentBundleRuntimeName(agent string, config []byte, allowed, approvalRequired []string, revision int) string {
	identity, _ := json.Marshal(struct {
		Allowed, ApprovalRequired []string
		Revision                  int `json:",omitempty"`
	}{allowed, approvalRequired, revision})
	sum := sha256.Sum256(append(append(slices.Clone(config), 0), identity...))
	return agent + "-kagent-" + hex.EncodeToString(sum[:])[:kagentBundleDigestChars]
}

// validateBundle checks what a bundle adds: names that fit the runtime's
// Kubernetes objects, and approvals only for tools a governed Agent calls.
func (o kagentWrapperOptions) validateBundle(wrapped []kagentWrappedAgent) error {
	if o.BundleDir == "" {
		if len(o.ApprovalRequiredTools) > 0 || len(o.AllowedTools) > 0 || o.RuntimeRevision != 0 {
			return fmt.Errorf("--allowed-tool, --approval-required-tool, and --runtime-revision apply only with --bundle-dir")
		}
		return nil
	}
	if o.RuntimeRevision < 0 {
		return fmt.Errorf("--runtime-revision must not be negative")
	}
	known := map[string]bool{}
	for _, tool := range o.AllowedTools {
		known[tool] = true
	}
	// A relay Service selects one runtime, so two governed Agents cannot
	// share one.
	relays := map[string]string{}
	for _, agent := range wrapped {
		name := metadataName(agent.Manifest)
		// The runtime's name is its Service's, a DNS label, and a label value.
		if problems := utilvalidation.IsDNS1035Label(kagentBundleRuntimeName(name, nil, nil, nil, 0)); len(problems) > 0 {
			return fmt.Errorf("kagent Agent %s: Orka Agent name %q is too long for its runtime's Service name (at most 45 characters, starting with a letter)", agent.Source, name)
		}
		if agent.Mode.Mode == kagentModeGoverned {
			service, namespace, _, err := kagentRelayService(agent.Mode.RelayOrigin)
			if err != nil {
				return fmt.Errorf("kagent Agent %s: %w", agent.Source, err)
			}
			key := namespace + "/" + service
			if other, taken := relays[key]; taken {
				return fmt.Errorf("kagent Agents %s and %s share relay Service %s; give each Agent its own, such as with {name} in --relay-url", other, agent.Source, key)
			}
			relays[key] = agent.Source
			// The registered tool policy is exact, so an empty one would
			// refuse every call a binding without tool names makes.
			if agent.Mode.AnyTool && len(o.AllowedTools) == 0 {
				return fmt.Errorf("kagent Agent %s: a tool binding names no tools, so kagent calls whatever the relay lists; name the Orka tools it may call with --allowed-tool", agent.Source)
			}
		}
		for _, tool := range agent.Mode.Tools {
			known[tool] = true
		}
	}
	for _, tool := range o.ApprovalRequiredTools {
		if !known[tool] {
			return fmt.Errorf("--approval-required-tool %q is not an allowed tool of any governed Agent", tool)
		}
	}
	return nil
}

// kagentBundlePolicy returns a governed Agent's Orka tool policy. Its allowed
// tools are the ones its bindings name and the ones --allowed-tool names; a
// binding that names none lets kagent call whatever Orka's MCP server lists.
// An observed Agent brokers no Orka tools.
func (o kagentWrapperOptions) kagentBundlePolicy(agent kagentWrappedAgent) (allowed, approvalRequired []string) {
	allowed, approvalRequired = []string{}, []string{}
	if agent.Mode.Mode != kagentModeGoverned {
		return allowed, approvalRequired
	}
	// The registered policy lists are explicit: an Agent with no tools
	// allows none, never null. --allowed-tool adds to the tools an Agent's
	// bindings name, so one import can name the tools of an Agent whose
	// binding names none without dropping another Agent's own.
	allowed = append(allowed, agent.Mode.Tools...)
	allowed = append(allowed, o.AllowedTools...)
	slices.Sort(allowed)
	allowed = slices.Compact(allowed)
	for _, tool := range o.ApprovalRequiredTools {
		if slices.Contains(allowed, tool) && !slices.Contains(approvalRequired, tool) {
			approvalRequired = append(approvalRequired, tool)
		}
	}
	slices.Sort(approvalRequired)
	return allowed, approvalRequired
}

// pinKagentBundleRuntimes names each Agent's runtime after its configuration
// and points the Orka Agent at it.
func pinKagentBundleRuntimes(o kagentWrapperOptions, wrapped []kagentWrappedAgent) ([]kagentWrappedAgent, error) {
	if o.BundleDir == "" {
		return wrapped, nil
	}
	for i, agent := range wrapped {
		config, err := o.wrapperConfig(agent)
		if err != nil {
			return nil, err
		}
		allowed, approvalRequired := o.kagentBundlePolicy(agent)
		runtime := kagentBundleRuntimeName(metadataName(agent.Manifest), config, allowed, approvalRequired, o.RuntimeRevision)
		agent.RuntimeName = runtime
		agent.Manifest = cloneManifest(agent.Manifest)
		spec, _ := agent.Manifest[kagentFieldSpec].(map[string]any)
		spec["runtime"] = map[string]any{"runtimeRef": map[string]any{cliNameKey: runtime}}
		agent.RequireReadyRuntime = true
		wrapped[i] = agent
	}
	return wrapped, nil
}

func cloneManifest(manifest map[string]any) map[string]any {
	data, _ := json.Marshal(manifest)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	return clone
}

// writeKagentBundles writes <dir>/<agent>/kagent.json and runtime.json.
// kagentBundleFiles returns the files of an Agent's deployment bundle.
func (o kagentWrapperOptions) kagentBundleFiles(agent kagentWrappedAgent) (map[string][]byte, error) {
	config, err := o.wrapperConfig(agent)
	if err != nil {
		return nil, err
	}
	version := agent.Version
	if version == "" {
		version = "v1"
	}
	sum := sha256.Sum256(config)
	bundle := kagentBundle{
		Agent: metadataName(agent.Manifest), Runtime: agent.RuntimeName,
		KagentNamespace: agent.SourceNamespace, KagentName: agent.SourceName, KagentVersion: version,
		Source:   kagentString(agent.Manifest, kagentFieldMetadata, kagentFieldAnnotations, kagentSourceAnnotation),
		Governed: agent.Mode.Mode == kagentModeGoverned, Model: kagentWrapperModel(agent),
		ConfigDigest: "sha256:" + hex.EncodeToString(sum[:]), Revision: o.RuntimeRevision,
	}
	if bundle.Governed {
		bundle.Relay = agent.Mode.RelayOrigin
	}
	bundle.AllowedTools, bundle.ApprovalRequiredTools = o.kagentBundlePolicy(agent)
	metadata, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string][]byte{kagentBundleConfigFile: config, kagentBundleRuntimeFile: append(metadata, '\n')}, nil
}

func writeKagentBundles(o kagentWrapperOptions, wrapped []kagentWrappedAgent) ([]string, error) {
	if o.BundleDir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(o.BundleDir, 0o750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(o.BundleDir)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck
	var written []string
	for _, agent := range wrapped {
		files, err := o.kagentBundleFiles(agent)
		if err != nil {
			return nil, err
		}
		name := metadataName(agent.Manifest)
		if err := root.MkdirAll(name, 0o750); err != nil {
			return nil, err
		}
		for file, data := range files {
			if err := root.WriteFile(filepath.Join(name, file), data, 0o600); err != nil {
				return nil, err
			}
		}
		written = append(written, filepath.Join(o.BundleDir, name))
	}
	slices.Sort(written)
	return written, nil
}

// kagentUID matches a Kubernetes object UID.
var kagentUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// verifyKagentBundles refuses an activation whose arguments pin anything but
// the bundles in the directory: those are the ones deployed, whose relays
// were checked before it. It reads the registration each deploy recorded,
// which activation then requires.
func verifyKagentBundles(o kagentWrapperOptions, wrapped []kagentWrappedAgent) error {
	if o.BundleDir == "" {
		return nil
	}
	root, err := os.OpenRoot(o.BundleDir)
	if err != nil {
		return fmt.Errorf("reading the deployment bundles: %w", err)
	}
	defer root.Close() //nolint:errcheck
	for i, agent := range wrapped {
		files, err := o.kagentBundleFiles(agent)
		if err != nil {
			return err
		}
		name := metadataName(agent.Manifest)
		for _, file := range slices.Sorted(maps.Keys(files)) {
			if have, err := root.ReadFile(filepath.Join(name, file)); err != nil || !bytes.Equal(have, files[file]) {
				return fmt.Errorf("kagent Agent %s: the bundle in %s is not the one these arguments pin (runtime %s); write it with --dry-run and deploy it before activating",
					agent.Source, filepath.Join(o.BundleDir, name), agent.RuntimeName)
			}
		}
		uid, err := root.ReadFile(filepath.Join(name, kagentBundleRegistrationFile))
		if wrapped[i].RuntimeUID = strings.TrimSpace(string(uid)); err != nil || !kagentUID.MatchString(wrapped[i].RuntimeUID) {
			return fmt.Errorf("kagent Agent %s: the bundle in %s records no registration (%s); deploy it before activating",
				agent.Source, filepath.Join(o.BundleDir, name), kagentBundleRegistrationFile)
		}
	}
	return nil
}

// kagentRelayService names the in-cluster Service a relay origin addresses,
// http://<service>.<namespace>.svc[.cluster.local]:<port>, which a runtime's
// render must create.
func kagentRelayService(origin string) (service, namespace string, port int, err error) {
	u, parseErr := url.Parse(origin)
	// The relay serves plain HTTP inside the cluster.
	if parseErr != nil || u.Scheme != kagentRelayScheme || u.Port() == "" {
		return "", "", 0, fmt.Errorf("relay %q must be http://<service>.<namespace>.svc:<port>", origin)
	}
	host := strings.TrimSuffix(strings.TrimSuffix(u.Hostname(), ".cluster.local"), ".svc")
	parts := strings.Split(host, ".")
	port, convErr := strconv.Atoi(u.Port())
	if len(parts) != 2 || convErr != nil || port < 1 || port > 65535 ||
		len(utilvalidation.IsDNS1035Label(parts[0])) > 0 || len(utilvalidation.IsDNS1123Label(parts[1])) > 0 {
		return "", "", 0, fmt.Errorf("relay %q must be http://<service>.<namespace>.svc:<port>", origin)
	}
	return parts[0], parts[1], port, nil
}

// requireReadyKagentRuntime refuses to point an Agent at a runtime Orka has
// not registered as ready, so a re-pin never sends new Tasks to a runtime
// that cannot serve them.
func requireReadyKagentRuntime(ctx context.Context, c *client.Client, namespace, runtime, uid string) error {
	value, err := c.DoJSON(ctx, http.MethodGet, "/api/v1/agent-runtimes/"+url.PathEscape(runtime), map[string]string{cliNamespaceQuery: namespace}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return fmt.Errorf("AgentRuntime %q is not registered; deploy and register it before activating the bundle", runtime)
		}
		return fmt.Errorf("reading AgentRuntime %q: %w", runtime, err)
	}
	runtimeObj, _ := value.(map[string]any)
	// The Orka API this import reaches must serve the registration the
	// bundle's deploy created, not one of the same name in another cluster.
	if got := kagentString(runtimeObj, "metadata", "uid"); got != uid {
		return fmt.Errorf("AgentRuntime %q has UID %q, not %q, the registration the bundle was deployed as; does the Orka CLI target the cluster the deploy did?", runtime, got, uid)
	}
	// Orka admits Tasks only to a Ready runtime that has observed its
	// current spec and is not being deleted.
	if kagentMap(runtimeObj, "metadata")["deletionTimestamp"] != nil {
		return fmt.Errorf("AgentRuntime %q is being deleted; deploy the bundle again under a new --runtime-revision", runtime)
	}
	if generation := kagentInt(runtimeObj, "metadata", "generation"); generation == 0 ||
		kagentInt(runtimeObj, "status", "observedGeneration") != generation {
		return fmt.Errorf("AgentRuntime %q has not observed its current spec; wait for it before activating the bundle", runtime)
	}
	if ready, _ := kagentMap(runtimeObj, "status")["ready"].(bool); ready {
		conditions, _ := kagentMap(runtimeObj, "status")["conditions"].([]any)
		for _, raw := range conditions {
			condition, _ := raw.(map[string]any)
			if condition["type"] == kagentConditionReady && condition["status"] == "True" {
				return nil
			}
		}
	}
	return fmt.Errorf("AgentRuntime %q is not Ready; wait for it before activating the bundle", runtime)
}
