// Package storageconfig loads the controller's storage configuration: which
// backend serves each storage contract (state.v1 and artifacts.v1) and how to
// reach it.
//
// The configuration comes from one of two sources. Without a configuration
// file, both contracts bind to embedded SQLite at --store-path, which is the
// default installation. With --storage-config, a file (normally rendered by
// the Helm chart from storage.bindings and storage.adapters) names the backend
// for each contract. Both sources produce the same Config, so startup,
// validation and status code does not depend on where the configuration came
// from.
//
// Loading never contacts a storage backend. Secret references are resolved
// separately, through a direct Kubernetes reader, before application storage
// opens. Until the storage wire transport exists, only embedded SQLite can
// serve a contract at runtime; see [Config.RequireEmbeddedSQLite].
package storageconfig

import (
	"errors"
	"fmt"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// SourceKind identifies where a Config came from.
type SourceKind string

const (
	// SourceFlags is the default configuration: both contracts on embedded
	// SQLite at --store-path.
	SourceFlags SourceKind = "Flags"
	// SourceFile is a configuration file named by --storage-config.
	SourceFile SourceKind = "File"
)

// EmbeddedSQLiteBackend is the reserved backend name that binds a contract to
// the in-process SQLite store. Adapter names cannot use it.
const EmbeddedSQLiteBackend = "sqlite"

// Contracts lists the storage contracts every configuration binds, in
// canonical order.
var Contracts = []string{storagev1.StateContract, storagev1.ArtifactsContract}

// Config is the loaded storage configuration.
type Config struct {
	// Source is where the configuration came from.
	Source SourceKind
	// Path is the configuration file for SourceFile, and empty otherwise.
	Path string
	// Digest identifies the bound configuration: "sha256:" followed by the
	// hex SHA-256 of its canonical encoding. It covers every binding's
	// backend, SQLite path, endpoint, TLS settings and Secret references,
	// never Secret values. It does not depend on the source, so the default
	// flags and a file binding both contracts to the same SQLite path have the
	// same digest.
	Digest string
	// Bindings holds one binding per contract in Contracts, keyed by contract.
	Bindings map[string]Binding
}

// Binding selects the backend that serves one contract.
type Binding struct {
	// Contract is storagev1.StateContract or storagev1.ArtifactsContract.
	Contract string `json:"contract"`
	// Backend is EmbeddedSQLiteBackend or the name of a configured adapter.
	Backend string `json:"backend"`
	// Source says how to reach the backend.
	Source Source `json:"source"`
}

// Source holds exactly one way to reach a backend.
type Source struct {
	EmbeddedSQLite *EmbeddedSQLiteSource `json:"embeddedSQLite,omitempty"`
	Adapter        *AdapterSource        `json:"adapter,omitempty"`
}

// EmbeddedSQLiteSource is the in-process SQLite store. Its file path is an
// installation setting (--store-path), not part of the configuration file.
type EmbeddedSQLiteSource struct {
	Path string `json:"path"`
}

// AdapterSource is a separately deployed storage adapter in the installation
// namespace, reached over TLS with a bearer token. Exactly one of ServiceRef
// and URL is set.
type AdapterSource struct {
	// ServiceRef names a Service in the installation namespace.
	ServiceRef *ServiceRef `json:"serviceRef,omitempty"`
	// URL is one https:// endpoint, without credentials, query or fragment.
	URL string `json:"url,omitempty"`
	// TLS configures server certificate verification.
	TLS *TLSConfig `json:"tls,omitempty"`
	// ClientAuth configures how Orka authenticates to the adapter.
	ClientAuth *ClientAuth `json:"clientAuth,omitempty"`
}

// ServiceRef names a Service port in the installation namespace.
type ServiceRef struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

// TLSConfig configures how Orka verifies an adapter's server certificate.
type TLSConfig struct {
	// ServerName is the name verified against the certificate. Empty leaves
	// the choice to the transport: the Service DNS name or the URL host.
	ServerName string `json:"serverName,omitempty"`
	// CASecretRef names a PEM CA bundle that replaces the system roots. Nil
	// uses the system roots.
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`
}

// ClientAuth configures adapter authentication.
type ClientAuth struct {
	// BearerTokenSecretRef names the token Orka presents. It is required.
	BearerTokenSecretRef *SecretKeyRef `json:"bearerTokenSecretRef,omitempty"`
}

// SecretKeyRef names one key of a Secret in the installation namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Reason is a stable, CamelCase reason for a configuration failure. Status,
// Events and logs report it unchanged.
type Reason string

const (
	// ReasonInvalidConfiguration marks flags or a configuration file that do
	// not describe a valid configuration.
	ReasonInvalidConfiguration Reason = "InvalidConfiguration"
	// ReasonSecretNotFound marks a referenced Secret that does not exist.
	ReasonSecretNotFound Reason = "SecretNotFound"
	// ReasonSecretKeyMissing marks a referenced Secret key that is absent or
	// empty.
	ReasonSecretKeyMissing Reason = "SecretKeyMissing"
	// ReasonInvalidSecret marks a Secret value that cannot be used, such as a
	// short token or a CA bundle without certificates.
	ReasonInvalidSecret Reason = "InvalidSecret"
	// ReasonSecretUnavailable marks a Secret read that failed for a reason
	// other than absence, such as an API outage or missing permission.
	ReasonSecretUnavailable Reason = "SecretUnavailable"
	// ReasonAdapterTransportUnavailable marks a contract bound to an adapter
	// while this controller has no storage wire transport.
	ReasonAdapterTransportUnavailable Reason = "AdapterTransportUnavailable"
)

// Error reports a configuration failure with a stable reason. Its message
// names flags, fields, Secrets and keys, never Secret values.
type Error struct {
	Reason Reason
	// Contract is the affected contract, when the failure concerns one.
	Contract string
	// Backend is the affected backend, when the failure concerns one.
	Backend string
	Err     error
}

func (e *Error) Error() string {
	switch {
	case e.Contract != "" && e.Backend != "":
		return fmt.Sprintf("%s: %s backend %q: %v", e.Reason, e.Contract, e.Backend, e.Err)
	case e.Backend != "":
		return fmt.Sprintf("%s: backend %q: %v", e.Reason, e.Backend, e.Err)
	default:
		return fmt.Sprintf("%s: %v", e.Reason, e.Err)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// ReasonOf returns the reason carried by err, if any.
func ReasonOf(err error) (Reason, bool) {
	if configErr, ok := errors.AsType[*Error](err); ok {
		return configErr.Reason, true
	}
	return "", false
}

func invalidf(format string, args ...any) error {
	return &Error{Reason: ReasonInvalidConfiguration, Err: fmt.Errorf(format, args...)}
}

// RequireEmbeddedSQLite returns the SQLite path when every contract is bound
// to embedded SQLite. A contract bound to an adapter fails with
// ReasonAdapterTransportUnavailable, because this controller cannot reach an
// adapter yet. It never substitutes SQLite for a configured adapter.
func (c *Config) RequireEmbeddedSQLite() (string, error) {
	path := ""
	for _, contract := range Contracts {
		binding, ok := c.Bindings[contract]
		if !ok {
			return "", invalidf("contract %s is not bound", contract)
		}
		if binding.Source.Adapter != nil {
			return "", &Error{
				Reason:   ReasonAdapterTransportUnavailable,
				Contract: contract,
				Backend:  binding.Backend,
				Err:      errors.New("this controller cannot reach storage adapters yet; bind the contract to sqlite"),
			}
		}
		if binding.Source.EmbeddedSQLite == nil {
			return "", invalidf("contract %s has no source", contract)
		}
		if path != "" && binding.Source.EmbeddedSQLite.Path != path {
			return "", invalidf("contracts are bound to different SQLite paths")
		}
		path = binding.Source.EmbeddedSQLite.Path
	}
	return path, nil
}
