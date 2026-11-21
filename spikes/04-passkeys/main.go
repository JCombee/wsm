// Command pkspike is spike 04-passkeys: go-webauthn with a strict RP ID,
// __Host- cookies and preview access, driven by headless Chromium with a
// virtual authenticator. Throwaway code, never part of the main module. It
// runs only on a disposable VM, through run.sh.
//
//	pkspike gencert -dir D
//	pkspike run -certs D -mode nss|spki|ignore [-quick] [-nosandbox] [-portmap]
//
// The modes spki, ignore and -nosandbox exist only to measure what the
// browser accepts; they are never used for the criteria.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	cdpwa "github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		log.Fatal("usage: s04pre gencert|run ...")
	}
	switch os.Args[1] {
	case "gencert":
		fs := flag.NewFlagSet("gencert", flag.ExitOnError)
		dir := fs.String("dir", "", "output dir")
		_ = fs.Parse(os.Args[2:])
		if err := gencert(*dir); err != nil {
			log.Fatal(err)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		certs := fs.String("certs", "", "cert dir")
		mode := fs.String("mode", "nss", "nss|spki|ignore")
		quick := fs.Bool("quick", false, "only TLS and registration")
		noSandbox := fs.Bool("nosandbox", false, "pass --no-sandbox")
		portmap := fs.Bool("portmap", false, "map :443 to 127.0.0.1:8443 in the resolver rules")
		chrome := fs.String("chrome", "/usr/bin/chromium", "browser binary")
		_ = fs.Parse(os.Args[2:])
		if err := run(*certs, *mode, *chrome, *quick, *noSandbox, *portmap); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func result(id, status, format string, a ...any) {
	fmt.Printf("RESULT\t%s\t%s\t%s\n", id, status, fmt.Sprintf(format, a...))
}

func check(id string, ok bool, format string, a ...any) {
	st := "FAIL"
	if ok {
		st = "PASS"
	}
	result(id, st, format, a...)
}

// ---------- certificates ----------

func gencert(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wsm spike04 preflight CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "dash.wsm.test"},
		DNSNames:     []string{"dash.wsm.test", "*.preview.wsm.test", "*.wsm-preview.test"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(leafCert.RawSubjectPublicKeyInfo)
	files := map[string][]byte{
		"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"leaf.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"leaf.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		"spki.txt": []byte(base64.StdEncoding.EncodeToString(sum[:]) + "\n"),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---------- webauthn user ----------

type user struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u *user) WebAuthnID() []byte                         { return u.id }
func (u *user) WebAuthnName() string                       { return u.name }
func (u *user) WebAuthnDisplayName() string                { return u.name }
func (u *user) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func randID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- server ----------

type token struct {
	host string
	exp  time.Time
}

type server struct {
	strict, loose *webauthn.WebAuthn

	mu          sync.Mutex
	u, lu       *user
	sess        map[string]*webauthn.SessionData // ceremony name -> session
	dashSess    map[string]bool                  // valid __Host-wsm_session values
	tokens      map[string]token
	lastToken   string
	previewSess map[string]string // __Host-wsm_preview value -> host
	records     map[string]string // tag -> what arrived
	strictSeen  map[string]bool   // host -> strict cookie seen on first proxied request
	upstream    *url.URL
	rorOn       bool
}

func newServer(upstream *url.URL) (*server, error) {
	strict, err := webauthn.New(&webauthn.Config{
		RPDisplayName: "wsm",
		RPID:          "dash.wsm.test",
		RPOrigins:     []string{"https://dash.wsm.test"},
	})
	if err != nil {
		return nil, err
	}
	loose, err := webauthn.New(&webauthn.Config{
		RPDisplayName: "wsm-loose",
		RPID:          "wsm.test",
		RPOrigins:     []string{"https://dash.wsm.test"},
	})
	if err != nil {
		return nil, err
	}
	return &server{
		strict: strict, loose: loose,
		u:           &user{id: []byte(randID()[:16]), name: "owner"},
		lu:          &user{id: []byte(randID()[:16]), name: "owner-loose"},
		sess:        map[string]*webauthn.SessionData{},
		dashSess:    map[string]bool{},
		tokens:      map[string]token{},
		previewSess: map[string]string{},
		records:     map[string]string{},
		strictSeen:  map[string]bool{},
		upstream:    upstream,
	}, nil
}

func hostOnly(h string) string {
	if x, _, err := net.SplitHostPort(h); err == nil {
		return x
	}
	return h
}

func isPreviewHost(h string) bool {
	return strings.HasSuffix(h, ".preview.wsm.test") || strings.HasSuffix(h, ".wsm-preview.test")
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	log.Printf("srv %s %s%s", r.Method, host, r.URL.Path)
	switch {
	case host == "dash.wsm.test":
		s.dash(w, r)
	case isPreviewHost(host):
		s.preview(w, r, host)
	default:
		http.Error(w, "unknown host", http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) validDash(r *http.Request) bool {
	c, err := r.Cookie("__Host-wsm_session")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dashSess[c.Value]
}

func (s *server) dash(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.URL.Path {
	case "/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>dash</title><p>dash</p>")
	case "/info":
		la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		writeJSON(w, map[string]string{"local": fmt.Sprint(la), "host": r.Host, "proto": r.Proto})
	case "/reg/begin", "/loose/reg/begin":
		rp, u, key := s.strict, s.u, "reg"
		if strings.HasPrefix(r.URL.Path, "/loose") {
			rp, u, key = s.loose, s.lu, "lreg"
		}
		cc, sd, err := rp.BeginRegistration(u, webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.sess[key] = sd
		writeJSON(w, cc)
	case "/reg/finish", "/loose/reg/finish":
		rp, u, key := s.strict, s.u, "reg"
		if strings.HasPrefix(r.URL.Path, "/loose") {
			rp, u, key = s.loose, s.lu, "lreg"
		}
		sd := s.sess[key]
		delete(s.sess, key)
		if sd == nil {
			http.Error(w, "no session", 400)
			return
		}
		c, err := rp.FinishRegistration(u, *sd, r)
		if err != nil {
			http.Error(w, "finish: "+err.Error()+" "+protocolDetail(err), 400)
			return
		}
		u.creds = append(u.creds, *c)
		fmt.Fprint(w, "ok")
	case "/login/begin":
		ca, sd, err := s.strict.BeginLogin(s.u)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.sess["login"] = sd
		writeJSON(w, ca)
	case "/login/finish":
		sd := s.sess["login"]
		delete(s.sess, "login")
		if sd == nil {
			http.Error(w, "no session", 400)
			return
		}
		if _, err := s.strict.FinishLogin(s.u, *sd, r); err != nil {
			http.Error(w, "finish: "+err.Error()+" "+protocolDetail(err), 400)
			return
		}
		v := randID()
		s.dashSess[v] = true
		w.Header().Add("Set-Cookie", "__Host-wsm_session="+v+"; Path=/; Secure; HttpOnly; SameSite=Lax")
		fmt.Fprint(w, "ok")
	case "/echo-cookies":
		// Report cookie names, and for the session only whether it is valid.
		var parts []string
		for _, c := range r.Cookies() {
			if c.Name == "__Host-wsm_session" {
				parts = append(parts, fmt.Sprintf("%s=<valid:%v>", c.Name, s.dashSess[c.Value]))
				continue
			}
			parts = append(parts, c.Name+"="+c.Value)
		}
		sort.Strings(parts)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, strings.Join(parts, "; "))
	case "/record":
		tag := r.URL.Query().Get("tag")
		got := "none"
		if c, err := r.Cookie("__Host-wsm_session"); err == nil {
			got = fmt.Sprintf("session(valid=%v)", s.dashSess[c.Value])
		}
		s.records[tag] = got + " origin=" + r.Header.Get("Origin") + " sfs=" + r.Header.Get("Sec-Fetch-Site")
		w.WriteHeader(http.StatusNoContent)
	case "/.well-known/webauthn":
		if !s.rorOn {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string][]string{"origins": {"https://a--b.preview.wsm.test"}})
	case "/open":
		host := r.URL.Query().Get("host")
		if !isPreviewHost(host) {
			http.Error(w, "bad host", 400)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>open</title><a id="go" href="https://%s/app?x=1">open</a>`, host)
	case "/ws":
		tag := r.URL.Query().Get("tag")
		got := "none"
		if c, err := r.Cookie("__Host-wsm_session"); err == nil {
			got = fmt.Sprintf("session(valid=%v)", s.dashSess[c.Value])
		}
		s.records["ws-"+tag] = fmt.Sprintf("method=%s proto=%s upgrade=%q cookie=%s origin=%s sfs=%s", r.Method, r.Proto, r.Header.Get("Upgrade"), got, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, "forbidden origin", http.StatusForbidden)
	case "/api/action":
		// The M4 pattern: a state-changing call needs a valid session and a same-origin request.
		tag := r.URL.Query().Get("tag")
		c, err := r.Cookie("__Host-wsm_session")
		valid := err == nil && s.dashSess[c.Value]
		sameOrigin := r.Header.Get("Sec-Fetch-Site") == "same-origin" && r.Header.Get("Origin") == "https://dash.wsm.test"
		status := http.StatusOK
		if !valid || !sameOrigin || r.Method != http.MethodPost {
			status = http.StatusForbidden
		}
		s.records["api-"+tag] = fmt.Sprintf("status=%d session=%v origin=%s sfs=%s", status, valid, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
		w.WriteHeader(status)
		fmt.Fprint(w, status)
	case "/preview-auth":
		c, err := r.Cookie("__Host-wsm_session")
		if err != nil || !s.dashSess[c.Value] {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		host := r.URL.Query().Get("host")
		next := r.URL.Query().Get("next")
		if !isPreviewHost(host) || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
			http.Error(w, "bad request", 400)
			return
		}
		t := randID()
		s.tokens[t] = token{host: host, exp: time.Now().Add(60 * time.Second)}
		s.lastToken = t
		http.Redirect(w, r, "https://"+host+"/__wsm/auth?token="+url.QueryEscape(t)+"&next="+url.QueryEscape(next), http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func protocolDetail(err error) string {
	if pe, ok := err.(*protocol.Error); ok {
		return "[" + pe.Type + ": " + pe.Details + " / " + pe.DevInfo + "]"
	}
	return ""
}

func (s *server) mintToken(host string, ttl time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := randID()
	s.tokens[t] = token{host: host, exp: time.Now().Add(ttl)}
	return t
}

func (s *server) record(tag string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[tag]
}

func (s *server) preview(w http.ResponseWriter, r *http.Request, host string) {
	switch r.URL.Path {
	case "/__t/blank":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>preview</title><p>preview</p>")
		return
	case "/__t/toss":
		// Attempts by sandbox-controlled content on a sibling host.
		for _, c := range []string{
			"__Host-wsm_session=evil-domain; Domain=wsm.test; Path=/; Secure",
			"__Host-wsm_session=evil-hostonly; Path=/; Secure",
			"__Host-nosecure=1; Path=/",
			"__Host-badpath=1; Path=/x; Secure",
			"__HOST-wsm_session=evil-case; Domain=wsm.test; Path=/; Secure",
			"__Secure-wsm_session=toss-secure; Domain=wsm.test; Path=/; Secure",
			"wsm_session=toss-plain; Domain=wsm.test; Path=/; Secure",
		} {
			w.Header().Add("Set-Cookie", c)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>toss</title><p>tossed</p>")
		return
	case "/__t/toss2":
		w.Header().Add("Set-Cookie", "wsm_x=toss-preview; Domain=wsm-preview.test; Path=/; Secure")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>toss2</title><p>tossed</p>")
		return
	case "/__t/strict/begin":
		s.mu.Lock()
		defer s.mu.Unlock()
		ca, sd, err := s.strict.BeginLogin(s.u)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.sess["slogin"] = sd
		writeJSON(w, ca)
		return
	case "/__t/strict/finish":
		s.mu.Lock()
		defer s.mu.Unlock()
		sd := s.sess["slogin"]
		delete(s.sess, "slogin")
		if sd == nil {
			http.Error(w, "no session", 400)
			return
		}
		if _, err := s.strict.FinishLogin(s.u, *sd, r); err != nil {
			fmt.Fprint(w, "REJECTED: "+err.Error()+" "+protocolDetail(err))
			return
		}
		fmt.Fprint(w, "ACCEPTED")
		return
	case "/__t/loose/begin":
		s.mu.Lock()
		defer s.mu.Unlock()
		ca, sd, err := s.loose.BeginLogin(s.lu)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.sess["llogin"] = sd
		writeJSON(w, ca)
		return
	case "/__t/loose/finish":
		s.mu.Lock()
		defer s.mu.Unlock()
		sd := s.sess["llogin"]
		delete(s.sess, "llogin")
		if sd == nil {
			http.Error(w, "no session", 400)
			return
		}
		if _, err := s.loose.FinishLogin(s.lu, *sd, r); err != nil {
			fmt.Fprint(w, "REJECTED: "+err.Error()+" "+protocolDetail(err))
			return
		}
		fmt.Fprint(w, "ACCEPTED")
		return
	case "/__wsm/auth":
		s.mu.Lock()
		t := r.URL.Query().Get("token")
		tk, ok := s.tokens[t]
		delete(s.tokens, t) // single use, also on failure
		if !ok || tk.host != host || time.Now().After(tk.exp) {
			s.mu.Unlock()
			http.Error(w, "invalid token", http.StatusForbidden)
			return
		}
		v := randID()
		s.previewSess[v] = host
		s.mu.Unlock()
		next := r.URL.Query().Get("next")
		if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
			next = "/"
		}
		w.Header().Add("Set-Cookie", "__Host-wsm_preview="+v+"; Path=/; Secure; HttpOnly; SameSite=Lax")
		w.Header().Add("Set-Cookie", "__Host-wsm_preview_strict="+v+"; Path=/; Secure; HttpOnly; SameSite=Strict")
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	// Everything else goes through the proxy, only with a valid preview cookie.
	c, err := r.Cookie("__Host-wsm_preview")
	s.mu.Lock()
	ok := err == nil && s.previewSess[c.Value] == host
	if ok {
		if _, seen := s.strictSeen[host]; !seen {
			_, serr := r.Cookie("__Host-wsm_preview_strict")
			s.strictSeen[host] = serr == nil
		}
	}
	s.mu.Unlock()
	if !ok {
		http.Redirect(w, r, "https://dash.wsm.test/preview-auth?host="+url.QueryEscape(host)+"&next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(s.upstream)
		stripCookies(pr.Out, "__Host-wsm_preview", "__Host-wsm_preview_strict")
	}}
	rp.ServeHTTP(w, r)
}

func stripCookies(r *http.Request, names ...string) {
	cs := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cs {
		if !slices.Contains(names, c.Name) {
			r.AddCookie(c)
		}
	}
}

// ---------- browser driver ----------

func run(certDir, mode, chrome string, quick, noSandbox, portmap bool) error {
	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "leaf.pem"), filepath.Join(certDir, "leaf.key"))
	if err != nil {
		return err
	}
	spki, err := os.ReadFile(filepath.Join(certDir, "spki.txt"))
	if err != nil {
		return err
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var names []string
		for _, c := range r.Cookies() {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "upstream path=%s cookies=%v", r.URL.RequestURI(), names)
	}))
	defer up.Close()
	upURL, _ := url.Parse(up.URL)

	s, err := newServer(upURL)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	for _, addr := range []string{"127.0.0.1:443", "127.0.0.1:8443"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			result("LISTEN-"+addr, "FAIL", "%v", err)
			continue
		}
		go func() { _ = srv.ServeTLS(ln, "", "") }()
	}
	defer srv.Close()

	rules := "MAP *.wsm.test 127.0.0.1, MAP *.wsm-preview.test 127.0.0.1"
	if portmap {
		rules = "MAP *.wsm.test:443 127.0.0.1:8443, MAP *.wsm-preview.test:443 127.0.0.1:8443"
	}
	udd, err := os.MkdirTemp("", "s04udd-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(udd)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.UserDataDir(udd),
		chromedp.Flag("host-resolver-rules", rules),
	)
	switch mode {
	case "nss":
	case "spki":
		opts = append(opts, chromedp.Flag("ignore-certificate-errors-spki-list", strings.TrimSpace(string(spki))))
	case "ignore":
		opts = append(opts, chromedp.IgnoreCertErrors)
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	if noSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	actx, acancel := chromedp.NewExecAllocator(ctx, opts...)
	defer acancel()
	bctx, bcancel := chromedp.NewContext(actx, chromedp.WithLogf(log.Printf))
	defer bcancel()

	ver, err := chromedp.CallBrowser(bctx, browser.GetVersion, cdp.Empty{})
	if err != nil {
		result("BROWSER-START", "FAIL", "%v", err)
		return nil
	}
	result("BROWSER-START", "PASS", "%s %s mode=%s nosandbox=%v portmap=%v", ver.Product, ver.UserAgent, mode, noSandbox, portmap)

	p := "[" + mode + "] "
	if err := chromedp.Do(bctx, chromedp.Navigate("https://dash.wsm.test/")); err != nil {
		result("TLS-NAV", "FAIL", p+"%v", err)
		return nil
	}
	origin, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`location.origin`))
	secure, _ := chromedp.Run(bctx, chromedp.Evaluate[bool](`window.isSecureContext && typeof PublicKeyCredential === 'function' && typeof PublicKeyCredential.parseCreationOptionsFromJSON === 'function'`))
	info, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`fetch('/info').then(r => r.text())`, chromedp.EvalAwaitPromise))
	check("TLS-NAV", origin == "https://dash.wsm.test" && secure, p+"origin=%s secure+pkc+json=%v info=%s", origin, secure, strings.TrimSpace(info))

	if _, err := chromedp.Call(bctx, cdpwa.Enable, cdpwa.EnableParams{}); err != nil {
		result("WA-ENABLE", "FAIL", p+"%v", err)
		return nil
	}
	va, err := chromedp.Call(bctx, cdpwa.AddVirtualAuthenticator, cdpwa.AddVirtualAuthenticatorParams{Options: &cdpwa.VirtualAuthenticatorOptions{
		Protocol:                    cdpwa.AuthenticatorProtocolCtap2,
		Ctap2version:                cdpwa.Ctap2versionCtap21,
		Transport:                   cdpwa.AuthenticatorTransportInternal,
		HasResidentKey:              true,
		HasUserVerification:         true,
		IsUserVerified:              true,
		AutomaticPresenceSimulation: true,
	}})
	if err != nil {
		result("WA-ENABLE", "FAIL", p+"add authenticator: %v", err)
		return nil
	}
	result("WA-ENABLE", "PASS", p+"authenticator %s", va.AuthenticatorID)

	reg, err := chromedp.Run(bctx, chromedp.Evaluate[string](regJS(""), chromedp.EvalAwaitPromise))
	check("PK-REGISTER", err == nil && reg == "finish 200 ok", p+"%q err=%v", reg, err)
	if quick {
		return nil
	}
	creds, err := chromedp.Call(bctx, cdpwa.GetCredentials, cdpwa.GetCredentialsParams{AuthenticatorID: va.AuthenticatorID})
	if err == nil {
		for _, c := range creds.Credentials {
			result("PK-CREDS", "INFO", "rpId=%s resident=%v signCount=%v", c.RpID, c.IsResidentCredential, c.SignCount)
		}
	}

	login, err := chromedp.Run(bctx, chromedp.Evaluate[string](loginJS, chromedp.EvalAwaitPromise))
	check("PK-LOGIN", err == nil && login == "finish 200 ok", "%q err=%v", login, err)

	s.mu.Lock()
	credID := base64.StdEncoding.EncodeToString(s.u.creds[0].ID)
	s.mu.Unlock()

	for _, h := range []string{"a--b.preview.wsm.test", "a--b.wsm-preview.test"} {
		if err := chromedp.Do(bctx, chromedp.Navigate("https://"+h+"/__t/blank")); err != nil {
			result("PK-PREVIEW-RPID", "FAIL", "%s nav: %v", h, err)
			continue
		}
		out, err := chromedp.Run(bctx, chromedp.Evaluate[string](fmt.Sprintf(previewGetJS, credID), chromedp.EvalAwaitPromise))
		check("PK-PREVIEW-RPID", err == nil && strings.HasPrefix(out, "SecurityError"), "%s -> %q err=%v", h, out, err)
	}

	// Loose RP (rpId wsm.test): the browser lets a sibling use it; the server must reject the origin.
	if err := chromedp.Do(bctx, chromedp.Navigate("https://dash.wsm.test/")); err == nil {
		lreg, err := chromedp.Run(bctx, chromedp.Evaluate[string](regJS("/loose"), chromedp.EvalAwaitPromise))
		result("PK-LOOSE-REG", "INFO", "%q err=%v", lreg, err)
	}
	if err := chromedp.Do(bctx, chromedp.Navigate("https://a--b.preview.wsm.test/__t/blank")); err == nil {
		out, err := chromedp.Run(bctx, chromedp.Evaluate[string](looseAttackJS, chromedp.EvalAwaitPromise))
		check("PK-SERVER-ORIGIN", err == nil && strings.HasPrefix(out, "finish 200 REJECTED"), "%q err=%v", out, err)
	}

	// __Host- cookies against a sibling host.
	if err := chromedp.Do(bctx, chromedp.Navigate("https://a--b.preview.wsm.test/__t/toss")); err != nil {
		result("COOKIE-TOSS", "FAIL", "nav: %v", err)
	}
	jar, err := chromedp.Call(bctx, network.GetCookies, network.GetCookiesParams{URLs: []string{"https://dash.wsm.test/", "https://a--b.preview.wsm.test/"}})
	if err != nil {
		result("COOKIE-JAR", "FAIL", "%v", err)
	} else {
		for _, c := range jar.Cookies {
			v := c.Value
			if c.Name == "__Host-wsm_session" && c.Domain == "dash.wsm.test" {
				v = "<real>"
			}
			result("COOKIE-JAR", "INFO", "%s=%s domain=%s path=%s secure=%v httponly=%v samesite=%s", c.Name, v, c.Domain, c.Path, c.Secure, c.HTTPOnly, c.SameSite)
		}
	}
	if err := chromedp.Do(bctx, chromedp.Navigate("https://dash.wsm.test/echo-cookies")); err == nil {
		body, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body.innerText`))
		check("COOKIE-DASH-SEES", strings.Contains(body, "__Host-wsm_session=<valid:true>") && strings.Count(body, "__Host-wsm_session=") == 1 && !strings.Contains(body, "evil"), "%q", body)
	}

	// SameSite=Lax: same-site sibling vs separate registrable domain.
	for _, tc := range []struct{ host, tag string }{{"a--b.preview.wsm.test", "same"}, {"a--b.wsm-preview.test", "cross"}} {
		if err := chromedp.Do(bctx, chromedp.Navigate("https://"+tc.host+"/__t/blank")); err != nil {
			continue
		}
		_, _ = chromedp.Run(bctx, chromedp.Evaluate[string](`fetch('https://dash.wsm.test/record?tag=`+tc.tag+`', {mode: 'no-cors', credentials: 'include', method: 'POST'}).then(() => 'sent', e => 'err ' + e)`, chromedp.EvalAwaitPromise))
		s.mu.Lock()
		got := s.records[tc.tag]
		s.mu.Unlock()
		result("SAMESITE-"+strings.ToUpper(tc.tag), "INFO", "%s -> dash POST no-cors: %s", tc.host, got)
	}

	// Preview login flow with a one-time token and a host-only cookie.
	for _, h := range []string{"a--b.wsm-preview.test", "c--d.preview.wsm.test"} {
		if err := chromedp.Do(bctx, chromedp.Navigate("https://"+h+"/app?x=1")); err != nil {
			result("PREVIEW-FLOW", "FAIL", "%s nav: %v", h, err)
			continue
		}
		loc, _ := chromedp.Run(bctx, chromedp.Location())
		body, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body.innerText`))
		dc, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.cookie`))
		check("PREVIEW-FLOW", loc == "https://"+h+"/app?x=1" && strings.HasPrefix(body, "upstream path=/app?x=1 cookies=[]") && dc == "", "%s loc=%s body=%q document.cookie=%q", h, loc, body, dc)
		s.mu.Lock()
		strictSeen := s.strictSeen[h]
		last := s.lastToken
		s.mu.Unlock()
		result("PREVIEW-STRICT", "INFO", "%s SameSite=Strict cookie on first proxied request after cross-site redirect: %v", h, strictSeen)
		if err := chromedp.Do(bctx, chromedp.Navigate("https://"+h+"/__wsm/auth?token="+url.QueryEscape(last)+"&next=/")); err == nil {
			b, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body.innerText`))
			check("PREVIEW-REPLAY", strings.Contains(b, "invalid token"), "%s %q", h, b)
		}
		other := s.mintToken("x--y.wsm-preview.test", time.Minute)
		if err := chromedp.Do(bctx, chromedp.Navigate("https://"+h+"/__wsm/auth?token="+url.QueryEscape(other)+"&next=/")); err == nil {
			b, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body.innerText`))
			check("PREVIEW-WRONGHOST", strings.Contains(b, "invalid token"), "%s %q", h, b)
		}
	}
	// ---- round 2 ----
	// Registration from a preview origin with the dashboard RP ID.
	if err := chromedp.Do(bctx, chromedp.Navigate("https://a--b.preview.wsm.test/__t/blank")); err == nil {
		out, err := chromedp.Run(bctx, chromedp.Evaluate[string](previewCreateJS, chromedp.EvalAwaitPromise))
		check("PK-PREVIEW-CREATE", err == nil && strings.HasPrefix(out, "SecurityError"), "%q err=%v", out, err)
		out, err = chromedp.Run(bctx, chromedp.Evaluate[string](previewOwnRPJS, chromedp.EvalAwaitPromise))
		result("PK-PREVIEW-OWNRP", "INFO", "%q err=%v", out, err)
	}
	// WebSocket handshakes and the CSRF pattern from both layouts.
	for _, tc := range []struct{ host, tag string }{{"a--b.preview.wsm.test", "same"}, {"a--b.wsm-preview.test", "cross"}} {
		if err := chromedp.Do(bctx, chromedp.Navigate("https://"+tc.host+"/__t/blank")); err != nil {
			continue
		}
		out, _ := chromedp.Run(bctx, chromedp.Evaluate[string](fmt.Sprintf(wsJS, tc.tag), chromedp.EvalAwaitPromise))
		result("WS-"+strings.ToUpper(tc.tag), "INFO", "%s client=%s server: %s", tc.host, out, s.record("ws-"+tc.tag))
		_, _ = chromedp.Run(bctx, chromedp.Evaluate[string](`fetch('https://dash.wsm.test/api/action?tag=`+tc.tag+`', {mode: 'no-cors', credentials: 'include', method: 'POST'}).then(() => 'sent', e => 'err ' + e)`, chromedp.EvalAwaitPromise))
		result("CSRF-"+strings.ToUpper(tc.tag), "INFO", "%s -> %s", tc.host, s.record("api-"+tc.tag))
	}
	if err := chromedp.Do(bctx, chromedp.Navigate("https://dash.wsm.test/")); err == nil {
		out, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`fetch('/api/action?tag=self', {method: 'POST'}).then(r => String(r.status), e => 'err ' + e)`, chromedp.EvalAwaitPromise))
		result("CSRF-SELF", "INFO", "client=%s server: %s", out, s.record("api-self"))
		out, _ = chromedp.Run(bctx, chromedp.Evaluate[string](fmt.Sprintf(wsJS, "self"), chromedp.EvalAwaitPromise))
		result("WS-SELF", "INFO", "client=%s server: %s", out, s.record("ws-self"))
	}
	// Preview flow started by a link click on the dashboard (renderer-initiated, cross-site).
	for _, h := range []string{"e--f.wsm-preview.test", "g--h.preview.wsm.test"} {
		if err := chromedp.Do(bctx, chromedp.Navigate("https://dash.wsm.test/open?host="+h)); err != nil {
			result("PREVIEW-CLICK", "FAIL", "%s open: %v", h, err)
			continue
		}
		_, _ = chromedp.Run(bctx, chromedp.Evaluate[chromedp.Void](`document.getElementById('go').click()`))
		want := "https://" + h + "/app?x=1"
		loc := ""
		for i := 0; i < 50; i++ {
			time.Sleep(200 * time.Millisecond)
			loc, _ = chromedp.Run(bctx, chromedp.Location())
			if loc == want {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
		body, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body ? document.body.innerText : ''`))
		s.mu.Lock()
		strictSeen, seen := s.strictSeen[h]
		s.mu.Unlock()
		check("PREVIEW-CLICK", loc == want && strings.HasPrefix(body, "upstream path=/app?x=1"), "%s loc=%s body=%q", h, loc, body)
		result("PREVIEW-CLICK-STRICT", "INFO", "%s Strict cookie on first proxied request: %v (recorded=%v)", h, strictSeen, seen)
	}
	// Expired token.
	if err := chromedp.Do(bctx, chromedp.Navigate("https://a--b.wsm-preview.test/__wsm/auth?token="+url.QueryEscape(s.mintToken("a--b.wsm-preview.test", -time.Second))+"&next=/")); err == nil {
		b, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body.innerText`))
		check("PREVIEW-EXPIRED", strings.Contains(b, "invalid token"), "%q", b)
	}
	// Cross-preview cookie tossing in the separate-domain layout.
	if err := chromedp.Do(bctx, chromedp.Navigate("https://a--b.wsm-preview.test/__t/toss2")); err == nil {
		if err := chromedp.Do(bctx, chromedp.Navigate("https://a--b.wsm-preview.test/app?y=1")); err == nil {
			body, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body.innerText`))
			result("PREVIEW-CROSS-TOSS", "INFO", "a--b app sees: %q", body)
		}
		if err := chromedp.Do(bctx, chromedp.Navigate("https://e--f.wsm-preview.test/app?y=1")); err == nil {
			body, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`document.body.innerText`))
			result("PREVIEW-CROSS-TOSS", "INFO", "e--f app sees: %q", body)
		}
	}
	// Misconfigured Related Origin Requests: the dashboard lists a preview origin.
	s.mu.Lock()
	s.rorOn = true
	s.mu.Unlock()
	if err := chromedp.Do(bctx, chromedp.Navigate("https://a--b.preview.wsm.test/__t/blank")); err == nil {
		out, err := chromedp.Run(bctx, chromedp.Evaluate[string](strictAttackJS, chromedp.EvalAwaitPromise))
		result("PK-ROR-MISCONFIG", "INFO", "%q err=%v", out, err)
	}
	return nil
}

func regJS(prefix string) string {
	return `(async () => {
  const b = await fetch('` + prefix + `/reg/begin', {method: 'POST'});
  if (!b.ok) return 'begin ' + b.status + ' ' + (await b.text()).trim();
  const o = await b.json();
  let c;
  try {
    c = await navigator.credentials.create({publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(o.publicKey)});
  } catch (e) { return 'create ' + e.name + ': ' + e.message; }
  const f = await fetch('` + prefix + `/reg/finish', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(c.toJSON())});
  return 'finish ' + f.status + ' ' + (await f.text()).trim();
})()`
}

const loginJS = `(async () => {
  const b = await fetch('/login/begin', {method: 'POST'});
  if (!b.ok) return 'begin ' + b.status + ' ' + (await b.text()).trim();
  const o = await b.json();
  let c;
  try {
    c = await navigator.credentials.get({publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(o.publicKey)});
  } catch (e) { return 'get ' + e.name + ': ' + e.message; }
  const f = await fetch('/login/finish', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(c.toJSON())});
  return 'finish ' + f.status + ' ' + (await f.text()).trim();
})()`

const previewGetJS = `(async () => {
  try {
    await navigator.credentials.get({publicKey: {
      challenge: crypto.getRandomValues(new Uint8Array(32)),
      rpId: 'dash.wsm.test',
      timeout: 5000,
      allowCredentials: [{type: 'public-key', id: Uint8Array.from(atob('%s'), c => c.charCodeAt(0))}],
    }});
    return 'resolved';
  } catch (e) { return e.name + ': ' + e.message; }
})()`

const looseAttackJS = `(async () => {
  const b = await fetch('/__t/loose/begin', {method: 'POST'});
  if (!b.ok) return 'begin ' + b.status + ' ' + (await b.text()).trim();
  const o = await b.json();
  let c;
  try {
    c = await navigator.credentials.get({publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(o.publicKey)});
  } catch (e) { return 'get ' + e.name + ': ' + e.message; }
  const f = await fetch('/__t/loose/finish', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(c.toJSON())});
  return 'finish ' + f.status + ' ' + (await f.text()).trim();
})()`

const previewCreateJS = `(async () => {
  try {
    await navigator.credentials.create({publicKey: {
      challenge: crypto.getRandomValues(new Uint8Array(32)),
      rp: {id: 'dash.wsm.test', name: 'evil'},
      user: {id: crypto.getRandomValues(new Uint8Array(16)), name: 'x', displayName: 'x'},
      pubKeyCredParams: [{type: 'public-key', alg: -7}],
      timeout: 5000,
    }});
    return 'resolved';
  } catch (e) { return e.name + ': ' + e.message; }
})()`

const previewOwnRPJS = `(async () => {
  const t0 = performance.now();
  try {
    await navigator.credentials.get({publicKey: {challenge: crypto.getRandomValues(new Uint8Array(32)), timeout: 5000}});
    return 'resolved';
  } catch (e) { return e.name + ': ' + e.message + ' after ' + Math.round(performance.now() - t0) + ' ms'; }
})()`

const wsJS = `new Promise(res => {
  const ws = new WebSocket('wss://dash.wsm.test/ws?tag=%s');
  ws.onopen = () => res('open');
  ws.onerror = () => res('error');
  setTimeout(() => res('timeout'), 3000);
})`

const strictAttackJS = `(async () => {
  const b = await fetch('/__t/strict/begin', {method: 'POST'});
  if (!b.ok) return 'begin ' + b.status + ' ' + (await b.text()).trim();
  const o = await b.json();
  let c;
  try {
    c = await navigator.credentials.get({publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(o.publicKey)});
  } catch (e) { return 'get ' + e.name + ': ' + e.message; }
  const f = await fetch('/__t/strict/finish', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(c.toJSON())});
  return 'finish ' + f.status + ' ' + (await f.text()).trim();
})()`
