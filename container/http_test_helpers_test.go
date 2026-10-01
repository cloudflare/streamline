package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func decodeTestAnnotationPNG(t *testing.T) []byte {
	t.Helper()
	frame, err := transparentAnnotationFrame("1x1")
	require.NoError(t, err)
	return frame
}

func receiveOutputChunk(t *testing.T, chunks <-chan []byte, timeout time.Duration) []byte {
	t.Helper()
	select {
	case chunk := <-chunks:
		return chunk
	case <-time.After(timeout):
		t.Fatal("timed out waiting for ffmpeg output")
		return nil
	}
}

func publishOutputForTest(h *HTTPStreamHandler, chunk []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publishOutputLocked(chunk)
}

func appendFFmpegOutputForTest(h *HTTPStreamHandler, chunk []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stdoutBuf = append(h.stdoutBuf, chunk...)
	h.processBoxes()
}

func mp4Box(boxType string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:8], boxType)
	copy(box[8:], payload)
	return box
}

func installFakeFFmpeg(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	require.NoError(t, os.WriteFile(ffmpegPath, []byte("#!/bin/sh\n"+body), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
