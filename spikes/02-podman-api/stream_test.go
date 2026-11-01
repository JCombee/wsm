package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func frame(stream byte, payload string) []byte {
	hdr := make([]byte, 8)
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return append(hdr, payload...)
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestDemux(t *testing.T) {
	tests := []struct {
		name       string
		in         []byte
		limit      int64
		wantOut    string
		wantErr    string
		wantStderr string
	}{
		{"empty", nil, 100, "", "", ""},
		{"stdout and stderr", join(frame(1, "out1 "), frame(2, "err"), frame(1, "out2")), 100, "out1 out2", "", "err"},
		{"zero-length frame", join(frame(1, ""), frame(1, "x")), 100, "x", "", ""},
		{"truncated header", []byte{1, 0, 0}, 100, "", "frame header", ""},
		{"truncated payload", frame(1, "abcdef")[:10], 100, "", "frame payload", ""},
		{"stdin stream", frame(0, "x"), 100, "", "unexpected stream", ""},
		{"unknown stream", frame(3, "x"), 100, "", "unexpected stream", ""},
		{"non-zero padding", []byte{1, 1, 0, 0, 0, 0, 0, 0}, 100, "", "malformed", ""},
		{"over limit", join(frame(1, "12345"), frame(1, "6")), 5, "", "exceeds", ""},
		{"huge declared size", []byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}, 100, "", "exceeds", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := demux(bytes.NewReader(tt.in), &out, &errOut, tt.limit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.wantOut || errOut.String() != tt.wantStderr {
				t.Fatalf("stdout %q stderr %q, want %q and %q", out.String(), errOut.String(), tt.wantOut, tt.wantStderr)
			}
		})
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 4}
	if _, err := b.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("de")); err == nil {
		t.Fatal("write past the limit succeeded")
	}
	if b.String() != "abc" {
		t.Fatalf("buffer = %q", b.String())
	}
}
