package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/publisher"
)

const (
	defaultListenAddress           = ":8080"
	defaultMaxConcurrentOperations = 4
	defaultMaxRequestBytes         = int64(2 << 20)
	defaultMaxResponseBytes        = int64(2 << 20)
	defaultMaxJournalBytes         = int64(1 << 30)
	defaultMaxDeltaBytes           = int64(256 << 20)
	defaultMaxBundleBytes          = int64(512 << 20)
	defaultMaxCommandOutput        = int64(1 << 20)
	defaultArtifactTimeout         = 2 * time.Minute
	defaultPublishTimeout          = 2 * time.Minute
	defaultCapabilityTTL           = time.Minute
)

//nolint:gocyclo // Startup validation keeps every independent bound explicit.
func normalizeConfig(config Config) (Config, error) {
	if config.ListenAddress == "" {
		config.ListenAddress = defaultListenAddress
	}
	if len(config.ControllerBearerToken) < 16 {
		return Config{}, fmt.Errorf("controller bearer token must be at least 16 bytes")
	}
	if len(config.OperationCapabilitySecret) < MinSecretBytes {
		return Config{}, fmt.Errorf("operation capability secret must be at least %d bytes", MinSecretBytes)
	}
	hasArtifactSecret := len(config.ArtifactCapabilitySecret) >= MinSecretBytes
	hasArtifactBroker := strings.TrimSpace(config.ArtifactAuthorizationBrokerURL) != ""
	if hasArtifactSecret == hasArtifactBroker {
		return Config{}, fmt.Errorf("exactly one artifact authorization mode must be configured")
	}
	for name, value := range map[string]string{
		"artifact root": config.ArtifactRoot, "journal root": config.JournalRoot,
		"temporary root": config.TempRoot,
	} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return Config{}, fmt.Errorf("%s must be an absolute clean path", name)
		}
	}
	if config.CredentialRoot != "" && (!filepath.IsAbs(config.CredentialRoot) || filepath.Clean(config.CredentialRoot) != config.CredentialRoot) {
		return Config{}, fmt.Errorf("credential root must be an absolute clean path")
	}
	hasCredentialRoot := strings.TrimSpace(config.CredentialRoot) != ""
	hasCredentialBroker := strings.TrimSpace(config.CredentialBrokerURL) != ""
	if hasCredentialRoot && hasCredentialBroker {
		return Config{}, fmt.Errorf("credential root and broker are mutually exclusive")
	}
	if config.DefaultGitCredential != nil {
		if err := validateCredentialReference(config.DefaultGitCredential, CredentialHTTPExtraHeader); err != nil {
			return Config{}, err
		}
		if !hasCredentialRoot && !hasCredentialBroker {
			return Config{}, fmt.Errorf("default credential requires a credential delivery mode")
		}
	}
	allowed, err := normalizeAllowedHosts(config.AllowedSCMHosts)
	if err != nil {
		return Config{}, err
	}
	config.AllowedSCMHosts = allowed
	proxyEnvironment, err := publisher.NormalizeProxyEnvironment(
		config.ProxyEnvironment.HTTPSProxy,
		config.ProxyEnvironment.NoProxy,
	)
	if err != nil {
		return Config{}, err
	}
	config.ProxyEnvironment = proxyEnvironment
	if config.MaxConcurrentOperations == 0 {
		config.MaxConcurrentOperations = defaultMaxConcurrentOperations
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = defaultMaxRequestBytes
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultMaxResponseBytes
	}
	if config.MaxJournalBytes == 0 {
		config.MaxJournalBytes = defaultMaxJournalBytes
	}
	if config.MaxDeltaBytes == 0 {
		config.MaxDeltaBytes = defaultMaxDeltaBytes
	}
	if config.MaxBundleBytes == 0 {
		config.MaxBundleBytes = defaultMaxBundleBytes
	}
	if config.MaxCommandOutput == 0 {
		config.MaxCommandOutput = defaultMaxCommandOutput
	}
	if config.PublishTimeout == 0 {
		config.PublishTimeout = defaultPublishTimeout
	}
	if config.ArtifactTimeout == 0 {
		config.ArtifactTimeout = defaultArtifactTimeout
	}
	if config.CapabilityTTL == 0 {
		config.CapabilityTTL = defaultCapabilityTTL
	}
	if config.VerifyAttempts == 0 {
		config.VerifyAttempts = 3
	}
	if config.VerifyBackoff == 0 {
		config.VerifyBackoff = 100 * time.Millisecond
	}
	if config.WorkspaceLimits == (WorkspaceLimits{}) {
		config.WorkspaceLimits = defaultWorkspaceLimits()
	}
	if err := config.WorkspaceLimits.normalize(defaultWorkspaceLimits()); err != nil {
		return Config{}, err
	}
	if config.MaxConcurrentOperations < 1 || config.MaxConcurrentOperations > 64 || config.MaxRequestBytes < 1 ||
		config.MaxResponseBytes < 1 || config.MaxJournalBytes < 1 || config.MaxDeltaBytes < 1 || config.MaxBundleBytes < 1 ||
		config.MaxCommandOutput < 1 || config.PublishTimeout <= 0 || config.ArtifactTimeout <= 0 ||
		config.CapabilityTTL <= 0 || config.CapabilityTTL > MaxCapabilityTTL || config.VerifyAttempts < 1 || config.VerifyAttempts > 20 ||
		config.VerifyBackoff < 0 {
		return Config{}, fmt.Errorf("service limits are invalid")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return config, nil
}

func (limits *WorkspaceLimits) normalize(defaults WorkspaceLimits) error {
	if limits.MaxEntries == 0 {
		limits.MaxEntries = defaults.MaxEntries
	}
	if limits.MaxFileBytes == 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxExpandedBytes == 0 {
		limits.MaxExpandedBytes = defaults.MaxExpandedBytes
	}
	if limits.MaxArtifactBytes == 0 {
		limits.MaxArtifactBytes = defaults.MaxArtifactBytes
	}
	if limits.MaxPathBytes == 0 {
		limits.MaxPathBytes = defaults.MaxPathBytes
	}
	if limits.MaxEntries < 1 || limits.MaxFileBytes < 1 || limits.MaxExpandedBytes < 1 ||
		limits.MaxArtifactBytes < 1 || limits.MaxPathBytes < 1 || limits.MaxFileBytes > limits.MaxExpandedBytes {
		return fmt.Errorf("workspace limits are invalid")
	}
	return nil
}

func mergeWorkspaceLimits(request, defaults WorkspaceLimits) (WorkspaceLimits, error) {
	result := request
	if result.MaxEntries == 0 {
		result.MaxEntries = defaults.MaxEntries
	}
	if result.MaxFileBytes == 0 {
		result.MaxFileBytes = defaults.MaxFileBytes
	}
	if result.MaxExpandedBytes == 0 {
		result.MaxExpandedBytes = defaults.MaxExpandedBytes
	}
	if result.MaxArtifactBytes == 0 {
		result.MaxArtifactBytes = defaults.MaxArtifactBytes
	}
	if result.MaxPathBytes == 0 {
		result.MaxPathBytes = defaults.MaxPathBytes
	}
	if err := result.normalize(defaults); err != nil {
		return WorkspaceLimits{}, err
	}
	if result.MaxEntries > defaults.MaxEntries || result.MaxFileBytes > defaults.MaxFileBytes ||
		result.MaxExpandedBytes > defaults.MaxExpandedBytes || result.MaxArtifactBytes > defaults.MaxArtifactBytes ||
		result.MaxPathBytes > defaults.MaxPathBytes {
		return WorkspaceLimits{}, invalidRequest("workspace request exceeds service limits", nil)
	}
	return result, nil
}

func readSecretFile(path string, minBytes int) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("secret path must be absolute and clean")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, fmt.Errorf("secret file is missing or unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	data = bytes.TrimSuffix(data, []byte("\r\n"))
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(data) < minBytes || bytes.ContainsAny(data, "\r\n\x00") {
		return nil, fmt.Errorf("secret file contents are invalid")
	}
	return data, nil
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
