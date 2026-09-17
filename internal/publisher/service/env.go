package service

import (
	"fmt"
	"os"
	"strings"

	"github.com/orka-agents/orka/internal/envutil"
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
	allowDevelopmentFallbacks, err := envutil.Bool(EnvAllowDevelopmentFallbacks)
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
		ListenAddress:                  envutil.String(EnvListenAddress, defaultListenAddress),
		ControllerBearerToken:          controllerToken,
		OperationCapabilitySecret:      operationSecret,
		ArtifactCapabilitySecret:       artifactSecret,
		ArtifactAuthorizationBrokerURL: artifactBrokerURL,
		ArtifactAPIURL:                 os.Getenv(EnvArtifactAPIURL),
		ArtifactRoot:                   envutil.String(EnvArtifactRoot, "/data/publications"),
		JournalRoot:                    envutil.String(EnvJournalRoot, "/data/service"),
		TempRoot:                       envutil.String(EnvTempRoot, "/tmp/orka-workspace-publisher"),
		CredentialRoot:                 credentialRoot,
		CredentialBrokerURL:            credentialBrokerURL,
		GitBinary:                      envutil.String(EnvGitBinary, "/usr/local/bin/git"),
		RequiredGitVersion:             envutil.String(EnvRequiredGitVersion, "2.55.0"),
		AllowedSCMHosts:                splitCSV(os.Getenv(EnvAllowedSCMHosts)),
	}
	if config.ArtifactAPIURL == "" {
		return Config{}, fmt.Errorf("%s is required", EnvArtifactAPIURL)
	}
	if name := os.Getenv(EnvDefaultGitCredentialName); name != "" {
		config.DefaultGitCredential = &CredentialReference{Name: name, Kind: CredentialHTTPExtraHeader}
	}
	if config.AllowFileRepositories, err = envutil.Bool(EnvAllowFileRepositories); err != nil {
		return Config{}, err
	}
	if config.ProxyEnvironment, err = loadSCMEgressProxyFromEnv(allowDevelopmentFallbacks); err != nil {
		return Config{}, err
	}
	if config.MaxConcurrentOperations, err = envutil.Int(EnvMaxConcurrentOperations, defaultMaxConcurrentOperations); err != nil {
		return Config{}, err
	}
	if config.MaxRequestBytes, err = envutil.Int64(EnvMaxRequestBytes, defaultMaxRequestBytes); err != nil {
		return Config{}, err
	}
	if config.MaxResponseBytes, err = envutil.Int64(EnvMaxResponseBytes, defaultMaxResponseBytes); err != nil {
		return Config{}, err
	}
	if config.MaxJournalBytes, err = envutil.Int64(EnvMaxJournalBytes, defaultMaxJournalBytes); err != nil {
		return Config{}, err
	}
	if config.MaxDeltaBytes, err = envutil.Int64(EnvMaxDeltaBytes, defaultMaxDeltaBytes); err != nil {
		return Config{}, err
	}
	if config.MaxBundleBytes, err = envutil.Int64(EnvMaxBundleBytes, defaultMaxBundleBytes); err != nil {
		return Config{}, err
	}
	if config.MaxCommandOutput, err = envutil.Int64(EnvMaxCommandOutput, defaultMaxCommandOutput); err != nil {
		return Config{}, err
	}
	if config.PublishTimeout, err = envutil.Duration(EnvPublishTimeout, defaultPublishTimeout); err != nil {
		return Config{}, err
	}
	if config.ArtifactTimeout, err = envutil.Duration(EnvArtifactTimeout, defaultArtifactTimeout); err != nil {
		return Config{}, err
	}
	if config.CapabilityTTL, err = envutil.Duration(EnvCapabilityTTL, defaultCapabilityTTL); err != nil {
		return Config{}, err
	}
	defaults := defaultWorkspaceLimits()
	if config.WorkspaceLimits.MaxEntries, err = envutil.Int(EnvWorkspaceMaxEntries, defaults.MaxEntries); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxFileBytes, err = envutil.Int64(EnvWorkspaceMaxFileBytes, defaults.MaxFileBytes); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxExpandedBytes, err = envutil.Int64(EnvWorkspaceMaxBytes, defaults.MaxExpandedBytes); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxArtifactBytes, err = envutil.Int64(EnvWorkspaceMaxArtifactBytes, defaults.MaxArtifactBytes); err != nil {
		return Config{}, err
	}
	if config.WorkspaceLimits.MaxPathBytes, err = envutil.Int(EnvWorkspaceMaxPathBytes, defaults.MaxPathBytes); err != nil {
		return Config{}, err
	}
	githubEnabled, err := envutil.Bool(EnvGitHubPREnabled)
	if err != nil {
		return Config{}, err
	}
	if githubEnabled {
		requestTimeout, timeoutErr := envutil.Duration(EnvGitHubRequestTimeout, defaultGitHubRequestTimeout)
		if timeoutErr != nil {
			return Config{}, timeoutErr
		}
		maxResponseBytes, limitErr := envutil.Int64(EnvGitHubMaxResponseBytes, defaultGitHubMaxResponseBytes)
		if limitErr != nil {
			return Config{}, limitErr
		}
		factory, factoryErr := NewGitHubPRReconcilerFactory(GitHubPRReconcilerFactoryConfig{
			APIBaseURL: envutil.String(EnvGitHubAPIBaseURL, defaultGitHubAPIBaseURL), RequestTimeout: requestTimeout, MaxResponseBytes: maxResponseBytes,
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
	required, err := envutil.Bool(EnvSCMEgressProxyRequired)
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
	return envutil.String(EnvCredentialRoot, "/var/run/secrets/orka-publisher/credentials"), "", nil
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
