package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/subtitle"
	"github.com/stretchr/testify/require"
)

func TestNewHTTPServerTimeouts(t *testing.T) {
	server := newHTTPServer(":0", http.NotFoundHandler())

	require.Equal(t, serverReadHeaderTimeout, server.ReadHeaderTimeout)
	require.Equal(t, serverIdleTimeout, server.IdleTimeout)
	require.Zero(t, server.ReadTimeout)
	require.Zero(t, server.WriteTimeout)
}

func TestParseMaxSessionDuration(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset is unlimited"},
		{name: "positive seconds", value: "3600", want: time.Hour},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "fractional", value: "1.5", wantErr: true},
		{name: "whitespace", value: " 1", wantErr: true},
		{name: "duration overflow", value: "9999999999999999999", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMaxSessionDuration(tt.value)
			if tt.wantErr {
				require.Error(t, err)
				require.Zero(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, got)
			}
		})
	}
}

func TestMaximumSessionDurationStopsProcessAndRelay(t *testing.T) {
	installFakeFFmpeg(t, "exec sleep 30\n")
	server := newServer(time.Hour)
	t.Cleanup(server.httpHandler.Stop)
	request := httptest.NewRequest(
		http.MethodPost,
		"/start",
		strings.NewReader(`{"input":{"type":"test"},"output":{"mode":"websocket"},`+
			`"pipeline":[{"op":"encode"}],`+
			`"session_id":"session-1"}`),
	)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	sessionID, active := server.httpHandler.activeSessionID()
	require.True(t, active)
	server.httpHandler.mu.RLock()
	process := server.httpHandler.ffmpeg.Process
	server.httpHandler.mu.RUnlock()

	relayStopped := make(chan struct{})
	server.relayMu.Lock()
	server.relayCancel = func() { close(relayStopped) }
	server.relayDone = relayStopped
	server.relayMu.Unlock()

	server.expireSession(sessionID)

	require.False(t, server.httpHandler.IsSessionActive())
	require.True(t, server.matchesActiveSessionID(""))
	select {
	case <-relayStopped:
	default:
		t.Fatal("relay publisher was not stopped")
	}
	require.Eventually(t, func() bool {
		return errors.Is(process.Signal(syscall.Signal(0)), os.ErrProcessDone)
	}, time.Second, time.Millisecond)
	server.sessionMu.Lock()
	require.Nil(t, server.sessionTimer)
	require.Zero(t, server.sessionTimerID)
	server.sessionMu.Unlock()
}

func TestMaximumSessionDurationCancelsStaleTimers(t *testing.T) {
	server := newServer(time.Hour)
	start := func(sessionID string) {
		request := httptest.NewRequest(
			http.MethodPost,
			"/start",
			strings.NewReader(`{"input":{"type":"webcam"},"output":{"mode":"websocket"},`+
				`"pipeline":[{"op":"encode"}],`+
				`"session_id":"`+sessionID+`"}`),
		)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}

	start("session-1")
	firstSessionID, active := server.httpHandler.activeSessionID()
	require.True(t, active)
	server.sessionMu.Lock()
	firstTimer := server.sessionTimer
	server.sessionMu.Unlock()
	require.NotNil(t, firstTimer)

	start("session-2")
	secondSessionID, active := server.httpHandler.activeSessionID()
	require.True(t, active)
	require.NotEqual(t, firstSessionID, secondSessionID)
	require.False(t, firstTimer.Stop())

	server.expireSession(firstSessionID)
	require.True(t, server.httpHandler.isSessionActiveForID(secondSessionID))

	server.sessionMu.Lock()
	secondTimer := server.sessionTimer
	server.sessionMu.Unlock()
	require.NotNil(t, secondTimer)
	stopRequest := httptest.NewRequest(http.MethodPost, "/stop", nil)
	stopRequest.Header.Set("X-Streamline-Session-ID", "session-2")
	stopResponse := httptest.NewRecorder()
	server.ServeHTTP(stopResponse, stopRequest)
	require.Equal(t, http.StatusOK, stopResponse.Code)
	require.False(t, secondTimer.Stop())
	server.sessionMu.Lock()
	require.Nil(t, server.sessionTimer)
	require.Zero(t, server.sessionTimerID)
	server.sessionMu.Unlock()
}

func TestAbortedReplacementStartPreservesActiveSessionTimer(t *testing.T) {
	server := newServer(time.Hour)
	startRequest := httptest.NewRequest(
		http.MethodPost,
		"/start",
		strings.NewReader(`{"input":{"type":"webcam"},"output":{"mode":"websocket"},`+
			`"pipeline":[{"op":"encode"}],`+
			`"session_id":"session-1"}`),
	)
	startResponse := httptest.NewRecorder()
	server.ServeHTTP(startResponse, startRequest)
	require.Equal(t, http.StatusOK, startResponse.Code, startResponse.Body.String())

	server.sessionMu.Lock()
	activeTimer := server.sessionTimer
	server.sessionMu.Unlock()
	require.NotNil(t, activeTimer)

	entered := make(chan struct{})
	server.subtitleFetcher = func(ctx context.Context, _ string) (*subtitle.FetchResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	replacement := httptest.NewRequest(
		http.MethodPost,
		"/start",
		strings.NewReader(`{"input":{"type":"hls",`+
			`"url":"https://videodelivery.net/video-id/manifest/video.m3u8"},`+
			`"output":{"mode":"websocket"},"pipeline":[{"op":"subtitle"},{"op":"encode"}],`+
			`"session_id":"session-2"}`),
	).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(httptest.NewRecorder(), replacement)
		close(done)
	}()
	<-entered
	cancel()
	<-done

	server.sessionMu.Lock()
	require.Same(t, activeTimer, server.sessionTimer)
	server.sessionMu.Unlock()
	require.True(t, server.httpHandler.IsSessionActive())
	require.True(t, server.matchesActiveSessionID("session-1"))
	require.True(t, activeTimer.Stop())
}

func TestIsOriginAllowed(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed []string
		want    bool
	}{
		{name: "non-browser client", allowed: []string{"https://example.com"}, want: true},
		{name: "exact origin", origin: "https://example.com", allowed: []string{"https://example.com"}, want: true},
		{
			name: "comma separated value whitespace", origin: "https://example.com",
			allowed: []string{" https://example.com "}, want: true,
		},
		{
			name: "rejects prefix attack", origin: "https://example.com.evil.test",
			allowed: []string{"https://example.com"},
		},
		{
			name: "localhost with port", origin: "http://localhost:4321",
			allowed: []string{"http://localhost:"}, want: true,
		},
		{
			name: "rejects different origin", origin: "https://other.example.com",
			allowed: []string{"https://example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOriginAllowed(tt.origin, tt.allowed); got != tt.want {
				t.Fatalf("isOriginAllowed(%q, %q) = %v, want %v", tt.origin, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestRetiredRoutesReturnNotFound(t *testing.T) {
	server := NewServer()
	paths := []string{
		"/ws",
		"/init.mp4",
		"/segments",
		"/segment/0",
		"/data",
		"/container-status",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			require.Equal(t, http.StatusNotFound, response.Code)
		})
	}
}
