package podman

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxImageOutput caps the progress stream of an image pull or build. The
// stream comes from a process the project user controls (and, for a build,
// from the build's own RUN steps), so it is untrusted (CLAUDE.md
// invariant 8).
const MaxImageOutput = 16 << 20

// maxOutputTail caps the build output quoted in an error.
const maxOutputTail = 4 << 10

// ContainerfileName is the only file in a build context.
const ContainerfileName = "Containerfile"

// Pull policies for ImagePull.
const (
	// PullMissing pulls only when the image is not present.
	PullMissing = "missing"
	// PullAlways pulls even when the image is present, to pick up a newer
	// image behind the same tag.
	PullAlways = "always"
)

var imageIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// streamMessage is one JSON object in a pull or build progress stream.
// Fields wsm does not use are ignored; every field it reads is typed.
type streamMessage struct {
	Stream      string `json:"stream"`
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// errOutputTooLarge is returned when a progress stream exceeds
// MaxImageOutput.
var errOutputTooLarge = fmt.Errorf("output exceeds %d bytes", MaxImageOutput)

// cappedReader reads at most n bytes and then fails with errOutputTooLarge,
// so a truncated stream is never mistaken for a complete one.
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		// One more byte tells a stream of exactly n bytes from a longer one.
		var b [1]byte
		if k, err := c.r.Read(b[:]); k > 0 {
			return 0, errOutputTooLarge
		} else if err != nil {
			return 0, err
		}
		return 0, nil
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	k, err := c.r.Read(p)
	c.n -= int64(k)
	return k, err
}

// tail keeps the last bytes of the text written to it.
type tail struct {
	buf []byte
	max int
}

func (t *tail) add(s string) {
	t.buf = append(t.buf, s...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
}

func (t *tail) String() string { return sanitize(string(t.buf)) }

// sanitize makes untrusted output safe to print on a terminal: invalid
// UTF-8 and control characters other than newline and tab become '?'.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		control := r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)
		if r == utf8.RuneError || (control && r != '\n' && r != '\t') {
			r = '?'
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// shorten caps an untrusted error message.
func shorten(s string) string {
	s = sanitize(s)
	if len(s) > maxErrorMessage {
		s = s[:maxErrorMessage] + "..."
	}
	return s
}

// readStream decodes a progress stream of JSON objects. It returns the
// first error message in the stream as an error, with the tail of the
// output before it.
func readStream(body io.Reader) error {
	dec := json.NewDecoder(&cappedReader{r: body, n: MaxImageOutput})
	out := &tail{max: maxOutputTail}
	for {
		var msg streamMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, errOutputTooLarge) {
				return err
			}
			return fmt.Errorf("read output: %w", err)
		}
		out.add(msg.Stream)
		errMsg := msg.Error
		if errMsg == "" && msg.ErrorDetail != nil {
			errMsg = msg.ErrorDetail.Message
		}
		if errMsg != "" {
			if t := out.String(); t != "" {
				return fmt.Errorf("%s\noutput:\n%s", shorten(errMsg), t)
			}
			return errors.New(shorten(errMsg))
		}
	}
}

// postStream sends a POST and reads the progress stream of the response.
func (c *Client) postStream(ctx context.Context, path string, q url.Values, contentType string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(path, q), body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := readLimited(resp.Body, MaxResponse)
		return parseAPIError(resp.StatusCode, data)
	}
	return readStream(resp.Body)
}

// ImagePull pulls ref with a pull policy, PullMissing or PullAlways. Podman
// streams progress as JSON objects; the stream is size limited and an error
// object in it fails the pull. Only public registries are used: wsm sends
// no credentials.
func (c *Client) ImagePull(ctx context.Context, ref, policy string) error {
	if policy != PullMissing && policy != PullAlways {
		return fmt.Errorf("pull %s: unknown policy %q", ref, policy)
	}
	q := url.Values{"reference": {ref}, "policy": {policy}}
	if err := c.postStream(ctx, "/images/pull", q, "", nil); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// buildContext returns a tar archive with one file, the Containerfile. The
// context holds nothing else, so COPY and ADD of local files cannot reach
// any file of the host.
func buildContext(containerfile string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     ContainerfileName,
		Mode:     0o644,
		Size:     int64(len(containerfile)),
		Format:   tar.FormatPAX,
	})
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(tw, containerfile); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ImageBuild builds containerfile into the image tag and returns the new
// image's ID. The build runs in the Podman of the socket's user, with
// Podman's defaults for networking, capabilities and isolation; there are
// no parameters for host networking, privileged mode, extra capabilities
// or volumes. The ID comes from inspecting tag after the build, not from
// the build output.
func (c *Client) ImageBuild(ctx context.Context, tag, containerfile string) (string, error) {
	tarball, err := buildContext(containerfile)
	if err != nil {
		return "", fmt.Errorf("build %s: context: %w", tag, err)
	}
	q := url.Values{"t": {tag}, "dockerfile": {ContainerfileName}, "rm": {"true"}}
	if err := c.postStream(ctx, "/build", q, "application/x-tar", bytes.NewReader(tarball)); err != nil {
		return "", fmt.Errorf("build %s: %w", tag, err)
	}
	id, err := c.ImageID(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("build %s: %w", tag, err)
	}
	return id, nil
}

// ImageID returns the ID of a local image: 64 lowercase hex characters.
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	var r struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &r); err != nil {
		return "", err
	}
	id := strings.TrimPrefix(r.ID, "sha256:")
	if !imageIDPattern.MatchString(id) {
		return "", fmt.Errorf("image %s: invalid id %q", ref, shorten(r.ID))
	}
	return id, nil
}
