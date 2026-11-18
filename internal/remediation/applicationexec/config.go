// Package applicationexec executes frozen application experiments on a dedicated
// target API. Orka Tasks and durable control-cluster receipts retain authority;
// no control-cluster credential or cross-cluster owner reference reaches a target.
package applicationexec

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const backendPolicy = "isolated-application-v1"

// Store is the existing verification record store. Target effect receipts use
// resource-version CAS on retained control-cluster ConfigMaps, like remediation
// coordinator state; neither a new database nor a new CRD is required.
type Store interface {
	InitializeKubernetesValidationStore(context.Context) error
	CreatePatchVerificationRun(context.Context, pv.Manifest, pv.Binding, map[string][]byte) error
	GetPatchVerificationRun(context.Context, string) (*pv.Record, error)
	RecordPatchVerificationEvidence(context.Context, pv.Binding, pv.ExecutionEvidence) error
	FinalizePatchVerificationRun(context.Context, pv.Binding) (*pv.Record, error)
	CancelPatchVerificationRun(context.Context, pv.Binding) (*pv.Record, error)
}

type Config struct {
	Enabled              bool
	Namespace            string
	WorkerImage          string
	WorkerImageID        string
	ToolchainImage       string
	ToolchainImageID     string
	GoVersion            string
	RuntimeImage         string
	RuntimeImageID       string
	RuntimeImages        map[string]string
	ResourcePolicy       *pv.ApplicationPolicy
	RuntimeClassName     string
	RuntimeQualification *pv.ApplicationRuntimeQualification
	SourceEndpoints      []SourceEndpoint
	Platform             string
	StorageClass         string
	NodeSelectorKey      string
	NodeSelectorValue    string
	TargetClusterUID     string
	ControlClusterUID    string
	TargetAPIServer      string
	TargetCAData         []byte
	CanaryHost           string
	CanaryPort           int
	ReconcileInterval    time.Duration
	RunTimeout           time.Duration
	CleanupTimeout       time.Duration
}

// SourceEndpoint is an operator-owned TLS pass-through endpoint, not a proxy
// configuration. The trusted preparer retains the public GitHub URL and SNI;
// only its DNS-to-IP routing and exact egress destination are replaced.
type SourceEndpoint struct {
	Host    string
	Address string
}

type Backend struct {
	control       client.Client
	controlReader client.Reader
	target        client.Client
	targetReader  client.Reader
	targetKube    kubernetes.Interface
	store         Store
	scheme        *runtime.Scheme
	config        Config
	effectGate    chan struct{}
	qualified     atomic.Bool
}

var _ contract.EvidenceScopedBackend = (*Backend)(nil)

// Readers must be uncached API readers. In particular, a stale informer cannot
// decide whether an exact target UID is absent or authorize a new effect.
func New(control client.Client, controlReader client.Reader, target client.Client, targetReader client.Reader,
	targetKube kubernetes.Interface, evidence Store, scheme *runtime.Scheme, config Config) (*Backend, error) {
	if !config.Enabled {
		return &Backend{config: config}, nil
	}
	config, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if control == nil || controlReader == nil || target == nil || targetReader == nil ||
		targetKube == nil || evidence == nil || scheme == nil || !validConfig(config) {
		return nil, contract.ErrInvalid
	}
	return &Backend{control: control, controlReader: controlReader, target: target, targetReader: targetReader,
		targetKube: targetKube, store: evidence, scheme: scheme, config: config, effectGate: make(chan struct{}, 1)}, nil
}

func normalizeConfig(config Config) (Config, error) {
	if config.ReconcileInterval == 0 {
		config.ReconcileInterval = time.Second
	}
	if config.RunTimeout == 0 {
		config.RunTimeout = 30 * time.Minute
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = 2 * time.Minute
	}
	config.WorkerImageID = imageDigest(config.WorkerImageID)
	config.ToolchainImageID = imageDigest(config.ToolchainImageID)
	if config.RuntimeImage == "" && config.RuntimeImageID == "" {
		config.RuntimeImage, config.RuntimeImageID = config.WorkerImage, config.WorkerImageID
	}
	config.RuntimeImageID = imageDigest(config.RuntimeImageID)
	config.TargetCAData = bytes.Clone(config.TargetCAData)
	if config.RuntimeQualification != nil {
		qualification := *config.RuntimeQualification
		if pv.ValidateApplicationRuntimeQualification(qualification) != nil ||
			qualification.TargetClusterUID != config.TargetClusterUID ||
			qualification.RuntimeClassName != config.RuntimeClassName {
			return Config{}, contract.ErrInvalid
		}
		config.RuntimeQualification = &qualification
	}
	config.SourceEndpoints = append([]SourceEndpoint(nil), config.SourceEndpoints...)
	if config.ResourcePolicy != nil {
		content, err := json.Marshal(config.ResourcePolicy)
		if err != nil {
			return Config{}, contract.ErrInvalid
		}
		var policy pv.ApplicationPolicy
		if json.Unmarshal(content, &policy) != nil {
			return Config{}, contract.ErrInvalid
		}
		for i := range policy.Fixtures {
			policy.Fixtures[i].ImageID = imageDigest(policy.Fixtures[i].ImageID)
		}
		if pv.ValidateApplicationPolicy(policy) != nil {
			return Config{}, contract.ErrInvalid
		}
		config.ResourcePolicy = &policy
	}
	config.RuntimeImages = maps.Clone(config.RuntimeImages)
	for image, id := range config.RuntimeImages {
		config.RuntimeImages[image] = imageDigest(id)
	}
	identities := make(map[string]string)
	images := [][2]string{{config.WorkerImage, config.WorkerImageID}, {config.ToolchainImage, config.ToolchainImageID}, {config.RuntimeImage, config.RuntimeImageID}}
	for image, id := range config.RuntimeImages {
		images = append(images, [2]string{image, id})
	}
	if config.ResourcePolicy != nil {
		for _, fixture := range config.ResourcePolicy.Fixtures {
			images = append(images, [2]string{fixture.Image, fixture.ImageID})
		}
	}
	for _, image := range images {
		if previous, found := identities[image[0]]; found && previous != image[1] {
			return Config{}, contract.ErrInvalid
		}
		identities[image[0]] = image[1]
	}
	return config, nil
}

func validConfig(c Config) bool {
	certificates := x509.NewCertPool()
	if len(c.TargetCAData) == 0 || len(c.TargetCAData) > 64<<10 || !certificates.AppendCertsFromPEM(c.TargetCAData) {
		return false
	}
	if !validConfigNames(c) || !validConfigImages(c) || !validConfigBounds(c) {
		return false
	}
	if _, _, err := apiAddress(c.TargetAPIServer); err != nil {
		return false
	}
	canary, err := netip.ParseAddr(c.CanaryHost)
	if err != nil || !canary.IsGlobalUnicast() || canary.IsLoopback() || canary.Zone() != "" ||
		c.CanaryPort < 1 || c.CanaryPort > 65535 {
		return false
	}
	return validSourceEndpoints(c.SourceEndpoints)
}

func validConfigImages(c Config) bool {
	if !pinnedImage(c.WorkerImage) || !pinnedImage(c.ToolchainImage) ||
		!digestPattern.MatchString(c.WorkerImageID) || !digestPattern.MatchString(c.ToolchainImageID) ||
		!pinnedImage(c.RuntimeImage) || !digestPattern.MatchString(c.RuntimeImageID) || !goVersionPattern.MatchString(c.GoVersion) ||
		(c.Platform != "linux/amd64" && c.Platform != "linux/arm64") || len(c.RuntimeImages) > 16 {
		return false
	}
	for image, id := range c.RuntimeImages {
		if !pinnedImage(image) || !digestPattern.MatchString(id) {
			return false
		}
	}
	return true
}

func validConfigNames(c Config) bool {
	if len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.StorageClass == "" || len(validation.IsDNS1123Subdomain(c.StorageClass)) != 0 ||
		(c.RuntimeClassName != "" && len(validation.IsDNS1123Subdomain(c.RuntimeClassName)) != 0) ||
		c.NodeSelectorKey == "" || len(validation.IsQualifiedName(c.NodeSelectorKey)) != 0 ||
		strings.HasPrefix(c.NodeSelectorKey, "kubernetes.io/") ||
		c.NodeSelectorValue == "" || len(validation.IsValidLabelValue(c.NodeSelectorValue)) != 0 {
		return false
	}
	return validUID(c.TargetClusterUID) && validUID(c.ControlClusterUID) && c.TargetClusterUID != c.ControlClusterUID
}

func validConfigBounds(c Config) bool {
	return c.ReconcileInterval >= time.Millisecond && c.ReconcileInterval <= 30*time.Second &&
		c.RunTimeout >= time.Second && c.RunTimeout <= 2*time.Hour &&
		c.CleanupTimeout >= time.Second && c.CleanupTimeout <= 5*time.Minute
}

func validSourceEndpoints(endpoints []SourceEndpoint) bool {
	if len(endpoints) < 1 || len(endpoints) > 2 {
		return false
	}
	hosts := make(map[string]bool)
	addresses := make(map[string]bool)
	for _, endpoint := range endpoints {
		ip, err := netip.ParseAddr(endpoint.Address)
		if err != nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.Zone() != "" ||
			(endpoint.Host != "github.com" && endpoint.Host != "api.github.com") || hosts[endpoint.Host] ||
			addresses[ip.String()] {
			return false
		}
		hosts[endpoint.Host], addresses[ip.String()] = true, true
	}
	return hosts["github.com"]
}

func (b *Backend) ConfigurationDigest() string {
	if b == nil || !b.config.Enabled {
		return ""
	}
	content, _ := json.Marshal(struct {
		Policy string
		Config Config
	}{backendPolicy, b.config})
	return pv.Digest(content)
}

func (b *Backend) EvidenceScope() contract.EvidenceScope {
	if b == nil || !b.config.Enabled || !b.qualified.Load() {
		return ""
	}
	return contract.ApplicationRuntime
}

func (b *Backend) allowedRuntime(image, id string) bool {
	if image == b.config.RuntimeImage {
		return imageDigest(id) == b.config.RuntimeImageID
	}
	if image == b.config.WorkerImage {
		return imageDigest(id) == b.config.WorkerImageID
	}
	if image == b.config.ToolchainImage {
		return imageDigest(id) == b.config.ToolchainImageID
	}
	expected, ok := b.config.RuntimeImages[image]
	return ok && expected == imageDigest(id)
}

func (b *Backend) ApplicationPlanDefaults() pv.ApplicationPlan {
	if b == nil || !b.config.Enabled {
		return pv.ApplicationPlan{}
	}
	plan := pv.ApplicationPlan{Version: pv.ApplicationProtocolVersion,
		Build:   pv.ApplicationBuild{Profile: pv.ApplicationBuildGoPackage, GoVersion: b.config.GoVersion},
		Runtime: pv.ApplicationRuntime{Image: b.config.RuntimeImage, ImageID: b.config.RuntimeImageID}}
	if policy := b.ApplicationPolicy(); policy != nil {
		plan.Permissions = policy.SubjectPermissions
	}
	return plan
}

func (b *Backend) ApplicationPolicy() *pv.ApplicationPolicy {
	if b == nil || !b.config.Enabled || b.config.ResourcePolicy == nil {
		return nil
	}
	content, _ := json.Marshal(b.config.ResourcePolicy)
	var policy pv.ApplicationPolicy
	if json.Unmarshal(content, &policy) != nil {
		return nil
	}
	return &policy
}

type targetIdentity struct {
	NodeName        string    `json:"nodeName"`
	NodeUID         types.UID `json:"nodeUID"`
	StorageClassUID types.UID `json:"storageClassUID"`
	RuntimeClassUID types.UID `json:"runtimeClassUID,omitempty"`
}

func (b *Backend) verifyTarget(ctx context.Context, expected *targetIdentity) (targetIdentity, error) {
	controlNS, targetNS := &corev1.Namespace{}, &corev1.Namespace{}
	key := client.ObjectKey{Name: metav1.NamespaceSystem}
	if err := b.controlReader.Get(ctx, key, controlNS); err != nil {
		return targetIdentity{}, safeError(contract.ErrUnavailable, err)
	}
	if err := b.targetReader.Get(ctx, key, targetNS); err != nil {
		return targetIdentity{}, safeError(contract.ErrUnavailable, err)
	}
	if string(controlNS.UID) != b.config.ControlClusterUID || string(targetNS.UID) != b.config.TargetClusterUID ||
		controlNS.UID == targetNS.UID || !controlNS.DeletionTimestamp.IsZero() || !targetNS.DeletionTimestamp.IsZero() {
		return targetIdentity{}, contract.ErrConflict
	}
	nodes := &corev1.NodeList{}
	if err := b.targetReader.List(ctx, nodes, client.MatchingLabels{b.config.NodeSelectorKey: b.config.NodeSelectorValue}); err != nil {
		return targetIdentity{}, safeError(contract.ErrUnavailable, err)
	}
	if len(nodes.Items) != 1 {
		return targetIdentity{}, ErrIsolation
	}
	node := &nodes.Items[0]
	class := &storagev1.StorageClass{}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Name: b.config.StorageClass}, class); err != nil {
		return targetIdentity{}, safeError(contract.ErrUnavailable, err)
	}
	if class.UID == "" || class.Provisioner == "" || class.ReclaimPolicy == nil ||
		*class.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete || !class.DeletionTimestamp.IsZero() {
		return targetIdentity{}, ErrIsolation
	}
	identity := targetIdentity{NodeName: node.Name, NodeUID: node.UID, StorageClassUID: class.UID}
	var runtimeClass *nodev1.RuntimeClass
	if b.config.RuntimeClassName != "" {
		runtimeClass = &nodev1.RuntimeClass{}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Name: b.config.RuntimeClassName}, runtimeClass); err != nil {
			return targetIdentity{}, safeError(contract.ErrUnavailable, err)
		}
		if runtimeClass.UID == "" || runtimeClass.Handler == "" || !runtimeClass.DeletionTimestamp.IsZero() {
			return targetIdentity{}, ErrIsolation
		}
		identity.RuntimeClassUID = runtimeClass.UID
	}
	if err := b.verifyQualifiedRuntime(node, runtimeClass); err != nil {
		return targetIdentity{}, err
	}
	if !b.validTargetNode(node) || (expected != nil && *expected != identity) {
		return targetIdentity{}, ErrIsolation
	}
	return identity, nil
}

func (b *Backend) validTargetNode(node *corev1.Node) bool {
	if node.UID == "" || !node.DeletionTimestamp.IsZero() || node.Spec.Unschedulable ||
		node.Labels["kubernetes.io/os"] != "linux" || node.Labels["kubernetes.io/arch"] != strings.TrimPrefix(b.config.Platform, "linux/") {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (b *Backend) lockEffects(ctx context.Context) error {
	select {
	case b.effectGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return safeError(contract.ErrUnavailable, ctx.Err())
	}
}

var (
	digestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	goVersionPattern = regexp.MustCompile(`^go1\.[0-9]+\.[0-9]+$`)
	uidPattern       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	ErrIsolation     = errors.New("application target isolation is not qualified")
	ErrUncertain     = errors.New("application target effect has an unresolved create outcome")
)

func validUID(uid string) bool { return uidPattern.MatchString(uid) }

func imageDigest(value string) string {
	_, digest, found := strings.Cut(value, "@")
	if !found {
		digest = strings.TrimPrefix(value, "containerd://")
	}
	if !digestPattern.MatchString(digest) {
		return ""
	}
	return digest
}

func pinnedImage(value string) bool {
	name, _, found := strings.Cut(value, "@")
	return found && name != "" && imageDigest(value) != "" && utf8.ValidString(value) &&
		!strings.ContainsAny(value, " \t\r\n\x00")
}

func apiAddress(endpoint string) (netip.Addr, int, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != "" {
		return netip.Addr{}, 0, contract.ErrInvalid
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.Zone() != "" {
		return netip.Addr{}, 0, contract.ErrInvalid
	}
	port := 443
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
	}
	if err != nil || port < 1 || port > 65535 {
		return netip.Addr{}, 0, contract.ErrInvalid
	}
	return ip, port, nil
}

type sanitizedError struct{ kind, cause error }

func (e sanitizedError) Error() string   { return e.kind.Error() }
func (e sanitizedError) Unwrap() []error { return []error{e.kind, e.cause} }
func (e sanitizedError) Retryable() bool {
	return e.kind == contract.ErrUnavailable && pvkube.IsTransientError(e.cause)
}

type noEffectsError struct{ cause error }

func (e noEffectsError) Error() string   { return e.cause.Error() }
func (e noEffectsError) Unwrap() error   { return e.cause }
func (e noEffectsError) NoEffects() bool { return true }

func safeError(kind, cause error) error {
	if cause == nil || errors.Is(cause, kind) {
		return kind
	}
	return sanitizedError{kind: kind, cause: cause}
}
