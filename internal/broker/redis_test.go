package broker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/JCombee/wsm/internal/blocks"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/sandbox"
)

// fakePods stands in for the sandbox manager. EnsureService calls record
// like the real one and then "touches the container", which it logs.
type fakePods struct {
	mu      sync.Mutex
	pulls   []string
	unknown map[string]bool
	// ensures logs "record <sandbox> <tag>" and "container <sandbox> <tag>".
	ensures []string
	// ensureErr is returned after record; beforeRecord runs before it.
	ensureErr    error
	beforeRecord func()
}

func (f *fakePods) PullServiceImage(_ context.Context, projectID string, blk blocks.Block, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, blk.ID+" "+tag)
	if f.unknown[tag] {
		return fmt.Errorf("%w: %s", sandbox.ErrUnknownTag, blk.ImageRef(tag))
	}
	return nil
}

func (f *fakePods) EnsureService(ctx context.Context, sandboxID string, blk blocks.Block, tag string, record func(context.Context) error) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("EnsureService without a deadline")
	}
	f.mu.Lock()
	before, ensureErr := f.beforeRecord, f.ensureErr
	f.mu.Unlock()
	if before != nil {
		before()
	}
	if err := record(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, "container "+sandboxID+" "+blk.ID+" "+tag)
	return ensureErr
}

func (f *fakePods) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.ensures
	f.ensures = nil
	return out
}

func newRedisEnv(t *testing.T) (*grantEnv, *fakePods) {
	t.Helper()
	e := newGrantEnv(t)
	pods := &fakePods{unknown: map[string]bool{}}
	e.b.Pods = pods
	if _, err := e.b.SetBlock(context.Background(), "a", "redis", "", []string{"8.10", "8.8"}); err != nil {
		t.Fatal(err)
	}
	pods.pulls = nil
	return e, pods
}

func TestEnableRedis(t *testing.T) {
	ctx := context.Background()
	e, pods := newRedisEnv(t)
	sa := e.sandbox(t, e.pa, "one")
	sb := e.sandbox(t, e.pb, "one")

	rows := func(id string) string {
		v, err := e.s.SandboxServices(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range v {
			out = append(out, r.BlockID+" "+r.Tag)
		}
		return strings.Join(out, ",")
	}

	// Denials: no ceiling, a tag outside it, a sandbox being deleted.
	if _, err := e.b.Enable(ctx, sb.ID, "redis", ""); !errors.Is(err, ErrDenied) {
		t.Errorf("project without a ceiling: err = %v", err)
	}
	if _, err := e.b.Enable(ctx, sa.ID, "redis", "8.4"); !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "8.10, 8.8") {
		t.Errorf("tag outside the ceiling: err = %v", err)
	}
	gone := e.sandbox(t, e.pa, "gone")
	if err := e.s.ClearMCPToken(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.Enable(ctx, gone.ID, "redis", ""); !errors.Is(err, ErrDenied) {
		t.Errorf("sandbox being deleted: err = %v", err)
	}
	if got := pods.take(); len(got) != 0 || rows(sb.ID) != "" || rows(gone.ID) != "" {
		t.Errorf("a denied call touched something: %v", got)
	}

	// The default tag; record makes the row before the container.
	c, err := e.b.Enable(ctx, sa.ID, "redis", "")
	if err != nil {
		t.Fatal(err)
	}
	want := Credentials{Block: "redis", Tag: "8.10", Host: "127.0.0.1", Port: 6379}
	if c != want {
		t.Errorf("credentials = %+v, want %+v", c, want)
	}
	if got := pods.take(); !reflect.DeepEqual(got, []string{"container " + sa.ID + " redis 8.10"}) {
		t.Errorf("ensures = %v", got)
	}
	if got := rows(sa.ID); got != "redis 8.10" {
		t.Errorf("rows = %q", got)
	}

	// Another tag updates the row and goes to EnsureService.
	if c, err := e.b.Enable(ctx, sa.ID, "redis", "8.8"); err != nil || c.Tag != "8.8" || c.Password != "" || c.User != "" || c.Database != "" {
		t.Errorf("8.8: %+v, %v", c, err)
	}
	if got := pods.take(); !reflect.DeepEqual(got, []string{"container " + sa.ID + " redis 8.8"}) {
		t.Errorf("ensures = %v", got)
	}
	if got := rows(sa.ID); got != "redis 8.8" {
		t.Errorf("rows = %q", got)
	}

	// The ceiling shrinks between the first check and record: record
	// refuses and the container is not touched.
	s2 := e.sandbox(t, e.pa, "two")
	pods.beforeRecord = func() {
		if err := e.s.SetProjectBlock(ctx, db.ProjectBlock{ProjectID: e.pa.ID, BlockID: "redis",
			Profile: "dev", ConfigJSON: `{"tags":["8.8"]}`}); err != nil {
			t.Error(err)
		}
	}
	if _, err := e.b.Enable(ctx, s2.ID, "redis", "8.10"); !errors.Is(err, ErrDenied) {
		t.Errorf("shrunk ceiling: err = %v", err)
	}
	pods.beforeRecord = nil
	if got := pods.take(); len(got) != 0 || rows(s2.ID) != "" {
		t.Errorf("shrunk ceiling touched the container: %v, rows %q", got, rows(s2.ID))
	}

	// Not available: no credentials.
	pods.ensureErr = fmt.Errorf("%w: no answer", sandbox.ErrServiceUnavailable)
	c, err = e.b.Enable(ctx, s2.ID, "redis", "8.8")
	if !errors.Is(err, ErrUnavailable) || c != (Credentials{}) {
		t.Errorf("unavailable: %+v, %v", c, err)
	}
	pods.ensureErr = sandbox.ErrServiceDenied
	if _, err := e.b.Enable(ctx, s2.ID, "redis", "8.8"); !errors.Is(err, ErrDenied) {
		t.Errorf("denied by the manager: err = %v", err)
	}
	pods.ensureErr = nil

	// services_available and sandbox_info.
	offers, err := e.b.Available(ctx, sa.ID)
	if err != nil {
		t.Fatal(err)
	}
	var redisOffer Offer
	for _, o := range offers {
		if o.Block == "redis" {
			redisOffer = o
		}
	}
	if !reflect.DeepEqual(redisOffer.EnabledTags, []string{"8.8"}) || redisOffer.DefaultTag != "8.8" {
		t.Errorf("redis offer = %+v", redisOffer)
	}
	en, err := e.b.Enabled(ctx, sa.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(en, []Enabled{{Block: "redis", Tag: "8.8", Host: "127.0.0.1", Port: 6379}}) {
		t.Errorf("Enabled = %+v", en)
	}

	// Without Pods there is no per-sandbox service.
	e.b.Pods = nil
	if _, err := e.b.Enable(ctx, sa.ID, "redis", "8.8"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no Pods: err = %v", err)
	}
}

func TestRedisCeiling(t *testing.T) {
	ctx := context.Background()
	e := newGrantEnv(t)
	pods := &fakePods{unknown: map[string]bool{"99.99": true}}
	e.b.Pods = pods

	helperPulls, _ := e.h.counts()
	// Every tag is pulled once, through Pods, and the ceiling is stored.
	c, err := e.b.SetBlock(ctx, "a", "redis", "", []string{"8.10", "8.8"})
	if err != nil || c.Profile != "dev" || strings.Join(c.Tags, ",") != "8.10,8.8" {
		t.Fatalf("SetBlock = %+v, %v", c, err)
	}
	if !reflect.DeepEqual(pods.pulls, []string{"redis 8.10", "redis 8.8"}) {
		t.Errorf("pulls = %v", pods.pulls)
	}
	if pulls, _ := e.h.counts(); pulls != helperPulls {
		t.Errorf("the helper pulled %d images for redis", pulls-helperPulls)
	}
	if insts, _ := e.s.ServiceInstances(ctx); len(insts) != 2 { // the two mariadb ones
		t.Errorf("instances = %+v", insts)
	}

	// An unknown tag is invalid, and no ceiling is stored.
	if _, err := e.b.SetBlock(ctx, "b", "redis", "", []string{"99.99"}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "tag 99.99 of block redis does not exist") {
		t.Errorf("unknown tag: err = %v", err)
	}
	if got := e.ceiling(t, "b"); got != "[]" {
		t.Errorf("ceiling of b = %s", got)
	}
	if _, err := e.b.SetBlock(ctx, "b", "redis", "", []string{"7.1"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("tag below the minimum: err = %v", err)
	}

	// A sandbox uses 8.10: shrinking past it or removing is a conflict.
	sa := e.sandbox(t, e.pa, "worker")
	if _, err := e.b.Enable(ctx, sa.ID, "redis", "8.10"); err != nil {
		t.Fatal(err)
	}
	before := e.ceiling(t, "a")
	pods.pulls = nil
	if _, err := e.b.SetBlock(ctx, "a", "redis", "", []string{"8.8"}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "worker") {
		t.Errorf("shrink: err = %v", err)
	}
	if err := e.b.RemoveBlock(ctx, "a", "redis"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "worker") {
		t.Errorf("remove: err = %v", err)
	}
	if got := e.ceiling(t, "a"); got != before {
		t.Errorf("ceiling changed: %s, want %s", got, before)
	}
	if len(pods.pulls) != 0 {
		t.Errorf("a refused SetBlock pulled: %v", pods.pulls)
	}
	// Keeping the used tag works; without use, removing works.
	if _, err := e.b.SetBlock(ctx, "a", "redis", "", []string{"8.10"}); err != nil {
		t.Errorf("keep 8.10: %v", err)
	}
	if err := e.s.DeleteSandbox(ctx, sa.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.b.RemoveBlock(ctx, "a", "redis"); err != nil {
		t.Errorf("remove without use: %v", err)
	}
	// MariaDB's ceiling still works as before.
	if got := e.ceiling(t, "a"); !strings.Contains(got, "mariadb") || strings.Contains(got, "redis") {
		t.Errorf("ceiling of a = %s", got)
	}
}
