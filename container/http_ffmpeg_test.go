package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/media"
	"github.com/stretchr/testify/require"
)

func TestHTTPStreamHandlerRTMPOutputHasNoStdoutOrSubscription(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("FFMPEG_ARGS_FILE", argsFile)
	installFakeFFmpeg(t, `printf '%s\n' "$@" > "$FFMPEG_ARGS_FILE"`+"\nexec sleep 30\n")
	destination := "rtmp://user:password@example.test/live/key"
	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType:     media.SourceRTMP,
		SourceURL:      "rtmp://source.example.test/live/input",
		DestinationURL: destination,
		OutputMode:     media.OutputRTMP,
	}))
	t.Cleanup(h.Stop)

	require.Nil(t, h.stdout)
	_, _, err := h.SubscribeOutput()
	require.EqualError(t, err, "output subscription is not available for RTMP output")
	require.Eventually(t, func() bool {
		args, readErr := os.ReadFile(argsFile)
		return readErr == nil && strings.Contains(string(args), destination) && !strings.Contains(string(args), "pipe:1")
	}, 3*time.Second, 10*time.Millisecond)
}

func TestHTTPStreamHandlerTracksFFmpegProgress(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.sessionID = 7

	require.True(t, h.updateFFmpegProgress(7, "frame=123"))
	require.True(t, h.updateFFmpegProgress(7, "fps=29.97"))
	require.True(t, h.updateFFmpegProgress(7, "bitrate=2510.4kbits/s"))
	require.True(t, h.updateFFmpegProgress(7, "out_time_us=4100000"))
	require.True(t, h.updateFFmpegProgress(7, "speed=0.987x"))
	require.True(t, h.updateFFmpegProgress(7, "progress=continue"))
	require.False(t, h.updateFFmpegProgress(7, "ordinary diagnostic"))

	metrics := h.Metrics()
	require.Equal(t, int64(123), metrics.Progress.Frame)
	require.InDelta(t, 29.97, metrics.Progress.FPS, 0.001)
	require.Equal(t, "2510.4kbits/s", metrics.Progress.Bitrate)
	require.Equal(t, int64(4100000), metrics.Progress.OutTimeUS)
	require.InDelta(t, 0.987, metrics.Progress.Speed, 0.001)
	require.Equal(t, "continue", metrics.Progress.State)
	require.False(t, metrics.Progress.UpdatedAt.IsZero())

	require.True(t, h.updateFFmpegProgress(6, "frame=999"))
	require.Equal(t, int64(123), h.Metrics().Progress.Frame)
}

func TestHTTPStreamHandlerRestartsFailedRTMPInput(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	require.NoError(t, os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nprintf output\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType: media.SourceRTMP,
		SourceURL:  "rtmp://example.test/live",
		OutputMode: media.OutputPreview,
	}))
	t.Cleanup(h.Stop)

	require.Eventually(t, func() bool {
		metrics := h.Metrics()
		return metrics.SessionActive && metrics.RestartCount > 0
	}, 2*time.Second, 10*time.Millisecond)

	h.Stop()
	require.False(t, h.Metrics().SessionActive)
}

func TestHTTPStreamHandlerRestartsRTMPInputWithRTMPOutput(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "count")
	t.Setenv("FFMPEG_COUNT_FILE", countFile)
	installFakeFFmpeg(t, `count=0
if [ -f "$FFMPEG_COUNT_FILE" ]; then read count < "$FFMPEG_COUNT_FILE"; fi
count=$((count + 1))
printf '%s\n' "$count" > "$FFMPEG_COUNT_FILE"
if [ "$count" -eq 1 ]; then exit 1; fi
exec sleep 30
`)
	subtitleFile := filepath.Join(t.TempDir(), "captions.srt")
	require.NoError(t, os.WriteFile(subtitleFile, []byte("captions"), 0o600))

	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType:     media.SourceRTMP,
		SourceURL:      "rtmp://source.example.test/live/input",
		DestinationURL: "rtmp://destination.example.test/live/output",
		OutputMode:     media.OutputRTMP,
		SubtitleFile:   subtitleFile,
	}))
	t.Cleanup(h.Stop)

	require.Eventually(t, func() bool {
		metrics := h.Metrics()
		return metrics.Running && metrics.RestartCount == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Nil(t, h.stdout)
	require.FileExists(t, subtitleFile)
	require.Equal(t, "rtmp", h.Metrics().OutputMode)

	h.Stop()
	require.NoFileExists(t, subtitleFile)
}

func TestHTTPStreamHandlerStopsPiPWhenWebcamIngestStalls(t *testing.T) {
	installFakeFFmpeg(t, "exec sleep 30\n")
	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType:     media.SourceRTMP,
		SourceURL:      "rtmp://source.example.test/live/input",
		DestinationURL: "rtmp://destination.example.test/live/output",
		OutputMode:     media.OutputRTMP,
		WebcamOverlay:  true,
		WebcamScale:    0.25,
		WebcamPosition: "top-right",
	}))
	t.Cleanup(h.Stop)

	_, err := h.IngestChunk([]byte{0x1A, 0x45, 0xDF, 0xA3})
	require.NoError(t, err)
	h.mu.Lock()
	h.lastIngestAt = time.Now().Add(-webcamIngestTimeout - time.Second)
	h.progress.State = "continue"
	h.progress.UpdatedAt = time.Now()
	h.mu.Unlock()

	require.Eventually(t, func() bool {
		metrics := h.Metrics()
		return !metrics.Running && !metrics.SessionActive
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, 0, h.Metrics().RestartCount)
}

func TestHTTPStreamHandlerKeepsStandaloneWebcamActiveAcrossIngestGap(t *testing.T) {
	installFakeFFmpeg(t, "exec sleep 30\n")
	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType:     media.SourceWebcam,
		DestinationURL: "rtmp://destination.example.test/live/output",
		OutputMode:     media.OutputRTMP,
	}))
	t.Cleanup(h.Stop)

	_, err := h.IngestChunk([]byte{0x1A, 0x45, 0xDF, 0xA3})
	require.NoError(t, err)
	h.mu.Lock()
	h.lastIngestAt = time.Now().Add(-rtmpOutputTimeout - time.Second)
	h.progress.State = "continue"
	h.progress.UpdatedAt = time.Now().Add(-rtmpOutputTimeout - time.Second)
	h.mu.Unlock()

	time.Sleep(1100 * time.Millisecond)
	require.True(t, h.Metrics().Running)
	require.True(t, h.Metrics().SessionActive)
}

func TestHTTPStreamHandlerCleansSubtitleAfterFinalExit(t *testing.T) {
	installFakeFFmpeg(t, "exit 0\n")
	subtitleFile := filepath.Join(t.TempDir(), "captions.srt")
	require.NoError(t, os.WriteFile(subtitleFile, []byte("captions"), 0o600))
	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType:   media.SourceHLS,
		SourceURL:    "https://example.test/video.m3u8",
		OutputMode:   media.OutputPreview,
		SubtitleFile: subtitleFile,
	}))
	t.Cleanup(h.Stop)

	require.Eventually(t, func() bool {
		_, err := os.Stat(subtitleFile)
		return !h.IsSessionActive() && errors.Is(err, os.ErrNotExist)
	}, time.Second, 10*time.Millisecond)
}

func TestHTTPStreamHandlerRestartsStalledRTMPInput(t *testing.T) {
	dir := t.TempDir()
	output := append(mp4Box("ftyp", []byte("file")), mp4Box("moov", []byte("metadata"))...)
	outputPath := filepath.Join(dir, "output.mp4")
	require.NoError(t, os.WriteFile(outputPath, output, 0o644))
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	script := fmt.Sprintf("#!/bin/sh\nsleep 0.1\ncat %q\nexec sleep 30\n", outputPath)
	require.NoError(t, os.WriteFile(ffmpegPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	h := NewHTTPStreamHandler()
	require.NoError(t, h.Start(media.SessionConfig{
		SourceType: media.SourceRTMP,
		SourceURL:  "rtmp://example.test/live",
		OutputMode: media.OutputPreview,
	}))
	t.Cleanup(h.Stop)

	chunks, cancel, err := h.SubscribeOutput()
	require.NoError(t, err)
	t.Cleanup(func() { cancel(nil) })
	require.Equal(t, output, receiveOutputChunk(t, chunks, time.Second))

	h.mu.Lock()
	h.lastOutputAt = time.Now().Add(-rtmpOutputTimeout - time.Second)
	h.mu.Unlock()

	require.Equal(t, output, receiveOutputChunk(t, chunks, 3*time.Second))
	metrics := h.Metrics()
	require.Equal(t, 1, metrics.RestartCount)
	require.True(t, metrics.Running)
	require.True(t, metrics.SessionActive)
}
