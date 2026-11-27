package storageconfig

import (
	"errors"
	"fmt"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type SourceKind string

const (
	SourceFlags SourceKind = "Flags"
	SourceFile  SourceKind = "File"
)

const EmbeddedSQLiteBackend = "sqlite"

var Contracts = []string{storagev1.StateContract, storagev1.ArtifactsContract}

type Config struct {
	Source   SourceKind
	Path     string
	Digest   string
	Bindings map[string]Binding
}

type Binding struct {
	Contract string `json:"contract"`
	Backend  string `json:"backend"`
	Source   Source `json:"source"`
}

type Source struct {
	EmbeddedSQLite *EmbeddedSQLiteSource `json:"embeddedSQLite,omitempty"`
	Adapter        *AdapterSource        `json:"adapter,omitempty"`
}

type EmbeddedSQLiteSource struct {
	Path string `json:"path"`
}

type AdapterSource struct {
	ServiceRef *ServiceRef `json:"serviceRef,omitempty"`
	URL        string      `json:"url,omitempty"`
	TLS        *TLSConfig  `json:"tls,omitempty"`
	ClientAuth *ClientAuth `json:"clientAuth,omitempty"`
}

type ServiceRef struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

type TLSConfig struct {
	ServerName  string        `json:"serverName,omitempty"`
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`
}

type ClientAuth struct {
	BearerTokenSecretRef *SecretKeyRef `json:"bearerTokenSecretRef,omitempty"`
}

type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type Reason string

const (
	ReasonInvalidConfiguration        Reason = "InvalidConfiguration"
	ReasonSecretNotFound              Reason = "SecretNotFound"
	ReasonSecretKeyMissing            Reason = "SecretKeyMissing"
	ReasonInvalidSecret               Reason = "InvalidSecret"
	ReasonSecretUnavailable           Reason = "SecretUnavailable"
	ReasonAdapterTransportUnavailable Reason = "AdapterTransportUnavailable"
)

type Error struct {
	Reason   Reason
	Contract string
	Backend  string
	Err      error
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

func ReasonOf(err error) (Reason, bool) {
	if configErr, ok := errors.AsType[*Error](err); ok {
		return configErr.Reason, true
	}
	return "", false
}

func invalidf(format string, args ...any) error {
	return &Error{Reason: ReasonInvalidConfiguration, Err: fmt.Errorf(format, args...)}
}

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
