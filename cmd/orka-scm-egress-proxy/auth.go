/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type proxyAuthenticator struct {
	tokenDigest [sha256.Size]byte
}

func loadProxyAuthenticator(path string) (*proxyAuthenticator, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("proxy token path must be absolute and clean")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 ||
		info.Size() < minProxyTokenBytes || info.Size() > maxProxyTokenBytes+2 {
		return nil, fmt.Errorf("proxy token file is missing or unsafe")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read proxy token file: %w", err)
	}
	value = bytes.TrimSuffix(value, []byte("\r\n"))
	value = bytes.TrimSuffix(value, []byte("\n"))
	return newProxyAuthenticator(value)
}

func ensureKubernetesServiceAccountTokenAbsent(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("kubernetes service-account token path must be absolute and clean")
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("kubernetes service-account token is mounted")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect Kubernetes service-account token path: %w", err)
	}
	return nil
}

func newProxyAuthenticator(token []byte) (*proxyAuthenticator, error) {
	if err := validateProxyToken(token); err != nil {
		return nil, err
	}
	return &proxyAuthenticator{tokenDigest: sha256.Sum256(token)}, nil
}

func validateProxyToken(token []byte) error {
	if len(token) < minProxyTokenBytes || len(token) > maxProxyTokenBytes {
		return fmt.Errorf("proxy token length is invalid")
	}
	for _, current := range token {
		if !isProxyTokenCharacter(current) {
			return fmt.Errorf("proxy token contains an unsupported character")
		}
	}
	return nil
}

func isProxyTokenCharacter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || strings.ContainsRune("-._~", rune(value))
}

func (a *proxyAuthenticator) authorized(request *http.Request) bool {
	if a == nil {
		return false
	}
	values := request.Header.Values("Proxy-Authorization")
	if len(values) != 1 {
		return false
	}
	const prefix = "Basic "
	if !strings.HasPrefix(values[0], prefix) || len(values[0]) > 1024 {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(values[0], prefix))
	if err != nil || bytes.ContainsAny(decoded, "\r\n\x00") {
		return false
	}
	username, token, found := bytes.Cut(decoded, []byte{':'})
	if !found || string(username) != proxyUsername {
		return false
	}
	digest := sha256.Sum256(token)
	return subtle.ConstantTimeCompare(digest[:], a.tokenDigest[:]) == 1
}
