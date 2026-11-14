package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newBox(t *testing.T, fill byte) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{fill}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealRoundTrip(t *testing.T) {
	b := newBox(t, 1)
	for _, plain := range [][]byte{{}, []byte("x"), bytes.Repeat([]byte("deploy key "), 100)} {
		sealed, err := b.Seal(plain, []byte("project-a"))
		if err != nil {
			t.Fatal(err)
		}
		if len(plain) > 8 && bytes.Contains(sealed, plain) {
			t.Errorf("sealed value contains the plaintext")
		}
		got, err := b.Open(sealed, []byte("project-a"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("Open = %q, want %q", got, plain)
		}
	}
	// A fresh nonce each time: the same input seals differently.
	s1, _ := b.Seal([]byte("same"), nil)
	s2, _ := b.Seal([]byte("same"), nil)
	if bytes.Equal(s1, s2) {
		t.Error("two seals of the same value are equal")
	}
	if _, err := New(make([]byte, 16)); err == nil {
		t.Error("New accepted a 16-byte key")
	}
}

func TestSealRejectsTampering(t *testing.T) {
	b := newBox(t, 1)
	ad := []byte("project-a")
	sealed, err := b.Seal([]byte("private key"), ad)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		c := bytes.Clone(sealed)
		c[i] ^= 0x01
		return c
	}
	tests := []struct {
		name   string
		box    *Box
		sealed []byte
		ad     []byte
	}{
		{"changed ciphertext", b, flip(len(sealed) / 2), ad},
		{"changed nonce", b, flip(0), ad},
		{"changed tag", b, flip(len(sealed) - 1), ad},
		{"wrong key", newBox(t, 2), sealed, ad},
		{"other additional data", b, sealed, []byte("project-b")},
		{"no additional data", b, sealed, nil},
		{"too short", b, sealed[:27], ad},
		{"empty", b, nil, ad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.box.Open(tt.sealed, tt.ad)
			if !errors.Is(err, ErrOpen) || got != nil {
				t.Errorf("Open = %q, %v; want ErrOpen", got, err)
			}
		})
	}
}

func TestSecretKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Size() != KeySize || len(key) != KeySize {
		t.Fatalf("created key file: mode %04o size %d, key %d bytes", fi.Mode().Perm(), fi.Size(), len(key))
	}
	if bytes.Equal(key, make([]byte, KeySize)) {
		t.Fatal("key is all zeros")
	}
	again, err := LoadOrCreateKey(path)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("second load: %v, same key %v", err, bytes.Equal(again, key))
	}

	write := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"too short", write("short.key", make([]byte, 31), 0o600)},
		{"too long", write("long.key", make([]byte, 33), 0o600)},
		{"empty", write("empty.key", nil, 0o600)},
		{"group readable", write("group.key", make([]byte, 32), 0o640)},
		{"world readable", write("world.key", make([]byte, 32), 0o644)},
		{"owner read only", write("ro.key", make([]byte, 32), 0o400)},
		{"directory", func() string { p := filepath.Join(dir, "d.key"); os.Mkdir(p, 0o700); return p }()},
		{"symlink", link},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := os.Lstat(tt.path)
			if _, err := LoadOrCreateKey(tt.path); err == nil {
				t.Fatal("LoadOrCreateKey accepted the file")
			}
			after, _ := os.Lstat(tt.path)
			if before.Mode() != after.Mode() || before.Size() != after.Size() {
				t.Errorf("file changed: %v %d -> %v %d", before.Mode(), before.Size(), after.Mode(), after.Size())
			}
		})
	}
}
