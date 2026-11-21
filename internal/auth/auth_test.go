package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/db"
)

const (
	dash    = "dash.wsm.test"
	preview = "wsm-preview.test"
	origin  = "https://" + dash
)

func newService(t *testing.T) (*Service, *db.Store) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	store := db.NewStore(sqlDB)
	s, err := New(store, Config{DashboardHost: dash, PreviewDomain: preview}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return s, store
}

// login creates a user and a session directly in the store and returns the
// cookie value and CSRF token.
func login(t *testing.T, s *Service, store *db.Store) (string, string) {
	t.Helper()
	u, err := s.owner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	se := db.Session{IDHash: hash, UserID: u.ID, CSRFToken: "csrf-" + hash[:8], ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.CreateSession(context.Background(), &se); err != nil {
		t.Fatal(err)
	}
	return token, se.CSRFToken
}

func TestNewRejectsBadConfig(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	store := db.NewStore(sqlDB)
	for _, cfg := range []Config{
		{DashboardHost: ""},
		{DashboardHost: "Dash.Example.com"},
		{DashboardHost: "dash.example.com:8443"},
		{DashboardHost: "dash.example.com", PreviewDomain: "example.com"},
		{DashboardHost: "dash.example.com", PreviewDomain: "dash.example.com"},
		{DashboardHost: "dash.example.com", PreviewDomain: "-bad.com"},
	} {
		if _, err := New(store, cfg, nil); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	if _, err := New(store, Config{DashboardHost: "dash.example.com", PreviewDomain: "preview.example.com"}, nil); err != nil {
		t.Errorf("same registrable domain: %v", err)
	}
}

func TestPreviewHost(t *testing.T) {
	tests := []struct {
		sbx, proj string
		ok        bool
	}{
		{"main", "shop", true},
		{"a-b", "c-d", true},
		{"a--b", "shop", false},
		{"main", "s--p", false},
		{strings.Repeat("a", 32), strings.Repeat("b", 32), false},
		{"", "shop", false},
	}
	for _, tt := range tests {
		host, ok := PreviewHost(tt.sbx, tt.proj, preview)
		if ok != tt.ok {
			t.Errorf("PreviewHost(%q, %q) ok = %v", tt.sbx, tt.proj, ok)
			continue
		}
		if !ok {
			continue
		}
		sbx, proj, ok := ParsePreviewHost(host, preview)
		if !ok || sbx != tt.sbx || proj != tt.proj {
			t.Errorf("ParsePreviewHost(%q) = %q, %q, %v", host, sbx, proj, ok)
		}
	}
	for _, host := range []string{
		"main.shop." + preview, "x.main--shop." + preview, "main--shop.other.test", preview,
		"--shop." + preview, "main--." + preview, "a--b--c." + preview, "MAIN--shop." + preview,
	} {
		if _, _, ok := ParsePreviewHost(host, preview); ok {
			t.Errorf("ParsePreviewHost(%q) accepted", host)
		}
	}
	if _, _, ok := ParsePreviewHost("main--shop."+preview+":443", preview); !ok {
		t.Error("a port should be ignored")
	}
}

func TestSafeNext(t *testing.T) {
	tests := map[string]string{
		"/app?x=1":                      "/app?x=1",
		"":                              "/",
		"app":                           "/",
		"//evil.test/":                  "/",
		"/\\evil.test":                  "/",
		"https://evil.test/":            "/",
		"/a\nb":                         "/",
		"/" + strings.Repeat("a", 3000): "/",
	}
	for in, want := range tests {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequire(t *testing.T) {
	s, store := newService(t)
	token, csrf := login(t, s, store)
	h := s.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserID(r.Context()) == "" {
			t.Error("no user in the context")
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	tests := []struct {
		name    string
		method  string
		cookie  string
		headers map[string]string
		want    int
	}{
		{name: "no session", method: "GET", want: 401},
		{name: "bad session", method: "GET", cookie: "nope", want: 401},
		{name: "get", method: "GET", cookie: token, want: 418},
		{name: "post ok", method: "POST", cookie: token,
			headers: map[string]string{"Origin": origin, "Sec-Fetch-Site": "same-origin", CSRFHeader: csrf}, want: 418},
		{name: "post without sec-fetch-site", method: "POST", cookie: token,
			headers: map[string]string{"Origin": origin, CSRFHeader: csrf}, want: 418},
		{name: "post without csrf", method: "POST", cookie: token,
			headers: map[string]string{"Origin": origin, "Sec-Fetch-Site": "same-origin"}, want: 403},
		{name: "post wrong csrf", method: "DELETE", cookie: token,
			headers: map[string]string{"Origin": origin, CSRFHeader: csrf + "x"}, want: 403},
		{name: "post from preview", method: "POST", cookie: token,
			headers: map[string]string{"Origin": "https://a--b." + preview, "Sec-Fetch-Site": "cross-site", CSRFHeader: csrf}, want: 403},
		{name: "post same-site", method: "POST", cookie: token,
			headers: map[string]string{"Origin": origin, "Sec-Fetch-Site": "same-site", CSRFHeader: csrf}, want: 403},
		{name: "post no origin", method: "PUT", cookie: token,
			headers: map[string]string{CSRFHeader: csrf}, want: 403},
		{name: "websocket ok", method: "GET", cookie: token,
			headers: map[string]string{"Upgrade": "websocket", "Origin": origin}, want: 418},
		{name: "websocket from preview", method: "GET", cookie: token,
			headers: map[string]string{"Upgrade": "websocket", "Origin": "https://a--b." + preview}, want: 403},
		{name: "websocket no origin", method: "GET", cookie: token,
			headers: map[string]string{"Upgrade": "websocket"}, want: 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "https://"+dash+"/api/v1/projects", nil)
			if tt.cookie != "" {
				r.AddCookie(&http.Cookie{Name: SessionCookie, Value: tt.cookie})
			}
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", w.Code, tt.want, w.Body)
			}
		})
	}
}

func TestRequireAcceptsAnyValidDuplicate(t *testing.T) {
	s, store := newService(t)
	token, _ := login(t, s, store)
	h := s.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest("GET", "https://"+dash+"/api/x", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "planted"})
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func post(t *testing.T, h http.Handler, path, body string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "https://"+dash+path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var sameOriginHeaders = map[string]string{"Origin": origin, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}

func TestEnrollBegin(t *testing.T) {
	s, _ := newService(t)
	h := s.Handler()
	link, _, err := s.NewEnrollToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host != dash || u.Path != "/enroll" {
		t.Fatalf("link %q", link)
	}
	token := u.Query().Get("token")

	if w := post(t, h, "/auth/enroll/begin", `{"token":"`+token+`"}`, map[string]string{"Origin": "https://evil.test"}); w.Code != 403 {
		t.Errorf("cross-origin begin: %d", w.Code)
	}
	if w := post(t, h, "/auth/enroll/begin", `{"token":"wrong"}`, sameOriginHeaders); w.Code != 403 {
		t.Errorf("wrong token: %d", w.Code)
	}
	w := post(t, h, "/auth/enroll/begin", `{"token":"`+token+`","name":"laptop"}`, sameOriginHeaders)
	if w.Code != 200 {
		t.Fatalf("begin: %d %s", w.Code, w.Body)
	}
	var opts struct {
		PublicKey struct {
			RP struct {
				ID string `json:"id"`
			} `json:"rp"`
			AuthenticatorSelection struct {
				ResidentKey string `json:"residentKey"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatal(err)
	}
	if opts.PublicKey.RP.ID != dash || opts.PublicKey.AuthenticatorSelection.ResidentKey != "required" {
		t.Errorf("options %+v", opts)
	}
	ck := w.Result().Cookies()
	if len(ck) != 1 || ck[0].Name != CeremonyCookie || !ck[0].Secure || !ck[0].HttpOnly ||
		ck[0].SameSite != http.SameSiteStrictMode || ck[0].Path != "/" || ck[0].Domain != "" {
		t.Errorf("ceremony cookie %+v", ck)
	}
	// Finishing a login with a registration ceremony fails, and uses it up.
	if w := post(t, h, "/auth/login/finish", `{}`, sameOriginHeaders, ck[0]); w.Code != 403 {
		t.Errorf("login finish with a registration ceremony: %d", w.Code)
	}
	if w := post(t, h, "/auth/enroll/finish", `{}`, sameOriginHeaders, ck[0]); w.Code != 403 {
		t.Errorf("reused ceremony: %d", w.Code)
	}
}

func TestLoginBegin(t *testing.T) {
	s, _ := newService(t)
	h := s.Handler()
	if w := post(t, h, "/auth/login/begin", ``, map[string]string{"Origin": origin, "Sec-Fetch-Site": "same-site"}); w.Code != 403 {
		t.Errorf("same-site begin: %d", w.Code)
	}
	w := post(t, h, "/auth/login/begin", ``, sameOriginHeaders)
	if w.Code != 200 {
		t.Fatalf("begin: %d %s", w.Code, w.Body)
	}
	var opts struct {
		PublicKey struct {
			Challenge        string `json:"challenge"`
			RPID             string `json:"rpId"`
			AllowCredentials []any  `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatal(err)
	}
	if opts.PublicKey.Challenge == "" || opts.PublicKey.RPID != dash || len(opts.PublicKey.AllowCredentials) != 0 {
		t.Errorf("options %+v", opts)
	}
	// A garbage assertion is refused.
	ck := w.Result().Cookies()[0]
	if w := post(t, h, "/auth/login/finish", `{"id":"x"}`, sameOriginHeaders, ck); w.Code != 403 {
		t.Errorf("garbage finish: %d", w.Code)
	}
}

func TestStatusAndLogout(t *testing.T) {
	s, store := newService(t)
	h := s.Handler()
	get := func(cookie string) Status {
		r := httptest.NewRequest("GET", "https://"+dash+"/auth/status", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var st Status
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := get(""); st.LoggedIn || st.Enrolled || st.CSRFToken != "" {
		t.Errorf("anonymous status %+v", st)
	}
	token, csrf := login(t, s, store)
	if st := get(token); !st.LoggedIn || st.CSRFToken != csrf {
		t.Errorf("status %+v", st)
	}
	ck := &http.Cookie{Name: SessionCookie, Value: token}
	if w := post(t, h, "/auth/logout", ``, sameOriginHeaders, ck); w.Code != 403 {
		t.Errorf("logout without csrf: %d", w.Code)
	}
	hdr := map[string]string{"Origin": origin, CSRFHeader: csrf}
	if w := post(t, h, "/auth/logout", ``, hdr, ck); w.Code != 204 {
		t.Errorf("logout: %d", w.Code)
	}
	if st := get(token); st.LoggedIn {
		t.Error("still logged in after logout")
	}
}

func TestPreviewAuth(t *testing.T) {
	s, store := newService(t)
	h := s.Handler()
	host := "main--shop." + preview
	do := func(rawQuery, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://"+dash+"/preview-auth?"+rawQuery, nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	q := url.Values{"host": {host}, "next": {"/app?x=1"}}.Encode()
	w := do(q, "")
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Fatalf("anonymous: %d %q", w.Code, w.Header().Get("Location"))
	}
	token, _ := login(t, s, store)
	for _, bad := range []string{"evil.test", "main--shop." + preview + ":444", "a.b." + preview, dash} {
		if w := do(url.Values{"host": {bad}}.Encode(), token); w.Code != 400 {
			t.Errorf("host %q: %d", bad, w.Code)
		}
	}
	w = do(q, token)
	loc, err := url.Parse(w.Header().Get("Location"))
	if w.Code != 303 || err != nil || loc.Scheme != "https" || loc.Host != host || loc.Path != "/__wsm/auth" ||
		loc.Query().Get("next") != "/app?x=1" {
		t.Fatalf("redirect %d %q", w.Code, w.Header().Get("Location"))
	}
	pt := loc.Query().Get("token")
	ctx := context.Background()
	if _, err := s.ConsumePreviewToken(ctx, pt, "other--shop."+preview); err == nil {
		t.Error("token accepted for another host")
	}
	// The failed attempt used the token up.
	if _, err := s.ConsumePreviewToken(ctx, pt, host); err == nil {
		t.Error("token accepted after a failed attempt")
	}

	pt2 := mintPreviewToken(t, s, token, host)
	value, err := s.ConsumePreviewToken(ctx, pt2, host)
	if err != nil {
		t.Fatal(err)
	}
	if !s.PreviewSessionValid(ctx, value, host) || s.PreviewSessionValid(ctx, value, "other--shop."+preview) {
		t.Error("preview session not bound to its host")
	}
	if _, err := s.ConsumePreviewToken(ctx, pt2, host); err == nil {
		t.Error("token replayed")
	}

	pt3 := mintPreviewToken(t, s, token, host)
	s.now = func() time.Time { return time.Now().Add(2 * PreviewTokenTTL) }
	store.Now = s.now
	if _, err := s.ConsumePreviewToken(ctx, pt3, host); err == nil {
		t.Error("expired token accepted")
	}
}

func mintPreviewToken(t *testing.T, s *Service, session, host string) string {
	t.Helper()
	r := httptest.NewRequest("GET", "https://"+dash+"/preview-auth?"+url.Values{"host": {host}}.Encode(), nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query().Get("token")
}

func TestEnrollTokenSingleUse(t *testing.T) {
	s, store := newService(t)
	ctx := context.Background()
	link, _, err := s.NewEnrollToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	hash := HashToken(u.Query().Get("token"))
	if err := store.ConsumeEnrollToken(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if err := store.ConsumeEnrollToken(ctx, hash); err == nil {
		t.Error("enroll token used twice")
	}
}
