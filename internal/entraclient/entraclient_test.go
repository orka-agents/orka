// Copyright (c) 2026. MIT License - see LICENSE file for details.

package entraclient

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCacheIsBounded(t *testing.T) {
	now := time.Now()
	cache := &Cache{Now: func() time.Time { return now }}
	fetch := func(context.Context) (string, time.Duration, error) { return "t", time.Hour, nil }
	for i := range 10 {
		if _, err := cache.Get(context.Background(), fmt.Sprint("expiring-", i), fetch); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(MaxCachedTokenLifetime)
	if _, err := cache.Get(context.Background(), "fresh", fetch); err != nil {
		t.Fatal(err)
	}
	if cache.Len() != 1 {
		t.Fatalf("cache holds %d entries, want expired entries reclaimed", cache.Len())
	}
	for i := range MaxCachedTokens + 10 {
		now = now.Add(time.Millisecond)
		if _, err := cache.Get(context.Background(), fmt.Sprint("key-", i), fetch); err != nil {
			t.Fatal(err)
		}
	}
	if cache.Len() != MaxCachedTokens {
		t.Fatalf("cache holds %d entries, want at most %d", cache.Len(), MaxCachedTokens)
	}
}

func TestCacheNeverOutlivesTheTokenOrTheCap(t *testing.T) {
	now := time.Now()
	cache := &Cache{Now: func() time.Time { return now }}
	calls := 0
	fetch := func(lifetime time.Duration) func(context.Context) (string, time.Duration, error) {
		return func(context.Context) (string, time.Duration, error) {
			calls++
			return fmt.Sprint("t", calls), lifetime, nil
		}
	}
	// A two-minute token is kept one minute: its lifetime less the margin.
	if _, err := cache.Get(context.Background(), "short", fetch(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Second)
	if token, _ := cache.Get(context.Background(), "short", fetch(2*time.Minute)); token != "t1" {
		t.Fatalf("token = %q, want the cached one", token)
	}
	now = now.Add(time.Second)
	if token, _ := cache.Get(context.Background(), "short", fetch(2*time.Minute)); token != "t2" {
		t.Fatalf("token = %q, want a fresh one after its lifetime less the margin", token)
	}
	// An hour-long token is kept at most MaxCachedTokenLifetime.
	if _, err := cache.Get(context.Background(), "long", fetch(time.Hour)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(MaxCachedTokenLifetime)
	if token, _ := cache.Get(context.Background(), "long", fetch(time.Hour)); token != "t4" {
		t.Fatalf("token = %q, want a fresh one after the cap", token)
	}
	// A token with no usable lifetime is never cached.
	if _, err := cache.Get(context.Background(), "unknown", fetch(0)); err != nil {
		t.Fatal(err)
	}
	if token, _ := cache.Get(context.Background(), "unknown", fetch(0)); token != "t6" {
		t.Fatalf("token = %q, want a fresh one when the lifetime is unknown", token)
	}
}

func TestCacheKeepsNothingFromAFailedFetch(t *testing.T) {
	cache := &Cache{}
	failure := errors.New("refused")
	if _, err := cache.Get(context.Background(), "k", func(context.Context) (string, time.Duration, error) {
		return "", 0, failure
	}); !errors.Is(err, failure) {
		t.Fatalf("error = %v, want the fetch error", err)
	}
	if cache.Len() != 0 {
		t.Fatalf("cache holds %d entries after a failure", cache.Len())
	}
}

func TestValidateAuthority(t *testing.T) {
	for _, value := range []string{"", "https://login.microsoftonline.us", "https://login.chinacloudapi.cn/"} {
		if err := ValidateAuthority(value); err != nil {
			t.Fatalf("ValidateAuthority(%q) = %v", value, err)
		}
	}
	for _, value := range []string{"http://login.microsoftonline.com", "https://user@login.example", "https://login.example/?x", "https://login.example/#f", "https://"} {
		if err := ValidateAuthority(value); err == nil {
			t.Fatalf("ValidateAuthority(%q) accepted", value)
		}
	}
}
