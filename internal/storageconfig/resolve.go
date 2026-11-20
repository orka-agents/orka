package storageconfig

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MinBearerTokenBytes is the shortest adapter bearer token accepted, the same
// floor AgentRuntime controller tokens use.
const MinBearerTokenBytes = 32

// Credentials is the Secret material for one adapter. Its String and
// GoString forms never include the values.
type Credentials struct {
	caBundle    []byte
	bearerToken []byte
}

// CABundle returns a copy of the PEM CA bundle, or nil when the adapter uses
// the system roots.
func (c Credentials) CABundle() []byte { return bytes.Clone(c.caBundle) }

// BearerToken returns a copy of the bearer token.
func (c Credentials) BearerToken() []byte { return bytes.Clone(c.bearerToken) }

func (c Credentials) String() string { return "storageconfig.Credentials{<redacted>}" }

// GoString keeps %#v from printing the token.
func (c Credentials) GoString() string { return c.String() }

// Resolved is a Config with the Secret material its adapters need.
type Resolved struct {
	Config *Config
	// Credentials holds one entry per adapter backend, keyed by backend name.
	// It is empty when every contract is bound to embedded SQLite.
	Credentials map[string]*Credentials
}

// Resolve reads every Secret the bound adapters reference from namespace,
// the installation namespace, through reader. Pass the manager's API reader,
// which reads directly from the API server: resolution runs before the cache
// starts and must not wait for it. A configuration bound only to embedded
// SQLite makes no API calls.
func (c *Config) Resolve(ctx context.Context, reader client.Reader, namespace string) (*Resolved, error) {
	resolved := &Resolved{Config: c, Credentials: map[string]*Credentials{}}
	var adapters []Binding
	for _, contract := range Contracts {
		binding, ok := c.Bindings[contract]
		if !ok {
			return nil, invalidf("contract %s is not bound", contract)
		}
		if binding.Source.Adapter == nil {
			continue
		}
		if _, seen := resolved.Credentials[binding.Backend]; seen {
			continue
		}
		resolved.Credentials[binding.Backend] = nil
		adapters = append(adapters, binding)
	}
	if len(adapters) == 0 {
		return resolved, nil
	}
	if namespace == "" {
		return nil, invalidf("the installation namespace is unknown, so adapter Secrets cannot be resolved")
	}
	secrets := secretReader{reader: reader, namespace: namespace, read: map[string]*corev1.Secret{}}
	for _, binding := range adapters {
		credentials, err := secrets.credentials(ctx, binding.Source.Adapter)
		if err != nil {
			if configErr, ok := errors.AsType[*Error](err); ok {
				configErr.Backend = binding.Backend
			}
			return nil, err
		}
		resolved.Credentials[binding.Backend] = credentials
	}
	return resolved, nil
}

// secretReader reads each Secret once per resolution, so one adapter's CA
// bundle and token come from the same Secret version when they share one.
type secretReader struct {
	reader    client.Reader
	namespace string
	read      map[string]*corev1.Secret
}

func (r *secretReader) credentials(ctx context.Context, adapter *AdapterSource) (*Credentials, error) {
	if adapter.ClientAuth == nil || adapter.ClientAuth.BearerTokenSecretRef == nil {
		return nil, invalidf("clientAuth.bearerTokenSecretRef is required")
	}
	token, err := r.value(ctx, *adapter.ClientAuth.BearerTokenSecretRef)
	if err != nil {
		return nil, err
	}
	if err := validateBearerToken(token); err != nil {
		return nil, secretError(ReasonInvalidSecret, r.namespace, *adapter.ClientAuth.BearerTokenSecretRef, err)
	}
	credentials := &Credentials{bearerToken: token}
	if adapter.TLS != nil && adapter.TLS.CASecretRef != nil {
		ca, err := r.value(ctx, *adapter.TLS.CASecretRef)
		if err != nil {
			return nil, err
		}
		if !x509.NewCertPool().AppendCertsFromPEM(ca) {
			return nil, secretError(ReasonInvalidSecret, r.namespace, *adapter.TLS.CASecretRef,
				errors.New("value holds no PEM certificate"))
		}
		credentials.caBundle = ca
	}
	return credentials, nil
}

func (r *secretReader) value(ctx context.Context, ref SecretKeyRef) ([]byte, error) {
	secret, ok := r.read[ref.Name]
	if !ok {
		secret = &corev1.Secret{}
		if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: ref.Name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, secretError(ReasonSecretNotFound, r.namespace, ref, errors.New("the Secret does not exist"))
			}
			return nil, secretError(ReasonSecretUnavailable, r.namespace, ref, err)
		}
		r.read[ref.Name] = secret
	}
	value := secret.Data[ref.Key]
	if len(value) == 0 {
		return nil, secretError(ReasonSecretKeyMissing, r.namespace, ref, errors.New("key is absent or empty"))
	}
	return bytes.Clone(value), nil
}

// validateBearerToken requires a token that fits an Authorization header
// unchanged: long enough, and only visible ASCII.
func validateBearerToken(token []byte) error {
	if len(token) < MinBearerTokenBytes {
		return fmt.Errorf("bearer token must contain at least %d bytes", MinBearerTokenBytes)
	}
	for _, b := range token {
		if b < 0x21 || b > 0x7e {
			return errors.New("bearer token must contain only visible ASCII characters, with no whitespace")
		}
	}
	return nil
}

func secretError(reason Reason, namespace string, ref SecretKeyRef, err error) error {
	return &Error{Reason: reason, Err: fmt.Errorf("secret %s/%s key %q: %w", namespace, ref.Name, ref.Key, err)}
}
