package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/JCombee/wsm/internal/auth"
	"github.com/JCombee/wsm/internal/db"
)

func TestHandler(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	a, err := auth.New(db.NewStore(sqlDB), auth.Config{DashboardHost: "dash.wsm.test"}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("api " + r.URL.Path)) })
	static := fstest.MapFS{
		"index.html":      {Data: []byte("<html>app</html>")},
		"assets/app.js":   {Data: []byte("js")},
		"assets/dir/x.js": {Data: []byte("x")},
	}
	h := Handler(a, api, static, "dash.wsm.test")
	tests := []struct {
		host, path string
		want       int
		body       string
	}{
		{"dash.wsm.test", "/", 200, "<html>app</html>"},
		{"dash.wsm.test", "/enroll", 200, "<html>app</html>"},
		{"dash.wsm.test", "/assets/app.js", 200, "js"},
		{"dash.wsm.test", "/.well-known/webauthn", 404, ""},
		{"dash.wsm.test", "/api/v1/projects", 401, ""},
		{"dash.wsm.test", "/auth/status", 200, `"logged_in":false`},
		{"evil.test", "/", 421, ""},
		{"127.0.0.1:8080", "/auth/status", 421, ""},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "http://"+tt.host+tt.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.want || !strings.Contains(w.Body.String(), tt.body) {
			t.Errorf("%s%s: %d %q, want %d %q", tt.host, tt.path, w.Code, w.Body, tt.want, tt.body)
		}
		if tt.want != 421 {
			if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
				t.Errorf("%s: CSP %q", tt.path, csp)
			}
			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Errorf("%s: no X-Frame-Options", tt.path)
			}
		}
	}
}
