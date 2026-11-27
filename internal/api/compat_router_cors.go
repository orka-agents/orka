/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"net/http"
	"strings"
)

func (r *CompatRouter) handleCORS(w http.ResponseWriter, req *http.Request, path string) bool {
	w.Header().Add("Vary", "Origin")
	origin := req.Header.Get("Origin")
	allowedOrigin := ""
	if origin != "" {
		for _, configured := range r.corsOrigins {
			configured = strings.TrimSpace(configured)
			if configured == "*" || configured == origin {
				allowedOrigin = configured
				break
			}
		}
	}
	if allowedOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
	}
	if req.Method != http.MethodOptions {
		return false
	}

	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	method := req.Header.Get("Access-Control-Request-Method")
	if !compatRouterRoute(method, path) {
		writeCompatRouterError(w, path, http.StatusNotFound, "unknown compatibility route")
		return true
	}
	if allowedOrigin == "" {
		writeCompatRouterError(w, path, http.StatusForbidden, "origin is not allowed")
		return true
	}
	headers := append(allowedCORSHeaders(ContextTokenConfig{}), compatRouterRequestHeaders...)
	headers = append(headers, compatRouterSDKHeaders...)
	for requested := range strings.SplitSeq(req.Header.Get("Access-Control-Request-Headers"), ",") {
		requested = strings.TrimSpace(requested)
		if requested == "" {
			continue
		}
		allowed := false
		for _, header := range headers {
			if strings.EqualFold(requested, header) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeCompatRouterError(w, path, http.StatusForbidden, "requested header is not allowed")
			return true
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", method)
	w.Header().Set("Access-Control-Allow-Headers", strings.Join(headers, ", "))
	w.WriteHeader(http.StatusNoContent)
	return true
}

var compatRouterSDKHeaders = []string{
	"Anthropic-Dangerous-Direct-Browser-Access",
	"X-Stainless-Lang", "X-Stainless-Package-Version",
	"X-Stainless-OS", "X-Stainless-Arch",
	"X-Stainless-Runtime", "X-Stainless-Runtime-Version",
	"X-Stainless-Retry-Count", "X-Stainless-Timeout", "X-Stainless-Helper-Method",
}
