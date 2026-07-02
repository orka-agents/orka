package llm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/orka-agents/orka/internal/tracing/genai"
)

// TelemetryIdentity is implemented by providers that can expose their concrete
// telemetry identity without overloading the user-facing Provider.Name().
type TelemetryIdentity interface {
	TelemetryProviderName() string
}

// ProviderTelemetryName returns the OpenTelemetry GenAI provider name for p.
func ProviderTelemetryName(p Provider) string {
	if p == nil {
		return ""
	}
	if identity, ok := p.(TelemetryIdentity); ok {
		if name := genai.NormalizeProviderName(identity.TelemetryProviderName()); name != "" {
			return name
		}
	}
	return genai.NormalizeProviderName(p.Name())
}

func responseProviderName(resp *CompletionResponse, fallback Provider) string {
	if resp != nil && strings.TrimSpace(resp.Provider) != "" {
		return genai.NormalizeProviderName(resp.Provider)
	}
	return ProviderTelemetryName(fallback)
}

func errorType(err error) string {
	if err == nil {
		return ""
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe.StatusCode > 0 {
		return fmt.Sprintf("%d", pe.StatusCode)
	}
	return fmt.Sprintf("%T", err)
}
