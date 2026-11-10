// Defensive JSON body decoder shared across inbound HTTP handlers.
//
// Replaces the bare `json.NewDecoder(r.Body).Decode(&body)` pattern that
// every inbound handler was using. That pattern has three problems for a
// production broker:
//
//  1. No body-size limit → arbitrary memory allocation on a malicious or
//     malformed client. http.MaxBytesReader caps and surfaces a clean error.
//
//  2. Bare json errors leak to client and don't log enough context for
//     postmortem. Example: a 2026-05-25 web-log entry "JSON parse error:
//     truncated string at position 343" — non-fatal, single occurrence, but
//     we could not tell from the log which endpoint, which tenant, which
//     content-length, which client IP. This helper logs all of those.
//
//  3. Inconsistent client responses: some handlers returned 400 with a
//     terse message, some returned the raw json error text. Now we always
//     return {"error":"<machine_code>", "detail":"<safe message>"} with
//     413 for "too large" and 400 for "malformed".
//
// Apply via:
//   var body MyRequestStruct
//   if !decodeJSONBody(w, r, &body, defaultMaxJSONBody) { return }
//   // ... body is populated and validated; proceed
//
// `disallowUnknownFields` is enabled — clients sending typos / extra fields
// get a useful 400 instead of silently dropped data. Caller can wrap with a
// permissive variant if needed.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// defaultMaxJSONBody is the cap applied unless a handler passes a different
// value. 1 MB is generous for any handler that takes user input (the
// largest current handler is /store which accepts vector batches); we
// can raise per-handler where genuinely needed (e.g. consolidation runs).
const defaultMaxJSONBody = 1 << 20 // 1 MiB

// decodeJSONBody decodes the request body into dst. Returns true on success.
// On failure it writes a proper HTTP response and returns false; the caller
// should simply `return` without further response writing.
//
// dst must be a non-nil pointer to a struct.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	if maxBytes <= 0 {
		maxBytes = defaultMaxJSONBody
	}
	// Wrap body in MaxBytesReader; this caps memory + surfaces a clean
	// *http.MaxBytesError on overflow.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		writeJSONBodyError(w, r, err, maxBytes)
		return false
	}

	// Reject trailing data (multiple JSON objects in one request body).
	// Tolerates trailing whitespace.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSONBodyError(w, r,
			errors.New("request body must contain only a single JSON object"),
			maxBytes)
		return false
	}

	return true
}

// writeJSONBodyError logs structured context and writes a typed 4xx response.
// Never echoes the body content back to the client (could be PII / secret).
func writeJSONBodyError(w http.ResponseWriter, r *http.Request, err error, maxBytes int64) {
	contentLength := r.ContentLength
	tenantHeader := r.Header.Get("X-HeurChain-Tenant")
	if tenantHeader == "" {
		tenantHeader = "(none)"
	}
	clientIP := r.Header.Get("X-Forwarded-For")
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}

	// Default response: 400 malformed
	status := http.StatusBadRequest
	code := "json_malformed"
	detail := "request body is not valid JSON"

	// Refine based on error class
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		status = http.StatusRequestEntityTooLarge // 413
		code = "json_too_large"
		detail = fmt.Sprintf("request body exceeds %d bytes", maxBytes)

	default:
		msg := err.Error()
		// Surface enough of the underlying parse error to be useful to the
		// client (e.g. "unexpected EOF", "unknown field 'foo'", "invalid
		// character ',' at position 343"). Strip Go-internal prefixes that
		// would leak handler types.
		msg = strings.TrimPrefix(msg, "json: ")
		msg = strings.TrimPrefix(msg, "json: cannot ")
		// Cap message length to avoid log-flooding on adversarial payloads.
		if len(msg) > 256 {
			msg = msg[:256] + "...(truncated)"
		}
		detail = msg

		// Classify common parse-error subtypes (matches Go encoding/json
		// internals as of go1.23):
		switch {
		case strings.Contains(msg, "unknown field"):
			code = "json_unknown_field"
		case strings.Contains(msg, "unexpected EOF"),
			strings.Contains(msg, "unexpected end"):
			code = "json_truncated"
		case strings.Contains(msg, "cannot unmarshal"):
			code = "json_type_mismatch"
		}
	}

	// Structured log for postmortem correlation (path, tenant, size, parse
	// failure mode). No body content logged.
	log.Printf(
		"jsonbody: path=%q method=%s tenant=%s client_ip=%s "+
			"content_length=%d max_bytes=%d code=%s detail=%q",
		r.URL.Path, r.Method, tenantHeader, clientIP,
		contentLength, maxBytes, code, detail,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Marshal the typed response; ignore marshal error (always succeeds for
	// this small struct, and there's nothing useful to do if it didn't).
	resp := map[string]string{"error": code, "detail": detail}
	_ = json.NewEncoder(w).Encode(resp)
}
