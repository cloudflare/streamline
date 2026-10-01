package main

import (
	"bytes"
	"os/exec"
	"testing"

	"github.com/cloudflare/streamline/container/media"
	"github.com/stretchr/testify/require"
)

func TestHTTPStreamHandlerSubscribeOutput(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true

	publishOutputForTest(h, []byte("before"))

	chunks, cancel, err := h.SubscribeOutput()
	require.NoError(t, err)
	t.Cleanup(func() { cancel(nil) })
	require.Equal(t, []byte("before"), <-chunks)

	publishOutputForTest(h, []byte("live"))
	require.Equal(t, []byte("live"), <-chunks)

	publishOutputForTest(h, []byte("queued"))
	cancel(nil)
	publishOutputForTest(h, []byte("gap"))

	reconnected, cancelReconnect, err := h.SubscribeOutput()
	require.NoError(t, err)
	t.Cleanup(func() { cancelReconnect(nil) })
	require.Equal(t, []byte("queuedgap"), <-reconnected)
}

func TestHTTPStreamHandlerRejectsSecondOutputSubscriber(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true

	_, cancel, err := h.SubscribeOutput()
	require.NoError(t, err)
	t.Cleanup(func() { cancel(nil) })

	_, _, err = h.SubscribeOutput()
	require.EqualError(t, err, "output subscriber already attached")
}

func TestHTTPStreamHandlerRejectsOverflowedBacklog(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true

	publishOutputForTest(h, make([]byte, maxStreamBufferBytes+1))

	_, _, err := h.SubscribeOutput()
	require.EqualError(t, err, "output reconnect buffer overflowed; restart the stream")
	require.True(t, h.streamBufOverflowed)
	require.Empty(t, h.streamBuf)
}

func TestHTTPStreamHandlerRetainsQueuedBytesWhenSubscriberFallsBehind(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true

	_, cancel, err := h.SubscribeOutput()
	require.NoError(t, err)

	for i := 0; i < outputSubscriberQueue; i++ {
		publishOutputForTest(h, []byte{'a'})
	}
	publishOutputForTest(h, []byte{'b'})

	_, _, err = h.SubscribeOutput()
	require.EqualError(t, err, "output subscriber already attached")
	cancel(nil)

	reconnected, cancelReconnect, err := h.SubscribeOutput()
	require.NoError(t, err)
	t.Cleanup(func() { cancelReconnect(nil) })
	require.Equal(t, append(bytes.Repeat([]byte{'a'}, outputSubscriberQueue), 'b'), <-reconnected)
}

func TestHTTPStreamHandlerDiscardsCancelledOutputFromPreviousSession(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true

	_, cancel, err := h.SubscribeOutput()
	require.NoError(t, err)
	publishOutputForTest(h, []byte("old queued output"))

	h.mu.Lock()
	h.closeOutputSubscriberLocked()
	h.sessionID++
	h.streamBuf = h.streamBuf[:0]
	h.mu.Unlock()

	cancel([]byte("old unwritten output"))
	require.Empty(t, h.streamBuf)
}

func TestHTTPStreamHandlerRetainsTerminalSubscriberBytes(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true

	_, cancel, err := h.SubscribeOutput()
	require.NoError(t, err)
	publishOutputForTest(h, []byte("queued"))

	h.mu.Lock()
	h.closeOutputSubscriberLocked()
	h.sessionActive = false
	h.mu.Unlock()
	cancel([]byte("unwritten"))

	require.Equal(t, []byte("unwrittenqueued"), h.drainRelayBuffer())
}

func TestHTTPStreamHandlerSubscribesToCompletedBacklog(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.relaySessionID = "session-1"
	h.streamBuf = []byte("complete output")

	chunks, cancel, err := h.SubscribeOutputForSessionID("session-1")
	require.NoError(t, err)
	t.Cleanup(func() { cancel(nil) })
	require.Equal(t, []byte("complete output"), <-chunks)
	_, open := <-chunks
	require.False(t, open)
}

func TestHTTPStreamHandlerPublishesCompleteFMP4Units(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true

	chunks, cancel, err := h.SubscribeOutput()
	require.NoError(t, err)
	t.Cleanup(func() { cancel(nil) })

	initSegment := append(mp4Box("ftyp", []byte("file")), mp4Box("moov", []byte("metadata"))...)
	appendFFmpegOutputForTest(h, initSegment[:len(initSegment)-2])
	select {
	case chunk := <-chunks:
		t.Fatalf("published incomplete init segment: %d bytes", len(chunk))
	default:
	}
	appendFFmpegOutputForTest(h, initSegment[len(initSegment)-2:])
	require.Equal(t, initSegment, <-chunks)

	mediaSegment := append(mp4Box("moof", []byte("fragment")), mp4Box("mdat", []byte("media"))...)
	appendFFmpegOutputForTest(h, mediaSegment[:len(mediaSegment)-1])
	select {
	case chunk := <-chunks:
		t.Fatalf("published incomplete media segment: %d bytes", len(chunk))
	default:
	}
	appendFFmpegOutputForTest(h, mediaSegment[len(mediaSegment)-1:])
	require.Equal(t, mediaSegment, <-chunks)
}
