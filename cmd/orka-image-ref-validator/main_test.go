package main

import (
	"strings"
	"testing"
)

func TestValidateImageReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, value := range []string{
		"docker.io/example/acp@" + digest,
		"registry--prod.example.com:5000/team/acp:release@" + digest,
		"[2001:db8::1]:5000/team/acp@" + digest,
		"acp@" + digest,
	} {
		if err := validateImageReference(value); err != nil {
			t.Errorf("validateImageReference(%q) error = %v", value, err)
		}
	}

	longPath := "docker.io/" + strings.Repeat("a", 256) + "@" + digest
	for _, value := range []string{
		"not-digest-pinned",
		"https://registry.example.com/team/acp@" + digest,
		"registry.example.com:notaport/team/acp@" + digest,
		"registry.example.com:70000/team/acp@" + digest,
		"[127.0.0.1]/team/acp@" + digest,
		"[:::]/team/acp@" + digest,
		"docker.io/team/@" + digest,
		"docker.io/example/acp\n#@" + digest,
		longPath,
	} {
		if err := validateImageReference(value); err == nil {
			t.Errorf("validateImageReference(%q) error = nil, want rejection", value)
		}
	}
}
