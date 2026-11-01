package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseAPIError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantMsg  string
		notFound bool
	}{
		{"podman body", 404, `{"cause":"no such pod","message":"no pod with name or ID sbx found","response":404}`, "no pod with name or ID sbx found", true},
		{"status in body is ignored", 500, `{"message":"boom","response":200}`, "boom", false},
		{"not json", 500, `<html>`, "Internal Server Error", false},
		{"empty", 409, ``, "Conflict", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", parseAPIError(tt.status, []byte(tt.body)))
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("not an APIError: %v", err)
			}
			if apiErr.Status != tt.status || apiErr.Message != tt.wantMsg {
				t.Fatalf("got %d %q, want %d %q", apiErr.Status, apiErr.Message, tt.status, tt.wantMsg)
			}
			if IsNotFound(err) != tt.notFound {
				t.Fatalf("IsNotFound = %v", !tt.notFound)
			}
		})
	}
}

func TestReadLimited(t *testing.T) {
	if _, err := readLimited(strings.NewReader("12345"), 5); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := readLimited(strings.NewReader("123456"), 5); err == nil {
		t.Fatal("over the limit succeeded")
	}
}

func TestEndpoint(t *testing.T) {
	got := endpoint("/exec/abc/resize", map[string][]string{"h": {"37"}, "w": {"123"}})
	want := "http://podman/" + apiVersion + "/libpod/exec/abc/resize?h=37&w=123"
	if got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
}

// fakePodman answers one request on the server end of a pipe with head, then
// writes stream as the hijacked payload.
func fakePodman(t *testing.T, server net.Conn, head, stream string) <-chan *http.Request {
	t.Helper()
	got := make(chan *http.Request, 1)
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			close(got)
			return
		}
		got <- req
		io.WriteString(server, head+stream)
	}()
	return got
}

func TestUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		head       string
		stream     string
		wantErr    string
		wantStream string
	}{
		{"switching protocols", "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", "hello", "", "hello"},
		{"plain 200", "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n", "hello", "", "hello"},
		{"not found", "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: 34\r\n\r\n", `{"message":"no such exec session"}`, "no such exec session", ""},
		{"conflict", "HTTP/1.1 409 Conflict\r\nContent-Length: 0\r\n\r\n", "", "status 409", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			reqs := fakePodman(t, server, tt.head, tt.stream)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint("/exec/x/start", nil), strings.NewReader(`{"Tty":true}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "tcp")
			hc, err := upgrade(client, req)

			r := <-reqs
			if r == nil || r.Header.Get("Upgrade") != "tcp" || r.URL.Path != "/"+apiVersion+"/libpod/exec/x/start" {
				t.Fatalf("server saw %+v", r)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(hc)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.wantStream {
				t.Fatalf("stream = %q, want %q", data, tt.wantStream)
			}
		})
	}
}
