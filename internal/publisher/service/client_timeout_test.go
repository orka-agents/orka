package service

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestNewClientDefaultHasNoClientLevelTimeout(t *testing.T) {
	client, err := NewClient(ClientConfig{
		BaseURL:          "https://publisher.example.test",
		BearerToken:      bytes.Repeat([]byte("t"), 32),
		CapabilitySecret: bytes.Repeat([]byte("s"), MinSecretBytes),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.httpClient.Timeout != 0 {
		t.Fatalf("default client timeout = %s, want 0 so caller deadlines govern operation timeouts", client.httpClient.Timeout)
	}
}

func TestRequestContextHonorsCallerDeadline(t *testing.T) {
	want := time.Now().Add(10 * time.Minute)
	parent, cancelParent := context.WithDeadline(context.Background(), want)
	defer cancelParent()
	ctx, cancel := requestContext(parent)
	defer cancel()
	got, ok := ctx.Deadline()
	if !ok || !got.Equal(want) {
		t.Fatalf("deadline = %v (ok=%t), want caller deadline %v preserved beyond the former client ceiling", got, ok, want)
	}
}

func TestRequestContextAppliesDefaultDeadlineWhenAbsent(t *testing.T) {
	ctx, cancel := requestContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a fallback deadline for contexts without one")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > defaultRequestTimeout+time.Second {
		t.Fatalf("fallback deadline in %s, want about %s", remaining, defaultRequestTimeout)
	}
}
