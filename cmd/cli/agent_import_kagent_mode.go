/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Wrapped kagent Agents run in one of two modes. Observed: kagent runs the
// Agent's model and tools; Orka governs dispatch and lifecycle, provided no
// one but kagent's controller can drive the Agent's sessions (on v1, reach
// Substrate's actor router; on v0, reach the Agent's Service, while the
// controller admits only the wrapper brokers and the wrapped Agents).
// Governed: every model and tool endpoint points at the wrapper's relay, so
// Orka runs the tools under its policy and serves the model. Only runtimes
// that forward the turn's bearer to models and MCP servers can be governed:
// the ADK harness on v1, and the Go runtime on v0.
const (
	kagentModeObserved   = "observed"
	kagentModeGoverned   = "governed"
	kagentModeTranslated = "translated"

	kagentDiscoveryLabel = "kagent.dev/discovery"
	// The relay may hold a tool call for an Orka approval (up to 15 minutes)
	// and waits 20 minutes; kagent's MCP client must wait strictly longer,
	// because it starts waiting before the relay does.
	kagentRelayTimeout = 20 * time.Minute
)

var kagentRevisionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// kagentMode is the onboarding verdict for one kagent Agent.
type kagentMode struct {
	Mode string
	// Reasons explain why an Agent is observed rather than governed.
	Reasons []string
	// RelayOrigin is the relay every governed endpoint points at.
	RelayOrigin string
	// Revision is what the wrapper pins: the Agent's latest successful kagent
	// revision on v1, or its Deployment's config hash on v0, if known.
	Revision string
	// PinProblems explain why a v0 Agent has no Revision to pin.
	PinProblems []string
}

// kagentRevisionEvidence binds a verdict to the pinned revision. The compiled
// egress allowlist, enforced by kagent's gateway, is the boundary that keeps a
// revision's model and tools behind the relay. The source snapshot proves the
// revision was compiled from the declared resources the verdict reads.
type kagentRevisionEvidence struct {
	// Revisions maps a kagent revision to its compiled egress destinations.
	Revisions map[string][]string
	// Sources maps a kagent revision to its source snapshot.
	Sources map[string]kagentRevisionSource
	// Accepted are origins a governed revision may reach besides its relay,
	// such as the runtime control-plane filter.
	Accepted []string
	// Controller is the kagent controller URL (--control-plane-url). A
	// governed v0 Agent must persist its sessions there.
	Controller string
}

// kagentRevisionSource is the part of a revision's source snapshot
// (kagent.agent_runtime_revision.source_snapshot) that names the objects the
// revision was compiled from.
type kagentRevisionSource struct {
	AgentUID string                `json:"agentUID"`
	Inputs   []kagentRevisionInput `json:"inputs"`
}

type kagentRevisionInput struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

// kagentObjects indexes the kagent resources supplied next to the Agents.
type kagentObjects map[string]map[string]any

func kagentObjectKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

// indexKagentObjects indexes the objects of one kagent version. A v0 Agent's
// Deployment and config Secret belong to the v0 index: the wrapper pins the
// Deployment's config hash, and the Secret holds the configuration it hashes.
func indexKagentObjects(objects []map[string]any, version string) kagentObjects {
	index := kagentObjects{}
	for _, obj := range objects {
		apiVersion, _ := obj["apiVersion"].(string)
		kind, _ := obj["kind"].(string)
		v0Workload := version == kagentV0APIVersion &&
			((apiVersion == "apps/v1" && kind == "Deployment") || (apiVersion == "v1" && kind == "Secret"))
		if apiVersion != version && !v0Workload {
			continue
		}
		index[kagentObjectKey(kind, kagentString(obj, "metadata", cliNamespaceQuery), kagentString(obj, "metadata", cliNameKey))] = obj
	}
	return index
}

func (o kagentObjects) lookup(kind, namespace, name string) map[string]any {
	return o[kagentObjectKey(kind, namespace, name)]
}

// kagentInputs records the related objects a classification reads, so the
// verdict can be bound to a revision compiled from exactly those objects.
type kagentInputs struct {
	objects kagentObjects
	read    map[string]map[string]any
}

func (i *kagentInputs) lookup(kind, namespace, name string) map[string]any {
	obj := i.objects.lookup(kind, namespace, name)
	if obj != nil {
		i.read[kagentObjectKey(kind, namespace, name)] = obj
	}
	return obj
}

// classifyKagentAgent decides governed or observed from the Agent and the
// related objects in the same input. relay is the wrapper relay's origin for
// this Agent, established by the operator; endpoint shape alone never proves
// that a URL is Orka's relay. evidence binds the verdict to what the pinned
// revision can reach. Anything it cannot prove keeps the Agent observed.
func classifyKagentAgent(agent map[string]any, related kagentObjects, relay string, evidence kagentRevisionEvidence) kagentMode {
	namespace := kagentString(agent, "metadata", cliNamespaceQuery)
	mode := kagentMode{Revision: kagentString(agent, "status", "latestSuccessfulRevision")}
	inputs := &kagentInputs{objects: related, read: map[string]map[string]any{}}
	var reasons []string
	if relay == "" {
		reasons = append(reasons, "no relay was named (--relay-url)")
	}
	// The wrapper pins the latest successful revision, so the specs checked
	// here must be the ones that revision was compiled from.
	desired := kagentString(agent, "status", "desiredRevision")
	switch generation, observed := kagentInt(agent, "metadata", "generation"), kagentInt(agent, "status", "observedGeneration"); {
	case generation == 0 || observed != generation:
		reasons = append(reasons, "kagent has not observed the Agent's current generation")
	case mode.Revision == "" || desired != mode.Revision:
		reasons = append(reasons, "the latest successful revision is not the one the current spec compiles to")
	}
	harnessName := kagentString(agent, "spec", "harnessRef", cliNameKey)
	reasons = append(reasons, harnessProblems(inputs.lookup("Harness", namespace, harnessName), harnessName)...)
	templateReasons, origins := templateProblems(agent, inputs, namespace)
	reasons = append(reasons, templateReasons...)
	for _, origin := range origins {
		if relay != "" && origin != relay {
			reasons = append(reasons, fmt.Sprintf("an endpoint points at %s, not the relay %s", origin, relay))
		}
	}
	reasons = append(reasons, egressProblems(evidence, mode.Revision, relay)...)
	reasons = append(reasons, sourceProblems(evidence, mode.Revision, agent, inputs.read)...)
	if len(reasons) > 0 {
		mode.Mode, mode.Reasons = kagentModeObserved, reasons
		return mode
	}
	mode.Mode, mode.RelayOrigin = kagentModeGoverned, relay
	return mode
}

func harnessProblems(harness map[string]any, name string) []string {
	if harness == nil {
		return []string{fmt.Sprintf("harness %q is not in the input", name)}
	}
	if kagentMap(harness, "spec", "kagent") == nil {
		return []string{fmt.Sprintf("harness %q is not the ADK (kagent) harness", name)}
	}
	var problems []string
	// Only the default workload is known to run the compiled configuration.
	workload := kagentMap(harness, "spec", "workload")
	for _, field := range []string{"image", "command", "args"} {
		if _, set := workload[field]; set {
			problems = append(problems, fmt.Sprintf("harness %q overrides the workload %s", name, field))
		}
	}
	if env, _ := kagentMap(harness, "spec")["env"].([]any); len(env) > 0 {
		problems = append(problems, fmt.Sprintf("harness %q injects environment overrides", name))
	}
	if kagentMap(harness, "spec", "kagent", "memory") != nil {
		problems = append(problems, fmt.Sprintf("harness %q enables memory", name))
	}
	if kagentMap(harness, "spec", "kagent", "compaction") != nil {
		problems = append(problems, fmt.Sprintf("harness %q compacts with its own model", name))
	}
	return problems
}

// templateProblems checks the Agent's model and tools, and returns the origins
// their endpoints point at.
func templateProblems(agent map[string]any, related *kagentInputs, namespace string) ([]string, []string) {
	template := kagentMap(agent, "spec", "template", "inline")
	if template == nil {
		ref := kagentString(agent, "spec", "template", "ref", cliNameKey)
		found := related.lookup("AgentTemplate", namespace, ref)
		if found == nil {
			return []string{fmt.Sprintf("agent template %q is not in the input", ref)}, nil
		}
		template = kagentMap(found, "spec")
	}
	var problems []string
	origins := map[string]bool{}
	// These checks read the declared resources. The revision-bound boundary
	// is the compiled egress checked below: with bash disabled for skills, a
	// local skill or plugin that survives in the pinned revision can reach
	// only the relay and the accepted origins, so it cannot act outside Orka.
	for _, field := range []string{"skills", "plugins"} {
		if items, _ := template[field].([]any); len(items) > 0 {
			problems = append(problems, fmt.Sprintf("the template uses %s, which run inside kagent", field))
		}
	}
	modelName := kagentString(template, "modelConfig", cliNameKey)
	switch model := related.lookup("ModelConfig", namespace, modelName); {
	case model == nil:
		problems = append(problems, fmt.Sprintf("model config %q is not in the input", modelName))
	case kagentString(model, "spec", "provider") != "OpenAI":
		problems = append(problems, fmt.Sprintf("model config %q does not use the OpenAI provider", modelName))
	case !kagentBool(model, "spec", "apiKeyPassthrough"):
		problems = append(problems, fmt.Sprintf("model config %q does not pass the caller's bearer through", modelName))
	case len(kagentMap(model, "spec", "defaultHeaders")) > 0:
		// kagent applies static headers after the passthrough bearer, so one
		// can replace the turn token the relay authorizes.
		problems = append(problems, fmt.Sprintf("model config %q sends static headers (defaultHeaders)", modelName))
	case !slices.Contains([]string{"", "chatCompletions"}, kagentString(model, "spec", "openAI", "apiFormat")):
		// kagent defaults apiFormat to chatCompletions; the relay serves only
		// /v1/chat/completions.
		problems = append(problems, fmt.Sprintf("model config %q uses the %s API; the relay serves only chat completions", modelName, kagentString(model, "spec", "openAI", "apiFormat")))
	default:
		if origin, ok := relayOrigin(kagentString(model, "spec", "openAI", "baseUrl"), "/v1"); ok {
			origins[origin] = true
		} else {
			problems = append(problems, fmt.Sprintf("model config %q does not point at a relay's /v1", modelName))
		}
	}
	tools, _ := template[kagentFieldTools].([]any)
	for i, raw := range tools {
		binding, _ := raw.(map[string]any)
		toolProblems, origin := toolBindingProblems(binding, i, related, namespace)
		problems = append(problems, toolProblems...)
		if origin != "" {
			origins[origin] = true
		}
	}
	sorted := make([]string, 0, len(origins))
	for origin := range origins {
		sorted = append(sorted, origin)
	}
	slices.Sort(sorted)
	return problems, sorted
}

// kagentV1WrapRefusals lists what keeps a v1 Agent from being wrapped in
// either mode. A binding that requires kagent approval pauses each call for a
// prompt the wrapper cannot answer, so the broker would cancel every such turn.
func kagentV1WrapRefusals(agent map[string]any, related kagentObjects, namespace string) []string {
	template := kagentMap(agent, kagentFieldSpec, "template", "inline")
	visited := map[string]bool{}
	if template == nil {
		ref := kagentString(agent, kagentFieldSpec, "template", "ref", cliNameKey)
		found := related.lookup("AgentTemplate", namespace, ref)
		if found == nil && ref != "" {
			return []string{fmt.Sprintf("agent template %q is not in the input, so the import cannot confirm it has no kagent approval bindings", ref)}
		}
		template = kagentMap(found, kagentFieldSpec)
		visited[ref] = true
	}
	return kagentV1ApprovalRefusals(template, related, namespace, "", visited)
}

// kagentV1ApprovalRefusals checks a template's bindings and those of the
// subagent templates kagent compiles into the same runtime, every one of
// which must be in the input.
func kagentV1ApprovalRefusals(template map[string]any, related kagentObjects, namespace, where string, visited map[string]bool) []string {
	var problems []string
	tools, _ := template[kagentFieldTools].([]any)
	for i, raw := range tools {
		binding, _ := raw.(map[string]any)
		if approval, set := kagentMap(binding, "mcp")["requireApproval"]; set && approval != nil && approval != false {
			if list, isList := approval.([]any); !isList || len(list) > 0 {
				problems = append(problems, fmt.Sprintf("%stool %d requires kagent approval (mcp.requireApproval), which a wrapped Agent cannot answer", where, i))
			}
		}
		ref := kagentString(binding, "subAgent", "templateRef", cliNameKey)
		if ref == "" || visited[ref] {
			continue
		}
		visited[ref] = true
		child := related.lookup("AgentTemplate", namespace, ref)
		if child == nil {
			problems = append(problems, fmt.Sprintf("%stool %d uses subagent template %q, which is not in the input, so the import cannot confirm it has no kagent approval bindings", where, i, ref))
			continue
		}
		problems = append(problems, kagentV1ApprovalRefusals(kagentMap(child, kagentFieldSpec), related, namespace,
			fmt.Sprintf("subagent template %q ", ref), visited)...)
	}
	return problems
}

func toolBindingProblems(binding map[string]any, index int, related *kagentInputs, namespace string) ([]string, string) {
	server := kagentMap(binding, "mcp", "server")
	if server == nil {
		return []string{fmt.Sprintf("tool %d is not an MCP binding", index)}, ""
	}
	serverName, _ := server[cliNameKey].(string)
	for field := range kagentMap(binding, "mcp") {
		if !kagentMCPBindingField(field) {
			// A kagent approval prompt cannot be answered by a wrapped Agent;
			// Orka's approval policy governs relayed tools instead.
			return []string{fmt.Sprintf("tool server %q binding sets mcp.%s", serverName, field)}, ""
		}
	}
	if kind, _ := server["kind"].(string); kind != "RemoteMCPServer" {
		return []string{fmt.Sprintf("tool server %q is a %s, not a RemoteMCPServer", serverName, kind)}, ""
	}
	remote := related.lookup("RemoteMCPServer", namespace, serverName)
	if remote == nil {
		return []string{fmt.Sprintf("tool server %q is not in the input", serverName)}, ""
	}
	var problems []string
	for _, reason := range governedRemoteMCPServerProblems(remote) {
		problems = append(problems, fmt.Sprintf("tool server %q %s", serverName, reason))
	}
	origin, _ := relayOrigin(kagentString(remote, "spec", "url"), "/mcp")
	return problems, origin
}

// egressProblems requires the pinned revision's compiled egress to reach only
// the relay and the accepted origins.
func egressProblems(evidence kagentRevisionEvidence, revision, relay string) []string {
	egress, ok := evidence.Revisions[revision]
	if !ok {
		return []string{fmt.Sprintf("no compiled egress was supplied for revision %q (--revision-egress)", revision)}
	}
	var problems []string
	for _, destination := range egress {
		if origin, ok := relayOrigin(destination, ""); !ok || (origin != relay && !slices.Contains(evidence.Accepted, origin)) {
			problems = append(problems, "the pinned revision can reach "+destination)
		}
	}
	return problems
}

// sourceProblems requires the pinned revision to have been compiled from
// exactly the objects the classification read. kagent does not change an
// Agent's generation when a referenced object changes, so until kagent
// recompiles, the latest successful revision can predate the objects in the
// input. The source snapshot records each input object's UID and generation.
func sourceProblems(evidence kagentRevisionEvidence, revision string, agent map[string]any, read map[string]map[string]any) []string {
	source, ok := evidence.Sources[revision]
	if !ok {
		return []string{fmt.Sprintf("no source snapshot was supplied for revision %q (--revision-sources)", revision)}
	}
	var problems []string
	if uid := kagentString(agent, "metadata", "uid"); uid == "" || uid != source.AgentUID {
		problems = append(problems, "the pinned revision was not compiled for this Agent's metadata.uid")
	}
	for _, key := range slices.Sorted(maps.Keys(read)) {
		obj := read[key]
		kind, name := kagentString(obj, "kind"), kagentString(obj, "metadata", cliNameKey)
		uid, generation := kagentString(obj, "metadata", "uid"), kagentInt(obj, "metadata", "generation")
		if uid == "" || generation == 0 || !slices.ContainsFunc(source.Inputs, func(input kagentRevisionInput) bool {
			return input.APIVersion == kagentV1APIVersion && input.Kind == kind && input.Name == name && input.UID == uid && input.Generation == generation
		}) {
			problems = append(problems, fmt.Sprintf("the pinned revision was not compiled from the %s %q in the input (metadata.uid and metadata.generation)", kind, name))
		}
	}
	return problems
}

// kagentFieldTools names a template's tool list and a binding's tool names.
const kagentFieldTools = "tools"

// kagentMCPBindingField reports the MCP binding fields both modes understand:
// the server and its tool names. Every other field changes behavior.
func kagentMCPBindingField(field string) bool {
	return field == "server" || field == kagentFieldTools
}

// kagentRelayOrigin expands the --relay-url template for one Agent.
func kagentRelayOrigin(template, name string) (string, error) {
	if template == "" {
		return "", nil
	}
	origin, ok := relayOrigin(strings.ReplaceAll(template, "{name}", name), "")
	if !ok {
		return "", fmt.Errorf("--relay-url must be an http(s) origin with no path, optionally using {name}")
	}
	return origin, nil
}

func governedRemoteMCPServerProblems(remote map[string]any) []string {
	var problems []string
	if kagentString(remote, "spec", "protocol") != "STREAMABLE_HTTP" {
		problems = append(problems, "does not use STREAMABLE_HTTP")
	}
	if _, ok := relayOrigin(kagentString(remote, "spec", "url"), "/mcp"); !ok {
		problems = append(problems, "does not point at a relay's /mcp")
	}
	if timeout, err := time.ParseDuration(kagentString(remote, "spec", "timeout")); err != nil || timeout <= kagentRelayTimeout {
		problems = append(problems, fmt.Sprintf("needs a timeout longer than %s", kagentRelayTimeout))
	}
	if labels := kagentMap(remote, "metadata", "labels"); labels == nil || labels[kagentDiscoveryLabel] != "disabled" {
		problems = append(problems, "does not disable discovery with "+kagentDiscoveryLabel+"=disabled")
	}
	if headers, _ := kagentMap(remote, "spec")["headersFrom"].([]any); len(headers) > 0 {
		problems = append(problems, "sends static headers")
	}
	return problems
}

// relayOrigin returns the http(s) origin of value when its path is exactly path.
func relayOrigin(value, path string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !kagentHTTPScheme(u.Scheme) || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.TrimSuffix(u.Path, "/") != path {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// kagentWrapperOptions configure the per-Agent wrapper configuration files.
type kagentWrapperOptions struct {
	Dir             string
	ControlPlaneURL string
	UserIDPrefix    string
	UpstreamURL     string
	UpstreamModel   string
}

// kagentWrapperConfig mirrors the wrapper's /agent/kagent.json.
type kagentWrapperConfig struct {
	Model    string                 `json:"model"`
	Kagent   kagentWrapperTarget    `json:"kagent"`
	Governed *kagentWrapperGoverned `json:"governed,omitempty"`
}

type kagentWrapperTarget struct {
	// Version selects the wrapper's kagent backend; it is omitted for v1.
	Version          string            `json:"version,omitempty"`
	ControlPlaneURL  string            `json:"controlPlaneURL"`
	Agent            map[string]string `json:"agent"`
	ExpectedRevision string            `json:"expectedRevision"`
	UserIDPrefix     string            `json:"userIDPrefix"`
}

type kagentWrapperGoverned struct {
	UpstreamBaseURL string `json:"upstreamBaseURL"`
	UpstreamModel   string `json:"upstreamModel"`
}

// kagentUserIDPrefixPattern matches the wrapper's user ID prefix rule.
var kagentUserIDPrefixPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

// validate applies the wrapper's own configuration rules, so the import never
// writes a file the wrapper refuses to load.
func (o kagentWrapperOptions) validate(wrapped []kagentWrappedAgent) error {
	if o.Dir == "" {
		return nil
	}
	if !kagentControlPlaneURL(o.ControlPlaneURL) {
		return fmt.Errorf("--control-plane-url must be the kagent controller's http(s) base URL with a DNS host name")
	}
	if !kagentUserIDPrefixPattern.MatchString(o.UserIDPrefix) {
		return fmt.Errorf("--user-id-prefix must be 1-32 lowercase letters, digits, or hyphens, starting and ending with a letter or digit")
	}
	for _, agent := range wrapped {
		if agent.SourceNamespace == "" {
			return fmt.Errorf("kagent Agent %s has no metadata.namespace; the wrapper configuration must name it, so export the Agent with kubectl get -o yaml", agent.Source)
		}
		if len(kagentWrapperModel(agent)) > 256 {
			return fmt.Errorf("kagent Agent %s: its namespace and name are too long for the wrapper's 256-byte model name", agent.Source)
		}
		if agent.Version == kagentVersionV0 {
			if !kagentConfigHashPattern.MatchString(agent.Mode.Revision) {
				why := "its Deployment has no " + kagentConfigHashAnnotation + " annotation"
				if len(agent.Mode.PinProblems) > 0 {
					why = strings.Join(agent.Mode.PinProblems, "; ")
				}
				return fmt.Errorf("kagent Agent %s has no config hash to pin (%s); export the Agent and its Deployment with kubectl get -o yaml after the rollout finishes", agent.Source, why)
			}
		} else if !kagentRevisionPattern.MatchString(agent.Mode.Revision) {
			return fmt.Errorf("kagent Agent %s has no status.latestSuccessfulRevision to pin; export it with kubectl get -o yaml after it is Ready", agent.Source)
		}
		if agent.Mode.Mode == kagentModeGoverned && (!kagentUpstreamURL(o.UpstreamURL) || !kagentSafeString(o.UpstreamModel, 256)) {
			return fmt.Errorf("kagent Agent %s is governed: --model-upstream-url (an http(s) base URL) and --model-upstream-model name the model Orka serves it", agent.Source)
		}
	}
	return nil
}

func kagentWrapperModel(agent kagentWrappedAgent) string {
	return "kagent/" + agent.SourceNamespace + "/" + agent.SourceName
}

// kagentControlPlaneURL accepts an http(s) origin whose host is a DNS name:
// Substrate egress rules match DNS names, not literal IPs.
func kagentControlPlaneURL(value string) bool {
	if _, ok := relayOrigin(value, ""); !ok || !kagentSafeString(value, 2048) || strings.Contains(value, "?") {
		return false
	}
	u, _ := url.Parse(value)
	host := u.Hostname()
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return net.ParseIP(host) == nil && len(utilvalidation.IsDNS1123Subdomain(host)) == 0
}

// kagentUpstreamURL accepts an http(s) base URL with an optional path and no
// credentials, query, or fragment.
func kagentUpstreamURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && kagentSafeString(value, 2048) && kagentHTTPScheme(u.Scheme) && u.Host != "" &&
		u.User == nil && !strings.ContainsAny(value, "?#") && u.RawPath == ""
}

func kagentHTTPScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// kagentSafeString reports whether value is nonempty, trimmed, at most limit
// bytes, and free of control characters.
func kagentSafeString(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// writeKagentWrapperConfigs writes <name>.kagent.json for each Agent. The
// output is deterministic, so a re-run produces the same image digest.
func writeKagentWrapperConfigs(o kagentWrapperOptions, wrapped []kagentWrappedAgent) ([]string, error) {
	if o.Dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(o.Dir, 0o750); err != nil {
		return nil, err
	}
	// The root confines every write to the directory; file names are
	// validated Orka resource names.
	root, err := os.OpenRoot(o.Dir)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck
	var written []string
	for _, agent := range wrapped {
		namespace, name := agent.SourceNamespace, agent.SourceName
		config := kagentWrapperConfig{
			Model: kagentWrapperModel(agent),
			Kagent: kagentWrapperTarget{
				Version:         agent.Version,
				ControlPlaneURL: strings.TrimSuffix(o.ControlPlaneURL, "/"), Agent: map[string]string{cliNamespaceQuery: namespace, cliNameKey: name},
				ExpectedRevision: agent.Mode.Revision, UserIDPrefix: o.UserIDPrefix,
			},
		}
		if agent.Mode.Mode == kagentModeGoverned {
			config.Governed = &kagentWrapperGoverned{UpstreamBaseURL: o.UpstreamURL, UpstreamModel: o.UpstreamModel}
		}
		data, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			return nil, err
		}
		file := metadataName(agent.Manifest) + ".kagent.json"
		if err := root.WriteFile(file, append(data, '\n'), 0o600); err != nil {
			return nil, err
		}
		written = append(written, filepath.Join(o.Dir, file))
	}
	slices.Sort(written)
	return written, nil
}

func describeKagentMode(mode kagentMode) string {
	switch mode.Mode {
	case kagentModeGoverned:
		return "governed via " + mode.RelayOrigin
	case kagentModeTranslated:
		return "translated to a native Orka Agent"
	}
	return "observed: " + strings.Join(mode.Reasons, "; ")
}

func kagentMap(obj map[string]any, path ...string) map[string]any {
	current := obj
	for _, key := range path {
		next, _ := current[key].(map[string]any)
		if next == nil {
			return nil
		}
		current = next
	}
	return current
}

func kagentString(obj map[string]any, path ...string) string {
	parent := kagentMap(obj, path[:len(path)-1]...)
	value, _ := parent[path[len(path)-1]].(string)
	return strings.TrimSpace(value)
}

func kagentInt(obj map[string]any, path ...string) int64 {
	parent := kagentMap(obj, path[:len(path)-1]...)
	value, _ := parent[path[len(path)-1]].(float64)
	return int64(value)
}

func kagentBool(obj map[string]any, path ...string) bool {
	parent := kagentMap(obj, path[:len(path)-1]...)
	value, _ := parent[path[len(path)-1]].(bool)
	return value
}

// readKagentRevisionEvidence loads --revision-egress and --accepted-egress.
func readKagentRevisionEvidence(path, sourcesPath string, accepted []string) (kagentRevisionEvidence, error) {
	evidence := kagentRevisionEvidence{Revisions: map[string][]string{}, Sources: map[string]kagentRevisionSource{}}
	for _, origin := range accepted {
		normalized, ok := relayOrigin(origin, "")
		if !ok {
			return evidence, fmt.Errorf("--accepted-egress %q must be an http(s) origin with no path", origin)
		}
		evidence.Accepted = append(evidence.Accepted, normalized)
	}
	if path != "" {
		data, err := readFileOrStdin(path)
		if err != nil {
			return evidence, fmt.Errorf("reading --revision-egress: %w", err)
		}
		if err := json.Unmarshal(data, &evidence.Revisions); err != nil {
			return evidence, fmt.Errorf("--revision-egress must be a JSON object of revision to destinations: %w", err)
		}
	}
	if sourcesPath != "" {
		data, err := readFileOrStdin(sourcesPath)
		if err != nil {
			return evidence, fmt.Errorf("reading --revision-sources: %w", err)
		}
		if err := json.Unmarshal(data, &evidence.Sources); err != nil {
			return evidence, fmt.Errorf("--revision-sources must be a JSON object of revision to source snapshot: %w", err)
		}
	}
	return evidence, nil
}
