package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// csrf.go hardens the mutating endpoints against cross-site request forgery and
// adds the baseline security response headers the app was missing.
//
// Why this matters here specifically: DockPilot authenticates with HTTP Basic
// auth, which the browser auto-attaches to *every* request to this origin. That
// means a malicious page the operator merely visits while logged in could fire
// `POST /containers/<id>/remove` (or trigger an exec) and the browser would
// happily authenticate the forged request. POST-gating alone does not stop this.
//
// Defence model (layered, no per-form template edits required):
//  1. Primary — same-origin enforcement on unsafe methods. Browsers set the
//     `Origin` header on cross-origin POSTs and cannot be tricked into forging
//     it from script, so requiring Origin (or, failing that, Referer) to match
//     our own host blocks the forged-POST attack outright. This is the
//     OWASP-recommended header-based CSRF defence and needs zero changes to the
//     29 existing forms.
//  2. Defence in depth — a double-submit CSRF cookie. Non-browser/programmatic
//     clients that send neither Origin nor Referer must echo the cookie value in
//     an `X-CSRF-Token` header. Same-origin form posts never hit this path.
//
// csrfCookieName is the double-submit cookie. SameSite=Strict means the browser
// will not even send it on a cross-site request, so it can never leak to a
// forging page.
const csrfCookieName = "dockpilot_csrf"

// safeMethod reports whether the HTTP method is read-only and therefore exempt
// from CSRF enforcement (it cannot change server state).
func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// newCSRFToken returns a cryptographically random 32-byte token, hex-encoded.
func newCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing is catastrophic and essentially never happens; fall
		// back to a fixed marker so the comparison below always fails closed.
		return ""
	}
	return hex.EncodeToString(b)
}

// requestIsHTTPS best-effort detects TLS so the cookie's Secure flag is set in
// production (DockPilot typically runs behind a TLS-terminating reverse proxy)
// but not on a plain-HTTP LAN dev box, where Secure would stop the cookie working.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ensureCSRFCookie sets the double-submit cookie on safe responses if it is
// missing, so the value is available to any client that wants to echo it.
func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return
	}
	tok := newCSRFToken()
	if tok == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: false, // readable by JS so fetch/XHR clients can send the header
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// allowedHosts is the set of host[:port] values an Origin/Referer may carry and
// still be considered same-site: our own Host plus any explicitly trusted
// origins from the TRUSTED_ORIGINS env var (comma-separated, e.g. a public
// reverse-proxy domain).
func allowedHosts(r *http.Request) map[string]struct{} {
	hosts := map[string]struct{}{}
	if r.Host != "" {
		hosts[strings.ToLower(r.Host)] = struct{}{}
	}
	// Behind a reverse proxy the browser's Origin reflects the public host,
	// which the proxy forwards as X-Forwarded-Host while r.Host is the internal
	// upstream address. Trust it so same-origin posts aren't rejected in prod.
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		for _, h := range strings.Split(xfh, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts[strings.ToLower(h)] = struct{}{}
			}
		}
	}
	for _, raw := range strings.Split(os.Getenv("TRUSTED_ORIGINS"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			hosts[strings.ToLower(u.Host)] = struct{}{}
		} else {
			hosts[strings.ToLower(raw)] = struct{}{}
		}
	}
	return hosts
}

// hostMatches parses a full URL (Origin or Referer) and reports whether its host
// is in the allowed set.
func hostMatches(rawURL string, allowed map[string]struct{}) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	_, ok := allowed[strings.ToLower(u.Host)]
	return ok
}

// validDoubleSubmit checks the X-CSRF-Token header against the cookie using a
// constant-time comparison. This path is only reached when a request carries no
// Origin/Referer at all (i.e. not a browser navigation).
func validDoubleSubmit(r *http.Request) bool {
	c, err := r.Cookie(csrfCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	hdr := r.Header.Get("X-CSRF-Token")
	if hdr == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(hdr)) == 1
}

// csrfProtect enforces same-origin on every state-changing request and seeds the
// double-submit cookie on safe ones.
func csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			ensureCSRFCookie(w, r)
			next.ServeHTTP(w, r)
			return
		}

		allowed := allowedHosts(r)

		// Primary defence: an Origin header that must match us. Present on all
		// modern cross-origin (and same-origin) POSTs; unforgeable from script.
		if origin := r.Header.Get("Origin"); origin != "" {
			if hostMatches(origin, allowed) {
				next.ServeHTTP(w, r)
				return
			}
			csrfReject(w, r, "origin not allowed")
			return
		}

		// Fallback: some browsers omit Origin on same-origin form posts but send
		// Referer. Accept a matching Referer.
		if ref := r.Header.Get("Referer"); ref != "" {
			if hostMatches(ref, allowed) {
				next.ServeHTTP(w, r)
				return
			}
			csrfReject(w, r, "referer not allowed")
			return
		}

		// No Origin and no Referer: not a browser navigation. Require the
		// double-submit token so legitimate API clients can still operate.
		if validDoubleSubmit(r) {
			next.ServeHTTP(w, r)
			return
		}
		csrfReject(w, r, "missing origin/referer and CSRF token")
	})
}

// csrfReject writes a uniform 403 for blocked state-changing requests and logs
// the headers that drove the decision so multi-homing/proxy mismatches can be
// diagnosed from the server log.
func csrfReject(w http.ResponseWriter, r *http.Request, reason string) {
	log.Printf("csrf reject: reason=%q method=%s path=%s host=%q origin=%q referer=%q xfh=%q xfproto=%q",
		reason, r.Method, r.URL.Path, r.Host,
		r.Header.Get("Origin"), r.Header.Get("Referer"),
		r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Proto"))
	http.Error(w, "forbidden: CSRF check failed ("+reason+")", http.StatusForbidden)
}

// secureHeaders adds baseline hardening headers to every response: deny framing
// (clickjacking), block MIME sniffing, suppress referrer leakage, and a CSP that
// keeps all script/style/connect targets same-origin. 'unsafe-inline' is
// required because the UI ships inline <style>, inline <script>, and inline
// event handlers (e.g. onsubmit="return confirm(...)").
func secureHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"object-src 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// "same-origin" (not "no-referrer"): no-referrer makes browsers send
		// Origin: null on same-origin form POSTs, which would trip our own CSRF
		// origin check. "same-origin" keeps the real Origin/Referer for our
		// same-origin forms while still leaking nothing to cross-origin targets.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
