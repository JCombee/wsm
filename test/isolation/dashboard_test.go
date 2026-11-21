//go:build isolation

package isolation

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// The run script starts wsm with these settings (/etc/wsm/wsm.env):
// --dashboard-host dash.wsm.test --preview-domain wsm-preview.test. The
// tests speak plain HTTP to the loopback listeners, as Caddy would.
const (
	dashHost      = "dash.wsm.test"
	dashOrigin    = "https://" + dashHost
	previewDomain = "wsm-preview.test"
	dashAddr      = "127.0.0.1:8080"
	previewAddr   = "127.0.0.1:8081"
)

// webClient sends dash.wsm.test to the dashboard listener and every
// preview host to the preview listener, and never follows redirects.
var webClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			target := dashAddr
			if strings.HasSuffix(host, "."+previewDomain) {
				target = previewAddr
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, target)
		},
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type webReq struct {
	method, url string
	body        any
	headers     map[string]string
	cookies     []*http.Cookie
}

func doWeb(t *testing.T, r webReq) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if r.body != nil {
		switch b := r.body.(type) {
		case string:
			body = strings.NewReader(b)
		default:
			data, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(data)
		}
	}
	method := r.method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctxT(t), method, r.url, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	for _, c := range r.cookies {
		req.AddCookie(c)
	}
	resp, err := webClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, r.url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, string(data)
}

func cookieOf(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var sameOrigin = map[string]string{"Origin": dashOrigin, "Sec-Fetch-Site": "same-origin"}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// softKey is a software passkey: what a browser and an authenticator do,
// so the tests can drive the real ceremonies.
type softKey struct {
	priv       *ecdsa.PrivateKey
	credID     []byte
	userHandle string
	count      uint32
}

const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
)

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return b
}

func authData(rpID string, flags byte, count uint32, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append(h[:], flags)
	out = binary.BigEndian.AppendUint32(out, count)
	return append(out, attested...)
}

type creationOptions struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"publicKey"`
}

type requestOptions struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		RPID      string `json:"rpId"`
	} `json:"publicKey"`
}

// register answers a creation request as a browser on origin would.
func (k *softKey) register(t *testing.T, opts creationOptions, origin string) map[string]any {
	t.Helper()
	pub, err := k.priv.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	cose, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: pub[1:33], -3: pub[33:65]})
	if err != nil {
		t.Fatal(err)
	}
	attested := make([]byte, 16) // AAGUID: zero
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(k.credID)))
	attested = append(attested, k.credID...)
	attested = append(attested, cose...)
	ad := authData(opts.PublicKey.RP.ID, flagUP|flagUV|flagAT, 0, attested)
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	if err != nil {
		t.Fatal(err)
	}
	k.userHandle = opts.PublicKey.User.ID
	return map[string]any{
		"id": b64(k.credID), "rawId": b64(k.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64(clientData("webauthn.create", opts.PublicKey.Challenge, origin)),
			"attestationObject": b64(att),
		},
	}
}

// assert answers a request as a browser on origin would.
func (k *softKey) assert(t *testing.T, opts requestOptions, origin string) map[string]any {
	t.Helper()
	k.count++
	cd := clientData("webauthn.get", opts.PublicKey.Challenge, origin)
	ad := authData(opts.PublicKey.RPID, flagUP|flagUV, k.count, nil)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64(k.credID), "rawId": b64(k.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64(cd),
			"authenticatorData": b64(ad),
			"signature":         b64(sig),
			"userHandle":        k.userHandle,
		},
	}
}

func newSoftKey(t *testing.T) *softKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	return &softKey{priv: priv, credID: id}
}

// enrollToken runs "wsm auth enroll" and returns the token from the link.
func enrollToken(t *testing.T) string {
	t.Helper()
	out, err := wsm(ctxT(t), "auth", "enroll")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`https://` + regexp.QuoteMeta(dashHost) + `/enroll\?token=(\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no enroll link in %q", out)
	}
	tok, err := url.QueryUnescape(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// session is a logged-in dashboard session.
type session struct {
	cookie *http.Cookie
	csrf   string
}

func (se session) headers() map[string]string {
	return map[string]string{"Origin": dashOrigin, "Sec-Fetch-Site": "same-origin", "X-WSM-CSRF": se.csrf}
}

func statusOf(t *testing.T, c *http.Cookie) (loggedIn bool, csrf string) {
	t.Helper()
	var cookies []*http.Cookie
	if c != nil {
		cookies = append(cookies, c)
	}
	resp, body := doWeb(t, webReq{url: "http://" + dashHost + "/auth/status", cookies: cookies})
	if resp.StatusCode != 200 {
		t.Fatalf("auth status: %d %s", resp.StatusCode, body)
	}
	var st struct {
		LoggedIn  bool   `json:"logged_in"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	return st.LoggedIn, st.CSRFToken
}

var (
	dashOnce sync.Once
	dashKey  *softKey
	dashSess session
	dashErr  string
)

// dashSession enrols a software passkey once and returns a session.
func dashSession(t *testing.T) session {
	t.Helper()
	dashOnce.Do(func() {
		if _, err := net.DialTimeout("tcp", dashAddr, 5*time.Second); err != nil {
			dashErr = fmt.Sprintf("the dashboard does not listen on %s; start wsm with --dashboard-host %s: %v", dashAddr, dashHost, err)
			return
		}
		dashKey = newSoftKey(t)
		tok := enrollToken(t)
		resp, body := doWeb(t, webReq{method: "POST", url: "http://" + dashHost + "/auth/enroll/begin",
			body: map[string]string{"token": tok, "name": "isolation"}, headers: sameOrigin})
		if resp.StatusCode != 200 {
			dashErr = fmt.Sprintf("enroll begin: %d %s", resp.StatusCode, body)
			return
		}
		var opts creationOptions
		json.Unmarshal([]byte(body), &opts)
		cer := cookieOf(resp, "__Host-wsm_ceremony")
		resp, body = doWeb(t, webReq{method: "POST", url: "http://" + dashHost + "/auth/enroll/finish",
			body: dashKey.register(t, opts, dashOrigin), headers: sameOrigin, cookies: []*http.Cookie{cer}})
		c := cookieOf(resp, "__Host-wsm_session")
		if resp.StatusCode != 204 || c == nil {
			dashErr = fmt.Sprintf("enroll finish: %d %s", resp.StatusCode, body)
			return
		}
		_, csrf := statusOf(t, c)
		dashSess = session{cookie: c, csrf: csrf}
	})
	if dashErr != "" {
		t.Fatal(dashErr)
	}
	return dashSess
}

// loginWith runs a discoverable login with the key, as a browser on origin.
func loginWith(t *testing.T, k *softKey, origin string) (*http.Response, string) {
	t.Helper()
	resp, body := doWeb(t, webReq{method: "POST", url: "http://" + dashHost + "/auth/login/begin", body: "{}", headers: sameOrigin})
	if resp.StatusCode != 200 {
		t.Fatalf("login begin: %d %s", resp.StatusCode, body)
	}
	var opts requestOptions
	json.Unmarshal([]byte(body), &opts)
	if opts.PublicKey.RPID != dashHost {
		t.Fatalf("rpId %q, want %q", opts.PublicKey.RPID, dashHost)
	}
	cer := cookieOf(resp, "__Host-wsm_ceremony")
	return doWeb(t, webReq{method: "POST", url: "http://" + dashHost + "/auth/login/finish",
		body: k.assert(t, opts, origin), headers: sameOrigin, cookies: []*http.Cookie{cer}})
}

func TestDashboardRequiresSession(t *testing.T) {
	dashSession(t)
	for _, tc := range []struct {
		url  string
		want int
	}{
		{"http://" + dashHost + "/api/v1/projects", 401},
		{"http://" + dashHost + "/.well-known/webauthn", 404},
		{"http://evil.test/auth/status", 421},
	} {
		resp, body := doWeb(t, webReq{url: tc.url})
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d: %s", tc.url, resp.StatusCode, tc.want, body)
		}
	}
	// A request with the plain loopback address as host is refused too
	// (DNS rebinding).
	resp, err := http.Get("http://" + dashAddr + "/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("loopback host: %d", resp.StatusCode)
	}
}

func TestPasskeyLoginAndCookies(t *testing.T) {
	se := dashSession(t)
	if !se.cookie.Secure || !se.cookie.HttpOnly || se.cookie.SameSite != http.SameSiteLaxMode ||
		se.cookie.Path != "/" || se.cookie.Domain != "" {
		t.Errorf("session cookie %+v", se.cookie)
	}
	// A login whose client data names a preview origin is refused.
	resp, body := loginWith(t, dashKey, "https://main--iso-a."+previewDomain)
	if resp.StatusCode != 403 || cookieOf(resp, "__Host-wsm_session") != nil {
		t.Errorf("login from a preview origin: %d %s", resp.StatusCode, body)
	}
	// An unknown key is refused.
	if resp, body := loginWith(t, newSoftKey(t), dashOrigin); resp.StatusCode != 403 {
		t.Errorf("login with an unknown key: %d %s", resp.StatusCode, body)
	}
	resp, body = loginWith(t, dashKey, dashOrigin)
	c := cookieOf(resp, "__Host-wsm_session")
	if resp.StatusCode != 204 || c == nil {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	if ok, _ := statusOf(t, c); !ok {
		t.Error("not logged in after login")
	}
	// The enroll link works once.
	tok := enrollToken(t)
	for i, want := range []int{200, 403} {
		resp, body := doWeb(t, webReq{method: "POST", url: "http://" + dashHost + "/auth/enroll/begin",
			body: map[string]string{"token": tok}, headers: sameOrigin})
		if i == 0 && resp.StatusCode == 200 {
			// Use the link up by finishing with a bad answer.
			cer := cookieOf(resp, "__Host-wsm_ceremony")
			doWeb(t, webReq{method: "POST", url: "http://" + dashHost + "/auth/enroll/finish", body: "{}",
				headers: sameOrigin, cookies: []*http.Cookie{cer}})
		}
		if resp.StatusCode != want {
			t.Errorf("enroll begin #%d: %d, want %d: %s", i+1, resp.StatusCode, want, body)
		}
	}
}

func TestDashboardCSRFAndOrigin(t *testing.T) {
	se := dashSession(t)
	u := "http://" + dashHost + "/api/v1/projects"
	// An invalid project body gets 400 once the request is through auth.
	bad := map[string]string{"slug": "Not Valid"}
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no csrf", map[string]string{"Origin": dashOrigin, "Sec-Fetch-Site": "same-origin"}, 403},
		{"wrong csrf", map[string]string{"Origin": dashOrigin, "X-WSM-CSRF": "x"}, 403},
		{"preview origin", map[string]string{"Origin": "https://main--iso-a." + previewDomain, "Sec-Fetch-Site": "cross-site", "X-WSM-CSRF": se.csrf}, 403},
		{"same-site", map[string]string{"Origin": dashOrigin, "Sec-Fetch-Site": "same-site", "X-WSM-CSRF": se.csrf}, 403},
		{"ok", se.headers(), 400},
	} {
		resp, body := doWeb(t, webReq{method: "POST", url: u, body: bad, headers: tc.headers, cookies: []*http.Cookie{se.cookie}})
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d: %s", tc.name, resp.StatusCode, tc.want, body)
		}
	}
	resp, body := doWeb(t, webReq{url: u, cookies: []*http.Cookie{se.cookie}})
	if resp.StatusCode != 200 || !strings.Contains(body, `"iso-a"`) {
		t.Errorf("list projects: %d %s", resp.StatusCode, body)
	}
}

func TestTerminalWebSocketOrigin(t *testing.T) {
	se := dashSession(t)
	ctx := ctxT(t)
	u := "ws://" + dashAddr + "/api/v1/projects/iso-a/sandboxes/main/terminal?rows=24&cols=80"
	hdr := func(origin string) http.Header {
		h := http.Header{"Cookie": {se.cookie.Name + "=" + se.cookie.Value}}
		if origin != "" {
			h.Set("Origin", origin)
		}
		return h
	}
	for _, origin := range []string{"https://main--iso-a." + previewDomain, "https://evil.test", ""} {
		c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{Host: dashHost, HTTPHeader: hdr(origin)})
		if err == nil {
			c.CloseNow()
			t.Errorf("terminal with Origin %q: connected", origin)
			continue
		}
		if resp == nil || resp.StatusCode != 403 {
			t.Errorf("terminal with Origin %q: %v", origin, err)
		}
	}
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{Host: dashHost, HTTPHeader: hdr(dashOrigin)})
	if err != nil {
		t.Fatalf("terminal from the dashboard origin: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("echo wsm-$((6*7))\r")); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(seen.String(), "wsm-42") {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, data, err := c.Read(rctx)
		cancel()
		if err != nil {
			break
		}
		seen.Write(data)
	}
	if !strings.Contains(seen.String(), "wsm-42") {
		t.Errorf("no command output over the dashboard terminal: %q", lastBytes(seen.String(), 500))
	}
	c.Close(websocket.StatusNormalClosure, "")
}

func TestFilesThroughHelper(t *testing.T) {
	se := dashSession(t)
	ctx := ctxT(t)
	base := "http://" + dashHost + "/api/v1/projects/iso-a/sandboxes/main/files"
	path := "/workspace/iso-file.txt"
	content := []byte("hello from the dashboard\n")
	resp, body := doWeb(t, webReq{method: "PUT", url: base + "/content?path=" + url.QueryEscape(path),
		body: map[string][]byte{"content": content}, headers: se.headers(), cookies: []*http.Cookie{se.cookie}})
	if resp.StatusCode != 200 {
		t.Fatalf("write: %d %s", resp.StatusCode, body)
	}
	out, err := inSandbox(ctx, s.a, "stat", "-c", "%u:%g %a", path)
	if err != nil || strings.TrimSpace(out) != "1000:1000 644" {
		t.Errorf("written file: %q %v", out, err)
	}
	if out, _ := inSandbox(ctx, s.a, "cat", path); out != string(content) {
		t.Errorf("content in the sandbox: %q", out)
	}
	resp, body = doWeb(t, webReq{url: base + "/content?path=" + url.QueryEscape(path), cookies: []*http.Cookie{se.cookie}})
	var fc struct {
		Content []byte `json:"content"`
	}
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &fc) != nil || !bytes.Equal(fc.Content, content) {
		t.Errorf("read: %d %s", resp.StatusCode, body)
	}
	resp, body = doWeb(t, webReq{url: base + "/list?path=/workspace", cookies: []*http.Cookie{se.cookie}})
	if resp.StatusCode != 200 || !strings.Contains(body, `"iso-file.txt"`) {
		t.Errorf("list: %d %s", resp.StatusCode, body)
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/workspace/../etc/passwd", 400}, // not clean
		{"workspace", 400},                // not absolute
		{"/etc/shadow", 422},              // the sandbox user cannot read it
		{"/workspace/nope", 404},
	} {
		resp, body := doWeb(t, webReq{url: base + "/content?path=" + url.QueryEscape(tc.path), cookies: []*http.Cookie{se.cookie}})
		if resp.StatusCode != tc.want {
			t.Errorf("read %s: %d, want %d: %s", tc.path, resp.StatusCode, tc.want, body)
		}
	}
	// The helper is read-only and the only host path in the sandbox.
	if out, err := inSandbox(ctx, s.a, "sh", "-c", "touch /opt/wsm/bin/x"); err == nil {
		t.Errorf("the sandbox wrote to /opt/wsm/bin: %s", out)
	}
	if out, err := inSandbox(ctx, s.a, "ls", "/opt/wsm/bin"); err != nil || strings.TrimSpace(out) != "wsm-fs" {
		t.Errorf("/opt/wsm/bin holds %q (%v), want only wsm-fs", out, err)
	}
}
