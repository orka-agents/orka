package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	toolAliasAnnotation               = "orka.ai/tool-alias"
	toolCacheIdenticalCallsAnnotation = "orka.ai/cache-identical-calls"
)

var toolAliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func toolAlias(tool *corev1alpha1.Tool) string {
	if tool == nil || tool.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(tool.Annotations[toolAliasAnnotation])
}

func registerToolAliases(customTools map[string]*corev1alpha1.Tool, loaded []*corev1alpha1.Tool) {
	for _, tool := range loaded {
		alias := toolAlias(tool)
		if alias == "" || alias == tool.Name {
			continue
		}
		if !toolAliasPattern.MatchString(alias) {
			fmt.Printf("Warning: tool %q has invalid alias %q; using its resource name\n", tool.Name, alias)
			continue
		}
		if reservedToolAlias(alias) {
			fmt.Printf("Warning: tool %q alias %q conflicts with a built-in tool; using its resource name\n", tool.Name, alias)
			continue
		}
		if existing, found := customTools[alias]; found && existing != tool {
			fmt.Printf(
				"Warning: tool %q alias %q conflicts with tool %q; using its resource name\n",
				tool.Name, alias, existing.Name,
			)
			continue
		}
		customTools[alias] = tool
	}
}

func advertisedCustomToolName(tool *corev1alpha1.Tool, customTools map[string]*corev1alpha1.Tool) string {
	alias := toolAlias(tool)
	if alias != "" && customTools[alias] == tool {
		return alias
	}
	return tool.Name
}

func cacheIdenticalToolCalls(tool *corev1alpha1.Tool, finalization bool) bool {
	if tool == nil || finalization || tool.Annotations == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(tool.Annotations[toolCacheIdenticalCallsAnnotation]), "true")
}

func resolvedCustomToolName(name string, customTools map[string]*corev1alpha1.Tool) string {
	if tool := customTools[name]; tool != nil {
		return tool.Name
	}
	return name
}

func reservedToolAlias(alias string) bool {
	return slices.Contains(tools.KnownBuiltInToolNames(), alias)
}
