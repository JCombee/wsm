// Package auth is the dashboard's login (ADR 0016) and the hand-over of
// access to preview hosts (ADR 0017): passkeys with go-webauthn, sessions
// in __Host- cookies, CSRF and Origin checks, and one-time preview tokens.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/JCombee/wsm/internal/db"
)

// Cookie names. Only __Host- cookies carry trust (spike 04).
const (
	SessionCookie   = "__Host-wsm_session"
	CeremonyCookie  = "__Host-wsm_ceremony"
	PreviewCookie   = "__Host-wsm_preview"
	CSRFHeader      = "X-WSM-CSRF"
	ownerName       = "owner"
	maxCeremonyBody = 64 << 10
)

// Lifetimes.
const (
	SessionTTL      = 7 * 24 * time.Hour
	CeremonyTTL     = 5 * time.Minute
	EnrollTTL       = 15 * time.Minute
	PreviewTokenTTL = 60 * time.Second
	PreviewTTL      = 12 * time.Hour
	touchInterval   = time.Minute
	maxCeremonies   = 256
)

// Config is the dashboard's identity.
type Config struct {
	// DashboardHost is the dashboard's host name, as the browser sees it
	// (e.g. dash.example.com). It is the passkey RP ID, and
	// https://DashboardHost is the only allowed origin.
	DashboardHost string
	// PreviewDomain is the domain under which previews live
	// (<sandbox>--<project>.<PreviewDomain>); empty disables previews.
	PreviewDomain string
}

// ValidHost reports whether h is a plausible DNS host name: lowercase
// labels of letters, digits and dashes, separated by dots.
func ValidHost(h string) bool {
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Service implements login, sessions and preview tokens.
type Service struct {
	store  *db.Store
	wa     *webauthn.WebAuthn
	cfg    Config
	origin string
	now    func() time.Time
	logf   func(format string, args ...any)

	mu         sync.Mutex
	ceremonies map[string]ceremony // keyed by the hash of the cookie value
}

type ceremony struct {
	kind      string // "register" or "login"
	data      webauthn.SessionData
	tokenHash string // register: the enroll token
	name      string // register: the passkey's name
	expires   time.Time
}

// New returns a Service for cfg.
func New(store *db.Store, cfg Config, logf func(format string, args ...any)) (*Service, error) {
	if !ValidHost(cfg.DashboardHost) {
		return nil, fmt.Errorf("dashboard host %q: use a lowercase DNS name without port", cfg.DashboardHost)
	}
	if cfg.PreviewDomain != "" {
		if !ValidHost(cfg.PreviewDomain) {
			return nil, fmt.Errorf("preview domain %q: use a lowercase DNS name", cfg.PreviewDomain)
		}
		if cfg.PreviewDomain == cfg.DashboardHost || strings.HasSuffix(cfg.DashboardHost, "."+cfg.PreviewDomain) {
			return nil, fmt.Errorf("the dashboard host must not lie under the preview domain")
		}
	}
	origin := "https://" + cfg.DashboardHost
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.DashboardHost,
		RPDisplayName: "wsm",
		RPOrigins:     []string{origin},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Service{store: store, wa: wa, cfg: cfg, origin: origin, now: time.Now, logf: logf,
		ceremonies: map[string]ceremony{}}, nil
}

// Origin returns the dashboard origin.
func (s *Service) Origin() string { return s.origin }

// newToken returns a random token for a cookie or link, and its hash.
func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("random token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken returns the stored form of a token: its SHA-256 as hex.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewEnrollToken creates a one-time registration link.
func (s *Service) NewEnrollToken(ctx context.Context) (string, time.Time, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.now().Add(EnrollTTL)
	if err := s.store.CreateEnrollToken(ctx, hash, expires); err != nil {
		return "", time.Time{}, err
	}
	return s.origin + "/enroll?token=" + url.QueryEscape(token), expires, nil
}

// waUser adapts a user and its passkeys to go-webauthn.
type waUser struct {
	u     db.User
	creds []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return w.u.Handle }
func (w *waUser) WebAuthnName() string                       { return w.u.Name }
func (w *waUser) WebAuthnDisplayName() string                { return w.u.Name }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (s *Service) loadUser(ctx context.Context, u db.User) (*waUser, error) {
	pks, err := s.store.Passkeys(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	w := &waUser{u: u}
	for _, pk := range pks {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(pk.CredentialJSON), &c); err != nil {
			return nil, fmt.Errorf("passkey %s: %w", pk.ID, err)
		}
		w.creds = append(w.creds, c)
	}
	return w, nil
}

func (s *Service) owner(ctx context.Context) (db.User, error) {
	handle := make([]byte, 32)
	if _, err := rand.Read(handle); err != nil {
		return db.User{}, err
	}
	return s.store.EnsureUser(ctx, ownerName, handle)
}

// startCeremony stores c and sets the ceremony cookie.
func (s *Service) startCeremony(w http.ResponseWriter, c ceremony) error {
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	now := s.now()
	c.expires = now.Add(CeremonyTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ceremonies) >= maxCeremonies {
		for k, v := range s.ceremonies {
			if now.After(v.expires) {
				delete(s.ceremonies, k)
			}
		}
		if len(s.ceremonies) >= maxCeremonies {
			return errBusy
		}
	}
	s.ceremonies[hash] = c
	http.SetCookie(w, &http.Cookie{Name: CeremonyCookie, Value: token, Path: "/", MaxAge: int(CeremonyTTL.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return nil
}

var errBusy = errors.New("too many login attempts in progress")

// takeCeremony removes and returns the ceremony of the request's cookie.
// A ceremony is used once, whatever the outcome.
func (s *Service) takeCeremony(w http.ResponseWriter, r *http.Request, kind string) (ceremony, bool) {
	http.SetCookie(w, &http.Cookie{Name: CeremonyCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	ck, err := r.Cookie(CeremonyCookie)
	if err != nil {
		return ceremony{}, false
	}
	hash := HashToken(ck.Value)
	s.mu.Lock()
	c, ok := s.ceremonies[hash]
	delete(s.ceremonies, hash)
	s.mu.Unlock()
	if !ok || c.kind != kind || s.now().After(c.expires) {
		return ceremony{}, false
	}
	return c, true
}

// sameOrigin checks a state-changing browser request: Origin must be the
// dashboard, and Sec-Fetch-Site same-origin when the browser sends it.
func (s *Service) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Origin") != s.origin {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Handler serves the /auth/ endpoints and /preview-auth.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/status", s.status)
	mux.HandleFunc("POST /auth/enroll/begin", s.enrollBegin)
	mux.HandleFunc("POST /auth/enroll/finish", s.enrollFinish)
	mux.HandleFunc("POST /auth/login/begin", s.loginBegin)
	mux.HandleFunc("POST /auth/login/finish", s.loginFinish)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /preview-auth", s.previewAuth)
	return mux
}

// Status is the response of GET /auth/status.
type Status struct {
	LoggedIn bool `json:"logged_in"`
	// Enrolled is true once a passkey exists.
	Enrolled  bool   `json:"enrolled"`
	CSRFToken string `json:"csrf_token,omitempty"`
}

func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := s.store.CountPasskeys(ctx)
	if err != nil {
		s.logf("auth status: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	st := Status{Enrolled: n > 0}
	if se, ok := s.session(r); ok {
		st.LoggedIn, st.CSRFToken = true, se.CSRFToken
	}
	writeJSON(w, http.StatusOK, st)
}

type enrollRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

func decodeSmall(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Service) enrollBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin request")
		return
	}
	ctx := r.Context()
	var req enrollRequest
	if err := decodeSmall(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = "passkey"
	}
	if len(req.Name) > 64 {
		writeError(w, http.StatusBadRequest, "name is longer than 64 bytes")
		return
	}
	hash := HashToken(req.Token)
	if err := s.store.CheckEnrollToken(ctx, hash); err != nil {
		writeError(w, http.StatusForbidden, "this registration link is invalid or expired")
		return
	}
	u, err := s.owner(ctx)
	if err != nil {
		s.logf("enroll: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	wu, err := s.loadUser(ctx, u)
	if err != nil {
		s.logf("enroll: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range wu.creds {
		exclude = append(exclude, c.Descriptor())
	}
	opts, data, err := s.wa.BeginRegistration(wu,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude))
	if err != nil {
		s.logf("enroll: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.startCeremony(w, ceremony{kind: "register", data: *data, tokenHash: hash, name: req.Name}); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *Service) enrollFinish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin request")
		return
	}
	ctx := r.Context()
	c, ok := s.takeCeremony(w, r, "register")
	if !ok {
		writeError(w, http.StatusForbidden, "no registration in progress")
		return
	}
	// The link is used up now, whether the registration succeeds or not.
	if err := s.store.ConsumeEnrollToken(ctx, c.tokenHash); err != nil {
		writeError(w, http.StatusForbidden, "this registration link is invalid or expired")
		return
	}
	u, err := s.owner(ctx)
	if err != nil {
		s.logf("enroll: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	wu, err := s.loadUser(ctx, u)
	if err != nil {
		s.logf("enroll: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCeremonyBody)
	cred, err := s.wa.FinishRegistration(wu, c.data, r)
	if err != nil {
		s.logf("enroll: finish: %v", describe(err))
		writeError(w, http.StatusBadRequest, "the passkey could not be registered")
		return
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	pk := db.Passkey{UserID: u.ID, CredentialID: cred.ID, CredentialJSON: string(raw), Name: c.name}
	if err := s.store.AddPasskey(ctx, &pk); err != nil {
		s.logf("enroll: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.logf("passkey %q registered", c.name)
	if err := s.startSession(w, r, u.ID); err != nil {
		s.logf("enroll: session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) loginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin request")
		return
	}
	opts, data, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.logf("login: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.startCeremony(w, ceremony{kind: "login", data: *data}); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *Service) loginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin request")
		return
	}
	ctx := r.Context()
	c, ok := s.takeCeremony(w, r, "login")
	if !ok {
		writeError(w, http.StatusForbidden, "no login in progress")
		return
	}
	var found *waUser
	handler := func(_, userHandle []byte) (webauthn.User, error) {
		u, err := s.store.UserByHandle(ctx, userHandle)
		if err != nil {
			return nil, err
		}
		found, err = s.loadUser(ctx, u)
		if err != nil {
			return nil, err
		}
		return found, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCeremonyBody)
	_, cred, err := s.wa.FinishPasskeyLogin(handler, c.data, r)
	if err != nil || found == nil {
		s.logf("login failed: %v", describe(err))
		writeError(w, http.StatusForbidden, "login failed")
		return
	}
	if cred.Authenticator.CloneWarning {
		s.logf("login refused: the sign counter of a passkey went backwards")
		writeError(w, http.StatusForbidden, "login failed")
		return
	}
	raw, err := json.Marshal(cred)
	if err == nil {
		err = s.store.UpdatePasskeyUse(ctx, cred.ID, string(raw))
	}
	if err != nil {
		s.logf("login: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.startSession(w, r, found.u.ID); err != nil {
		s.logf("login: session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// describe adds go-webauthn's detail to an error for the log.
func describe(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Error() + ": " + pe.DevInfo
	}
	if err == nil {
		return "no user"
	}
	return err.Error()
}

func (s *Service) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	csrf, _, err := newToken()
	if err != nil {
		return err
	}
	ua := r.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	se := db.Session{IDHash: hash, UserID: userID, CSRFToken: csrf, ExpiresAt: s.now().Add(SessionTTL).UTC(), UserAgent: ua}
	if err := s.store.CreateSession(r.Context(), &se); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: token, Path: "/", MaxAge: int(SessionTTL.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return nil
}

// session returns the valid session of a request. A browser may send more
// than one cookie with the session name; any valid one counts.
func (s *Service) session(r *http.Request) (db.Session, bool) {
	for _, ck := range r.CookiesNamed(SessionCookie) {
		if ck.Value == "" || len(ck.Value) > 128 {
			continue
		}
		se, err := s.store.SessionByHash(r.Context(), HashToken(ck.Value))
		if err != nil {
			continue
		}
		if s.now().Sub(se.LastSeenAt) > touchInterval {
			if err := s.store.TouchSession(r.Context(), se.IDHash); err != nil {
				s.logf("touch session: %v", err)
			}
		}
		return se, true
	}
	return db.Session{}, false
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	se, ok := s.session(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	if !s.sameOrigin(r) || !validCSRF(r, se) {
		writeError(w, http.StatusForbidden, "cross-origin request or missing CSRF token")
		return
	}
	if err := s.store.DeleteSession(r.Context(), se.IDHash); err != nil {
		s.logf("logout: %v", err)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func validCSRF(r *http.Request, se db.Session) bool {
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(se.CSRFToken)) == 1
}

type ctxKey struct{}

// UserID returns the logged-in user of a request that passed Require.
func UserID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Require lets a request through only with a valid session. A WebSocket
// handshake also needs Origin = the dashboard, because handshakes carry no
// Sec-Fetch-Site; any other request that is not GET or HEAD also needs
// Sec-Fetch-Site same-origin (when sent) and the session's CSRF token.
func (s *Service) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		se, ok := s.session(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		switch {
		case isWebSocket(r):
			if r.Header.Get("Origin") != s.origin {
				writeError(w, http.StatusForbidden, "cross-origin request")
				return
			}
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
		default:
			if !s.sameOrigin(r) || !validCSRF(r, se) {
				writeError(w, http.StatusForbidden, "cross-origin request or missing CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, se.UserID)))
	})
}

func isWebSocket(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// PreviewHost returns the preview host of a sandbox, or false when the
// slugs cannot form one: each must be free of "--", and the label must fit
// in 63 bytes.
func PreviewHost(sandboxSlug, projectSlug, domain string) (string, bool) {
	if domain == "" || sandboxSlug == "" || projectSlug == "" ||
		strings.Contains(sandboxSlug, "--") || strings.Contains(projectSlug, "--") {
		return "", false
	}
	label := sandboxSlug + "--" + projectSlug
	if len(label) > 63 {
		return "", false
	}
	return label + "." + domain, true
}

// ParsePreviewHost splits a preview host into its sandbox and project
// slugs. host may carry a port, which is ignored.
func ParsePreviewHost(host, domain string) (sandboxSlug, projectSlug string, ok bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if domain == "" {
		return "", "", false
	}
	label, found := strings.CutSuffix(host, "."+domain)
	if !found || strings.Contains(label, ".") || !ValidHost(label) {
		return "", "", false
	}
	if strings.Count(label, "--") != 1 {
		return "", "", false
	}
	sandboxSlug, projectSlug, _ = strings.Cut(label, "--")
	if sandboxSlug == "" || projectSlug == "" {
		return "", "", false
	}
	return sandboxSlug, projectSlug, true
}

// SafeNext returns next when it is a local path, and "/" otherwise.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || len(next) > 2048 {
		return "/"
	}
	for _, c := range next {
		if c == '\\' || c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	return next
}

// previewAuth hands access to one preview host: it mints a one-time token
// bound to that host and redirects there. Without a session it sends the
// browser to the login page first.
func (s *Service) previewAuth(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if _, _, ok := ParsePreviewHost(host, s.cfg.PreviewDomain); !ok || strings.Contains(host, ":") {
		writeError(w, http.StatusBadRequest, "not a preview host")
		return
	}
	next := SafeNext(r.URL.Query().Get("next"))
	se, ok := s.session(r)
	if !ok {
		back := "/preview-auth?" + url.Values{"host": {host}, "next": {next}}.Encode()
		http.Redirect(w, r, "/login?next="+url.QueryEscape(back), http.StatusSeeOther)
		return
	}
	token, hash, err := newToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.CreatePreviewToken(r.Context(), hash, se.UserID, host, s.now().Add(PreviewTokenTTL).UTC()); err != nil {
		s.logf("preview auth: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	target := "https://" + host + "/__wsm/auth?" + url.Values{"token": {token}, "next": {next}}.Encode()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ConsumePreviewToken exchanges a preview token for a preview session on
// host and returns the session cookie value.
func (s *Service) ConsumePreviewToken(ctx context.Context, token, host string) (string, error) {
	if token == "" || len(token) > 128 {
		return "", fmt.Errorf("preview token: %w", db.ErrNotFound)
	}
	userID, err := s.store.ConsumePreviewToken(ctx, HashToken(token), host)
	if err != nil {
		return "", err
	}
	value, hash, err := newToken()
	if err != nil {
		return "", err
	}
	ps := db.PreviewSession{IDHash: hash, UserID: userID, Host: host, ExpiresAt: s.now().Add(PreviewTTL).UTC()}
	if err := s.store.CreatePreviewSession(ctx, &ps); err != nil {
		return "", err
	}
	return value, nil
}

// PreviewSessionValid reports whether a preview cookie value is valid for
// host.
func (s *Service) PreviewSessionValid(ctx context.Context, value, host string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	ok, err := s.store.PreviewSessionValid(ctx, HashToken(value), host)
	if err != nil {
		s.logf("preview session: %v", err)
	}
	return ok
}

// PreviewDomain returns the configured preview domain.
func (s *Service) PreviewDomain() string { return s.cfg.PreviewDomain }

// Cleanup removes expired sessions and tokens every interval until ctx
// ends.
func (s *Service) Cleanup(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.store.DeleteExpiredAuth(ctx); err != nil && ctx.Err() == nil {
			s.logf("auth cleanup: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
