package preview

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/auth"
	"github.com/JCombee/wsm/internal/db"
)

const (
	dash   = "dash.wsm.test"
	domain = "wsm-preview.test"
	host   = "main--shop." + domain
)

func TestStripCookie(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; __Host-wsm_preview=secret; b=2")
	h.Add("Cookie", "__Host-wsm_preview=x")
	StripCookie(h, auth.PreviewCookie)
	if got := h.Values("Cookie"); len(got) != 1 || got[0] != "a=1; b=2" {
		t.Errorf("Cookie = %q", got)
	}
	h = http.Header{"Cookie": {"__Host-wsm_preview=only"}}
	StripCookie(h, auth.PreviewCookie)
	if _, ok := h["Cookie"]; ok {
		t.Error("empty Cookie header kept")
	}
}

type env struct {
	proxy *Proxy
	auth  *auth.Service
	store *db.Store
	seen  chan *http.Request
}

func setup(t *testing.T, running bool) *env {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	store := db.NewStore(sqlDB)
	a, err := auth.New(store, auth.Config{DashboardHost: dash, PreviewDomain: domain}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{auth: a, store: store, seen: make(chan *http.Request, 10)}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.seen <- r.Clone(context.Background())
		http.SetCookie(w, &http.Cookie{Name: auth.PreviewCookie, Value: "app-set"})
		http.SetCookie(w, &http.Cookie{Name: "app", Value: "1"})
		io.WriteString(w, "hello from the app")
	}))
	t.Cleanup(app.Close)
	_, portStr, _ := net.SplitHostPort(app.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	e.proxy = New(a, func(_ context.Context, proj, sbx string) (int, bool, error) {
		if proj != "shop" || sbx != "main" {
			return 0, false, db.ErrNotFound
		}
		return port, running, nil
	}, t.Logf)
	return e
}

// grant walks the dashboard flow with a session made in the store and
// returns the preview cookie value.
func (e *env) grant(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	u, err := e.store.EnsureUser(ctx, "owner", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	token := "tok-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := e.store.CreatePreviewToken(ctx, auth.HashToken(token), u.ID, host, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://"+host+AuthPath+"?"+url.Values{"token": {token}, "next": {"/app?x=1"}}.Encode(), nil)
	w := httptest.NewRecorder()
	e.proxy.ServeHTTP(w, r)
	if w.Code != 303 || w.Header().Get("Location") != "/app?x=1" {
		t.Fatalf("auth: %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.PreviewCookie {
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
				t.Errorf("preview cookie %+v", c)
			}
			// Replaying the token fails.
			w := httptest.NewRecorder()
			e.proxy.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Errorf("replay: %d", w.Code)
			}
			return c.Value
		}
	}
	t.Fatal("no preview cookie")
	return ""
}

func TestProxyRedirectsWithoutSession(t *testing.T) {
	e := setup(t, true)
	r := httptest.NewRequest("GET", "https://"+host+"/app?x=1", nil)
	r.AddCookie(&http.Cookie{Name: auth.PreviewCookie, Value: "forged"})
	w := httptest.NewRecorder()
	e.proxy.ServeHTTP(w, r)
	loc, err := url.Parse(w.Header().Get("Location"))
	if w.Code != 303 || err != nil || loc.Host != dash || loc.Path != "/preview-auth" ||
		loc.Query().Get("host") != host || loc.Query().Get("next") != "/app?x=1" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	r = httptest.NewRequest("POST", "https://"+host+"/form", nil)
	w = httptest.NewRecorder()
	e.proxy.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("POST without session: %d", w.Code)
	}
	select {
	case <-e.seen:
		t.Error("the app got a request without a session")
	default:
	}
}

func TestProxyForwardsAndStripsCookie(t *testing.T) {
	e := setup(t, true)
	value := e.grant(t)
	r := httptest.NewRequest("GET", "https://"+host+"/app?x=1", nil)
	r.Header.Set("Cookie", "theirs=1; "+auth.PreviewCookie+"="+value)
	w := httptest.NewRecorder()
	e.proxy.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "hello from the app" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := <-e.seen
	if c := got.Header.Get("Cookie"); c != "theirs=1" {
		t.Errorf("the app saw Cookie %q", c)
	}
	if got.Host != host || got.Header.Get("X-Forwarded-Proto") != "https" || got.URL.RequestURI() != "/app?x=1" {
		t.Errorf("forwarded host %q proto %q uri %q", got.Host, got.Header.Get("X-Forwarded-Proto"), got.URL.RequestURI())
	}
	sc := w.Header().Values("Set-Cookie")
	if len(sc) != 1 || !strings.HasPrefix(sc[0], "app=1") {
		t.Errorf("Set-Cookie = %q", sc)
	}
	// The cookie belongs to this host only.
	r = httptest.NewRequest("GET", "https://other--shop."+domain+"/", nil)
	r.AddCookie(&http.Cookie{Name: auth.PreviewCookie, Value: value})
	w = httptest.NewRecorder()
	e.proxy.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Errorf("other host: %d", w.Code)
	}
}

func TestProxyReservedAndUnknown(t *testing.T) {
	e := setup(t, false)
	value := e.grant(t)
	for _, tt := range []struct {
		host, path string
		want       int
	}{
		{host, "/__wsm/other", 404},
		{"evil.test", "/", 404},
		{"a.b." + domain, "/", 404},
		{host, "/", 503},
		{"x--shop." + domain, "/", 303},
	} {
		r := httptest.NewRequest("GET", "https://"+tt.host+tt.path, nil)
		r.AddCookie(&http.Cookie{Name: auth.PreviewCookie, Value: value})
		w := httptest.NewRecorder()
		e.proxy.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Errorf("%s%s: %d, want %d", tt.host, tt.path, w.Code, tt.want)
		}
	}
}
