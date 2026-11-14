package main

// http.go holds the shared response plumbing.
//
// There are no {"ok": …} envelopes here: the answer to "did it work" is the
// HTTP status code. A success body is the data, an error body is
// {error, code}. One rule, with nothing for the client to forget to check.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Machine-readable error codes. The text is written for humans and may
// change; the code is part of the contract.
const (
	codeBadRequest           = "bad_request"
	codeUnauthorized         = "unauthorized"
	codeRefused              = "refused"
	codeNotFound             = "not_found"
	codeAmbiguous            = "ambiguous"
	codeBusy                 = "busy"
	codeStreaming            = "streaming"
	codeQbitUnavailable      = "qbittorrent_unavailable"
	codeInternal             = "internal"
	codeUnavailable          = "unavailable"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeMisdirected          = "misdirected"
)

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, code, format string, a ...any) {
	writeJSON(w, status, apiError{Error: fmt.Sprintf(format, a...), Code: code})
}

func writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// decodeBody reads the JSON request body. An empty body is an empty object:
// commands without parameters have no body at all. wrap has already refused
// a body that is not declared as application/json.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	return nil
}

// checkForgery refuses a state-changing request that a web page open in the
// user's browser could have sent by itself. Such a page cannot attach the
// token, but the token is optional and off by default.
//
// A cross-site fetch with a JSON body needs a CORS preflight, which this
// server never answers, so the browser does not send it. A "simple" request
// needs none: a text/plain or form body still goes out, and it can carry
// JSON. So a body must be declared as application/json, as every real client
// does; a command without a body has nothing to check.
//
// Sec-Fetch-Site is defence in depth. Browsers set it and other clients do
// not, so "cross-site" means a page on another site made the request. It is
// no substitute for the content type check: browsers only send it to
// trustworthy origins (HTTPS, localhost), not to a plain http:// LAN address.
func checkForgery(r *http.Request) *denial {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return deny(http.StatusForbidden, codeRefused,
			"cross-site requests are not accepted")
	}
	// -1 is a body of unknown length (chunked): it is still a body.
	if r.ContentLength != 0 && !isJSONMediaType(r.Header.Get("Content-Type")) {
		return deny(http.StatusUnsupportedMediaType, codeUnsupportedMediaType,
			"request body must be application/json")
	}
	return nil
}

// isJSONMediaType accepts application/json with any parameters (charset);
// mime.ParseMediaType lower-cases the type and rejects malformed values.
func isJSONMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

// hostAllowlist is TORRMEDIA_HOSTS, parsed once at startup: the names the
// API may be reached by besides IP addresses, localhost and *.local. The zero
// value allows none.
type hostAllowlist struct {
	names    map[string]bool // lower-case, without a trailing dot
	suffixes []string        // ".example.org": any name ending in it
}

// parseHostAllowlist reads a comma-separated list of host names. An entry
// starting with "." matches that suffix; case does not matter, and empty
// entries are ignored.
func parseHostAllowlist(raw string) hostAllowlist {
	var list hostAllowlist
	for _, entry := range strings.Split(raw, ",") {
		entry = normalizeHostName(strings.TrimSpace(entry))
		switch {
		case entry == "":
		case strings.HasPrefix(entry, "."):
			list.suffixes = append(list.suffixes, entry)
		default:
			if list.names == nil {
				list.names = map[string]bool{}
			}
			list.names[entry] = true
		}
	}
	return list
}

func (l hostAllowlist) allows(host string) bool {
	if l.names[host] {
		return true
	}
	for _, suffix := range l.suffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// normalizeHostName lower-cases a host name and drops the trailing dot of a
// fully qualified one: "Mac-Mini.local." and "mac-mini.local" are one name.
func normalizeHostName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}

// requestHostName returns the host of a Host header value without the port
// and without the brackets of an IPv6 literal.
func requestHostName(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if len(host) > 1 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	return normalizeHostName(host)
}

// checkHost refuses a request addressed to a name this server was not told
// about, which is what DNS rebinding looks like. A page on the attacker's
// domain points that domain at this server's address once it has loaded; the
// browser then sends the page's requests here as same-origin ones, so the
// page reads every response and may use any method, with no preflight and no
// Sec-Fetch-Site: cross-site. Only the Host header still carries the
// attacker's name.
//
// An IP literal cannot be rebound. localhost and *.localhost resolve to
// loopback in browsers, and *.local is multicast DNS, answered only on the
// local link. Any other name has to be listed in TORRMEDIA_HOSTS. The check
// covers every route, /health and streams included: a rebound page must not
// read file names or video either.
func (s *server) checkHost(r *http.Request) *denial {
	host := requestHostName(r.Host)
	if builtinHost(host) || s.hosts.allows(host) {
		return nil
	}
	return deny(http.StatusMisdirectedRequest, codeMisdirected, fmt.Sprintf(
		"host %q is not served here; to use this name, add it to TORRMEDIA_HOSTS", host))
}

func builtinHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local")
}

func (s *server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := s.checkHost(r); d != nil {
			writeDenial(w, d)
			return
		}
		if d := checkForgery(r); d != nil {
			writeDenial(w, d)
			return
		}
		if s.token != "" && r.URL.Path != "/health" {
			// Constant-time comparison: a plain != returns at the first
			// mismatching byte and makes the token guessable by timing.
			expected := "Bearer " + s.token
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
				writeErr(w, http.StatusUnauthorized, codeUnauthorized, "missing or invalid token")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if s.draining.Load() {
				writeErr(w, http.StatusServiceUnavailable, codeUnavailable,
					"torrmedia is shutting down")
				return
			}
			log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
