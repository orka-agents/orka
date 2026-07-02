/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// helpers

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

func makeProvider(name, ns string, ptype corev1alpha1.ProviderType, secretName, defaultModel string) *corev1alpha1.Provider { //nolint:unparam
	return &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1alpha1.ProviderSpec{
			Type:         ptype,
			DefaultModel: defaultModel,
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: secretName},
		},
	}
}

func makeSecret(name, ns, key, value string) *corev1.Secret { //nolint:unparam
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func makeAgent(name, ns string, providerRef *corev1alpha1.ProviderReference, model *corev1alpha1.ModelConfig) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: providerRef,
			Model:       model,
		},
	}
}

// Tests

func TestProviderResolver_LookupProvider(t *testing.T) {
	provider := makeProvider("my-provider", "default", corev1alpha1.ProviderTypeOpenAI, "my-secret", "gpt-4")
	k := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(provider).Build()
	r := NewProviderResolver(k, DefaultChatConfig())

	t.Run("found", func(t *testing.T) {
		p, err := r.LookupProvider(context.Background(), "my-provider", "default")
		require.NoError(t, err)
		assert.Equal(t, "my-provider", p.Name)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := r.LookupProvider(context.Background(), "nonexistent", "default")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestProviderResolver_ResolveAPIKey(t *testing.T) {
	provider := makeProvider("p", "default", corev1alpha1.ProviderTypeOpenAI, "my-secret", "")

	t.Run("default key name", func(t *testing.T) {
		secret := makeSecret("my-secret", "default", "api-key", "sk-test-123")
		k := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(provider, secret).Build()
		r := NewProviderResolver(k, DefaultChatConfig())

		key, err := r.ResolveAPIKey(context.Background(), provider)
		require.NoError(t, err)
		assert.Equal(t, "sk-test-123", key)
	})

	t.Run("custom key name", func(t *testing.T) {
		p := &corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default"},
			Spec: corev1alpha1.ProviderSpec{
				Type:      corev1alpha1.ProviderTypeOpenAI,
				SecretRef: corev1alpha1.ProviderSecretRef{Name: "custom-secret", Key: "token"},
			},
		}
		secret := makeSecret("custom-secret", "default", "token", "sk-custom")
		k := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(p, secret).Build()
		r := NewProviderResolver(k, DefaultChatConfig())

		key, err := r.ResolveAPIKey(context.Background(), p)
		require.NoError(t, err)
		assert.Equal(t, "sk-custom", key)
	})

	t.Run("secret not found", func(t *testing.T) {
		k := fake.NewClientBuilder().WithScheme(newScheme()).Build()
		r := NewProviderResolver(k, DefaultChatConfig())

		_, err := r.ResolveAPIKey(context.Background(), provider)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get provider secret")
	})

	t.Run("secret missing key", func(t *testing.T) {
		secret := makeSecret("my-secret", "default", "wrong-key", "value")
		k := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(provider, secret).Build()
		r := NewProviderResolver(k, DefaultChatConfig())

		_, err := r.ResolveAPIKey(context.Background(), provider)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `has no key "api-key"`)
	})
}

func TestProviderResolver_Resolve(t *testing.T) {
	const (
		ns                 = "default"
		openaiProviderName = "openai"
	)

	// Shared objects
	openaiProvider := makeProvider(openaiProviderName, ns, corev1alpha1.ProviderTypeOpenAI, "openai-secret", "gpt-4")
	openaiSecret := makeSecret("openai-secret", ns, "api-key", "sk-openai")
	anthropicProvider := makeProvider("anthropic", ns, corev1alpha1.ProviderTypeAnthropic, "anthropic-secret", "claude-sonnet-4-20250514")
	anthropicSecret := makeSecret("anthropic-secret", ns, "api-key", "sk-anthropic")
	defaultProvider := makeProvider("default", ns, corev1alpha1.ProviderTypeOpenAI, "default-secret", "gpt-3.5-turbo")
	defaultSecret := makeSecret("default-secret", ns, "api-key", "sk-default")

	tests := []struct {
		name      string
		objects   []runtime.Object
		config    ChatConfig
		opts      ResolveOpts
		wantModel string
		wantErr   string
		wantPType string // provider Name()
	}{
		{
			name:    "explicit provider name",
			objects: []runtime.Object{openaiProvider, openaiSecret},
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				ProviderName: openaiProviderName,
				Model:        "gpt-4o",
				Namespace:    ns,
			},
			wantModel: "gpt-4o",
			wantPType: openaiProviderName,
		},
		{
			name:    "explicit provider uses default model from CRD",
			objects: []runtime.Object{openaiProvider, openaiSecret},
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				ProviderName: openaiProviderName,
				Namespace:    ns,
			},
			wantModel: "gpt-4",
			wantPType: openaiProviderName,
		},
		{
			name:    "model str provider/model format",
			objects: []runtime.Object{anthropicProvider, anthropicSecret},
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				ModelStr:  "anthropic/claude-sonnet-4-20250514",
				Namespace: ns,
			},
			wantModel: "claude-sonnet-4-20250514",
			wantPType: "anthropic",
		},
		{
			name:    "model str plain model uses config default provider",
			objects: []runtime.Object{openaiProvider, openaiSecret},
			config: func() ChatConfig {
				c := DefaultChatConfig()
				c.Provider = openaiProviderName
				return c
			}(),
			opts: ResolveOpts{
				ModelStr:  "gpt-4o",
				Namespace: ns,
			},
			wantModel: "gpt-4o",
			wantPType: openaiProviderName,
		},
		{
			name:    "model str falls through to default provider CRD",
			objects: []runtime.Object{defaultProvider, defaultSecret},
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				ModelStr:  "some-model",
				Namespace: ns,
			},
			wantModel: "some-model",
			wantPType: openaiProviderName,
		},
		{
			name: "agent ref with provider and model",
			objects: []runtime.Object{
				openaiProvider, openaiSecret,
				makeAgent("my-agent", ns,
					&corev1alpha1.ProviderReference{Name: openaiProviderName},
					&corev1alpha1.ModelConfig{Name: "gpt-4o"},
				),
			},
			config: DefaultChatConfig(),
			opts: ResolveOpts{
				AgentRef:  "my-agent",
				Namespace: ns,
			},
			wantModel: "gpt-4o",
			wantPType: openaiProviderName,
		},
		{
			name: "agent ref without provider falls to config provider",
			objects: []runtime.Object{
				openaiProvider, openaiSecret,
				makeAgent("agent-no-prov", ns, nil, &corev1alpha1.ModelConfig{Name: "gpt-4o"}),
			},
			config: func() ChatConfig {
				c := DefaultChatConfig()
				c.Provider = openaiProviderName
				return c
			}(),
			opts: ResolveOpts{
				AgentRef:  "agent-no-prov",
				Namespace: ns,
			},
			wantModel: "gpt-4o",
			wantPType: openaiProviderName,
		},
		{
			name:    "agent not found",
			objects: []runtime.Object{},
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				AgentRef:  "nonexistent",
				Namespace: ns,
			},
			wantErr: "agent \"nonexistent\" not found",
		},
		{
			name:    "provider not found (explicit)",
			objects: []runtime.Object{},
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				ProviderName: "missing",
				Namespace:    ns,
			},
			wantErr: "not found",
		},
		{
			name:    "no provider configured and no default CRD",
			objects: []runtime.Object{},
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				Namespace: ns,
			},
			wantErr: "no provider",
		},
		{
			name:    "secret not found during resolve",
			objects: []runtime.Object{openaiProvider}, // no secret
			config:  DefaultChatConfig(),
			opts: ResolveOpts{
				ProviderName: openaiProviderName,
				Namespace:    ns,
			},
			wantErr: "failed to get provider secret",
		},
		{
			name: "azure provider includes API version",
			objects: func() []runtime.Object {
				p := &corev1alpha1.Provider{
					ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: ns},
					Spec: corev1alpha1.ProviderSpec{
						Type:      corev1alpha1.ProviderTypeAzureOpenAI,
						BaseURL:   "https://my.openai.azure.com",
						SecretRef: corev1alpha1.ProviderSecretRef{Name: "azure-secret"},
						Azure: &corev1alpha1.AzureConfig{
							DeploymentName: "gpt4-deploy",
							APIVersion:     "2024-02-15-preview",
						},
					},
				}
				s := makeSecret("azure-secret", ns, "api-key", "sk-azure")
				return []runtime.Object{p, s}
			}(),
			config: DefaultChatConfig(),
			opts: ResolveOpts{
				ProviderName: "azure",
				Model:        "gpt-4",
				Namespace:    ns,
			},
			wantModel: "gpt-4",
			wantPType: openaiProviderName, // azure-openai uses the openai provider internally
		},
		{
			name: "require model with empty model from provider CRD",
			objects: func() []runtime.Object {
				p := makeProvider("default", ns, corev1alpha1.ProviderTypeOpenAI, "empty-model-secret", "fallback-model")
				s := makeSecret("empty-model-secret", ns, "api-key", "sk-x")
				return []runtime.Object{p, s}
			}(),
			config: func() ChatConfig {
				c := DefaultChatConfig()
				c.Model = ""
				return c
			}(),
			opts: ResolveOpts{
				Namespace:    ns,
				RequireModel: true,
			},
			wantModel: "fallback-model",
			wantPType: openaiProviderName,
		},
		{
			name: "require model with no model anywhere errors",
			objects: func() []runtime.Object {
				p := makeProvider("default", ns, corev1alpha1.ProviderTypeOpenAI, "no-model-secret", "")
				s := makeSecret("no-model-secret", ns, "api-key", "sk-x")
				return []runtime.Object{p, s}
			}(),
			config: func() ChatConfig {
				c := DefaultChatConfig()
				c.Model = ""
				return c
			}(),
			opts: ResolveOpts{
				Namespace:    ns,
				RequireModel: true,
			},
			wantErr: "no model specified",
		},
		{
			name:    "fallback chain: explicit -> config.Provider -> default",
			objects: []runtime.Object{defaultProvider, defaultSecret},
			config: func() ChatConfig {
				c := DefaultChatConfig()
				// config.Provider points to nonexistent, should fall to "default"
				c.Provider = "nonexistent-config-prov"
				return c
			}(),
			opts: ResolveOpts{
				ModelStr:  "also-nonexistent/some-model",
				Namespace: ns,
			},
			wantModel: "some-model",
			wantPType: openaiProviderName,
		},
		{
			name: "opts.Model overrides agent model",
			objects: []runtime.Object{
				openaiProvider, openaiSecret,
				makeAgent("override-agent", ns,
					&corev1alpha1.ProviderReference{Name: openaiProviderName},
					&corev1alpha1.ModelConfig{Name: "agent-model"},
				),
			},
			config: DefaultChatConfig(),
			opts: ResolveOpts{
				AgentRef:  "override-agent",
				Model:     "explicit-model",
				Namespace: ns,
			},
			wantModel: "explicit-model",
			wantPType: openaiProviderName,
		},
		{
			name: "config model used as last resort",
			objects: []runtime.Object{
				makeProvider(openaiProviderName, ns, corev1alpha1.ProviderTypeOpenAI, "openai-secret", ""),
				openaiSecret,
			},
			config: func() ChatConfig {
				c := DefaultChatConfig()
				c.Model = "config-fallback-model"
				return c
			}(),
			opts: ResolveOpts{
				ProviderName: openaiProviderName,
				Namespace:    ns,
			},
			wantModel: "config-fallback-model",
			wantPType: openaiProviderName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(tt.objects...).Build()
			resolver := NewProviderResolver(k, tt.config)

			provider, model, err := resolver.Resolve(context.Background(), tt.opts)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantModel, model)
			if tt.wantPType != "" {
				assert.Equal(t, tt.wantPType, provider.Name())
			}
		})
	}
}
