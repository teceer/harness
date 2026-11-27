package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

// diffboardPrefix is where harness serve exposes diffboard's design boards:
// the canvas on which agents propose a data model, process and assumptions
// and the user accepts or rejects them, from the phone too. diffboard
// listens on loopback like harness, so this proxy is what puts it on the
// tailnet, behind the harness token.
const diffboardPrefix = "/diffboard"

// diffboardCookie carries the token for the page's own requests (scripts,
// fetches, the event stream), which cannot set an Authorization header.
const diffboardCookie = "harness_diffboard"

// diffboardTarget is the local diffboard server: $HARNESS_DIFFBOARD_URL or
// diffboard's default port.
func diffboardTarget() *url.URL {
	raw := os.Getenv("HARNESS_DIFFBOARD_URL")
	if raw == "" {
		raw = "http://127.0.0.1:4700"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		u, _ = url.Parse("http://127.0.0.1:4700")
	}
	return u
}

// diffboardAllowed: only the design boards go through. The rest of
// diffboard edits working-tree files and opens editors, which has no
// business on the tailnet.
func diffboardAllowed(path string) bool {
	switch path {
	case "/design", "/design.js", "/design.css", "/events", "/api/designs":
		return true
	}
	return strings.HasPrefix(path, "/api/designs/") || strings.HasPrefix(path, "/api/d/")
}

func (s *webServer) diffboardProxy() http.Handler {
	target := diffboardTarget()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, diffboardPrefix)
			pr.Out.URL.RawPath = ""
			q := pr.Out.URL.Query()
			q.Del("t")
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			// diffboard compares the browser's Origin with this host
			pr.SetXForwarded()
		},
		FlushInterval: -1, // the event stream
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("diffboard proxy: %v", err)
			http.Error(w, "diffboard is not running on this machine: start it with `diffboard start`", http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, diffboardPrefix)
		if path == "" || path == "/" {
			http.Redirect(w, r, diffboardPrefix+"/design"+keepQuery(r), http.StatusFound)
			return
		}
		if !diffboardAllowed(path) {
			http.NotFound(w, r)
			return
		}
		given, fromQuery := s.diffboardToken(r)
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) != 1 {
			if path == "/design" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, diffboardLogin)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if fromQuery {
			// keep the token out of the address bar and history from here on
			http.SetCookie(w, &http.Cookie{
				Name: diffboardCookie, Value: s.token, Path: diffboardPrefix + "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
				Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
				MaxAge: 90 * 24 * 3600,
			})
			if r.Method == http.MethodGet && path == "/design" {
				q := r.URL.Query()
				q.Del("t")
				target := diffboardPrefix + path
				if enc := q.Encode(); enc != "" {
					target += "?" + enc
				}
				http.Redirect(w, r, target, http.StatusSeeOther) // the #fragment stays in the browser
				return
			}
		}
		if r.Method != http.MethodGet {
			log.Printf("diffboard: %s %s from %s", r.Method, path, r.RemoteAddr)
		}
		proxy.ServeHTTP(w, r)
	})
}

// diffboardToken reads the token from the header, the query (first visit)
// or the cookie set on that first visit.
func (s *webServer) diffboardToken(r *http.Request) (string, bool) {
	if h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); h != "" {
		return h, false
	}
	if t := r.URL.Query().Get("t"); t != "" {
		return t, true
	}
	if c, err := r.Cookie(diffboardCookie); err == nil {
		return c.Value, false
	}
	return "", false
}

func keepQuery(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

// diffboardLogin is the page a link without the token lands on. The harness
// web app on this origin keeps the token in localStorage: reuse it. Else the
// user pastes it once (the Harness app shows it in settings, or
// ~/.harness/web-token); it never travels in a chat message.
const diffboardLogin = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>Design boards</title>
<style>body{font:16px -apple-system,system-ui,sans-serif;margin:0;padding:max(24px,env(safe-area-inset-top)) 20px;background:#fafafa;color:#242529}
@media (prefers-color-scheme:dark){body{background:#282c33;color:#dce0e5}input{background:#363c46;color:inherit}}
input{font:16px ui-monospace,Menlo,monospace;width:100%;box-sizing:border-box;padding:12px;border:1px solid #c9c9ca;border-radius:10px;margin:12px 0}
button{font:600 16px -apple-system,sans-serif;width:100%;padding:13px;border:0;border-radius:10px;background:#5c78e2;color:#fff}
p{color:#7e8086;font-size:14px;line-height:1.45}</style></head><body>
<h2>Design boards</h2>
<p>This page is behind your harness token. Open it from the Harness app (◧), or paste the token once: it is kept in this browser.</p>
<form id="f"><input id="t" type="password" autocomplete="current-password" placeholder="harness token" autofocus><button>Open</button></form>
<script>
const go = t => { const u = new URL(location.href); u.searchParams.set('t', t); location.replace(u.toString()); };
const tried = new URLSearchParams(location.search).has('t');
const known = localStorage.getItem('harness-token');
if (tried) document.querySelector('p').textContent = 'That token was not accepted. Paste the current one.';
else if (known) go(known);
document.getElementById('f').onsubmit = e => { e.preventDefault(); const t = document.getElementById('t').value.trim(); if (t) { localStorage.setItem('harness-token', t); go(t); } };
</script></body></html>`
