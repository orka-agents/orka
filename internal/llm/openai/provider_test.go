/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openai

import (
	"testing"

	"github.com/sozercan/mercan/internal/llm"
)

func TestNewProvider(t *testing.T) {
	tests := []struct {
		name    string
		config  llm.ProviderConfig
		wantErr bool
	}{
		{
			name: "with API key",
			config: llm.ProviderConfig{
				APIKey: "test-api-key",
			},
			wantErr: false,
		},
		{
			name: "without API key",
			config: llm.ProviderConfig{
				APIKey: "",
			},
			wantErr: true,
		},
		{
			name: "with base URL",
			config: llm.ProviderConfig{
				APIKey:  "test-api-key",
				BaseURL: "https://custom.api.com",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := NewProvider(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewProvider() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && provider == nil {
				t.Error("NewProvider() returned nil provider")
			}
		})
	}
}

func TestProvider_Name(t *testing.T) {
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if name := provider.Name(); name != "openai" {
		t.Errorf("Name() = %v, want openai", name)
	}
}

func TestNewProvider_APIKeyRequired(t *testing.T) {
	_, err := NewProvider(llm.ProviderConfig{})
	if err == nil {
		t.Error("NewProvider() expected error for missing API key")
	}
	if err != llm.ErrAPIKeyRequired {
		t.Errorf("NewProvider() error = %v, want ErrAPIKeyRequired", err)
	}
}

func TestProvider_Implements_Interface(t *testing.T) {
	// Verify that Provider implements llm.Provider at compile time
	var _ llm.Provider = (*Provider)(nil)
}

func TestProvider_ConfigStorage(t *testing.T) {
	config := llm.ProviderConfig{
		APIKey:     "test-key",
		BaseURL:    "https://api.example.com",
		MaxRetries: 3,
		Timeout:    30,
	}

	provider, err := NewProvider(config)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if provider.config.APIKey != config.APIKey {
		t.Errorf("config.APIKey = %v, want %v", provider.config.APIKey, config.APIKey)
	}
	if provider.config.BaseURL != config.BaseURL {
		t.Errorf("config.BaseURL = %v, want %v", provider.config.BaseURL, config.BaseURL)
	}
}

func TestProvider_ClientNotNil(t *testing.T) {
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if provider.client == nil {
		t.Error("client should not be nil")
	}
}
