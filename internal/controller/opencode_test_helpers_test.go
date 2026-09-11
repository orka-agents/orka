/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"

const (
	testOpenCodeContextWindow int32 = 32768
	testOpenCodeMaxTokens     int32 = 4096
)

func testOpenCodeModelConfig() *corev1alpha1.ModelConfig {
	contextWindow := testOpenCodeContextWindow
	maxTokens := testOpenCodeMaxTokens
	return &corev1alpha1.ModelConfig{
		Name:          "openai/gpt-test",
		ContextWindow: &contextWindow,
		MaxTokens:     &maxTokens,
	}
}
