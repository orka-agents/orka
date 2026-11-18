/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestParseModelSettings(t *testing.T) {
	tests := []struct {
		name            string
		temperature     string
		maxTokens       string
		wantTemperature float64
		wantSet         bool
		wantMaxTokens   int
	}{
		{name: "unset", wantMaxTokens: 4096},
		{name: "explicit zero", temperature: "0", maxTokens: "256", wantSet: true, wantMaxTokens: 256},
		{name: "upper temperature bound", temperature: "2", wantTemperature: 2, wantSet: true, wantMaxTokens: 4096},
		{name: "fractional temperature", temperature: "0.75", wantTemperature: 0.75, wantSet: true, wantMaxTokens: 4096},
		{name: "large output cap", maxTokens: "8192", wantMaxTokens: 8192},
		{name: "smallest output cap", maxTokens: "1", wantMaxTokens: 1},
		{name: "zero output cap", maxTokens: "0", wantMaxTokens: 4096},
		{name: "negative output cap", maxTokens: "-256", wantMaxTokens: 4096},
		{name: "maximum int32 output cap", maxTokens: "2147483647", wantMaxTokens: 2147483647},
		{name: "minimum int32 output cap", maxTokens: "-2147483648", wantMaxTokens: 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings, err := parseModelSettings(workerenv.AIWorkerEnv{
				Temperature: tt.temperature,
				MaxTokens:   tt.maxTokens,
			})
			if err != nil {
				t.Fatalf("parseModelSettings() error = %v", err)
			}
			if settings.temperature != tt.wantTemperature ||
				settings.temperatureSet != tt.wantSet || settings.maxTokens != tt.wantMaxTokens {
				t.Fatalf("settings = %+v, want temperature %v (set %t), max tokens %d",
					settings, tt.wantTemperature, tt.wantSet, tt.wantMaxTokens)
			}
		})
	}
}

func TestParseModelSettingsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name        string
		temperature string
		maxTokens   string
		wantField   string
	}{
		{name: "negative temperature", temperature: "-0.1", wantField: workerenv.AITemperature},
		{name: "temperature above range", temperature: "2.01", wantField: workerenv.AITemperature},
		{name: "NaN temperature", temperature: "NaN", wantField: workerenv.AITemperature},
		{name: "infinite temperature", temperature: "Inf", wantField: workerenv.AITemperature},
		{name: "positive infinite temperature", temperature: "+Inf", wantField: workerenv.AITemperature},
		{name: "negative infinite temperature", temperature: "-Inf", wantField: workerenv.AITemperature},
		{name: "temperature overflow", temperature: "1e309", wantField: workerenv.AITemperature},
		{name: "malformed temperature", temperature: "invalid-temperature-value", wantField: workerenv.AITemperature},
		{name: "whitespace temperature", temperature: " ", wantField: workerenv.AITemperature},
		{name: "malformed output cap", maxTokens: "invalid-max-tokens-value", wantField: workerenv.AIMaxTokens},
		{name: "fractional output cap", maxTokens: "256.5", wantField: workerenv.AIMaxTokens},
		{name: "output cap overflow", maxTokens: "2147483648", wantField: workerenv.AIMaxTokens},
		{name: "output cap underflow", maxTokens: "-2147483649", wantField: workerenv.AIMaxTokens},
		{name: "int64 output cap overflow", maxTokens: "9223372036854775808", wantField: workerenv.AIMaxTokens},
		{name: "whitespace output cap", maxTokens: " ", wantField: workerenv.AIMaxTokens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseModelSettings(workerenv.AIWorkerEnv{
				Temperature: tt.temperature,
				MaxTokens:   tt.maxTokens,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantField) {
				t.Fatalf("parseModelSettings() error = %v, want validation failure naming %s", err, tt.wantField)
			}
		})
	}
}
