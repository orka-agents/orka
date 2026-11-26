/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// ahpDefaultBranchHost is the only host Orka asks for a repository's
	// default branch. The namespace's repository credential is never sent
	// anywhere else, and no other address is reached for a user.
	ahpDefaultBranchHost    = "github.com"
	ahpDefaultBranchBase    = "https://" + ahpDefaultBranchHost + "/"
	ahpDefaultBranchTimeout = 10 * time.Second
	ahpRefAdvertisementMax  = 64 << 10
)

var ahpGitHTTPClient = &http.Client{
	Timeout: ahpDefaultBranchTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != via[0].URL.Host || req.URL.Scheme != connectorSchemeHTTPS {
			return errors.New("redirect to another host refused")
		}
		return nil
	},
}

// ahpOnGitHub reports whether a repository URL names a github.com repository,
// the only host Orka sends a namespace's repository credential to.
func ahpOnGitHub(repo string) bool {
	parsed, err := url.Parse(repo)
	return err == nil && parsed.Scheme == connectorSchemeHTTPS && parsed.Host == ahpDefaultBranchHost && parsed.User == nil
}

// defaultBranch reads a repository's default branch from git's smart HTTP
// ref advertisement, without cloning. Any failure leaves it unknown.
func (b *ahpBackend) defaultBranch(ctx context.Context, namespace, repo string) string {
	parsed, err := url.Parse(repo)
	if err != nil || parsed.Scheme != connectorSchemeHTTPS || parsed.Host != ahpDefaultBranchHost ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, ahpDefaultBranchTimeout)
	defer cancel()
	// The request goes to the fixed github.com base; only the repository
	// path comes from the project.
	target := ahpDefaultBranchBase + strings.TrimPrefix(parsed.EscapedPath(), "/") + ".git/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ""
	}
	if token := b.gitToken(ctx, namespace); token != "" {
		req.SetBasicAuth("x-access-token", token)
	}
	resp, err := b.gitHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	branch, err := ahpAdvertisedHead(io.LimitReader(resp.Body, ahpRefAdvertisementMax))
	if err != nil {
		return ""
	}
	return branch
}

// gitToken returns the namespace's repository credential, if configured.
func (b *ahpBackend) gitToken(ctx context.Context, namespace string) string {
	return strings.TrimSpace(string(b.gitSecretToken(ctx, namespace)))
}

// ahpAdvertisedHead finds the branch HEAD points at in a smart HTTP ref
// advertisement: the symref capability on the first ref line.
func ahpAdvertisedHead(body io.Reader) (string, error) {
	reader := bufio.NewReader(body)
	for range 4 {
		line, flush, err := ahpReadPktLine(reader)
		if err != nil {
			return "", err
		}
		if flush || strings.HasPrefix(line, "# service=") {
			continue
		}
		_, capabilities, found := strings.Cut(line, "\x00")
		if !found {
			return "", errors.New("the first ref carries no capabilities")
		}
		for capability := range strings.FieldsSeq(capabilities) {
			if target, ok := strings.CutPrefix(capability, "symref=HEAD:refs/heads/"); ok && target != "" {
				return target, nil
			}
		}
		return "", errors.New("HEAD is not a branch")
	}
	return "", errors.New("no ref advertisement")
}

func ahpReadPktLine(reader *bufio.Reader) (string, bool, error) {
	var size [4]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return "", false, err
	}
	n, err := strconv.ParseUint(string(size[:]), 16, 16)
	if err != nil {
		return "", false, fmt.Errorf("invalid pkt-line length")
	}
	if n == 0 {
		return "", true, nil
	}
	if n < 4 {
		return "", false, fmt.Errorf("invalid pkt-line length")
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(string(payload), "\n"), false, nil
}
