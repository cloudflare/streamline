package main

import (
	"testing"

	"github.com/cloudflare/streamline/container/media"
	"github.com/cloudflare/streamline/container/protocol"
	"github.com/stretchr/testify/require"
)

func TestExtractVideoID(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected string
	}{
		{name: "standard Stream URL", url: "https://videodelivery.net/abc123/manifest/video.m3u8", expected: "abc123"},
		{name: "URL without trailing path", url: "https://videodelivery.net/abc123", expected: "abc123"},
		{
			name: "customer cloudflarestream manifest URL",
			url:  "https://customer-demo.cloudflarestream.com/abc123/manifest/video.m3u8", expected: "abc123",
		},
		{name: "non-Stream URL", url: "https://example.com/video.m3u8", expected: ""},
		{name: "empty string", url: "", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, extractVideoID(tt.url))
		})
	}
}

func TestBuildMediaConfig(t *testing.T) {
	t.Run("maps webcam input transform", func(t *testing.T) {
		transform := &protocol.InputTransform{Scale: 0.25, Position: "top-right"}
		result := buildMediaConfig(media.SourceRTMP, streamRtmpURL("input-key"), transform, []protocol.Operation{
			{Op: "encode", Params: map[string]interface{}{
				"codec": "h264", "resolution": "1280x720", "fps": float64(30),
			}},
		}, protocol.OutputConfig{Mode: "rtmp", Key: "output-key"})

		require.True(t, result.WebcamOverlay)
		require.Equal(t, 0.25, result.WebcamScale)
		require.Equal(t, "top-right", result.WebcamPosition)
		require.Equal(t, media.SourceRTMP, result.SourceType)
		require.Equal(t, streamRtmpURL("input-key"), result.SourceURL)
	})

	t.Run("maps static overlay and encode operations", func(t *testing.T) {
		result := buildMediaConfig(media.SourceWebcam, "", nil, []protocol.Operation{
			{Op: "overlay", Params: map[string]interface{}{
				"image": "/app/assets/streamline-logo.png", "position": "top-right",
			}},
			{Op: "encode", Params: map[string]interface{}{
				"codec": "h264", "preset": "slow", "bitrate": "3500k", "resolution": "1920x1080",
				"fps": float64(60), "gop": float64(120),
			}},
		}, protocol.OutputConfig{Mode: "rtmp", Key: "output-key"})

		require.Equal(t, media.SessionConfig{
			StaticLogoOverlay: true,
			SourceType:        media.SourceWebcam,
			DestinationURL:    "rtmps://live.cloudflare.com:443/live/output-key",
			OutputMode:        media.OutputRTMP,
			Filters:           media.FilterConfig{Contrast: 1.0, Saturation: 1.0, Gamma: 1.0},
			Encode: media.EncodeConfig{
				Preset: "slow", Bitrate: "3500k", Resolution: "1920x1080", FPS: 60, GOP: 120,
			},
		}, result)
	})

	t.Run("maps annotation filters subtitles and preview output", func(t *testing.T) {
		result := buildMediaConfig(media.SourceHLS, "https://videodelivery.net/abc123/manifest/video.m3u8", nil, []protocol.Operation{
			{Op: "overlay", Params: map[string]interface{}{"image": "annotation", "position": "full"}},
			{Op: "filter", Params: map[string]interface{}{"preset": "brightness", "amount": 0.25}},
			{Op: "filter", Params: map[string]interface{}{"preset": "saturation", "amount": float64(0)}},
			{Op: "filter", Params: map[string]interface{}{"preset": "rotate", "degrees": float64(90)}},
			{Op: "subtitle", Params: map[string]interface{}{"source": "auto"}},
		}, protocol.OutputConfig{Mode: "websocket"})

		require.Equal(t, media.SessionConfig{
			SourceType: media.SourceHLS, SourceURL: "https://videodelivery.net/abc123/manifest/video.m3u8",
			OutputMode: media.OutputPreview, AnnotationOverlay: true, BurnSubtitles: true,
			Filters: media.FilterConfig{
				Brightness: 0.25, Contrast: 1.0, Saturation: 0, HasSaturation: true, Gamma: 1.0, Rotate: 90,
			},
		}, result)
	})

	t.Run("maps static logo and annotation overlays independently", func(t *testing.T) {
		result := buildMediaConfig(media.SourceHLS, "https://example.test/video.m3u8", nil, []protocol.Operation{
			{Op: "overlay", Params: map[string]interface{}{
				"image": "/app/assets/streamline-logo.png", "position": "top-right",
			}},
			{Op: "overlay", Params: map[string]interface{}{"image": "annotation", "position": "full"}},
			{Op: "encode"},
		}, protocol.OutputConfig{Mode: "websocket"})

		require.True(t, result.StaticLogoOverlay)
		require.True(t, result.AnnotationOverlay)
	})
}
