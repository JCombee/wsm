package podman

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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

// execServer is a fake exec API on loopback TCP. Its exec start hijacks
// the connection, reads stdin to EOF, then answers with frames that echo
// the input's length on stdout and write "warn" on stderr.
func execServer(t *testing.T, exitCode int) (*Client, *[]byte, *map[string]any) {
	t.Helper()
	var stdin []byte
	var created map[string]any
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/ctr/exec"):
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"e1"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/e1/start"):
			io.Copy(io.Discard, r.Body)
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			if created["AttachStdin"] == true {
				stdin, _ = io.ReadAll(brw)
			}
			conn.Write(join(frame(1, fmt.Sprintf("got %d", len(stdin))), frame(2, "warn")))
		case strings.HasSuffix(r.URL.Path, "/exec/e1/json"):
			fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, exitCode)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	c := NewClientDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	})
	t.Cleanup(c.Close)
	return c, &stdin, &created
}

func TestRunInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, stdin, created := execServer(t, 3)
	input := bytes.Repeat([]byte("bundle"), 100000)
	res, err := c.RunInput(ctx, "ctr", bytes.NewReader(input), "dd", "of=/tmp/x", "status=none")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(*stdin, input) {
		t.Errorf("server got %d bytes of stdin, want %d", len(*stdin), len(input))
	}
	if res.Stdout != fmt.Sprintf("got %d", len(input)) || res.Stderr != "warn" || res.ExitCode != 3 {
		t.Errorf("result %+v", res)
	}
	if (*created)["AttachStdin"] != true || fmt.Sprint((*created)["Cmd"]) != "[dd of=/tmp/x status=none]" {
		t.Errorf("exec create %v", *created)
	}

	// Without stdin nothing is attached and nothing is read.
	c2, _, created2 := execServer(t, 0)
	res, err = c2.Run(ctx, "ctr", "true")
	if err != nil || res.ExitCode != 0 || res.Stdout != "got 0" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if (*created2)["AttachStdin"] != false {
		t.Errorf("Run attached stdin: %v", *created2)
	}
	if _, err := c2.Output(ctx, "ctr", "true"); err != nil {
		t.Errorf("Output: %v", err)
	}
	c3, _, _ := execServer(t, 1)
	if _, err := c3.Output(ctx, "ctr", "false"); err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("Output on exit 1: %v", err)
	}
}
