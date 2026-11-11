// Package executor runs trusted preparation and protected verification through
// controller-owned Orka Tasks and Kubernetes Jobs, never controller subprocesses.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Config struct {
	Enabled              bool
	Namespace            string
	GeneralWorkerImage   string
	GeneralWorkerImageID string
	HelperImage          string
	HelperImageID        string
	ToolImage            string
	ToolImageID          string
	Platform             string
	Profile              string
	NodeSelectorKey      string
	NodeSelectorValue    string
	LocalServices        bool
	CanaryHost           string
	CanaryPort           int
	ReconcileInterval    time.Duration
	RunTimeout           time.Duration
	CleanupTimeout       time.Duration
}

type Backend struct {
	client     client.Client
	reader     client.Reader
	kubeClient kubernetes.Interface
	scheme     *runtime.Scheme
	config     Config
	validation *pvkube.Service
	effectGate chan struct{}
	readReport func(context.Context, string, string) (pv.PreparationReport, error)
}

var _ contract.EvidenceScopedBackend = (*Backend)(nil)

func New(c client.Client, reader client.Reader, kubeClient kubernetes.Interface, store pvkube.Store, scheme *runtime.Scheme, config Config) (*Backend, error) {
	if !config.Enabled {
		return &Backend{config: config}, nil
	}
	if config.ReconcileInterval == 0 {
		config.ReconcileInterval = time.Second
	}
	if config.RunTimeout == 0 {
		config.RunTimeout = 30 * time.Minute
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = 30 * time.Second
	}
	config.GeneralWorkerImageID = imageDigest(config.GeneralWorkerImageID)
	config.HelperImageID = imageDigest(config.HelperImageID)
	if c == nil || reader == nil || kubeClient == nil || store == nil || scheme == nil ||
		!validWorkerConfiguration(config) || !validRuntimeConfiguration(config) {
		return nil, contract.ErrInvalid
	}
	service, err := pvkube.New(c, reader, kubeClient, store, scheme, pvkube.Config{
		Enabled: true, Namespace: config.Namespace, HelperImage: config.HelperImage, HelperImageID: config.HelperImageID,
		ToolImage: config.ToolImage, ToolImageID: config.ToolImageID,
		Platform: config.Platform, Profile: config.Profile,
		NodeSelectorKey: config.NodeSelectorKey, NodeSelectorValue: config.NodeSelectorValue,
		LocalServices: config.LocalServices, CanaryHost: config.CanaryHost, CanaryPort: config.CanaryPort,
		ReconcileInterval: config.ReconcileInterval, RunTimeout: config.RunTimeout,
		PreparationConfiguration: configurationDigest(config),
	})
	if err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	backend := &Backend{client: c, reader: reader, kubeClient: kubeClient, scheme: scheme, config: config, validation: service, effectGate: make(chan struct{}, 1)}
	backend.readReport = backend.readPodReport
	service.SetJobRenderer(backend.BuildValidationJob)
	return backend, nil
}

func validWorkerConfiguration(config Config) bool {
	if len(validation.IsDNS1123Label(config.Namespace)) != 0 ||
		!pinnedImage(config.GeneralWorkerImage) || !pinnedImage(config.HelperImage) || !pinnedImage(config.ToolImage) ||
		imageDigest(config.GeneralWorkerImageID) == "" || imageDigest(config.HelperImageID) == "" ||
		imageDigest(config.ToolImageID) == "" ||
		(config.Platform != "linux/amd64" && config.Platform != "linux/arm64") ||
		config.Profile != pv.LocalServices || !config.LocalServices ||
		config.NodeSelectorKey == "" || len(validation.IsQualifiedName(config.NodeSelectorKey)) != 0 ||
		config.NodeSelectorValue == "" || len(validation.IsValidLabelValue(config.NodeSelectorValue)) != 0 ||
		strings.HasPrefix(config.NodeSelectorKey, "kubernetes.io/") {
		return false
	}
	return true
}

func validRuntimeConfiguration(config Config) bool {
	canary, err := netip.ParseAddr(config.CanaryHost)
	if err != nil || canary.IsLoopback() || !canary.IsGlobalUnicast() || canary.Zone() != "" ||
		config.CanaryPort < 1 || config.CanaryPort > 65535 ||
		config.ReconcileInterval < time.Millisecond || config.ReconcileInterval > 30*time.Second ||
		config.RunTimeout < time.Second || config.RunTimeout > 2*time.Hour ||
		config.CleanupTimeout < time.Millisecond || config.CleanupTimeout > 2*time.Minute {
		return false
	}
	return true
}

func (b *Backend) ValidationService() *pvkube.Service { return b.validation }

func (b *Backend) EvidenceScope() contract.EvidenceScope {
	if b == nil || !b.config.Enabled || b.config.Profile != pv.LocalServices || !b.config.LocalServices {
		return ""
	}
	return contract.SourceHarness
}

// ConfigurationDigest is frozen in coordinator state. Normalized configuration
// and policy versions define the compatibility boundary for resumed authority.
func (b *Backend) ConfigurationDigest() string {
	if b == nil || !b.config.Enabled {
		return ""
	}
	return configurationDigest(b.config)
}

func configurationDigest(config Config) string {
	content, _ := json.Marshal(struct {
		PreparationPolicy string
		WorkerPolicy      string
		Config            Config
	}{pv.PreparationPolicy, pv.KubernetesPolicyVersion, config})
	return pv.Digest(content)
}

func (b *Backend) lockEffects(ctx context.Context) error {
	select {
	case b.effectGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return safeError(contract.ErrUnavailable, ctx.Err())
	}
}

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func imageDigest(image string) string {
	_, digest, found := strings.Cut(image, "@")
	if !found {
		digest = strings.TrimPrefix(image, "containerd://")
	}
	if !digestPattern.MatchString(digest) {
		return ""
	}
	return digest
}

func pinnedImage(image string) bool {
	name, _, found := strings.Cut(image, "@")
	return found && name != "" && utf8.ValidString(image) && imageDigest(image) != "" && !strings.ContainsAny(image, "\r\n\t\x00 ")
}

type sanitizedError struct {
	kind  error
	cause error
}

type noEffectsError struct {
	cause error
}

func (e noEffectsError) Error() string   { return e.cause.Error() }
func (e noEffectsError) Unwrap() error   { return e.cause }
func (e noEffectsError) NoEffects() bool { return true }

func (e sanitizedError) Error() string   { return e.kind.Error() }
func (e sanitizedError) Unwrap() []error { return []error{e.kind, e.cause} }

func (e sanitizedError) Retryable() bool {
	return e.kind == contract.ErrUnavailable && pvkube.IsTransientError(e.cause)
}

func safeError(kind, cause error) error {
	if cause == nil || errors.Is(cause, kind) {
		return kind
	}
	return sanitizedError{kind: kind, cause: cause}
}
