package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/media"
	"github.com/cloudflare/streamline/container/protocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestPublishOutputSendsMediaFrames(t *testing.T) {
	received := make(chan []byte, 1)
	connected := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Values("CF-Access-Client-Id"))
		require.Empty(t, r.Header.Values("CF-Access-Client-Secret"))
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		close(connected)
		_, message, err := conn.ReadMessage()
		require.NoError(t, err)
		received <- message
	}))
	t.Cleanup(server.Close)

	h := NewHTTPStreamHandler()
	h.config.OutputMode = media.OutputPreview
	h.ffmpeg = &exec.Cmd{}
	h.isRunning = true
	s := &Server{httpHandler: h}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- s.publishOutput(ctx, protocol.RelayConfig{
			URL:   "ws" + strings.TrimPrefix(server.URL, "http"),
			Token: "test-token",
		}, "")
	}()

	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("publisher did not connect")
	}
	require.Eventually(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.outputSubscriber != nil
	}, time.Second, time.Millisecond)

	publishOutputForTest(h, []byte{1, 2, 3})
	select {
	case frame := <-received:
		require.Equal(t, []byte{1, 2, 3}, frame)
	case <-time.After(time.Second):
		t.Fatal("publisher did not send media")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publisher did not stop")
	}
}

func TestCoalesceOutputChunks(t *testing.T) {
	chunks := make(chan []byte, 3)
	chunks <- []byte{3, 4}
	chunks <- []byte{5, 6}
	close(chunks)

	batch, closed := coalesceOutputChunks([]byte{1, 2}, chunks)
	require.Equal(t, []byte{1, 2, 3, 4, 5, 6}, batch)
	require.True(t, closed)
}
