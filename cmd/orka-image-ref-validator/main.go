package main

import (
	_ "crypto/sha256"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	distributionref "github.com/distribution/reference"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "at least one container image reference is required")
		os.Exit(2)
	}
	for _, value := range os.Args[1:] {
		if err := validateImageReference(value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func validateImageReference(value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("container image reference contains a newline")
	}
	named, err := distributionref.ParseNormalizedNamed(value)
	if err != nil {
		return fmt.Errorf("invalid container image reference %q: %w", value, err)
	}
	if err := validateRegistryTransport(distributionref.Domain(named)); err != nil {
		return fmt.Errorf("invalid container image reference %q: %w", value, err)
	}
	digested, ok := named.(distributionref.Digested)
	if !ok {
		return fmt.Errorf("container image reference %q is not digest pinned", value)
	}
	digest := digested.Digest().String()
	if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("container image reference %q must use a sha256 digest", value)
	}
	return nil
}

func validateRegistryTransport(domain string) error {
	port := ""
	if after, ok := strings.CutPrefix(domain, "["); ok {
		var host string
		if strings.Contains(domain, "]:") {
			var err error
			host, port, err = net.SplitHostPort(domain)
			if err != nil {
				return fmt.Errorf("invalid bracketed registry host: %w", err)
			}
		} else {
			if !strings.HasSuffix(domain, "]") {
				return fmt.Errorf("invalid bracketed registry host")
			}
			host = strings.TrimSuffix(after, "]")
		}
		ip := net.ParseIP(host)
		if ip == nil || !strings.Contains(host, ":") {
			return fmt.Errorf("bracketed registry host is not a valid IPv6 address")
		}
	} else if strings.Contains(domain, ":") {
		var err error
		_, port, err = net.SplitHostPort(domain)
		if err != nil {
			return fmt.Errorf("invalid registry host or port: %w", err)
		}
	}
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return fmt.Errorf("registry port must be between 1 and 65535")
		}
	}
	return nil
}
