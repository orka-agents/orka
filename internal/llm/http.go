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

func SharedHTTPClient() *http.Client {
	return sharedHTTPClient()
}

var sharedHTTPClient = sync.OnceValue(func() *http.Client {
	transport := http.DefaultTransport
	if standard, ok := transport.(*http.Transport); ok {
		clone := standard.Clone()
		clone.ResponseHeaderTimeout = 10 * time.Minute
		transport = clone
	}
	return &http.Client{Transport: transport}
})
