package service

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/publisher"
)

const (
	EnvListenAddress                  = "ORKA_PUBLISHER_LISTEN_ADDRESS"
	EnvControllerTokenFile            = "ORKA_PUBLISHER_CONTROLLER_TOKEN_FILE"
	EnvOperationCapabilitySecretFile  = "ORKA_PUBLISHER_OPERATION_CAPABILITY_SECRET_FILE"
	EnvArtifactCapabilitySecretFile   = "ORKA_PUBLISHER_ARTIFACT_CAPABILITY_SECRET_FILE"
	EnvArtifactAuthorizationBrokerURL = "ORKA_PUBLISHER_ARTIFACT_AUTHORIZATION_BROKER_URL"
	EnvArtifactAPIURL                 = "ORKA_PUBLISHER_ARTIFACT_API_URL"
	EnvArtifactRoot                   = "ORKA_PUBLISHER_ARTIFACT_ROOT"
	EnvJournalRoot                    = "ORKA_PUBLISHER_JOURNAL_ROOT"
	EnvTempRoot                       = "ORKA_PUBLISHER_TEMP_ROOT"
	EnvCredentialRoot                 = "ORKA_PUBLISHER_CREDENTIAL_ROOT"
	EnvCredentialBrokerURL            = "ORKA_PUBLISHER_CREDENTIAL_BROKER_URL"
	EnvDefaultGitCredentialName       = "ORKA_PUBLISHER_DEFAULT_GIT_CREDENTIAL_NAME"
	EnvGitBinary                      = "ORKA_PUBLISHER_GIT_BINARY"
	EnvRequiredGitVersion             = "ORKA_PUBLISHER_REQUIRED_GIT_VERSION"
	EnvAllowedSCMHosts                = "ORKA_PUBLISHER_ALLOWED_SCM_HOSTS"
	EnvAllowFileRepositories          = "ORKA_PUBLISHER_ALLOW_FILE_REPOSITORIES"
	EnvMaxConcurrentOperations        = "ORKA_PUBLISHER_MAX_CONCURRENT_OPERATIONS"
	EnvMaxRequestBytes                = "ORKA_PUBLISHER_MAX_REQUEST_BYTES"
	EnvMaxResponseBytes               = "ORKA_PUBLISHER_MAX_RESPONSE_BYTES"
	EnvMaxJournalBytes                = "ORKA_PUBLISHER_MAX_JOURNAL_BYTES"
	EnvMaxDeltaBytes                  = "ORKA_PUBLISHER_MAX_DELTA_BYTES"
	EnvMaxBundleBytes                 = "ORKA_PUBLISHER_MAX_BUNDLE_BYTES"
	EnvMaxCommandOutput               = "ORKA_PUBLISHER_MAX_COMMAND_OUTPUT_BYTES"
	EnvPublishTimeout                 = "ORKA_PUBLISHER_PUBLISH_TIMEOUT"
	EnvArtifactTimeout                = "ORKA_PUBLISHER_ARTIFACT_TIMEOUT"
	EnvCapabilityTTL                  = "ORKA_PUBLISHER_CAPABILITY_TTL"
	EnvWorkspaceMaxEntries            = "ORKA_PUBLISHER_WORKSPACE_MAX_ENTRIES"
	EnvWorkspaceMaxFileBytes          = "ORKA_PUBLISHER_WORKSPACE_MAX_FILE_BYTES"
	EnvWorkspaceMaxBytes              = "ORKA_PUBLISHER_WORKSPACE_MAX_BYTES"
	EnvWorkspaceMaxArtifactBytes      = "ORKA_PUBLISHER_WORKSPACE_MAX_ARTIFACT_BYTES"
	EnvWorkspaceMaxPathBytes          = "ORKA_PUBLISHER_WORKSPACE_MAX_PATH_BYTES"
	EnvGitHubPREnabled                = "ORKA_PUBLISHER_GITHUB_PR_ENABLED"
	EnvGitHubAPIBaseURL               = "ORKA_PUBLISHER_GITHUB_API_BASE_URL"
	EnvGitHubRequestTimeout           = "ORKA_PUBLISHER_GITHUB_REQUEST_TIMEOUT"
	EnvGitHubMaxResponseBytes         = "ORKA_PUBLISHER_GITHUB_MAX_RESPONSE_BYTES"
	EnvSCMEgressProxyRequired         = "ORKA_PUBLISHER_SCM_EGRESS_PROXY_REQUIRED"
	EnvAllowDevelopmentFallbacks      = "ORKA_PUBLISHER_ALLOW_DEVELOPMENT_FALLBACKS"
)

//nolint:gocyclo // Startup validation keeps every independent environment bound explicit.
func LoadConfigFromEnv() (Config, error) {
	controllerToken, err := readRequiredSecretEnv(EnvControllerTokenFile, 16)
	if err != nil {
		return Config{}, err
	}
	operationSecret, err := readRequiredSecretEnv(EnvOperationCapabilitySecretFile, MinSecretBytes)
	if err != nil {
		return Config{}, err
	}
	allowDevelopmentFallbacks, err := parseBoolEnv(EnvAllowDevelopmentFallbacks)
	if err != nil {
		return Config{}, err
	}
	artifactSecret, artifactBrokerURL, err := loadArtifactAuthorizationFromEnv(allowDevelopmentFallbacks)
	if err != nil {
		return Config{}, err
	}
	credentialRoot, credentialBrokerURL, err := loadCredentialDeliveryFromEnv(allowDevelopmentFallbacks)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		ListenAddress:                  envDefault(EnvListenAddress, defaultListenAddress),
		ControllerBearerToken:          controllerToken,
		OperationCapabilitySecret:      operationSecret,
		ArtifactCapabilitySecret:       artifactSecret,
		ArtifactAuthorizationBrokerURL: artifactBrokerURL,
		ArtifactAPIURL:                 os.Getenv(EnvArtifactAPIURL),
		ArtifactRoot:                   envDefault(EnvArtifactRoot, "/data/publications"),
		JournalRoot:                    envDefault(EnvJournalRoot, "/data/service"),
		TempRoot:                       envDefault(EnvTempRoot, "/tmp/orka-workspace-publisher"),
		CredentialRoot:                 credentialRoot,
		CredentialBrokerURL:            credentialBrokerURL,
		GitBinary:                      envDefault(EnvGitBinary, "/usr/local/bin/git"),
		RequiredGitVersion:             envDefault(EnvRequiredGitVersion, "2.55.0"),
		AllowedSCMHosts:                splitCSV(os.Getenv(EnvAllowedSCMHosts)),
	}
	if config.ArtifactAPIURL == "" {
		return Config{}, fmt.Errorf("%s is required", EnvArtifactAPIURL)
	}
	if name := os.Getenv(EnvDefaultGitCredentialName); name != "" {
		config.DefaultGitCredential = &CredentialReference{Name: name, Kind: CredentialHTTPExtraHeader}
	}
	if config.AllowFileRepositories, err = parseBoolEnv(EnvAllowFileRepositories); err != nil {
		return Config{}, err
	}
	if config.ProxyEnvironment, err = loadSCMEgressProxyFromEnv(allowDevelopmentFallbacks); err != nil {
		return Config{}, err
	}
	if config.MaxConcurrentOperations, err = parseIntEnv(EnvMaxConcurrentOperations, defaultMaxConcurrentOperations); err != nil {
		return Config{}, err
	}
	if config.MaxRequestBytes, err = parseInt64Env(EnvMaxRequestBytes, defaultMaxRequestBytes); err != nil {
		return Config{}, err
	}
	if config.MaxResponseBytes, err = parseInt64Env(EnvMaxResponseBytes, defaultMaxResponseBytes); err != nil {
		return Config{}, err
	}
	if config.MaxJournalBytes, err = parseInt64Env(EnvMaxJournalBytes, defaultMaxJournalBytes); err != nil {
		return Config{}, err
	}
	if config.MaxDeltaBytes, err = parseInt64Env(EnvMaxDeltaBytes, defaultMaxDeltaBytes); err != nil {
		return Config{}, err
	}
	if config.MaxBundleBytes, err = parseInt64Env(EnvMaxBundleBytes, defaultMaxBundleBytes); err != nil {
		return Config{}, err
	}
	if config.MaxCommandOutput, err = parseInt64Env(EnvMaxCommandOutput, defaultMaxCommandOutput); err != nil {
		return Config{}, err
	}
	if config.PublishTimeout, err = parseDurationEnv(EnvPublishTimeout, defaultPublishTimeout); err != nil {
		return Config{}, err
	}
	if config.ArtifactTimeout, err = parseDurationEnv(EnvArtifactTimeout, defaultArtifactTimeout); err != nil {
		return Config{}, err
	}
	if config.CapabilityTTL, err = parseDurationEnv(EnvCapabilityTTL, defaultCapabilityTTL); err != nil {
		return Config{}, err
	}
	defaults := defaultWorkspaceLimits()
	if config.WorkspaceLimits.MaxEntries, err = parseIntEnv(EnvWorkspaceMaxEntries, defaults.MaxEntries); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxFileBytes, err = parseInt64Env(EnvWorkspaceMaxFileBytes, defaults.MaxFileBytes); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxExpandedBytes, err = parseInt64Env(EnvWorkspaceMaxBytes, defaults.MaxExpandedBytes); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxArtifactBytes, err = parseInt64Env(EnvWorkspaceMaxArtifactBytes, defaults.MaxArtifactBytes); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxPathBytes, err = parseIntEnv(EnvWorkspaceMaxPathBytes, defaults.MaxPathBytes); err != nil {
		return Config{}, err
	}
	githubEnabled, err := parseBoolEnv(EnvGitHubPREnabled)
	if err != nil {
		return Config{}, err
	}
	if githubEnabled {
		requestTimeout, timeoutErr := parseDurationEnv(EnvGitHubRequestTimeout, defaultGitHubRequestTimeout)
		if timeoutErr != nil {
			return Config{}, timeoutErr
		}
		maxResponseBytes, limitErr := parseInt64Env(EnvGitHubMaxResponseBytes, defaultGitHubMaxResponseBytes)
		if limitErr != nil {
			return Config{}, limitErr
		}
		factory, factoryErr := NewGitHubPRReconcilerFactory(GitHubPRReconcilerFactoryConfig{
			APIBaseURL: envDefault(EnvGitHubAPIBaseURL, defaultGitHubAPIBaseURL), RequestTimeout: requestTimeout, MaxResponseBytes: maxResponseBytes,
		})
		if factoryErr != nil {
			return Config{}, fmt.Errorf("GitHub pull request reconciliation configuration is invalid: %w", factoryErr)
		}
		config.PRFactory = factory
	} else {
		for _, name := range []string{EnvGitHubAPIBaseURL, EnvGitHubRequestTimeout, EnvGitHubMaxResponseBytes} {
			if strings.TrimSpace(os.Getenv(name)) != "" {
				return Config{}, fmt.Errorf("%s requires %s=true", name, EnvGitHubPREnabled)
			}
		}
	}
	return normalizeConfig(config)
}

func loadSCMEgressProxyFromEnv(allowDevelopmentFallbacks bool) (publisher.ProxyEnvironment, error) {
	required, err := parseBoolEnv(EnvSCMEgressProxyRequired)
	if err != nil {
		return publisher.ProxyEnvironment{}, err
	}
	if !required {
		if allowDevelopmentFallbacks {
			return publisher.ProxyEnvironment{}, nil
		}
		return publisher.ProxyEnvironment{}, fmt.Errorf(
			"%s must be true unless %s=true",
			EnvSCMEgressProxyRequired,
			EnvAllowDevelopmentFallbacks,
		)
	}
	environment, err := publisher.NormalizeProxyEnvironment(os.Getenv("HTTPS_PROXY"), os.Getenv("NO_PROXY"))
	if err != nil || environment.HTTPSProxy == "" {
		return publisher.ProxyEnvironment{}, fmt.Errorf("SCM egress proxy configuration is invalid")
	}
	return environment, nil
}

func loadArtifactAuthorizationFromEnv(allowDevelopmentFallbacks bool) ([]byte, string, error) {
	brokerURL := strings.TrimSpace(os.Getenv(EnvArtifactAuthorizationBrokerURL))
	secretFile := strings.TrimSpace(os.Getenv(EnvArtifactCapabilitySecretFile))
	if brokerURL != "" {
		if secretFile != "" {
			return nil, "", fmt.Errorf(
				"%s and %s are mutually exclusive",
				EnvArtifactAuthorizationBrokerURL,
				EnvArtifactCapabilitySecretFile,
			)
		}
		return nil, brokerURL, nil
	}
	if !allowDevelopmentFallbacks {
		return nil, "", fmt.Errorf(
			"%s is required unless %s=true",
			EnvArtifactAuthorizationBrokerURL,
			EnvAllowDevelopmentFallbacks,
		)
	}
	secret, err := readRequiredSecretEnv(EnvArtifactCapabilitySecretFile, MinSecretBytes)
	return secret, "", err
}

func loadCredentialDeliveryFromEnv(allowDevelopmentFallbacks bool) (string, string, error) {
	brokerURL := strings.TrimSpace(os.Getenv(EnvCredentialBrokerURL))
	credentialRoot := strings.TrimSpace(os.Getenv(EnvCredentialRoot))
	if brokerURL != "" {
		if credentialRoot != "" {
			return "", "", fmt.Errorf("%s and %s are mutually exclusive", EnvCredentialBrokerURL, EnvCredentialRoot)
		}
		return "", brokerURL, nil
	}
	if !allowDevelopmentFallbacks {
		return "", "", fmt.Errorf(
			"%s is required unless %s=true",
			EnvCredentialBrokerURL,
			EnvAllowDevelopmentFallbacks,
		)
	}
	return envDefault(EnvCredentialRoot, "/var/run/secrets/orka-publisher/credentials"), "", nil
}

func readRequiredSecretEnv(name string, minimum int) ([]byte, error) {
	path := os.Getenv(name)
	if path == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	value, err := readSecretFile(path, minimum)
	if err != nil {
		return nil, fmt.Errorf("%s is invalid: %w", name, err)
	}
	return value, nil
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func parseIntEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(value) != raw {
		return 0, fmt.Errorf("%s must be a canonical platform-sized integer", name)
	}
	return value, nil
}

func parseInt64Env(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != raw {
		return 0, fmt.Errorf("%s must be a canonical integer", name)
	}
	return value, nil
}

func parseBoolEnv(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}

func parseDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration", name)
	}
	return value, nil
}
