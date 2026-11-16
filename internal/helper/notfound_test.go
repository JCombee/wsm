package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/blocks"
)

// pullOps is fakeOps whose pull goes through SystemOps.pullImage with a
// fake podman runner.
type pullOps struct {
	fakeOps
	sys *SystemOps
}

func (p *pullOps) PullServiceImage(ctx context.Context, service, tag string) error {
	b, err := blocks.LookupShared(service)
	if err != nil {
		return err
	}
	return p.sys.pullImage(ctx, account{name: ServiceUser, uid: 999, home: ServiceHome}, b, tag)
}

func TestPullNotFound(t *testing.T) {
	// The fake runner: the image never exists locally; pulling 99.9 fails
	// the way podman does for an unknown tag, 98.9 fails otherwise.
	runner := func(_ context.Context, _ account, _ *strings.Reader, args ...string) (string, error) {
		if args[0] == "image" {
			return "", errors.New("exit status 1")
		}
		switch {
		case strings.HasSuffix(args[len(args)-1], ":99.9"):
			out := "Error: initializing source docker://mariadb:99.9: reading manifest 99.9 in docker.io/library/mariadb: manifest unknown"
			return out, fmt.Errorf("podman pull: exit status 125: %s", out)
		case strings.HasSuffix(args[len(args)-1], ":98.9"):
			out := "Error: toomanyrequests: rate limit"
			return out, fmt.Errorf("podman pull: exit status 125: %s", out)
		}
		return "", nil
	}
	ops := &pullOps{sys: &SystemOps{runPodman: runner, Logf: t.Logf}}
	srv := &Server{Ops: ops, AllowedUID: uint32(os.Getuid()), Logf: t.Logf}

	t.Run("helper", func(t *testing.T) {
		raw := rawExchange(t, srv, []byte(`{"type":"pull_service_image","service":"mariadb","tag":"99.9"}`))
		var resp Response
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if resp.OK || resp.Code != CodeNotFound {
			t.Errorf("unknown tag: response %+v, want code %s", resp, CodeNotFound)
		}
		raw = rawExchange(t, srv, []byte(`{"type":"pull_service_image","service":"mariadb","tag":"98.9"}`))
		resp = Response{}
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.OK || resp.Code != "" {
			t.Errorf("other pull error: response %+v, want no code", resp)
		}
		if !strings.Contains(rawExchange(t, srv, []byte(`{"type":"pull_service_image","service":"mariadb","tag":"11.8"}`)), `"ok":true`) {
			t.Error("a good pull failed")
		}
	})

	t.Run("client", func(t *testing.T) {
		err := clientDo(t, srv, Request{Type: TypePullServiceImage, Service: "mariadb", Tag: "99.9"})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown tag: err = %v, want ErrNotFound", err)
		}
		err = clientDo(t, srv, Request{Type: TypePullServiceImage, Service: "mariadb", Tag: "98.9"})
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("other pull error: err = %v, want a plain error", err)
		}
		// An unknown code stays a plain error.
		for _, resp := range []string{`{"ok":false,"error":"x","code":"gone"}`, `{"ok":false,"error":"x"}`} {
			client, server := connPair(t)
			go func() {
				defer server.Close()
				server.SetDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, MaxMessage)
				server.Read(buf)
				server.Write([]byte(resp))
			}()
			req := Request{Type: TypeSetupWSM0}
			data, _ := json.Marshal(req)
			err := exchange(context.Background(), client, req, data)
			client.Close()
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("response %s: err = %v, want a plain error", resp, err)
			}
		}
	})
}
