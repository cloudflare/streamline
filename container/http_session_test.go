package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/streamline/container/media"
	"github.com/stretchr/testify/require"
)

func TestHTTPStreamHandlerCleansReplacedSubtitleFiles(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first.srt")
	second := filepath.Join(t.TempDir(), "second.srt")
	require.NoError(t, os.WriteFile(first, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("second"), 0o600))
	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType: media.SourceWebcam, OutputMode: media.OutputPreview, SubtitleFile: first,
	}))
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType: media.SourceWebcam, OutputMode: media.OutputPreview, SubtitleFile: second,
	}))
	require.NoFileExists(t, first)
	require.FileExists(t, second)

	h.Stop()
	require.NoFileExists(t, second)
}

func TestHTTPStreamHandlerRequiresExplicitSourceType(t *testing.T) {
	h := NewHTTPStreamHandler()

	err := h.Start(media.SessionConfig{OutputMode: media.OutputPreview})

	require.EqualError(t, err, `unsupported media source type ""`)
	require.False(t, h.IsSessionActive())
}

func TestHTTPStreamHandlerRejectsStaleOutputAndAnnotationSessionID(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.config.AnnotationOverlay = true
	h.sessionActive = true
	h.relaySessionID = "session-2"

	_, _, err := h.SubscribeOutputForSessionID("session-1")
	require.ErrorIs(t, err, errSessionSuperseded)
	_, err = h.beginAnnotationUpdateForSessionID("session-1")
	require.ErrorIs(t, err, errSessionSuperseded)
}

func TestHTTPControlRequestsAreSessionIDFenced(t *testing.T) {
	server := NewServer()
	start := func(sessionID string) {
		body := fmt.Sprintf(
			`{"input":{"type":"webcam"},"output":{"mode":"websocket"},`+
				`"pipeline":[{"op":"encode"}],"session_id":%q}`,
			sessionID,
		)
		request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
	}

	start("session-1")
	start("session-2")

	ingestRequest := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader([]byte{0x1A, 0x45, 0xDF, 0xA3}))
	ingestRequest.Header.Set("X-Streamline-Session-ID", "session-1")
	ingestResponse := httptest.NewRecorder()
	server.ServeHTTP(ingestResponse, ingestRequest)
	require.Equal(t, http.StatusConflict, ingestResponse.Code)

	stopRequest := httptest.NewRequest(http.MethodPost, "/stop", nil)
	stopRequest.Header.Set("X-Streamline-Session-ID", "session-1")
	stopResponse := httptest.NewRecorder()
	server.ServeHTTP(stopResponse, stopRequest)
	require.Equal(t, http.StatusConflict, stopResponse.Code)
	require.True(t, server.httpHandler.IsSessionActive())

	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.Header.Set("X-Streamline-Session-ID", "session-1")
	metricsResponse := httptest.NewRecorder()
	server.ServeHTTP(metricsResponse, metricsRequest)
	require.Equal(t, http.StatusConflict, metricsResponse.Code)

	currentStopRequest := httptest.NewRequest(http.MethodPost, "/stop", nil)
	currentStopRequest.Header.Set("X-Streamline-Session-ID", "session-2")
	currentStopResponse := httptest.NewRecorder()
	server.ServeHTTP(currentStopResponse, currentStopRequest)
	require.Equal(t, http.StatusOK, currentStopResponse.Code)
	require.False(t, server.httpHandler.IsSessionActive())
}
