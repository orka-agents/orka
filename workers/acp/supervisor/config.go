package supervisor

import (
	"context"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspacedelta"
)

const OperationCapabilityHeader = "X-Orka-Operation-Capability"

const sessionIdentityExhaustionReserve = 1

type ProviderProfile struct {
	Kind                  string
	Model                 string
	Command               string
	Args                  []string
	Environment           map[string]string
	ProjectSession        func(harnessv2.CreateRuntimeSessionRequest, acp.SessionPaths, ProviderProxyBinding) (ProviderSessionProjection, error)
	EnvironmentForSession func(harnessv2.CreateRuntimeSessionRequest, acp.SessionPaths, ProviderProxyBinding) (map[string]string, error)
	AuthMethodID          string
	AdapterName           string
	AdapterDigest         string
	ValidateWorkspace     func(acp.SessionPaths) error
	PrepareSession        func(acp.SessionPaths) error
	BindProviderProxy     func(acp.SessionPaths, ProviderProxyBinding) error
}

type ProviderSessionProjection struct {
	Instructions          *acp.InstructionProjection
	AdditionalArgs        []string
	Environment           map[string]string
	NewSessionMeta        acp.Meta
	AgentDiagnosticFilter *AgentDiagnosticFilter
	RequiredModeID        string
}

type AgentDiagnosticFilter struct {
	Startup func(text string) (summary string, ok bool)
}

type WorkspaceMaterializer interface {
	Materialize(context.Context, harnessv2.CreateRuntimeSessionRequest, string) error
}

type WorkspaceMaterializerFunc func(context.Context, harnessv2.CreateRuntimeSessionRequest, string) error

func (f WorkspaceMaterializerFunc) Materialize(ctx context.Context, request harnessv2.CreateRuntimeSessionRequest, destination string) error {
	return f(ctx, request, destination)
}

type Config struct {
	Tracer        trace.Tracer
	GenAITracer   trace.Tracer
	Meter         metric.Meter
	ListenAddress string
	Fence         harnessv2.Fence
	Capabilities  harnessv2.CapabilitiesResponse
	Provider      ProviderProfile

	ControllerBearerToken string
	CapabilitySecret      []byte
	RequireCapabilities   bool

	SessionBaseDir              string
	DurableWorkspaceDir         string
	DurableWorkspaceKey         string
	UIDAllocator                *acp.UIDAllocator
	ProviderProxy               ProviderProxyConfig
	MCPBroker                   MCPBroker
	WorkspaceMaterializer       WorkspaceMaterializer
	ArtifactUploader            *RemoteArtifactUploader
	DeltaOptions                workspacedelta.Options
	E2EPromptWriteFaultRecorder E2EPromptWriteFaultRecorder

	InitializeTimeout time.Duration
	PermissionTimeout time.Duration
	CancelGrace       time.Duration

	E2EPromptWriteAmbiguityMarker string

	Toolboxes          []harnessv2.RuntimeToolbox
	ToolboxMountMethod string
	Skills             []harnessv2.RuntimeSkill
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return fmt.Errorf("listen address is required")
	}
	if err := c.Fence.Validate(false); err != nil {
		return fmt.Errorf("supervisor fence: %w", err)
	}
	if c.Fence.RuntimeSessionUID != "" || c.Fence.RuntimeSessionGeneration != 0 {
		return fmt.Errorf("supervisor base fence must not contain runtime-session identity")
	}
	if err := c.Capabilities.Validate(); err != nil {
		return fmt.Errorf("capabilities: %w", err)
	}
	if c.Capabilities.RuntimeProfileDigest != c.Fence.RuntimeProfileDigest {
		return fmt.Errorf("capabilities and fence runtime profile digests differ")
	}
	if c.Capabilities.ProfileDigestSchemaVersion != c.Fence.ProfileDigestSchemaVersion {
		return fmt.Errorf("capabilities and fence profile digest schema versions differ")
	}
	if err := c.validateProvider(); err != nil {
		return err
	}
	if c.Capabilities.SupportsFoundryRecovery && (c.Provider.Kind != providerKindFoundry || !c.RequireCapabilities) {
		return fmt.Errorf("foundry recovery requires a Foundry provider and operation capabilities")
	}
	if len(c.ControllerBearerToken) < 32 {
		return fmt.Errorf("controller bearer token must be at least 32 bytes")
	}
	if c.RequireCapabilities && len(c.CapabilitySecret) < harnessv2.MinCapabilitySecretBytes {
		return fmt.Errorf("operation capability secret must be at least %d bytes", harnessv2.MinCapabilitySecretBytes)
	}
	if err := c.validateWorkspaceDirectories(); err != nil {
		return err
	}
	if c.UIDAllocator == nil {
		return fmt.Errorf("UID allocator is required")
	}
	requiredIdentityCapacity := uint64(c.Capabilities.Limits.MaxResidentSessions) + sessionIdentityExhaustionReserve
	if uint64(c.UIDAllocator.Capacity()) < requiredIdentityCapacity {
		return fmt.Errorf("UID allocator capacity must provide at least %d resident identities plus the exhaustion reserve", c.Capabilities.Limits.MaxResidentSessions)
	}
	if c.WorkspaceMaterializer == nil {
		return fmt.Errorf("workspace materializer is required")
	}
	if _, _, err := c.ProviderProxy.normalized(); err != nil {
		return err
	}
	if c.MCPBroker == nil {
		return fmt.Errorf("controller MCP broker is required")
	}
	if c.InitializeTimeout < 0 || c.PermissionTimeout < 0 || c.CancelGrace < 0 {
		return fmt.Errorf("runtime timeouts must be non-negative")
	}
	if err := c.validatePromptWriteFault(); err != nil {
		return err
	}
	return nil
}

func (c Config) validateProvider() error {
	if strings.TrimSpace(c.Provider.Kind) == "" || strings.TrimSpace(c.Provider.Model) == "" {
		return fmt.Errorf("provider kind and model are required")
	}
	if len(c.Capabilities.Provider.ProviderKinds) != 1 || c.Capabilities.Provider.ProviderKinds[0] != c.Provider.Kind {
		return fmt.Errorf("provider capability kind does not match configured provider")
	}
	if c.Provider.Command == "" || !filepath.IsAbs(c.Provider.Command) {
		return fmt.Errorf("provider adapter command must be absolute")
	}
	if strings.TrimSpace(c.Provider.AdapterName) == "" {
		return fmt.Errorf("provider adapter name is required")
	}
	if got := c.Capabilities.AdapterDigests[c.Provider.AdapterName]; got == "" || got != c.Provider.AdapterDigest {
		return fmt.Errorf("provider adapter digest does not match advertised capability")
	}
	return nil
}

func (c Config) validateWorkspaceDirectories() error {
	if c.SessionBaseDir == "" || !filepath.IsAbs(c.SessionBaseDir) {
		return fmt.Errorf("session base directory must be absolute")
	}
	if c.DurableWorkspaceDir != "" && !filepath.IsAbs(c.DurableWorkspaceDir) {
		return fmt.Errorf("durable workspace directory must be absolute when set")
	}
	if c.DurableWorkspaceKey != "" {
		if c.DurableWorkspaceDir == "" || !acp.IsValidSessionPathComponent(c.DurableWorkspaceKey) {
			return fmt.Errorf("a stable durable workspace key requires a durable root and a safe directory component")
		}
		if c.Capabilities.Limits.MaxResidentSessions != 1 || c.Capabilities.Limits.MaxConcurrentPrompts != 1 {
			return fmt.Errorf("a stable durable workspace key requires a dedicated single-session pool")
		}
	}
	if c.DurableWorkspaceDir != "" {
		relative, err := filepath.Rel(filepath.Clean(c.DurableWorkspaceDir), filepath.Clean(c.SessionBaseDir))
		if err != nil {
			return fmt.Errorf("compare session and durable workspace directories: %w", err)
		}
		if relative == "." || (relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)) {
			return fmt.Errorf("session base directory must not equal or be beneath the durable workspace directory")
		}
	}
	return nil
}

func (c Config) validatePromptWriteFault() error {
	if marker := c.E2EPromptWriteAmbiguityMarker; marker != "" {
		if strings.TrimSpace(marker) != marker || len(marker) > 128 ||
			!strings.HasPrefix(marker, "ORKA_E2E_") || !strings.HasSuffix(marker, "_OK") {
			return fmt.Errorf("E2E prompt write ambiguity marker must be an ORKA_E2E_*_OK token")
		}
		for _, value := range marker {
			if (value < 'A' || value > 'Z') && (value < '0' || value > '9') && value != '_' {
				return fmt.Errorf("E2E prompt write ambiguity marker must contain only uppercase ASCII letters, digits, and underscores")
			}
		}
		if c.DurableWorkspaceDir == "" && c.E2EPromptWriteFaultRecorder == nil {
			return fmt.Errorf("E2E prompt write fault recorder is required without a durable workspace")
		}
	}
	return nil
}

func (c Config) bearerMatches(value string) bool {
	value = strings.TrimSpace(value)
	token, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || token == "" || strings.TrimSpace(token) != token || len(token) != len(c.ControllerBearerToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(c.ControllerBearerToken)) == 1
}

func EmptyWorkspaceMaterializer() WorkspaceMaterializer {
	return WorkspaceMaterializerFunc(func(_ context.Context, request harnessv2.CreateRuntimeSessionRequest, destination string) error {
		if request.Workspace.Baseline.Artifact != nil {
			return fmt.Errorf("workspace artifact materialization is not configured")
		}
		if err := os.MkdirAll(destination, 0o700); err != nil {
			return err
		}
		return nil
	})
}
