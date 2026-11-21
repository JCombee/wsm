package sandbox

import (
	"reflect"
	"testing"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/fsproto"
	"github.com/JCombee/wsm/internal/podman"
)

func TestPreviewSpecs(t *testing.T) {
	p := db.Project{ID: "abcdefgh2345", Slug: "shop", AppPort: 8000, CPUMillis: 1000, MemBytes: 1 << 30, PidsLimit: 100}
	sb := db.Sandbox{ID: "bcdefgh23456", Slug: "main", HostPort: 47001}
	h := Host{PreviewDomain: "wsm-preview.test", HelperDir: DefaultHelperDir}

	pod := PodSpec(p, sb)
	want := []podman.PortMapping{{HostIP: "127.0.0.1", HostPort: 47001, ContainerPort: 8000, Protocol: "tcp"}}
	if !reflect.DeepEqual(pod.PortMappings, want) {
		t.Errorf("port mappings %+v", pod.PortMappings)
	}
	work := WorkSpec(p, sb, nil, "", h)
	if work.Env["WSM_PREVIEW_URL"] != "https://main--shop.wsm-preview.test/" {
		t.Errorf("WSM_PREVIEW_URL = %q", work.Env["WSM_PREVIEW_URL"])
	}
	mounts := []podman.Mount{{Destination: fsproto.Dir, Type: "bind", Source: DefaultHelperDir,
		Options: []string{"ro", "nosuid", "nodev", "rbind"}}}
	if !reflect.DeepEqual(work.Mounts, mounts) {
		t.Errorf("mounts %+v", work.Mounts)
	}

	// Without an app port, a preview domain or a helper there is nothing.
	p.AppPort = 0
	if pod := PodSpec(p, sb); len(pod.PortMappings) != 0 {
		t.Errorf("port mappings without an app port: %+v", pod.PortMappings)
	}
	work = WorkSpec(p, sb, nil, "", Host{})
	if _, ok := work.Env["WSM_PREVIEW_URL"]; ok || len(work.Mounts) != 0 {
		t.Errorf("env %v mounts %v", work.Env, work.Mounts)
	}
	p.AppPort = 8000
	sb.Slug = "a--b"
	if u := h.PreviewURL(p, sb); u != "" {
		t.Errorf("preview URL for a slug with -- : %q", u)
	}
}
