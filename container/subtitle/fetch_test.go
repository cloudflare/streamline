package subtitle

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFetchURLBoundsResponse(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, maxSubtitleResponseBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	_, err := fetchURL(context.Background(), server.URL)

	require.ErrorContains(t, err, "subtitle response exceeds")
}

func TestFetchURLHonorsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := fetchURL(ctx, server.URL)

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestFetchHLSSubtitleSegmentsBoundsAggregate(t *testing.T) {
	segment := bytes.Repeat([]byte{'x'}, maxSubtitleResponseBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(segment)
	}))
	t.Cleanup(server.Close)
	manifest := "#EXTM3U\n" + strings.Repeat("segment.vtt\n", 5)

	_, err := fetchHLSSubtitleSegments(
		context.Background(),
		server.URL+"/captions/index.m3u8",
		[]byte(manifest),
	)

	require.ErrorContains(t, err, "subtitle segments exceed")
}
