package enforcement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Coordinator is supplied by the operator, never by a Task or Agent.
type Coordinator interface {
	ProviderSetDigest(nodeName, nodeUID string) (string, error)
	Ensure(context.Context, *corev1alpha1.ContainerEnforcementBinding, *enforcementv1.Requirement) ([]*enforcementv1.Enforced, *enforcementv1.EnsureResponse, error)
	Retire(context.Context, *corev1alpha1.ContainerEnforcementBinding) (bool, error)
}

type NodeProviders struct {
	NodeName  string           `json:"nodeName"`
	NodeUID   string           `json:"nodeUID"`
	Providers []ProviderConfig `json:"providers"`
}

type RegistryConfig struct {
	Nodes []NodeProviders `json:"nodes"`
}

type nodeProviders struct {
	name    string
	digest  string
	config  []ProviderConfig
	clients []*Client
}

// Registry retains routes for old Node incarnations too: removing or changing
// one while it owns a live binding fails closed rather than losing cleanup.
type Registry struct {
	mu           sync.Mutex
	nodes        map[string]*nodeProviders
	path         string
	sourceDigest [sha256.Size]byte
	closed       bool
}

func LoadRegistry(path string) (*Registry, error) {
	config, digest, err := readRegistry(path)
	if err != nil {
		return nil, err
	}
	registry, err := NewRegistry(config)
	if err != nil {
		return nil, err
	}
	registry.path, registry.sourceDigest = path, digest
	return registry, nil
}

// NewProjectedRegistry defers lookup until an optional Kubernetes projection exists.
// Missing or invalid data still refuses every enforcement lookup.
func NewProjectedRegistry(path string) (*Registry, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("projected enforcement registry requires an absolute path")
	}
	return &Registry{nodes: make(map[string]*nodeProviders), path: path}, nil
}

func readRegistry(path string) (RegistryConfig, [sha256.Size]byte, error) {
	var config RegistryConfig
	var digest [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return config, digest, fmt.Errorf("read operator enforcement registry: %w", err)
	}
	blob, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err := errors.Join(err, file.Close()); err != nil {
		return config, digest, fmt.Errorf("read operator enforcement registry: %w", err)
	}
	if len(blob) > 1<<20 {
		return config, digest, errors.New("operator enforcement registry exceeds metadata bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(blob))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, digest, fmt.Errorf("decode operator enforcement registry: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return config, digest, errors.New("enforcement registry must contain exactly one bounded object")
	}
	return config, sha256.Sum256(blob), nil
}

func (r *Registry) refreshLocked() error {
	if r.closed {
		return errors.New("operator enforcement registry is closed")
	}
	if r.path == "" {
		return nil
	}
	config, digest, err := readRegistry(r.path)
	if err != nil {
		return err
	}
	if digest == r.sourceDigest {
		return nil
	}
	candidate, err := NewRegistry(config)
	if err != nil {
		return err
	}
	defer candidate.Close()
	for uid, previous := range r.nodes {
		next := candidate.nodes[uid]
		if next == nil || previous.name != next.name || previous.digest != next.digest ||
			!slices.Equal(previous.config, next.config) {
			return errors.New("operator enforcement registry updates may only add Node incarnations")
		}
	}
	for uid, next := range candidate.nodes {
		if r.nodes[uid] == nil {
			r.nodes[uid] = next
			delete(candidate.nodes, uid)
		}
	}
	r.sourceDigest = digest
	return nil
}

func NewRegistry(config RegistryConfig) (*Registry, error) {
	if len(config.Nodes) == 0 {
		return nil, errors.New("operator enforcement registry needs explicit Node incarnations")
	}
	registry := &Registry{nodes: make(map[string]*nodeProviders)}
	success := false
	defer func() {
		if !success {
			registry.Close()
		}
	}()
	for _, node := range config.Nodes {
		if !validText(node.NodeName, 253) || !validText(node.NodeUID, 256) ||
			len(node.Providers) == 0 || len(node.Providers) > 32 || registry.nodes[node.NodeUID] != nil {
			return nil, errors.New("enforcement registry needs unique Node UIDs and nonempty mandatory providers")
		}
		entry := &nodeProviders{name: node.NodeName}
		registry.nodes[node.NodeUID] = entry
		providers := slices.Clone(node.Providers)
		slices.SortFunc(providers, func(a, b ProviderConfig) int {
			if a.Name < b.Name {
				return -1
			}
			if a.Name > b.Name {
				return 1
			}
			return 0
		})
		entry.config = providers
		type peer struct {
			Name, Endpoint, ServerName, CA string
		}
		peers := make([]peer, 0, len(providers))
		lastName := ""
		for _, config := range providers {
			if config.Name == lastName {
				return nil, errors.New("duplicate mandatory provider name")
			}
			client, err := NewClient(config)
			if err != nil {
				return nil, err
			}
			entry.clients = append(entry.clients, client)
			peers = append(peers, peer{config.Name, config.Endpoint, config.ServerName, client.caDigest})
			lastName = config.Name
		}
		encoded, err := json.Marshal(peers)
		if err != nil {
			return nil, fmt.Errorf("encode mandatory provider identities: %w", err)
		}
		sum := sha256.Sum256(append([]byte("orka.enforcement.providers/v1\x00"), encoded...))
		entry.digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	success = true
	return registry, nil
}

func (r *Registry) Close() {
	if r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			return
		}
		r.closed = true
		for _, entry := range r.nodes {
			for _, client := range entry.clients {
				if err := client.Close(); err != nil {
					logf.Log.Error(err, "close enforcement provider client", "nodeName", entry.name)
				}
			}
		}
	}
}

func (r *Registry) ProviderSetDigest(nodeName, nodeUID string) (string, error) {
	if r == nil {
		return "", errors.New("no operator-trusted enforcement registry configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerSetDigestLocked(nodeName, nodeUID)
}

func (r *Registry) providerSetDigestLocked(nodeName, nodeUID string) (string, error) {
	if err := r.refreshLocked(); err != nil {
		return "", err
	}
	entry := r.nodes[nodeUID]
	if entry == nil || entry.name != nodeName {
		return "", errors.New("no enforcement providers registered for this exact Node incarnation")
	}
	return entry.digest, nil
}

func (r *Registry) boundProviders(binding *corev1alpha1.ContainerEnforcementBinding) ([]*Client, error) {
	if binding == nil {
		return nil, errors.New("persisted physical container binding required")
	}
	if r == nil {
		return nil, errors.New("no operator-trusted enforcement registry configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	digest, err := r.providerSetDigestLocked(binding.NodeName, binding.NodeUID)
	if err != nil {
		return nil, err
	}
	if digest != binding.ProviderSetDigest {
		return nil, errors.New("mandatory provider identity/trust changed while the container binding owns cleanup")
	}
	return r.nodes[binding.NodeUID].clients, nil
}

func (r *Registry) Ensure(ctx context.Context, binding *corev1alpha1.ContainerEnforcementBinding, requirement *enforcementv1.Requirement) ([]*enforcementv1.Enforced, *enforcementv1.EnsureResponse, error) {
	providers, err := r.boundProviders(binding)
	if err != nil {
		return nil, nil, err
	}
	digest, err := RequirementDigest(requirement)
	if err != nil {
		return nil, nil, err
	}
	if binding.RequirementDigest != digest {
		return nil, nil, errors.New("container binding requirement changed")
	}
	facts, err := KubernetesFacts(binding)
	if err != nil {
		return nil, nil, err
	}
	return EnsureAll(ctx, providers, &enforcementv1.EnsureRequest{
		BindingId: binding.BindingID, Requirement: requirement, TargetFacts: facts,
	})
}

func (r *Registry) Retire(ctx context.Context, binding *corev1alpha1.ContainerEnforcementBinding) (bool, error) {
	providers, err := r.boundProviders(binding)
	if err != nil {
		return false, err
	}
	for _, provider := range providers {
		result, err := provider.Retire(ctx, binding.BindingID)
		if err != nil {
			return false, err
		}
		if refused := result.GetRefused(); refused != nil {
			return false, fmt.Errorf("enforcement retirement refused: %s", refused.Reason)
		}
		if result.GetRetired() == nil {
			return false, nil
		}
	}
	return true, nil
}
