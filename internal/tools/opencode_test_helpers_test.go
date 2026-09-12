/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"

func testOpenCodeModelConfig(name string) *corev1alpha1.ModelConfig {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	return &corev1alpha1.ModelConfig{Name: name, ContextWindow: &contextWindow, MaxTokens: &maxTokens}
}
