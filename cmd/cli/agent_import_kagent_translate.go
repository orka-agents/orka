/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// kagentTranslationTable documents how translate mode maps a kagent v1 or v0
// Agent onto a native Orka Agent. Every other field is refused rather than
// dropped.
const kagentTranslationTable = `  kagent v1 (api.kagent.dev/v1alpha3)     Orka Agent
  template.inline.systemPrompt            spec.systemPrompt.inline
  template.inline.systemPromptFrom        spec.systemPrompt.configMapRef
  template.inline.description             not carried; Orka Agents have no description

  kagent v0 (kagent.dev/v1alpha2)         Orka Agent
  declarative.systemMessage               spec.systemPrompt.inline
  declarative.systemMessageFrom
    (type ConfigMap)                      spec.systemPrompt.configMapRef
  description, iconUrl, documentationUrl,
    version, provider                     not carried; Orka Agents have no agent card
  declarative.stream, declarative.runtime,
    deployment placement settings         not carried; kagent runtime details

  Both versions                           Orka Agent
  ModelConfig provider OpenAI/Anthropic   spec.model.provider openai/anthropic
  ModelConfig model                       spec.model.name
  ModelConfig openAI|anthropic
    .temperature / .maxTokens             spec.model.temperature / .maxTokens
  ModelConfig with a custom baseUrl       spec.providerRef (--provider-ref)
  MCP tool <server>/<tool>                spec.tools (--tool <server>/<tool>=<Orka Tool>)
  ModelConfig apiKeySecret                not carried; Orka uses its own provider credentials
  ModelConfig stream, openAI.apiFormat    not carried; wire details Orka's provider decides`

// Field and kind names translation reads and writes.
const (
	kagentFieldSpec         = "spec"
	kagentFieldMetadata     = "metadata"
	kagentFieldDescription  = "description"
	kagentKindAgent         = "Agent"
	kagentProviderAnthropic = "anthropic"
	kagentProviderOpenAI    = "openai"
	// kagentModelProviderOpenAI is kagent's name for OpenAI, the provider of a
	// ModelConfig that names none.
	kagentModelProviderOpenAI = "OpenAI"
)

// kagentTranslateOptions map what translation cannot infer.
type kagentTranslateOptions struct {
	// Tools maps "<RemoteMCPServer>/<tool>" to an existing Orka Tool name.
	Tools map[string]string
	// ProviderRef names an Orka Provider for models with a custom endpoint.
	ProviderRef string
}

func parseKagentToolMappings(values []string) (map[string]string, error) {
	mappings := map[string]string{}
	for _, value := range values {
		source, target, ok := strings.Cut(value, "=")
		server, tool, hasTool := strings.Cut(source, "/")
		if !ok || !hasTool || strings.TrimSpace(server) == "" || strings.TrimSpace(tool) == "" || strings.TrimSpace(target) == "" {
			return nil, fmt.Errorf("--tool %q must look like <RemoteMCPServer>/<tool>=<Orka Tool>", value)
		}
		mappings[strings.TrimSpace(source)] = strings.TrimSpace(target)
	}
	return mappings, nil
}

// translateKagentAgent returns the native Orka Agent spec for a kagent Agent,
// or every reason it cannot be translated.
func translateKagentAgent(agent map[string]any, related kagentObjects, options kagentTranslateOptions) (map[string]any, []string) {
	namespace := kagentString(agent, kagentFieldMetadata, cliNamespaceQuery)
	var problems []string
	refuse := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	// A kagent v1 Agent names only its harness and template; any other field
	// would be dropped, so it is refused.
	for field := range kagentMap(agent, kagentFieldSpec) {
		if field != "harnessRef" && field != "template" {
			refuse("spec.%s has no Orka equivalent", field)
		}
	}

	harnessName := kagentString(agent, kagentFieldSpec, "harnessRef", cliNameKey)
	if harness := related.lookup("Harness", namespace, harnessName); harness == nil {
		refuse("harness %q is not in the input", harnessName)
	} else if kagentMap(harness, kagentFieldSpec, "kagent") == nil {
		refuse("harness %q is not the ADK (kagent) harness", harnessName)
	} else {
		// A native Orka Agent runs Orka's own loop, so any harness setting
		// that changes the agent's behavior cannot carry over. Substrate
		// placement is infrastructure and does not.
		for field := range kagentMap(harness, kagentFieldSpec) {
			if field != "kagent" && field != "substrate" {
				refuse("harness %q sets %s, which Orka Agents do not have", harnessName, field)
			}
		}
		for field := range kagentMap(harness, kagentFieldSpec, "kagent") {
			refuse("harness %q uses %s, which Orka Agents do not have", harnessName, field)
		}
	}

	template := kagentMap(agent, kagentFieldSpec, "template", "inline")
	if template == nil {
		ref := kagentString(agent, kagentFieldSpec, "template", "ref", cliNameKey)
		if found := related.lookup("AgentTemplate", namespace, ref); found != nil {
			template = kagentMap(found, kagentFieldSpec)
		} else {
			refuse("agent template %q is not in the input", ref)
			return nil, problems
		}
	}
	translated := map[string]string{kagentFieldDescription: "", "systemPrompt": "", "systemPromptFrom": "", "modelConfig": "", kagentFieldTools: ""}
	for field := range template {
		if _, ok := translated[field]; !ok {
			refuse("template field %q has no Orka equivalent", field)
		}
	}
	spec := map[string]any{}
	switch {
	case kagentString(template, "systemPrompt") != "" && kagentMap(template, "systemPromptFrom") != nil:
		refuse("the template sets both systemPrompt and systemPromptFrom")
	case kagentString(template, "systemPrompt") != "":
		spec["systemPrompt"] = map[string]any{"inline": template["systemPrompt"]}
	case kagentMap(template, "systemPromptFrom") != nil:
		from := kagentMap(template, "systemPromptFrom")
		spec["systemPrompt"] = map[string]any{"configMapRef": map[string]any{
			cliNameKey: kagentString(from, cliNameKey), "key": kagentString(from, "key"),
		}}
	}

	modelName := kagentString(template, "modelConfig", cliNameKey)
	if model := related.lookup("ModelConfig", namespace, modelName); model == nil {
		refuse("model config %q is not in the input", modelName)
	} else {
		translatedModel, providerRef, modelProblems := translateKagentModel(model, options.ProviderRef)
		for _, problem := range modelProblems {
			refuse("model config %q %s", modelName, problem)
		}
		spec[kagentFieldModel] = translatedModel
		if providerRef != "" {
			spec["providerRef"] = map[string]any{cliNameKey: providerRef}
		}
	}

	tools, _ := template[kagentFieldTools].([]any)
	var refs []any
	for i, raw := range tools {
		binding, _ := raw.(map[string]any)
		server := kagentMap(binding, "mcp", "server")
		if server == nil {
			refuse("tool %d is not an MCP binding", i)
			continue
		}
		// Every binding field changes behavior; an approval requirement in
		// particular must never be dropped by a plain Orka Tool reference.
		for field := range kagentMap(binding, "mcp") {
			if !kagentMCPBindingField(field) {
				refuse("tool %d sets mcp.%s, which has no Orka equivalent; give the Orka Tool its own policy", i, field)
			}
		}
		serverName, _ := server[cliNameKey].(string)
		names, _ := kagentMap(binding, "mcp")[kagentFieldTools].([]any)
		if len(names) == 0 {
			refuse("tool server %q binds every tool; list them so each can be mapped", serverName)
		}
		for _, rawName := range names {
			name, _ := rawName.(string)
			target, ok := options.Tools[serverName+"/"+name]
			if !ok {
				refuse("tool %s/%s has no Orka Tool; pass --tool %s/%s=<Orka Tool>", serverName, name, serverName, name)
				continue
			}
			refs = append(refs, map[string]any{cliNameKey: target})
		}
	}
	if len(refs) > 0 {
		spec["tools"] = refs
	}
	sort.Strings(problems)
	return spec, problems
}

// translateKagentModel maps a kagent ModelConfig onto spec.model. kagent's
// credential is never carried over: an Orka Agent uses Orka's provider
// credentials, or the Provider named by --provider-ref.
func translateKagentModel(model map[string]any, providerRef string) (map[string]any, string, []string) {
	var problems []string
	spec := kagentMap(model, kagentFieldSpec)
	kagentProvider := kagentModelProvider(model)
	provider := map[string]string{kagentModelProviderOpenAI: kagentProviderOpenAI, "Anthropic": kagentProviderAnthropic}[kagentProvider]
	settingsField := map[string]string{kagentProviderOpenAI: "openAI", kagentProviderAnthropic: "anthropic"}[provider]
	if provider == "" {
		problems = append(problems, fmt.Sprintf("uses provider %q; only OpenAI and Anthropic translate", kagentProvider))
	}
	for field := range spec {
		switch field {
		case "provider", kagentFieldModel, "apiKeySecret", "apiKeySecretKey", kagentFieldStream, settingsField:
		default:
			problems = append(problems, fmt.Sprintf("sets %s, which has no Orka equivalent", field))
		}
	}
	out := map[string]any{"name": kagentString(model, kagentFieldSpec, kagentFieldModel)}
	if provider != "" {
		out["provider"] = provider
	}
	ref := ""
	settings := kagentMap(spec, settingsField)
	for field := range settings {
		switch field {
		case "temperature":
			value, err := strconv.ParseFloat(kagentString(settings, "temperature"), 64)
			if err != nil || value < 0 || value > 2 {
				problems = append(problems, "has a temperature Orka cannot accept")
			} else {
				out["temperature"] = value
			}
		case "maxTokens":
			if tokens, ok := settings["maxTokens"].(float64); ok && tokens > 0 && tokens <= 1<<31-1 {
				out["maxTokens"] = int32(tokens)
			} else {
				problems = append(problems, "has an invalid maxTokens")
			}
		case "apiFormat":
			// kagent defaults the OpenAI wire shape; Orka's provider owns its
			// own, and the shape does not change the agent's behavior.
		case "baseUrl":
			// An empty baseUrl is kagent's default endpoint.
			if kagentString(settings, "baseUrl") == "" {
				break
			}
			if providerRef == "" {
				problems = append(problems, "uses a custom endpoint; map it to an Orka Provider with --provider-ref")
			}
			ref = providerRef
		default:
			problems = append(problems, fmt.Sprintf("sets %s.%s, which has no Orka equivalent", settingsField, field))
		}
	}
	return out, ref, problems
}

// translatedKagentAgent builds the native Orka Agent manifest.
func translatedKagentAgent(apiVersion, source, name string, spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": corev1alpha1.GroupVersion.String(),
		"kind":       kagentKindAgent,
		kagentFieldMetadata: map[string]any{
			cliNameKey:    name,
			"annotations": map[string]any{kagentSourceAnnotation: apiVersion + " " + source},
		},
		kagentFieldSpec: spec,
	}
}
