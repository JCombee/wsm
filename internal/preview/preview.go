// Package preview is the reverse proxy for sandbox previews (ADR 0017).
// It serves every <sandbox>--<project>.<preview domain> host: without a
// valid __Host-wsm_preview cookie it sends the browser to the dashboard,
// which hands over a one-time token; with one it forwards to the port the
// sandbox's pod publishes on 127.0.0.1. Its own cookie never reaches the
// sandbox, in either direction.
package preview

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JCombee/wsm/internal/auth"
	"github.com/JCombee/wsm/internal/db"
)

// AuthPath is reserved on every preview host; /__wsm/ is never forwarded.
const (
	AuthPath     = "/__wsm/auth"
	ReservedPath = "/__wsm/"
)

// Limits of the proxy.
const (
	MaxHeaderBytes        = 64 << 10
	responseHeaderTimeout = 60 * time.Second
	dialTimeout           = 5 * time.Second
)

// Target resolves a preview host to the sandbox's published port on
// 127.0.0.1. It fails with db.ErrNotFound when there is no such sandbox or
// it has no preview.
type Target func(ctx context.Context, projectSlug, sandboxSlug string) (port int, running bool, err error)

// Proxy serves previews.
type Proxy struct {
	Auth   *auth.Service
	Target Target
	Logf   func(format string, args ...any)

	rp *httputil.ReverseProxy
}

// New returns a Proxy.
func New(a *auth.Service, target Target, logf func(format string, args ...any)) *Proxy {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &Proxy{Auth: a, Target: target, Logf: logf}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		ModifyResponse: modifyResponse,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            (&net.Dialer{Timeout: dialTimeout}).DialContext,
			ResponseHeaderTimeout:  responseHeaderTimeout,
			MaxResponseHeaderBytes: MaxHeaderBytes,
			MaxIdleConnsPerHost:    4,
			IdleConnTimeout:        90 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logf("preview %s: %v", r.Host, err)
			plain(w, http.StatusBadGateway, "the app in this sandbox does not answer")
		},
	}
	return p
}

type portKey struct{}

func plain(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	sbx, proj, ok := auth.ParsePreviewHost(host, p.Auth.PreviewDomain())
	if !ok {
		plain(w, http.StatusNotFound, "unknown preview host")
		return
	}
	if r.URL.Path == AuthPath {
		p.authorize(w, r, host)
		return
	}
	if strings.HasPrefix(r.URL.Path, ReservedPath) {
		plain(w, http.StatusNotFound, "not found")
		return
	}
	if !p.hasSession(r, host) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			plain(w, http.StatusUnauthorized, "log in through the dashboard first")
			return
		}
		q := url.Values{"host": {host}, "next": {auth.SafeNext(r.URL.RequestURI())}}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, p.Auth.Origin()+"/preview-auth?"+q.Encode(), http.StatusSeeOther)
		return
	}
	port, running, err := p.Target(r.Context(), proj, sbx)
	switch {
	case errors.Is(err, db.ErrNotFound):
		plain(w, http.StatusNotFound, "this sandbox has no preview")
		return
	case err != nil:
		p.Logf("preview %s: %v", host, err)
		plain(w, http.StatusInternalServerError, "internal error")
		return
	case !running:
		plain(w, http.StatusServiceUnavailable, "this sandbox is not running")
		return
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), portKey{}, port)))
}

func (p *Proxy) hasSession(r *http.Request, host string) bool {
	for _, ck := range r.CookiesNamed(auth.PreviewCookie) {
		if p.Auth.PreviewSessionValid(r.Context(), ck.Value, host) {
			return true
		}
	}
	return false
}

// authorize exchanges a one-time token for the preview cookie.
func (p *Proxy) authorize(w http.ResponseWriter, r *http.Request, host string) {
	if r.Method != http.MethodGet {
		plain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	value, err := p.Auth.ConsumePreviewToken(r.Context(), q.Get("token"), host)
	if err != nil {
		plain(w, http.StatusForbidden, "this preview link is invalid or expired; open the preview from the dashboard again")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: auth.PreviewCookie, Value: value, Path: "/",
		MaxAge: int(auth.PreviewTTL.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, auth.SafeNext(q.Get("next")), http.StatusSeeOther)
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	port, _ := pr.In.Context().Value(portKey{}).(int)
	pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)})
	// Apps build links from the Host header, so it stays the preview host.
	pr.Out.Host = pr.In.Host
	pr.SetXForwarded()
	pr.Out.Header.Set("X-Forwarded-Proto", "https")
	StripCookie(pr.Out.Header, auth.PreviewCookie)
}

// StripCookie removes every cookie called name from the Cookie headers.
func StripCookie(h http.Header, name string) {
	var keep []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, _, _ := strings.Cut(part, "=")
			if strings.TrimSpace(n) == name {
				continue
			}
			keep = append(keep, part)
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}

// modifyResponse drops any Set-Cookie for the proxy's own cookie, so an
// app cannot replace or clear it.
func modifyResponse(resp *http.Response) error {
	lines := resp.Header.Values("Set-Cookie")
	if len(lines) == 0 {
		return nil
	}
	resp.Header.Del("Set-Cookie")
	for _, line := range lines {
		n, _, _ := strings.Cut(line, "=")
		if strings.EqualFold(strings.TrimSpace(n), auth.PreviewCookie) {
			continue
		}
		resp.Header.Add("Set-Cookie", line)
	}
	return nil
}
