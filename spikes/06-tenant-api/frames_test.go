package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	fw := &frameWriter{w: &buf}
	big := bytes.Repeat([]byte("x"), maxFramePayload+10)
	if err := fw.write(frameStdout, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := fw.write(frameStderr, big); err != nil {
		t.Fatal(err)
	}
	if err := fw.write(frameExit, nil); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		typ byte
		n   int
	}{{frameStdout, 5}, {frameStderr, maxFramePayload}, {frameStderr, 10}, {frameExit, 0}}
	for _, w := range want {
		typ, p, err := readFrame(&buf)
		if err != nil || typ != w.typ || len(p) != w.n {
			t.Fatalf("readFrame = %x, %d bytes, %v; want %x, %d", typ, len(p), err, w.typ, w.n)
		}
	}
	if _, _, err := readFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last frame: %v, want EOF", err)
	}
}

func TestReadFrameRejects(t *testing.T) {
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], maxFramePayload+1)
	if _, _, err := readFrame(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
	binary.BigEndian.PutUint32(hdr[1:], 10)
	if _, _, err := readFrame(bytes.NewReader(append(hdr[:], 'a', 'b'))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame: %v, want ErrUnexpectedEOF", err)
	}
}

func TestDemuxRejectsUnknownStream(t *testing.T) {
	frame := []byte{3, 0, 0, 0, 0, 0, 0, 1, 'x'}
	if err := demux(bytes.NewReader(frame), func(byte, []byte) error { return nil }); err == nil {
		t.Fatal("stream 3 accepted")
	}
}
