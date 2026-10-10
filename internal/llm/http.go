/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package llm

import (
	"net/http"
	"sync"
	"time"
)

// SharedHTTPClient reuses connections across request-scoped provider SDK clients.
// Credentials and middleware remain on each SDK client, not this HTTP client.
// Callers must not modify the returned client or its transport.
func SharedHTTPClient() *http.Client {
	return sharedHTTPClient()
}

var sharedHTTPClient = sync.OnceValue(func() *http.Client {
	transport := http.DefaultTransport
	if standard, ok := transport.(*http.Transport); ok {
		clone := standard.Clone()
		// Preserve the OpenAI and Anthropic SDKs' response-header deadline without
		// creating a fresh connection pool every time credentials are resolved.
		clone.ResponseHeaderTimeout = 10 * time.Minute
		transport = clone
	}
	return &http.Client{Transport: transport}
})
