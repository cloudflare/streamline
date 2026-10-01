package main

import (
	"bytes"
	"crypto/rand"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/media"
	"github.com/stretchr/testify/require"
)

func TestHTTPStreamHandlerFeedsAnnotationPipe(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	capturePath := filepath.Join(dir, "annotation.png")
	cfg := media.SessionConfig{
		SourceType:        media.SourceHLS,
		SourceURL:         "input.mp4",
		OutputMode:        media.OutputPreview,
		AnnotationOverlay: true,
		Encode:            media.EncodeConfig{Resolution: "16x16"},
	}
	expected, err := transparentAnnotationFrame(cfg.Encode.Resolution)
	require.NoError(t, err)
	script := "#!/bin/sh\ndd bs=1 count=\"$ANNOTATION_BYTES\" <&3 > \"$ANNOTATION_CAPTURE\" 2>/dev/null\nexec sleep 30\n"
	require.NoError(t, os.WriteFile(ffmpegPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANNOTATION_BYTES", strconv.Itoa(len(expected)))
	t.Setenv("ANNOTATION_CAPTURE", capturePath)

	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(cfg))
	t.Cleanup(h.Stop)

	require.Eventually(t, func() bool {
		captured, readErr := os.ReadFile(capturePath)
		return readErr == nil && bytes.Equal(expected, captured)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestHTTPStreamHandlerPrimesLargeAnnotationAfterProcessStart(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	capturePath := filepath.Join(dir, "annotation.png")
	image := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	_, err := rand.Read(image.Pix)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image))
	frame := encoded.Bytes()
	require.Greater(t, len(frame), 64*1024)

	script := "#!/bin/sh\ndd bs=1 count=\"$ANNOTATION_BYTES\" <&3 > \"$ANNOTATION_CAPTURE\" 2>/dev/null\nexec sleep 30\n"
	require.NoError(t, os.WriteFile(ffmpegPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANNOTATION_BYTES", strconv.Itoa(len(frame)))
	t.Setenv("ANNOTATION_CAPTURE", capturePath)

	h := NewHTTPStreamHandler()
	h.config = media.SessionConfig{
		SourceType:        media.SourceHLS,
		SourceURL:         "input.mp4",
		OutputMode:        media.OutputPreview,
		AnnotationOverlay: true,
		Encode:            media.EncodeConfig{Resolution: "256x256"},
	}
	h.sessionActive = true
	h.sessionID = 1
	h.setAnnotationFrame(frame)
	started := make(chan error, 1)
	go func() { started <- h.startFfmpeg(h.sessionID) }()
	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		h.mu.Lock()
		h.stopAnnotationWriterLocked(false)
		h.mu.Unlock()
		t.Fatal("startFfmpeg blocked while priming a large annotation")
	}
	t.Cleanup(h.Stop)

	require.Eventually(t, func() bool {
		captured, readErr := os.ReadFile(capturePath)
		return readErr == nil && bytes.Equal(frame, captured)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestHTTPStreamHandlerStartsWebcamBeforeLargeAnnotationDrains(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	capturePath := filepath.Join(dir, "annotation.png")
	image := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	_, err := rand.Read(image.Pix)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image))
	frame := encoded.Bytes()
	require.Greater(t, len(frame), 64*1024)

	script := "#!/bin/sh\ndd bs=1 count=4 <&0 >/dev/null 2>&1\ndd bs=1 count=\"$ANNOTATION_BYTES\" <&3 > \"$ANNOTATION_CAPTURE\" 2>/dev/null\nexec sleep 30\n"
	require.NoError(t, os.WriteFile(ffmpegPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANNOTATION_BYTES", strconv.Itoa(len(frame)))
	t.Setenv("ANNOTATION_CAPTURE", capturePath)

	h := NewHTTPStreamHandler()
	cfg := media.SessionConfig{
		SourceType:        media.SourceWebcam,
		OutputMode:        media.OutputPreview,
		AnnotationOverlay: true,
		Encode:            media.EncodeConfig{Resolution: "256x256"},
	}
	require.NoError(t, h.Start(cfg))
	t.Cleanup(h.Stop)
	require.NoError(t, h.UpdateAnnotation(frame))

	started := make(chan error, 1)
	go func() {
		_, ingestErr := h.IngestChunk([]byte{0x1a, 0x45, 0xdf, 0xa3})
		started <- ingestErr
	}()
	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("webcam ingest blocked while ffmpeg waited for its first input bytes")
	}

	require.Eventually(t, func() bool {
		captured, readErr := os.ReadFile(capturePath)
		return readErr == nil && bytes.Equal(frame, captured)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestAnnotationWriterStopsWhilePipeIsFull(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()

	stop := make(chan struct{})
	ready := make(chan error, 1)
	done := make(chan error, 1)
	frame := bytes.Repeat([]byte{1}, maxAnnotationImageBytes)
	go func() {
		done <- replayAnnotationFrames(nil, writer, func() []byte { return frame }, stop, ready)
	}()

	select {
	case err := <-ready:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("annotation writer did not report that initial pipe priming started")
	}
	close(stop)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("annotation writer did not stop while the pipe was full")
	}
}

func TestAnnotationWriterDrainsRepeatedLargeFramesAtRetryCadence(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()

	stop := make(chan struct{})
	ready := make(chan error, 1)
	done := make(chan error, 1)
	const frameSize = 128 * 1024
	frameStarts := make(chan time.Time, 8)
	go func() {
		frameIndex := byte(0)
		done <- replayAnnotationFrames(nil, writer, func() []byte {
			frameIndex++
			frameStarts <- time.Now()
			return bytes.Repeat([]byte{frameIndex}, frameSize)
		}, stop, ready)
	}()

	select {
	case err := <-ready:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("annotation writer did not begin the initial frame")
	}

	frames := make([]byte, frameSize*3)
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4*1024)
		read := 0
		for read < len(frames) {
			n, err := reader.Read(buffer)
			if err != nil {
				readDone <- err
				return
			}
			copy(frames[read:], buffer[:n])
			read += n
			time.Sleep(4 * time.Millisecond)
		}
		readDone <- nil
	}()
	select {
	case err := <-readDone:
		require.NoError(t, err)
		expected := append(bytes.Repeat([]byte{1}, frameSize), bytes.Repeat([]byte{2}, frameSize)...)
		expected = append(expected, bytes.Repeat([]byte{3}, frameSize)...)
		require.Equal(t, expected, frames)
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("large repeated annotation frames drained at the normal frame interval")
	}

	starts := make([]time.Time, 3)
	for index := range starts {
		select {
		case starts[index] = <-frameStarts:
		case <-time.After(time.Second):
			t.Fatal("annotation writer did not start three frames")
		}
	}
	for index := 1; index < len(starts); index++ {
		interval := starts[index].Sub(starts[index-1])
		require.Less(t, interval, 300*time.Millisecond)
		require.Greater(t, interval, 100*time.Millisecond)
	}

	close(stop)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("annotation writer did not stop after repeated large frames")
	}
}
