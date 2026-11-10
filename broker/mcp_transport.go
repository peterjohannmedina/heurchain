package main

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

func supportedMCPVersion(v string) bool {
	switch v {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return true
	}
	return false
}

// Check Origin before authentication to prevent browser DNS-rebinding attacks.
// Non-browser clients omit Origin. Extra browser origins need explicit config.
func prepareMCPRequest(w http.ResponseWriter, r *http.Request) bool {
	setCORSHeaders(w)
	w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate, MCP-Protocol-Version, X-HeurChain-Request-ID")
	origin := r.Header.Get("Origin")
	if origin != "" {
		// A browser origin is allowed when it matches the host serving this
		// request, or when an operator names it explicitly.
		ok := false
		if u, err := url.Parse(origin); err == nil && u.Host == r.Host {
			ok = true
		}
		for _, value := range strings.Fields(os.Getenv("HEURCHAIN_MCP_ALLOWED_ORIGINS")) {
			if origin == value {
				ok = true
			}
		}
		if !ok {
			jsonError(w, "origin not allowed", http.StatusForbidden)
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-HeurChain-API-Key, X-HeurChain-Tenant, MCP-Protocol-Version, MCP-Session-Id, Last-Event-ID")
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}
