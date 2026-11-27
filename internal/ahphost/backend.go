/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type SessionBackend interface {
	Name() string
	Agents(ctx context.Context, principal Principal) ([]ahptypes.AgentInfo, error)
	CreateSession(ctx context.Context, principal Principal, req CreateSessionRequest, emit Emitter) (*CreatedSession, error)
	DisposeSession(ctx context.Context, principal Principal, session string) error
	AuthorizeClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction) error
	HandleClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction, emit Emitter)
}

type CreateSessionRequest struct {
	Session            string
	Provider           string
	WorkingDirectories []string
	Config             map[string]json.RawMessage
}

type CreatedSession struct {
	State      ahptypes.SessionState
	Chats      map[string]ahptypes.ChatState
	Changesets map[string]ahptypes.ChangesetState
}

type Emitter interface {
	Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error
}

type RootContributor interface {
	RootExtensions(ctx context.Context, principal Principal) (meta, config map[string]json.RawMessage, err error)
}

type ConnectionPreparer interface {
	PrepareConnection(ctx context.Context, principal Principal)
}

var ErrUnknownExtension = errors.New("unknown extension method")

type ExtensionHandler interface {
	HandleExtension(ctx context.Context, principal Principal, method string, params json.RawMessage, emit Emitter) (any, error)
}

type ActionReleaser interface {
	ReleaseClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction)
}

type ResourceReader interface {
	ReadResource(ctx context.Context, principal Principal, session, channel, uri string) (ahptypes.ResourceReadResult, error)
}

var ErrResourceNotFound = errors.New("resource not found")

type Rejection struct {
	Reason string
}

func (r *Rejection) Error() string { return "action rejected: " + r.Reason }

type Registry struct {
	mu       sync.RWMutex
	backends []SessionBackend
}

func (r *Registry) Register(backend SessionBackend) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.backends {
		if existing.Name() == backend.Name() {
			return fmt.Errorf("duplicate AHP session backend %q", backend.Name())
		}
	}
	r.backends = append(r.backends, backend)
	return nil
}

func (r *Registry) list() []SessionBackend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]SessionBackend{}, r.backends...)
}

func NewRegistry(backends ...SessionBackend) (*Registry, error) {
	seen := map[string]bool{}
	for _, backend := range backends {
		if seen[backend.Name()] {
			return nil, fmt.Errorf("duplicate AHP session backend %q", backend.Name())
		}
		seen[backend.Name()] = true
	}
	return &Registry{backends: backends}, nil
}

func (r *Registry) Agents(ctx context.Context, principal Principal) ([]ahptypes.AgentInfo, error) {
	agents := []ahptypes.AgentInfo{}
	if r == nil {
		return agents, nil
	}
	for _, backend := range r.list() {
		offered, err := backend.Agents(ctx, principal)
		if err != nil {
			return nil, err
		}
		agents = append(agents, offered...)
	}
	return agents, nil
}

func (r *Registry) ForProvider(ctx context.Context, principal Principal, provider string) (SessionBackend, string, error) {
	if r == nil {
		return nil, "", fmt.Errorf("no session backends are configured")
	}
	for _, backend := range r.list() {
		offered, err := backend.Agents(ctx, principal)
		if err != nil {
			return nil, "", err
		}
		for _, agent := range offered {
			if provider == "" || agent.Provider == provider {
				return backend, agent.Provider, nil
			}
		}
	}
	return nil, "", fmt.Errorf("no agent provider %q is available", provider)
}

func (r *Registry) Named(name string) (SessionBackend, bool) {
	if r == nil {
		return nil, false
	}
	for _, backend := range r.list() {
		if backend.Name() == name {
			return backend, true
		}
	}
	return nil, false
}

func (r *Registry) rootExtensions(ctx context.Context, principal Principal) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	if r == nil {
		return nil, nil, nil
	}
	var meta, config map[string]json.RawMessage
	for _, backend := range r.list() {
		contributor, ok := backend.(RootContributor)
		if !ok {
			continue
		}
		m, c, err := contributor.RootExtensions(ctx, principal)
		if err != nil {
			return nil, nil, err
		}
		for key, value := range m {
			if meta == nil {
				meta = map[string]json.RawMessage{}
			}
			meta[key] = value
		}
		for key, value := range c {
			if config == nil {
				config = map[string]json.RawMessage{}
			}
			config[key] = value
		}
	}
	return meta, config, nil
}

func (r *Registry) prepareConnection(ctx context.Context, principal Principal) {
	if r == nil {
		return
	}
	for _, backend := range r.list() {
		if preparer, ok := backend.(ConnectionPreparer); ok {
			preparer.PrepareConnection(ctx, principal)
		}
	}
}

func (r *Registry) extension(ctx context.Context, principal Principal, method string, params json.RawMessage, emit Emitter) (any, error) {
	if r != nil {
		for _, backend := range r.list() {
			handler, ok := backend.(ExtensionHandler)
			if !ok {
				continue
			}
			result, err := handler.HandleExtension(ctx, principal, method, params, emit)
			if errors.Is(err, ErrUnknownExtension) {
				continue
			}
			return result, err
		}
	}
	return nil, ErrUnknownExtension
}

func (r *Registry) readRootResource(ctx context.Context, principal Principal, uri string) (ahptypes.ResourceReadResult, error) {
	if r != nil {
		for _, backend := range r.list() {
			reader, ok := backend.(ResourceReader)
			if !ok {
				continue
			}
			result, err := reader.ReadResource(ctx, principal, "", ahptypes.RootResourceURI, uri)
			if errors.Is(err, ErrResourceNotFound) {
				continue
			}
			return result, err
		}
	}
	return ahptypes.ResourceReadResult{}, ErrResourceNotFound
}
