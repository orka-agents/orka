/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerOptionsValidateTLSFiles(t *testing.T) {
	tests := []struct {
		name     string
		certFile string
		keyFile  string
		wantErr  bool
		wantTLS  bool
	}{
		{name: "plain HTTP"},
		{name: "TLS", certFile: "/tls/tls.crt", keyFile: "/tls/tls.key", wantTLS: true},
		{name: "certificate only", certFile: "/tls/tls.crt", wantErr: true},
		{name: "key only", keyFile: "/tls/tls.key", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := serverOptions{tlsCertFile: tt.certFile, tlsKeyFile: tt.keyFile}
			err := options.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := options.tlsEnabled(); got != tt.wantTLS {
				t.Fatalf("tlsEnabled() = %v, want %v", got, tt.wantTLS)
			}
		})
	}
}

func TestNewServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	tests := []struct {
		name    string
		options serverOptions
		wantTLS bool
	}{
		{
			name:    "HTTP",
			options: serverOptions{listenAddr: "127.0.0.1:8090"},
		},
		{
			name: "HTTPS",
			options: serverOptions{
				listenAddr:  "127.0.0.1:8443",
				tlsCertFile: "/tls/tls.crt",
				tlsKeyFile:  "/tls/tls.key",
			},
			wantTLS: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, err := newServer(tt.options, handler)
			if err != nil {
				t.Fatalf("newServer() error = %v", err)
			}
			if server.Addr != tt.options.listenAddr {
				t.Fatalf("Addr = %q, want %q", server.Addr, tt.options.listenAddr)
			}
			if server.ReadHeaderTimeout != defaultReadHeaderTimeout {
				t.Fatalf("ReadHeaderTimeout = %s, want %s", server.ReadHeaderTimeout, defaultReadHeaderTimeout)
			}
			if server.ReadTimeout != defaultReadTimeout {
				t.Fatalf("ReadTimeout = %s, want %s", server.ReadTimeout, defaultReadTimeout)
			}
			if server.WriteTimeout != defaultWriteTimeout {
				t.Fatalf("WriteTimeout = %s, want %s", server.WriteTimeout, defaultWriteTimeout)
			}
			if server.IdleTimeout != defaultIdleTimeout {
				t.Fatalf("IdleTimeout = %s, want %s", server.IdleTimeout, defaultIdleTimeout)
			}

			if tt.wantTLS {
				if server.TLSConfig == nil {
					t.Fatal("TLSConfig is nil")
				}
				if server.TLSConfig.MinVersion != tls.VersionTLS12 {
					t.Fatalf("TLS minimum version = %d, want %d", server.TLSConfig.MinVersion, tls.VersionTLS12)
				}
			} else if server.TLSConfig != nil {
				t.Fatalf("TLSConfig = %#v, want nil", server.TLSConfig)
			}

			recorder := httptest.NewRecorder()
			server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("handler status = %d, want %d", recorder.Code, http.StatusNoContent)
			}
		})
	}
}

func TestNewServerRejectsPartialTLSConfiguration(t *testing.T) {
	_, err := newServer(serverOptions{tlsCertFile: "/tls/tls.crt"}, http.NotFoundHandler())
	if err == nil {
		t.Fatal("newServer() error = nil, want partial TLS configuration error")
	}
}
