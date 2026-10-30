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

// SessionBackend runs AHP sessions for the agents it offers. The host owns
// the protocol, the journal and fan-out; a backend owns what a session does.
// Every call carries the principal so the backend can authorize the Orka
// operation it causes with that principal's constraints.
type SessionBackend interface {
	// Name identifies the backend in the session catalog.
	Name() string
	// Agents lists the agents this backend offers the principal.
	Agents(ctx context.Context, principal Principal) ([]ahptypes.AgentInfo, error)
	// CreateSession starts a session. The host records the returned states and
	// marks the session ready.
	CreateSession(ctx context.Context, principal Principal, req CreateSessionRequest, emit Emitter) (*CreatedSession, error)
	// DisposeSession stops a session and releases what it holds.
	DisposeSession(ctx context.Context, principal Principal, session string) error
	// AuthorizeClientAction decides whether a client-dispatched action that the
	// host already validated may be applied, as the Orka operation it causes.
	// Returning a *Rejection rejects it with a reason the client sees.
	AuthorizeClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction) error
	// HandleClientAction runs the side effects of a client action after the
	// host applied and journaled it. It runs off the connection's read loop.
	HandleClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction, emit Emitter)
}

// CreateSessionRequest is a validated createSession command.
type CreateSessionRequest struct {
	Session            string
	Provider           string
	WorkingDirectories []string
	Config             map[string]json.RawMessage
}

// CreatedSession is a new session's initial state and the states of the
// chats and changesets it lists.
type CreatedSession struct {
	State      ahptypes.SessionState
	Chats      map[string]ahptypes.ChatState
	Changesets map[string]ahptypes.ChangesetState
}

// Emitter delivers a backend's actions for one user through the host, which
// reduces, journals and fans them out. On the root channel only configuration
// and agent changes are accepted.
type Emitter interface {
	Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error
}

// RootContributor adds a backend's extensions to each user's root state:
// _meta entries and configuration values. Both are merged across backends.
type RootContributor interface {
	RootExtensions(ctx context.Context, principal Principal) (meta, config map[string]json.RawMessage, err error)
}

// ErrUnknownExtension is returned by an ExtensionHandler for a method it does not serve.
var ErrUnknownExtension = errors.New("unknown extension method")

// ExtensionHandler serves host-specific commands in the extensions/ namespace.
// Errors other than ErrUnknownExtension and *Rejection reach the client as an
// internal error.
type ExtensionHandler interface {
	HandleExtension(ctx context.Context, principal Principal, method string, params json.RawMessage, emit Emitter) (any, error)
}

// ActionReleaser is a backend that holds something for an action while
// authorizing it, such as room in a queue. When the host cannot apply an
// authorized action, HandleClientAction never runs for it, and the host calls
// ReleaseClientAction instead so the backend can let go of what it held.
type ActionReleaser interface {
	ReleaseClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction)
}

// ResourceReader serves resourceRead for the channels of a backend's sessions.
// It returns ErrResourceNotFound for anything it does not serve. A read on the
// root channel arrives with an empty session: the backend resolves which of
// the user's sessions, if any, owns the resource.
type ResourceReader interface {
	ReadResource(ctx context.Context, principal Principal, session, channel, uri string) (ahptypes.ResourceReadResult, error)
}

// ErrResourceNotFound reports a resource the backend does not serve.
var ErrResourceNotFound = errors.New("resource not found")

// Rejection rejects a client action with a reason the client sees.
type Rejection struct {
	Reason string
}

func (r *Rejection) Error() string { return "action rejected: " + r.Reason }

// Registry holds the session backends in the order their agents are listed.
type Registry struct {
	mu       sync.RWMutex
	backends []SessionBackend
}

// Register adds a backend after the registry was built, before the host
// serves connections. Backend names must stay unique.
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

// NewRegistry builds a registry. Backend names must be unique.
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

// Agents lists every backend's agents for the principal.
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

// ForProvider returns the backend offering the provider to the principal. An
// empty provider selects the first agent offered.
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

// Named returns the backend with the given name.
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

// rootExtensions merges every backend's root extensions.
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

// extension routes an extensions/ method to the first backend that serves it.
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

// readRootResource offers a root-channel read to every backend that serves resources.
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
