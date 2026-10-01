package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/media"
	"github.com/stretchr/testify/require"
)

func TestHTTPIngestRejectsOversizedBodies(t *testing.T) {
	server := NewServer()
	request := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader(make([]byte, maxHTTPIngestBodyBytes+1)))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
}

func TestHTTPIngestCannotRestartStoppedSession(t *testing.T) {
	server := NewServer()
	require.NoError(t, server.httpHandler.Start(media.SessionConfig{
		SourceType: media.SourceWebcam, OutputMode: media.OutputPreview,
	}))
	server.httpHandler.Stop()
	request := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader([]byte{0x1A, 0x45, 0xDF, 0xA3}))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.False(t, server.httpHandler.IsRunning())
	require.False(t, server.httpHandler.IsSessionActive())
}

func TestValidIngestRequestID(t *testing.T) {
	tests := map[string]string{
		"accepts UUID":          "f188913b-4b17-4141-a09b-79933f42a81e",
		"accepts underscore":    "ingest_request-1",
		"rejects empty":         "",
		"rejects whitespace":    "ingest request",
		"rejects newline":       "ingest\nrequest",
		"rejects over 64 bytes": strings.Repeat("a", 65),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			got := validIngestRequestID(value)
			if strings.HasPrefix(name, "accepts") {
				require.Equal(t, value, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
}

func TestHTTPStreamHandlerStopsRTMPDestinationWhenWebcamIngestWriteStalls(t *testing.T) {
	installFakeFFmpeg(t, "exec sleep 30\n")

	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType:     media.SourceWebcam,
		DestinationURL: "rtmp://destination.example.test/live/output",
		OutputMode:     media.OutputRTMP,
	}))
	t.Cleanup(h.Stop)

	chunk := make([]byte, maxHTTPIngestBodyBytes)
	copy(chunk, []byte{0x1A, 0x45, 0xDF, 0xA3})
	ingestDone := make(chan error, 1)
	go func() {
		_, err := h.IngestChunk(chunk)
		ingestDone <- err
	}()
	require.Eventually(t, func() bool {
		return h.Metrics().IngestWriting
	}, 2*time.Second, 10*time.Millisecond)

	h.mu.Lock()
	h.ingestWriteStartedAt = time.Now().Add(-rtmpOutputTimeout - time.Second)
	h.mu.Unlock()

	metrics := h.Metrics()
	require.True(t, metrics.IngestWriting)
	require.GreaterOrEqual(t, metrics.IngestWriteAgeMS, rtmpOutputTimeout.Milliseconds())
	require.Equal(t, len(chunk), metrics.IngestWriteBytes)

	select {
	case err := <-ingestDone:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("blocked webcam ingest write was not interrupted")
	}

	require.Eventually(t, func() bool {
		return !h.Metrics().SessionActive
	}, 2*time.Second, 10*time.Millisecond)
	require.Contains(t, h.Metrics().LastFFmpegExitError, "signal: killed")
}
