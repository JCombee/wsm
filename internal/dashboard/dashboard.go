// Package dashboard puts the web dashboard together (ADR 0016): the
// login endpoints, the API behind a session, and the embedded frontend.
// It listens on loopback behind a TLS proxy such as Caddy.
package dashboard

import (
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"

	"github.com/JCombee/wsm/internal/auth"
)

// csp keeps the frontend to its own origin. Monaco needs inline styles and
// workers.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; " +
	"object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Handler returns the dashboard: /auth/ and /preview-auth for login and
// previews, /api/ for the API (with a session), and the frontend in static
// for everything else. Requests for another host than a's dashboard host
// get 421, which keeps DNS rebinding away from the loopback listener.
func Handler(a *auth.Service, apiHandler http.Handler, static fs.FS, dashboardHost string) http.Handler {
	mux := http.NewServeMux()
	authH := a.Handler()
	mux.Handle("/auth/", authH)
	mux.Handle("/preview-auth", authH)
	// A related-origins document would let previews use the passkey.
	mux.HandleFunc("/.well-known/", http.NotFound)
	mux.Handle("/api/", a.Require(http.StripPrefix("/api", apiHandler)))
	mux.Handle("/", spa(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != dashboardHost {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		mux.ServeHTTP(w, r)
	})
}

// spa serves the built frontend: a file when it exists, index.html for
// any other GET, so client-side routes such as /enroll work.
func spa(static fs.FS) http.Handler {
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if fi, err := fs.Stat(static, name); err == nil && !fi.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		data, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, "the frontend is not built; run make web", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)
	})
}
