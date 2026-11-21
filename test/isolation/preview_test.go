//go:build isolation

package isolation

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	previewSlug = "iso-p"
	appPort     = 8080
)

// httpEchoProbe serves HTTP on the given port inside a workspace
// container: it echoes the Cookie and Host headers it got, and tries to set
// the proxy's own cookie.
func httpEchoProbe(port string) int {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Host-wsm_preview", Value: "from-the-app", Path: "/", Secure: true})
		http.SetCookie(w, &http.Cookie{Name: "app", Value: "1", Path: "/"})
		fmt.Fprintf(w, "cookie=%s\nhost=%s\nproto=%s\n", r.Header.Get("Cookie"), r.Host, r.Header.Get("X-Forwarded-Proto"))
	})
	if err := http.ListenAndServe("0.0.0.0:"+port, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// previewProject creates a project with an app port, a running sandbox and
// the echo server in it.
func previewProject(ctx context.Context, t *testing.T) project {
	t.Helper()
	p, err := createProjectFrom(ctx, s, previewSlug, "--image", testImage, "--app-port", strconv.Itoa(appPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wsm(context.Background(), "project", "delete", previewSlug) })
	if err := shareImage(ctx, s, s.a, p); err != nil {
		t.Fatal(err)
	}
	if err := createSandbox(ctx, s, &p); err != nil {
		t.Fatal(err)
	}
	installProbe(ctx, t, p)
	if _, err := asUser(ctx, p, "podman", "exec", "-d", "-e", probeEnv+"=httpecho:"+strconv.Itoa(appPort),
		p.container, probeInSandbox); err != nil {
		t.Fatal(err)
	}
	return p
}

// hostPort returns the 127.0.0.1 port where p's pod publishes the app.
func hostPort(ctx context.Context, t *testing.T, p project) int {
	t.Helper()
	out, err := asUser(ctx, p, "podman", "port", p.container)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(fmt.Sprintf(`%d/tcp -> 127\.0\.0\.1:(\d+)`, appPort)).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no 127.0.0.1 port mapping for %d: %q", appPort, out)
	}
	port, _ := strconv.Atoi(m[1])
	return port
}

func TestPreview(t *testing.T) {
	se := dashSession(t)
	ctx := ctxImage(t)
	p := previewProject(ctx, t)
	host := "main--" + previewSlug + "." + previewDomain

	env, err := containerEnv(ctx, p, p.container)
	if err != nil {
		t.Fatal(err)
	}
	if got := env["WSM_PREVIEW_URL"]; got != "https://"+host+"/" {
		t.Errorf("WSM_PREVIEW_URL = %q", got)
	}

	// The port is published on 127.0.0.1 only; root reaches it, other
	// project users do not.
	port := hostPort(ctx, t, p)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: root cannot reach the published app port %s: %v", addr, err)
		}
		time.Sleep(time.Second)
	}
	for _, other := range []project{s.a, s.b} {
		blockedBy(t, "loopback_blocked", other.slug+" user to the preview port", func() (bool, string) {
			return probeAs(ctx, other.user, "tcp:"+addr)
		})
	}

	// Without a cookie: off to the dashboard.
	resp, body := doWeb(t, webReq{url: "http://" + host + "/echo?x=1"})
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 303 || loc == nil || loc.Host != dashHost || loc.Path != "/preview-auth" ||
		loc.Query().Get("host") != host || loc.Query().Get("next") != "/echo?x=1" {
		t.Fatalf("no cookie: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	// The dashboard hands out a one-time token for that host.
	resp, _ = doWeb(t, webReq{url: "http://" + dashHost + loc.RequestURI(), cookies: []*http.Cookie{se.cookie}})
	tokURL, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 303 || tokURL == nil || tokURL.Host != host || tokURL.Path != "/__wsm/auth" {
		t.Fatalf("preview-auth: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Another host cannot use it, and the failed try uses it up.
	other := "other--" + previewSlug + "." + previewDomain
	if resp, _ := doWeb(t, webReq{url: "http://" + other + tokURL.RequestURI()}); resp.StatusCode != 403 {
		t.Errorf("token on another host: %d", resp.StatusCode)
	}
	if resp, _ := doWeb(t, webReq{url: "http://" + host + tokURL.RequestURI()}); resp.StatusCode != 403 {
		t.Errorf("token after a failed try: %d", resp.StatusCode)
	}
	// A fresh token gives the preview cookie.
	resp, _ = doWeb(t, webReq{url: "http://" + dashHost + loc.RequestURI(), cookies: []*http.Cookie{se.cookie}})
	tokURL, _ = url.Parse(resp.Header.Get("Location"))
	resp, _ = doWeb(t, webReq{url: "http://" + host + tokURL.RequestURI()})
	pc := cookieOf(resp, "__Host-wsm_preview")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/echo?x=1" || pc == nil {
		t.Fatalf("auth: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !pc.Secure || !pc.HttpOnly || pc.SameSite != http.SameSiteLaxMode || pc.Domain != "" || pc.Path != "/" {
		t.Errorf("preview cookie %+v", pc)
	}
	if resp, _ := doWeb(t, webReq{url: "http://" + host + tokURL.RequestURI()}); resp.StatusCode != 403 {
		t.Errorf("token replay: %d", resp.StatusCode)
	}

	// Through the proxy: the app answers, which also proves the
	// "ct state established,related" rule lets its replies out. The app
	// never sees the proxy's cookie, and cannot set it.
	req := webReq{url: "http://" + host + "/echo?x=1", headers: map[string]string{"Cookie": "theirs=1; " + pc.Name + "=" + pc.Value}}
	resp, body = doWeb(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("through the proxy: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "cookie=theirs=1\n") || strings.Contains(body, pc.Value) {
		t.Errorf("the app saw: %q", body)
	}
	if !strings.Contains(body, "host="+host+"\n") || !strings.Contains(body, "proto=https\n") {
		t.Errorf("forwarded headers: %q", body)
	}
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "__Host-wsm_preview=") {
			t.Errorf("the app set the proxy cookie: %q", sc)
		}
	}
	// The cookie belongs to this host only.
	resp, _ = doWeb(t, webReq{url: "http://" + other + "/", cookies: []*http.Cookie{pc}})
	if resp.StatusCode != 303 {
		t.Errorf("cookie on another preview host: %d", resp.StatusCode)
	}
	// /__wsm/ is never forwarded, and the dashboard session is no preview
	// session.
	if resp, _ := doWeb(t, webReq{url: "http://" + host + "/__wsm/x", cookies: []*http.Cookie{pc}}); resp.StatusCode != 404 {
		t.Errorf("/__wsm/x: %d", resp.StatusCode)
	}
	if resp, _ := doWeb(t, webReq{url: "http://" + host + "/echo", cookies: []*http.Cookie{se.cookie}}); resp.StatusCode != 303 {
		t.Errorf("dashboard cookie on the preview: %d", resp.StatusCode)
	}

	// A stopped sandbox has no preview.
	if _, err := wsm(ctx, "sandbox", "stop", previewSlug+"/main"); err != nil {
		t.Fatal(err)
	}
	resp, body = doWeb(t, req)
	if resp.StatusCode != 503 {
		t.Errorf("stopped sandbox: %d %s", resp.StatusCode, body)
	}
}
