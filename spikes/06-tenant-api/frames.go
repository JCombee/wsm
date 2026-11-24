package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The exec stream after the upgrade is a sequence of frames: one type byte,
// a big-endian uint32 length and at most maxFramePayload bytes.
const (
	execProtocol = "wsm-exec.v1"

	frameStdin    byte = 0x00 // client: data for the process's stdin
	frameStdinEOF byte = 0x01 // client: end of stdin
	frameStop     byte = 0x02 // client: stop the process (TERM, KILL after 5 s)
	frameStdout   byte = 0x10 // server: stdout data
	frameStderr   byte = 0x11 // server: stderr data
	frameExit     byte = 0x12 // server: ExitInfo as JSON, the last frame
	frameError    byte = 0x13 // server: a protocol or limit error as text

	maxFramePayload = 1 << 20
)

// ExitInfo is the payload of the exit frame.
type ExitInfo struct {
	Code     *int   `json:"code"`
	TimedOut bool   `json:"timed_out"`
	Stopped  bool   `json:"stopped"`
	Reason   string `json:"reason,omitempty"`
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFramePayload {
		return fmt.Errorf("frame payload of %d bytes is too large", len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(append(hdr[:], payload...)); err != nil {
		return err
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFramePayload {
		return 0, nil, fmt.Errorf("frame payload of %d bytes is too large", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// frameWriter serializes frames from several goroutines. After the first
// write error it refuses every later write.
type frameWriter struct {
	mu     sync.Mutex
	w      io.Writer
	broken error
}

func (fw *frameWriter) write(typ byte, payload []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.broken != nil {
		return fw.broken
	}
	for {
		chunk := payload
		if len(chunk) > maxFramePayload {
			chunk = chunk[:maxFramePayload]
		}
		if err := writeFrame(fw.w, typ, chunk); err != nil {
			fw.broken = err
			return err
		}
		payload = payload[len(chunk):]
		if len(payload) == 0 {
			return nil
		}
	}
}
