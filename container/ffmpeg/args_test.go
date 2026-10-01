package ffmpeg

import (
	"fmt"
	"testing"

	"github.com/cloudflare/streamline/container/media"
	"github.com/stretchr/testify/require"
)

func TestBuildPipelineArgs(t *testing.T) {
	tests := []struct {
		name     string
		cfg      media.SessionConfig
		input    string
		output   string
		expected []string
	}{
		{
			name:   "webcam passthrough to rtmp",
			cfg:    media.SessionConfig{},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "/dev/fd/3",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "webcam passthrough with preview mode",
			cfg: media.SessionConfig{
				OutputMode: media.OutputPreview,
			},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "/dev/fd/3",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", "30",
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-crf", "20",
				"-maxrate", defaultTranscodeBitrate,
				"-bufsize", defaultTranscodeBitrate,
				"-an",
				"-tune", "zerolatency",
				"-profile:v", "baseline",
				"-level", "3.1",
				"-flush_packets", "1",
				"-f", "mp4",
				"-movflags", "frag_keyframe+empty_moov+default_base_moof",
				"pipe:1",
			},
		},
		{
			name: "HLS source passthrough",
			cfg: media.SessionConfig{
				SourceType: media.SourceHLS,
			},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-fflags", "+genpts",
				"-copyts",
				"-i", "/dev/fd/3",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-tune", "zerolatency",
				"-profile:v", "baseline",
				"-level", "3.1",
				"-flush_packets", "1",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "rtmps source passthrough",
			cfg: media.SessionConfig{
				SourceType: media.SourceRTMP,
			},
			input:  "rtmps://live.cloudflare.com:443/live/playback-key",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-rw_timeout", "5000000",
				"-rtmp_live", "live",
				"-fflags", "+genpts",
				"-i", "rtmps://live.cloudflare.com:443/live/playback-key",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "copy",
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-tune", "zerolatency",
				"-profile:v", "baseline",
				"-level", "3.1",
				"-flush_packets", "1",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "passthrough with subtitle burn-in",
			cfg: media.SessionConfig{
				BurnSubtitles: true,
				SubtitleFile:  "/tmp/sub.srt",
			},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "/dev/fd/3",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[v0]format=yuv420p,subtitles=/tmp/sub.srt:force_style='BackColour=&H80000000,BorderStyle=3,Outline=4,Shadow=0'[vout]",
				"-map", "[vout]",
				"-map", "0:a?",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildPipelineArgs(tt.cfg, tt.input, tt.output)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildArgsUsesFirstExtraFileForStdinAnnotation(t *testing.T) {
	args := buildPipelineArgs(media.SessionConfig{AnnotationOverlay: true}, "pipe:0", "pipe:1")

	require.Contains(t, args, "/dev/fd/3")
	require.NotContains(t, args, "/dev/fd/4")
}

func TestBuildDirectWebcamPiPArgs(t *testing.T) {
	baseConfig := media.SessionConfig{
		WebcamOverlay:  true,
		WebcamScale:    0.25,
		WebcamPosition: "top-right",
		Encode: media.EncodeConfig{
			Resolution: "1280x720", FPS: 30,
		},
	}

	t.Run("preview remains video only", func(t *testing.T) {
		cfg := baseConfig
		cfg.SourceType = media.SourceRTMP
		cfg.OutputMode = media.OutputPreview
		args := buildPipelineArgs(cfg, "rtmps://live.cloudflare.com:443/live/input-key", "pipe:1")

		require.Contains(t, args, "pipe:0")
		require.Contains(t, args, "[vout]")
		require.Contains(t, args, "0:a?")
		require.Contains(t, args, "-an")
		require.Contains(t, args,
			"[0:v]scale=1280:720:force_original_aspect_ratio=decrease,"+
				"pad=1280:720:(ow-iw)/2:(oh-ih)/2:black,setpts=PTS-STARTPTS[v0];"+
				"[1:v]setpts=PTS-STARTPTS,scale=320:180:force_original_aspect_ratio=decrease,setsar=1[webcam];"+
				"[v0][webcam]overlay=max(0\\,W-w-10):min(10\\,H-h):format=auto:shortest=1[v1];"+
				"[v1]format=yuv420p[vout]")
	})

	t.Run("RTMP output copies primary audio", func(t *testing.T) {
		cfg := baseConfig
		cfg.SourceType = media.SourceRTMP
		cfg.Encode.Resolution = "1920x1080"
		args := buildPipelineArgs(cfg, "rtmps://live.cloudflare.com:443/live/input-key", "rtmp://out")

		require.Contains(t, args, "0:a?")
		require.Contains(t, args, "copy")
		require.Contains(t, args, "4.0")
		require.NotContains(t, args, "3.1")
		require.NotContains(t, args, "-an")
	})

	t.Run("annotation overlay preserves primary audio mapping", func(t *testing.T) {
		cfg := baseConfig
		cfg.SourceType = media.SourceRTMP
		cfg.AnnotationOverlay = true
		args := buildPipelineArgs(cfg, "rtmps://live.cloudflare.com:443/live/input-key", "rtmp://out")

		require.Contains(t, args, "0:a?")
		require.Contains(t, args, "copy")
	})

	t.Run("HLS output retains primary audio", func(t *testing.T) {
		cfg := baseConfig
		cfg.SourceType = media.SourceHLS
		cfg.OutputMode = media.OutputPreview
		args := buildPipelineArgs(cfg, "https://videodelivery.net/video-id/manifest/video.m3u8", "pipe:1")

		require.Contains(t, args, "0:a?")
		require.Contains(t, args, "-c:a")
		require.Contains(t, args, "aac")
		require.NotContains(t, args, "-an")
	})
}

func TestAppendDirectInputArgsSkipsTLSVerifyForRTMPSWhenEnabled(t *testing.T) {
	t.Setenv("INSECURE_SKIP_VERIFY", "1")

	result := appendDirectInputArgs([]string{"-y"}, "rtmps://live.cloudflare.com:443/live/playback-key")

	require.Equal(t, []string{
		"-y",
		"-rw_timeout", "5000000",
		"-rtmp_live", "live",
		"-tls_verify", "0",
		"-fflags", "+genpts",
		"-i", "rtmps://live.cloudflare.com:443/live/playback-key",
	}, result)
}

func TestBuildOverlayPipelineArgs(t *testing.T) {
	tests := []struct {
		name     string
		cfg      media.SessionConfig
		input    string
		output   string
		expected []string
	}{
		{
			name: "fixed logo overlay",
			cfg: media.SessionConfig{
				StaticLogoOverlay: true,
			},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "/dev/fd/3",
				"-i", "/app/assets/cf-logo.png",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[1:v]format=rgba,scale=600:-1[logo];[v0][logo]overlay=W-w-10:10:format=auto[v1];[v1]format=yuv420p[vout]",
				"-map", "[vout]",
				"-map", "0:a?",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "annotation overlay on webcam input",
			cfg: media.SessionConfig{
				AnnotationOverlay: true,
			},
			input:  "pipe:0",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "pipe:0",
				"-framerate", fmt.Sprintf("%d", media.AnnotationOverlayFPS),
				"-f", "image2pipe", "-vcodec", "png", "-probesize", "32", "-i", "/dev/fd/3",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[1:v]fps=5,format=rgba[annotation];[v0][annotation]overlay=0:0:format=auto:eof_action=repeat[v1];[v1]format=yuv420p[vout]",
				"-map", "[vout]",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "static and annotation overlays stack in order",
			cfg: media.SessionConfig{
				StaticLogoOverlay: true,
				AnnotationOverlay: true,
			},
			input:  "pipe:0",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "pipe:0",
				"-i", "/app/assets/cf-logo.png",
				"-framerate", fmt.Sprintf("%d", media.AnnotationOverlayFPS),
				"-f", "image2pipe", "-vcodec", "png", "-probesize", "32", "-i", "/dev/fd/3",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[1:v]format=rgba,scale=600:-1[logo];[v0][logo]overlay=W-w-10:10:format=auto[v1];[2:v]fps=5,format=rgba[annotation];[v1][annotation]overlay=0:0:format=auto:eof_action=repeat[v2];[v2]format=yuv420p[vout]",
				"-map", "[vout]",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildPipelineArgs(tt.cfg, tt.input, tt.output)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildFilterPipelineArgs(t *testing.T) {
	tests := []struct {
		name     string
		cfg      media.SessionConfig
		input    string
		output   string
		expected []string
	}{
		{
			name: "filter with brightness only",
			cfg: media.SessionConfig{
				Filters: media.FilterConfig{
					Brightness: 0.5,
					Contrast:   1.0,
					Saturation: 1.0,
					Gamma:      1.0,
				},
			},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "/dev/fd/3",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[v0]eq=brightness=0.50[v1];[v1]format=yuv420p[vout]",
				"-map", "[vout]",
				"-map", "0:a?",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "filter with multiple effects",
			cfg: media.SessionConfig{
				Filters: media.FilterConfig{
					Blur:          2.0,
					Contrast:      1.5,
					HasContrast:   true,
					Saturation:    0.5,
					HasSaturation: true,
					Gamma:         1.0,
					HasGamma:      true,
					Flip:          true,
					Rotate:        90,
				},
			},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "/dev/fd/3",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[v0]eq=contrast=1.50:saturation=0.50,boxblur=2.0:1,hflip,transpose=1[v1];[v1]format=yuv420p[vout]",
				"-map", "[vout]",
				"-map", "0:a?",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "filter preserves explicit zero saturation",
			cfg: media.SessionConfig{
				Filters: media.FilterConfig{
					Saturation:    0.0,
					HasSaturation: true,
				},
			},
			input:  "/dev/fd/3",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "/dev/fd/3",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[v0]eq=saturation=0.00[v1];[v1]format=yuv420p[vout]",
				"-map", "[vout]",
				"-map", "0:a?",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "filter preset can also carry annotation overlay",
			cfg: media.SessionConfig{
				AnnotationOverlay: true,
				Filters: media.FilterConfig{
					Brightness: 0.25,
					Contrast:   1.0,
					Saturation: 1.0,
					Gamma:      1.0,
				},
			},
			input:  "pipe:0",
			output: "rtmp://out",
			expected: []string{
				"-y",
				"-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "pipe:0",
				"-framerate", fmt.Sprintf("%d", media.AnnotationOverlayFPS),
				"-f", "image2pipe", "-vcodec", "png", "-probesize", "32", "-i", "/dev/fd/3",
				"-filter_complex", "[0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black[v0];[v0]eq=brightness=0.25[v1];[1:v]fps=5,format=rgba[annotation];[v1][annotation]overlay=0:0:format=auto:eof_action=repeat[v2];[v2]format=yuv420p[vout]",
				"-map", "[vout]",
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", defaultTranscodeGOP,
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildPipelineArgs(tt.cfg, tt.input, tt.output)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestAppendEncodeArgs(t *testing.T) {
	tests := []struct {
		name     string
		cfg      media.SessionConfig
		output   string
		expected []string
	}{
		{
			name: "default encode params",
			cfg:  media.SessionConfig{},
			expected: []string{
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", "120",
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "custom encode params",
			cfg: media.SessionConfig{
				Encode: media.EncodeConfig{
					Preset:     "slow",
					Bitrate:    "5000k",
					Resolution: "1920x1080",
					FPS:        60,
					GOP:        120,
				},
			},
			expected: []string{
				"-c:v", "libx264",
				"-preset", "slow",
				"-b:v", "5000k",
				"-g", "120",
				"-bf", "0",
				"-s", "1920x1080",
				"-r", "60",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "HLS source encode params keep RTMP compatibility flags",
			cfg: media.SessionConfig{
				SourceType: media.SourceHLS,
			},
			expected: []string{
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", "120",
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-max_muxing_queue_size", "1024",
				"-flvflags", "no_duration_filesize",
				"-tune", "zerolatency",
				"-profile:v", "baseline",
				"-level", "3.1",
				"-flush_packets", "1",
				"-f", "flv",
				"rtmp://out",
			},
		},
		{
			name: "preview mode",
			cfg: media.SessionConfig{
				OutputMode: media.OutputPreview,
			},
			expected: []string{
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", "30",
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-crf", "20",
				"-maxrate", defaultTranscodeBitrate,
				"-bufsize", defaultTranscodeBitrate,
				"-an",
				"-tune", "zerolatency",
				"-profile:v", "baseline",
				"-level", "3.1",
				"-flush_packets", "1",
				"-f", "mp4",
				"-movflags", "frag_keyframe+empty_moov+default_base_moof",
				"pipe:1",
			},
		},
		{
			name: "preview mode keeps audio for HLS input",
			cfg: media.SessionConfig{
				OutputMode: media.OutputPreview,
				SourceType: media.SourceHLS,
			},
			expected: []string{
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", "30",
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-crf", "20",
				"-maxrate", defaultTranscodeBitrate,
				"-bufsize", defaultTranscodeBitrate,
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
				"-tune", "zerolatency",
				"-profile:v", "baseline",
				"-level", "3.1",
				"-flush_packets", "1",
				"-f", "mp4",
				"-movflags", "frag_keyframe+empty_moov+default_base_moof",
				"pipe:1",
			},
		},
		{
			name: "preview mode disables audio for RTMP input",
			cfg: media.SessionConfig{
				OutputMode: media.OutputPreview,
				SourceType: media.SourceRTMP,
			},
			expected: []string{
				"-c:v", "libx264",
				"-preset", "fast",
				"-b:v", defaultTranscodeBitrate,
				"-g", "30",
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-crf", "20",
				"-maxrate", defaultTranscodeBitrate,
				"-bufsize", defaultTranscodeBitrate,
				"-an",
				"-tune", "zerolatency",
				"-profile:v", "baseline",
				"-level", "3.1",
				"-flush_packets", "1",
				"-f", "mp4",
				"-movflags", "frag_keyframe+empty_moov+default_base_moof",
				"pipe:1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := appendEncodeArgs([]string{}, tt.cfg, "rtmp://out")
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildArgsSelectsSessionIO(t *testing.T) {
	source := "rtmp://source-user:source-pass@example.test/live/input"
	destination := "rtmps://dest-user:dest-pass@example.test/live/output"

	t.Run("preview reads the direct source and writes fMP4 to stdout", func(t *testing.T) {
		args, err := BuildArgs(media.SessionConfig{
			SourceType:     media.SourceRTMP,
			SourceURL:      source,
			DestinationURL: destination,
			OutputMode:     media.OutputPreview,
		})

		require.NoError(t, err)
		require.Contains(t, args, source)
		require.Contains(t, args, "pipe:1")
		require.NotContains(t, args, destination)
	})

	t.Run("RTMP output writes directly to the destination", func(t *testing.T) {
		args, err := BuildArgs(media.SessionConfig{
			SourceType:     media.SourceRTMP,
			SourceURL:      source,
			DestinationURL: destination,
			OutputMode:     media.OutputRTMP,
		})

		require.NoError(t, err)
		require.NotContains(t, args, "pipe:1")
		require.Contains(t, args, destination)
	})

	t.Run("test mode uses the synthetic input", func(t *testing.T) {
		args, err := BuildArgs(media.SessionConfig{
			SourceType: media.SourceTest, OutputMode: media.OutputPreview,
		})

		require.NoError(t, err)
		require.Equal(t, []string{
			"-re",
			"-f", "lavfi",
			"-i", "testsrc=duration=60:size=160x120:rate=10",
			"-pix_fmt", "yuv420p",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-threads", "1",
			"-g", "30",
			"-crf", "32",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-min_frag_duration", "2000000",
			"pipe:1",
		}, args)
	})
}

func TestBuildArgsRejectsIgnoredTestInputConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		change func(*media.SessionConfig)
	}{
		{name: "static logo", change: func(cfg *media.SessionConfig) { cfg.StaticLogoOverlay = true }},
		{name: "annotation", change: func(cfg *media.SessionConfig) { cfg.AnnotationOverlay = true }},
		{name: "webcam overlay", change: func(cfg *media.SessionConfig) { cfg.WebcamOverlay = true }},
		{name: "subtitles", change: func(cfg *media.SessionConfig) { cfg.BurnSubtitles = true }},
		{name: "filters", change: func(cfg *media.SessionConfig) { cfg.Filters.Flip = true }},
		{name: "encode parameters", change: func(cfg *media.SessionConfig) { cfg.Encode.Preset = "fast" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := media.SessionConfig{SourceType: media.SourceTest, OutputMode: media.OutputPreview}
			tt.change(&cfg)

			_, err := BuildArgs(cfg)

			require.EqualError(t, err, "test input does not support overlays, filters, subtitles, or encode parameters")
		})
	}
}

func TestH264Level(t *testing.T) {
	require.Equal(t, "3.1", h264Level("1280x720", 30))
	require.Equal(t, "4.0", h264Level("1920x1080", 30))
	require.Equal(t, "4.2", h264Level("1920x1080", 60))
	require.Equal(t, "5.2", h264Level("3840x2160", 60))
	require.Equal(t, "6.1", h264Level("3840x2160", 240))
}

func TestScaledSizeForResolution(t *testing.T) {
	width, height := scaledSizeForResolution("1280x720", 0.25)
	require.Equal(t, 320, width)
	require.Equal(t, 180, height)

	width, height = scaledSizeForResolution("1280x720", 1)
	require.Equal(t, 1260, width)
	require.Equal(t, 700, height)
}

func TestAppendDirectInputArgs(t *testing.T) {
	t.Run("http input does not disable TLS by default", func(t *testing.T) {
		result := appendDirectInputArgs(nil, "https://example.com/video.m3u8")
		require.Equal(t, []string{
			"-re",
			"-fflags", "+genpts",
			"-copyts",
			"-i", "https://example.com/video.m3u8",
		}, result)
	})

	t.Run("http input can disable TLS verification for local dev", func(t *testing.T) {
		t.Setenv("INSECURE_SKIP_VERIFY", "1")

		result := appendDirectInputArgs(nil, "https://example.com/video.m3u8")
		require.Equal(t, []string{
			"-re",
			"-tls_verify", "0",
			"-fflags", "+genpts",
			"-copyts",
			"-i", "https://example.com/video.m3u8",
		}, result)
	})
}
