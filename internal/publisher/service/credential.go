package service

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/orka-agents/orka/internal/publisher"
)

type credentialManager struct {
	provider   credentialProvider
	tempRoot   string
	realGit    string
	defaultGit *CredentialReference
}

type operationCredential struct {
	gitBinary string
	filePath  string
	cleanup   func()
}

func (m *credentialManager) gitCredential(ctx context.Context, parent Operation, metadata OperationMetadata, repository publisher.Repository, reference *CredentialReference) (operationCredential, error) {
	if err := ctx.Err(); err != nil {
		return operationCredential{}, err
	}
	if reference == nil {
		reference = m.defaultGit
	}
	if reference == nil {
		return operationCredential{gitBinary: m.realGit, cleanup: func() {}}, nil
	}
	if err := validateCredentialReference(reference, CredentialHTTPExtraHeader); err != nil {
		return operationCredential{}, err
	}
	parsed, err := url.Parse(repository.URL)
	if err != nil || parsed.Scheme != "https" {
		return operationCredential{}, apiError(ErrCredential, "invalid_credential_ref", "Git credentials require an HTTPS repository", 400, false, nil)
	}
	if m.provider == nil {
		return operationCredential{}, apiError(ErrCredential, "credential_unavailable", "credential provider is not configured", 503, false, nil)
	}
	header, _, err := m.provider.Read(ctx, CredentialMaterialRequest{ParentOperation: parent, Metadata: metadata, Reference: *reference})
	if err != nil {
		return operationCredential{}, err
	}
	if !strings.HasPrefix(strings.ToLower(string(header)), "authorization:") {
		return operationCredential{}, apiError(ErrCredential, "invalid_credential_ref", "Git credential file must contain one Authorization header", 400, false, nil)
	}
	directory, err := os.MkdirTemp(m.tempRoot, "orka-publisher-credential-")
	if err != nil {
		return operationCredential{}, apiError(ErrCredential, "credential_unavailable", "operation credential could not be materialized", 503, true, err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	if err := os.Chmod(directory, 0o700); err != nil {
		cleanup()
		return operationCredential{}, err
	}
	configPath := filepath.Join(directory, "gitconfig")
	config := "[http]\n\tfollowRedirects = false\n\tsslVerify = true\n[http \"" + escapeGitConfig(repository.URL) + "\"]\n\textraHeader = \"" + escapeGitConfig(string(header)) + "\"\n"
	if err := writeDurable(configPath, []byte(config), 0o600); err != nil {
		cleanup()
		return operationCredential{}, apiError(ErrCredential, "credential_unavailable", "operation credential could not be materialized", 503, true, err)
	}
	wrapperPath := filepath.Join(directory, "git")
	script := "#!/bin/sh\nset -eu\nexport GIT_CONFIG_GLOBAL=" + shellQuote(configPath) + "\nexport GIT_CONFIG_NOSYSTEM=1\nexec " + shellQuote(m.realGit) + " \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0o700); err != nil {
		cleanup()
		return operationCredential{}, apiError(ErrCredential, "credential_unavailable", "operation credential could not be materialized", 503, true, err)
	}
	return operationCredential{gitBinary: wrapperPath, cleanup: cleanup}, nil
}

func (m *credentialManager) forgeCredential(ctx context.Context, parent Operation, metadata OperationMetadata, reference *CredentialReference) (operationCredential, error) {
	if err := ctx.Err(); err != nil {
		return operationCredential{}, err
	}
	if reference == nil {
		return operationCredential{cleanup: func() {}}, nil
	}
	if err := validateCredentialReference(reference, CredentialForgeToken); err != nil {
		return operationCredential{}, err
	}
	if m.provider == nil {
		return operationCredential{}, apiError(ErrCredential, "credential_unavailable", "credential provider is not configured", 503, false, nil)
	}
	value, _, err := m.provider.Read(ctx, CredentialMaterialRequest{ParentOperation: parent, Metadata: metadata, Reference: *reference})
	if err != nil {
		return operationCredential{}, err
	}
	trimmed := strings.TrimSpace(string(value))
	if strings.HasPrefix(strings.ToLower(trimmed), "authorization:") {
		parts := strings.Fields(strings.TrimSpace(trimmed[len("Authorization:"):]))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || strings.TrimSpace(parts[1]) == "" {
			return operationCredential{}, apiError(ErrCredential, "invalid_credential_ref", "forge credential Authorization header must use one Bearer token", 400, false, nil)
		}
		value = []byte(parts[1])
	}
	directory, err := os.MkdirTemp(m.tempRoot, "orka-publisher-forge-credential-")
	if err != nil {
		return operationCredential{}, apiError(ErrCredential, "credential_unavailable", "operation credential could not be materialized", 503, true, err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	if err := os.Chmod(directory, 0o700); err != nil {
		cleanup()
		return operationCredential{}, err
	}
	path := filepath.Join(directory, "credential")
	if err := writeDurable(path, value, 0o600); err != nil {
		cleanup()
		return operationCredential{}, apiError(ErrCredential, "credential_unavailable", "operation credential could not be materialized", 503, true, err)
	}
	return operationCredential{filePath: path, cleanup: cleanup}, nil
}

func readCredentialFile(root, name string, limit int64) ([]byte, error) {
	if root == "" {
		return nil, apiError(ErrCredential, "credential_unavailable", "credential storage is not configured", 503, false, nil)
	}
	if !credentialNamePattern.MatchString(name) {
		return nil, apiError(ErrCredential, "invalid_credential_ref", "credential reference name is invalid", 400, false, nil)
	}
	path := filepath.Join(root, name)
	if filepath.Dir(path) != root {
		return nil, apiError(ErrCredential, "invalid_credential_ref", "credential reference escapes the configured root", 400, false, nil)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit || info.Mode().Perm()&0o077 != 0 {
		return nil, apiError(ErrCredential, "credential_unavailable", "credential file is missing or unsafe", 503, false, err)
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, apiError(ErrCredential, "credential_unavailable", "credential file could not be read", 503, true, err)
	}
	value = bytes.TrimSuffix(value, []byte("\r\n"))
	value = bytes.TrimSuffix(value, []byte("\n"))
	if len(value) == 0 || bytes.ContainsAny(value, "\r\n\x00") {
		return nil, apiError(ErrCredential, "invalid_credential_ref", "credential file must contain one non-empty line", 400, false, nil)
	}
	return value, nil
}

func escapeGitConfig(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"")
	return replacer.Replace(value)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func ensureGitBinary(path string) (string, error) {
	if path == "" {
		for _, candidate := range []string{"/usr/local/bin/git", "/opt/homebrew/bin/git", "/usr/bin/git", "/bin/git"} {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("no reviewed Git binary was found")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("git binary path must be absolute and clean")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("git binary is not an executable regular file")
	}
	return path, nil
}
