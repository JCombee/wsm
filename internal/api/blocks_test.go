package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/broker"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/podman/podmantest"
	"github.com/JCombee/wsm/internal/sandbox"
)

// fakeServices is a fake wsm-helper whose ensured instances answer with a
// MariaDB greeting for their tag.
type fakeServices struct {
	mu        sync.Mutex
	ready     map[int]string
	passwords []string
}

func (f *fakeServices) PullServiceImage(_ context.Context, _, tag string) error {
	switch tag {
	case "99.9":
		return fmt.Errorf("helper pull_service_image: %w: manifest unknown", helper.ErrNotFound)
	case "98.9":
		return errors.New("toomanyrequests")
	}
	return nil
}

func (f *fakeServices) EnsureServiceInstance(_ context.Context, _, tag string, port int, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready[port] = tag + ".9-MariaDB"
	f.passwords = append(f.passwords, password)
	return nil
}

func (f *fakeServices) AllowService(context.Context, int, int) error  { return nil }
func (f *fakeServices) RevokeService(context.Context, int, int) error { return nil }

func (f *fakeServices) dial(_ context.Context, port int) (net.Conn, error) {
	f.mu.Lock()
	v, ok := f.ready[port]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		payload := append(append([]byte{0x0a}, v...), 0, 1, 2, 3)
		n := len(payload)
		server.Write(append([]byte{byte(n), byte(n >> 8), byte(n >> 16), 0}, payload...))
	}()
	return client, nil
}

func TestBlocksAPI(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	store := db.NewStore(sqlDB)
	lp := podmantest.New(t, http.HandlerFunc(okLibpod))
	fake := &fakeServices{ready: map[int]string{}}
	s := &Server{
		Manager: &sandbox.Manager{Store: store, Users: nopUsers{},
			Podman: func(int) *podman.Client { return lp.Client() }, Secrets: testBox(t), Logf: t.Logf},
		Broker: &broker.Broker{Store: store, Helper: fake, Secrets: testBox(t),
			Addr: netip.MustParseAddr("10.255.0.1"), Dial: fake.dial,
			ReadyTimeout: 100 * time.Millisecond, PollInterval: 10 * time.Millisecond, Logf: t.Logf},
		UserName: func(uid int) string { return fmt.Sprintf("wsm-p-%d", uid) },
		Logf:     t.Logf,
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	c := NewClientDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	if _, err := c.CreateProject(ctx, CreateProjectRequest{Slug: "app", Image: "img"}); err != nil {
		t.Fatal(err)
	}

	var bodies []string
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		bodies = append(bodies, string(data))
		return resp.StatusCode, string(data)
	}

	// PUT, GET, services, DELETE.
	b, err := c.SetBlock(ctx, "app", "mariadb", SetBlockRequest{Tags: []string{"11.8", "12.3"}})
	if err != nil {
		t.Fatalf("SetBlock: %v", err)
	}
	if b.Block != "mariadb" || b.Profile != "dev" || strings.Join(b.Tags, ",") != "11.8,12.3" {
		t.Errorf("SetBlock = %+v", b)
	}
	if code, body := call(http.MethodPut, "/v1/projects/app/blocks/mariadb", `{"profile":"dev","tags":["11.8","12.3"]}`); code != 200 {
		t.Errorf("PUT: %d %s", code, body)
	}
	if code, body := call(http.MethodGet, "/v1/projects/app/blocks", ""); code != 200 ||
		body != `[{"block":"mariadb","profile":"dev","tags":["11.8","12.3"]}]`+"\n" {
		t.Errorf("GET blocks: %d %s", code, body)
	}
	code, body := call(http.MethodGet, "/v1/services", "")
	if code != 200 || !strings.Contains(body, `"address":"10.255.0.1:20000"`) ||
		!strings.Contains(body, `"state":"ready","version":"12.3.9-MariaDB"`) {
		t.Errorf("GET services: %d %s", code, body)
	}
	svcs, err := c.Services(ctx)
	if err != nil || len(svcs) != 2 || svcs[0].Tag != "11.8" || svcs[1].Port != 20001 {
		t.Errorf("Services = %+v, %v", svcs, err)
	}
	if code, body := call(http.MethodDelete, "/v1/projects/app/blocks/mariadb", ""); code != 204 {
		t.Errorf("DELETE: %d %s", code, body)
	}
	if bs, err := c.Blocks(ctx, "app"); err != nil || len(bs) != 0 {
		t.Errorf("Blocks after delete = %+v, %v", bs, err)
	}
	if code, _ := call(http.MethodDelete, "/v1/projects/app/blocks/mariadb", ""); code != 404 {
		t.Errorf("second DELETE: %d", code)
	}

	for _, tt := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"unknown project get", "GET", "/v1/projects/none/blocks", "", 404},
		{"unknown project put", "PUT", "/v1/projects/none/blocks/mariadb", `{"tags":["11.8"]}`, 404},
		{"unknown project delete", "DELETE", "/v1/projects/none/blocks/mariadb", "", 404},
		{"unknown block", "PUT", "/v1/projects/app/blocks/nosuchblock", `{"tags":["11.8"]}`, 400},
		{"redis below minimum", "PUT", "/v1/projects/app/blocks/redis", `{"tags":["7.1"]}`, 400},
		{"unknown profile", "PUT", "/v1/projects/app/blocks/mariadb", `{"profile":"prod","tags":["11.8"]}`, 400},
		{"bad tag", "PUT", "/v1/projects/app/blocks/mariadb", `{"tags":["latest"]}`, 400},
		{"old tag", "PUT", "/v1/projects/app/blocks/mariadb", `{"tags":["10.6"]}`, 400},
		{"no tags", "PUT", "/v1/projects/app/blocks/mariadb", `{"tags":[]}`, 400},
		{"unknown field", "PUT", "/v1/projects/app/blocks/mariadb", `{"tags":["11.8"],"image":"evil"}`, 400},
		{"too large", "PUT", "/v1/projects/app/blocks/mariadb", `{"tags":["11.8"],"profile":"` + strings.Repeat("a", maxBlockBody) + `"}`, 400},
		{"tag that does not exist", "PUT", "/v1/projects/app/blocks/mariadb", `{"tags":["99.9"]}`, 400},
		{"pull fails otherwise", "PUT", "/v1/projects/app/blocks/mariadb", `{"tags":["98.9"]}`, 500},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if code, body := call(tt.method, tt.path, tt.body); code != tt.want {
				t.Errorf("status %d, want %d: %s", code, tt.want, body)
			}
		})
	}
	if bs, _ := c.Blocks(ctx, "app"); len(bs) != 0 {
		t.Errorf("a rejected PUT changed the ceiling: %+v", bs)
	}
	if code, body := call(http.MethodPut, "/v1/projects/app/blocks/mariadb", `{"tags":["99.9"]}`); code != 400 ||
		!strings.Contains(body, "tag 99.9 of block mariadb does not exist") {
		t.Errorf("unknown tag: %d %s", code, body)
	}
	if svcs, err := c.Services(ctx); err != nil || len(svcs) != 2 {
		t.Errorf("an unknown tag left an instance: %+v, %v", svcs, err)
	}

	// No response carries a secret.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.passwords) == 0 {
		t.Fatal("no passwords seen")
	}
	for _, body := range bodies {
		low := strings.ToLower(body)
		if strings.Contains(low, "secret") || strings.Contains(low, "password") {
			t.Errorf("response mentions a secret: %s", body)
		}
		for _, pw := range fake.passwords {
			if strings.Contains(body, pw) {
				t.Errorf("response contains a password")
			}
		}
	}
}
