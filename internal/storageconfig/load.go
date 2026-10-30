package storageconfig

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"

	yamlv3 "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// DefaultStorePath is the default embedded SQLite database file.
const DefaultStorePath = "/data/orka.db"

// MaxFileBytes bounds the configuration file. It matches the largest
// ConfigMap Kubernetes stores.
const MaxFileBytes = 1 << 20

// adapterURLPattern is the endpoint form AgentRuntime registrations use,
// restricted to https: no whitespace, credentials, query or fragment.
var adapterURLPattern = regexp.MustCompile(`^https://[^\s@?#]+$`)

// Flags holds the storage command-line flags.
type Flags struct {
	storeBackend storeBackendFlag
	// StorePath is the embedded SQLite file used by every contract bound to
	// sqlite.
	StorePath string
	// ConfigPath is the configuration file; empty selects the default
	// configuration.
	ConfigPath string
}

// storeBackendFlag records whether --store-backend was given, so a
// configuration file and a backend flag cannot both select storage.
type storeBackendFlag struct {
	value string
	set   bool
}

func (f *storeBackendFlag) String() string {
	if f == nil {
		return ""
	}
	return f.value
}

func (f *storeBackendFlag) Set(value string) error {
	f.value = value
	f.set = true
	return nil
}

// AddFlags registers --store-backend, --store-path and --storage-config.
func (f *Flags) AddFlags(fs *flag.FlagSet) {
	f.storeBackend = storeBackendFlag{value: EmbeddedSQLiteBackend}
	fs.Var(&f.storeBackend, "store-backend",
		"Storage `backend` when --storage-config is not set. Only sqlite is supported. Cannot be combined with --storage-config.")
	fs.StringVar(&f.StorePath, "store-path", DefaultStorePath,
		"Path to the embedded SQLite database file, used by every storage contract bound to sqlite.")
	fs.StringVar(&f.ConfigPath, "storage-config", "",
		"Path to a storage configuration file that binds each storage contract to sqlite or to a storage adapter. "+
			"Empty binds both contracts to embedded SQLite at --store-path.")
}

// Load builds the configuration from the flags and, when --storage-config is
// set, the configuration file. It reads no Secrets and contacts no backend.
func (f *Flags) Load() (*Config, error) {
	var cfg *Config
	if f.ConfigPath == "" {
		backend := EmbeddedSQLiteBackend
		if f.storeBackend.set {
			backend = f.storeBackend.value
		}
		if backend != EmbeddedSQLiteBackend {
			return nil, invalidf("unsupported --store-backend %q: only %s is supported; use --storage-config to bind a storage adapter",
				backend, EmbeddedSQLiteBackend)
		}
		if f.StorePath == "" {
			return nil, invalidf("--store-path must not be empty")
		}
		cfg = &Config{Source: SourceFlags, Bindings: make(map[string]Binding, len(Contracts))}
		for _, contract := range Contracts {
			cfg.Bindings[contract] = sqliteBinding(contract, f.StorePath)
		}
	} else {
		if f.storeBackend.set {
			return nil, invalidf("--store-backend cannot be combined with --storage-config, which selects the backend for every contract")
		}
		data, err := readFile(f.ConfigPath)
		if err != nil {
			return nil, err
		}
		cfg, err = parse(data, f.StorePath)
		if err != nil {
			return nil, err
		}
		cfg.Source = SourceFile
		cfg.Path = f.ConfigPath
	}
	digest, err := computeDigest(cfg.Bindings)
	if err != nil {
		return nil, err
	}
	cfg.Digest = digest
	return cfg, nil
}

func readFile(path string) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- operator-supplied configuration path.
	if err != nil {
		return nil, invalidf("read storage configuration: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, invalidf("read storage configuration: %w", err)
	}
	if len(data) > MaxFileBytes {
		return nil, invalidf("storage configuration %s exceeds %d bytes", path, MaxFileBytes)
	}
	return data, nil
}

// file is the configuration file format. Contract names are the short forms
// state and artifacts; adapters are keyed by name.
type file struct {
	Bindings *fileBindings             `json:"bindings"`
	Adapters map[string]*AdapterSource `json:"adapters,omitempty"`
}

type fileBindings struct {
	State     string `json:"state"`
	Artifacts string `json:"artifacts"`
}

// parse decodes and validates a configuration file. It holds at most one
// YAML document, field names match exactly, and unknown and duplicate fields
// are rejected, so no setting is silently ignored.
func parse(data []byte, storePath string) (*Config, error) {
	if err := requireOneDocument(data); err != nil {
		return nil, invalidf("decode storage configuration: %w", err)
	}
	jsonData, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, invalidf("decode storage configuration: %w", err)
	}
	var decoded file
	strictErrs, err := kjson.UnmarshalStrict(jsonData, &decoded)
	if err != nil {
		return nil, invalidf("decode storage configuration: %w", err)
	}
	if len(strictErrs) > 0 {
		return nil, invalidf("decode storage configuration: %w", errors.Join(strictErrs...))
	}
	if errs := validateFile(&decoded, storePath); len(errs) > 0 {
		return nil, &Error{Reason: ReasonInvalidConfiguration, Err: errs.ToAggregate()}
	}
	cfg := &Config{Bindings: make(map[string]Binding, len(Contracts))}
	for contract, backend := range map[string]string{
		storagev1.StateContract:     decoded.Bindings.State,
		storagev1.ArtifactsContract: decoded.Bindings.Artifacts,
	} {
		if backend == EmbeddedSQLiteBackend {
			cfg.Bindings[contract] = sqliteBinding(contract, storePath)
			continue
		}
		cfg.Bindings[contract] = Binding{
			Contract: contract,
			Backend:  backend,
			Source:   Source{Adapter: cloneAdapter(decoded.Adapters[backend])},
		}
	}
	return cfg, nil
}

// requireOneDocument rejects a file with more than one YAML document. The
// strict decoder reads only the first, so a later document would otherwise
// go unvalidated and outside the digest.
func requireOneDocument(data []byte) error {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	for documents := 0; ; documents++ {
		var node yamlv3.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if documents == 1 {
			return errors.New("the file must hold one YAML document")
		}
	}
}

func sqliteBinding(contract, path string) Binding {
	return Binding{
		Contract: contract,
		Backend:  EmbeddedSQLiteBackend,
		Source:   Source{EmbeddedSQLite: &EmbeddedSQLiteSource{Path: path}},
	}
}

func validateFile(decoded *file, storePath string) field.ErrorList {
	var errs field.ErrorList
	bindingsPath := field.NewPath("bindings")
	if decoded.Bindings == nil {
		return append(errs, field.Required(bindingsPath, "both state and artifacts must be bound"))
	}
	bound := map[string]bool{}
	usesSQLite := false
	for _, binding := range []struct {
		name    string
		backend string
	}{
		{name: "state", backend: decoded.Bindings.State},
		{name: "artifacts", backend: decoded.Bindings.Artifacts},
	} {
		path := bindingsPath.Child(binding.name)
		switch {
		case binding.backend == "":
			errs = append(errs, field.Required(path, fmt.Sprintf("%q or the name of a configured adapter", EmbeddedSQLiteBackend)))
		case binding.backend == EmbeddedSQLiteBackend:
			usesSQLite = true
		case decoded.Adapters[binding.backend] == nil:
			errs = append(errs, field.Invalid(path, binding.backend, "names no configured adapter"))
		default:
			bound[binding.backend] = true
		}
	}
	if usesSQLite && storePath == "" {
		errs = append(errs, field.Required(bindingsPath, "--store-path must be set when a contract is bound to sqlite"))
	}
	adaptersPath := field.NewPath("adapters")
	for _, name := range slices.Sorted(maps.Keys(decoded.Adapters)) {
		path := adaptersPath.Key(name)
		if name == EmbeddedSQLiteBackend {
			errs = append(errs, field.Invalid(path, name, "is reserved for embedded SQLite"))
			continue
		}
		for _, msg := range validation.IsDNS1123Label(name) {
			errs = append(errs, field.Invalid(path, name, msg))
		}
		adapter := decoded.Adapters[name]
		if adapter == nil {
			errs = append(errs, field.Required(path, "exactly one of serviceRef or url"))
			continue
		}
		if !bound[name] {
			errs = append(errs, field.Invalid(path, name, "is not bound to any contract"))
		}
		errs = append(errs, validateAdapter(adapter, path)...)
	}
	return errs
}

func validateAdapter(adapter *AdapterSource, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	switch {
	case adapter.ServiceRef != nil && adapter.URL != "":
		errs = append(errs, field.Forbidden(path, "set exactly one of serviceRef or url"))
	case adapter.ServiceRef == nil && adapter.URL == "":
		errs = append(errs, field.Required(path, "exactly one of serviceRef or url"))
	case adapter.ServiceRef != nil:
		refPath := path.Child("serviceRef")
		for _, msg := range validation.IsDNS1035Label(adapter.ServiceRef.Name) {
			errs = append(errs, field.Invalid(refPath.Child("name"), adapter.ServiceRef.Name, msg))
		}
		if adapter.ServiceRef.Port < 1 || adapter.ServiceRef.Port > 65535 {
			errs = append(errs, field.Invalid(refPath.Child("port"), adapter.ServiceRef.Port, "must be between 1 and 65535"))
		}
	default:
		errs = append(errs, validateAdapterURL(adapter.URL, path.Child("url"))...)
	}
	if adapter.TLS != nil {
		tlsPath := path.Child("tls")
		if adapter.TLS.ServerName != "" {
			for _, msg := range validation.IsDNS1123Subdomain(adapter.TLS.ServerName) {
				errs = append(errs, field.Invalid(tlsPath.Child("serverName"), adapter.TLS.ServerName, msg))
			}
		}
		if adapter.TLS.CASecretRef != nil {
			errs = append(errs, validateSecretKeyRef(adapter.TLS.CASecretRef, tlsPath.Child("caSecretRef"))...)
		}
	}
	authPath := path.Child("clientAuth", "bearerTokenSecretRef")
	if adapter.ClientAuth == nil || adapter.ClientAuth.BearerTokenSecretRef == nil {
		errs = append(errs, field.Required(authPath, "adapters must authenticate Orka with a bearer token"))
	} else {
		errs = append(errs, validateSecretKeyRef(adapter.ClientAuth.BearerTokenSecretRef, authPath)...)
	}
	return errs
}

// validateAdapterURL never echoes the URL, which could carry credentials.
func validateAdapterURL(raw string, path *field.Path) field.ErrorList {
	const detail = "must be one https:// URL without whitespace, credentials, query or fragment"
	if !adapterURLPattern.MatchString(raw) {
		return field.ErrorList{field.Invalid(path, field.OmitValueType{}, detail)}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return field.ErrorList{field.Invalid(path, field.OmitValueType{}, detail)}
	}
	return nil
}

func validateSecretKeyRef(ref *SecretKeyRef, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if ref.Name == "" {
		errs = append(errs, field.Required(path.Child("name"), "Secret name in the installation namespace"))
	} else {
		for _, msg := range validation.IsDNS1123Subdomain(ref.Name) {
			errs = append(errs, field.Invalid(path.Child("name"), ref.Name, msg))
		}
	}
	if ref.Key == "" {
		errs = append(errs, field.Required(path.Child("key"), "Secret data key"))
	} else {
		for _, msg := range validation.IsConfigMapKey(ref.Key) {
			errs = append(errs, field.Invalid(path.Child("key"), ref.Key, msg))
		}
	}
	return errs
}

func cloneAdapter(adapter *AdapterSource) *AdapterSource {
	clone := *adapter
	if adapter.ServiceRef != nil {
		ref := *adapter.ServiceRef
		clone.ServiceRef = &ref
	}
	// An empty tls block means the same as none, so it must not change the
	// digest.
	clone.TLS = nil
	if adapter.TLS != nil && (adapter.TLS.ServerName != "" || adapter.TLS.CASecretRef != nil) {
		tls := *adapter.TLS
		tls.CASecretRef = cloneSecretKeyRef(adapter.TLS.CASecretRef)
		clone.TLS = &tls
	}
	if adapter.ClientAuth != nil {
		clone.ClientAuth = &ClientAuth{BearerTokenSecretRef: cloneSecretKeyRef(adapter.ClientAuth.BearerTokenSecretRef)}
	}
	return &clone
}

func cloneSecretKeyRef(ref *SecretKeyRef) *SecretKeyRef {
	if ref == nil {
		return nil
	}
	clone := *ref
	return &clone
}
