// Package ffmpeg builds ffmpeg argument lists for the media pass-through
// pipeline.
package ffmpeg

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/streamline/container/media"
)

// defaultTranscodeBitrate is used when source bitrate is unknown.
const (
	defaultTranscodeBitrate = "2M"
	defaultTranscodeGOP     = "120" // ~4s at 30fps, ~5s at 24fps, ~2s at 60fps
	defaultAudioBitrate     = "128k"
	rtmpReadTimeout         = 5 * time.Second
)

const annotationProbeSize = 32

// shouldSkipTLSVerify enables ffmpeg's TLS bypass for local development only.
func shouldSkipTLSVerify() bool {
	return os.Getenv("INSECURE_SKIP_VERIFY") == "1"
}

// appendDirectInputArgs appends the common ffmpeg input flags for HLS/RTMP
// sources that ffmpeg reads directly instead of via browser WebM chunks.
func appendDirectInputArgs(args []string, videoInput string) []string {
	if strings.HasPrefix(videoInput, "http") {
		args = append(args, "-re")
		if shouldSkipTLSVerify() {
			args = append(args, "-tls_verify", "0")
		}
	}

	if strings.HasPrefix(videoInput, "rtmp://") || strings.HasPrefix(videoInput, "rtmps://") {
		args = append(args,
			"-rw_timeout", strconv.FormatInt(rtmpReadTimeout.Microseconds(), 10),
			"-rtmp_live", "live",
		)
		if strings.HasPrefix(videoInput, "rtmps://") && shouldSkipTLSVerify() {
			args = append(args, "-tls_verify", "0")
		}
		// RTMP: do not use -copyts; input timestamps are relative to stream
		// start and can confuse the muxer when overlay is active.
		return append(args, "-fflags", "+genpts", "-i", videoInput)
	}

	return append(args,
		"-fflags", "+genpts",
		"-copyts",
		"-i", videoInput,
	)
}

func annotationInputPath() string {
	return "/dev/fd/3"
}

func webcamOverlayPositionExpressions(position string) (string, string) {
	switch position {
	case "top-left":
		return "min(10\\,W-w)", "min(10\\,H-h)"
	case "bottom-left":
		return "min(10\\,W-w)", "max(0\\,H-h-10)"
	case "bottom-right":
		return "max(0\\,W-w-10)", "max(0\\,H-h-10)"
	case "top-right":
		fallthrough
	default:
		return "max(0\\,W-w-10)", "min(10\\,H-h)"
	}
}

func buildVideoFilters(f media.FilterConfig) string {
	contrast := 1.0
	if f.HasContrast {
		contrast = f.Contrast
	}

	saturation := 1.0
	if f.HasSaturation {
		saturation = f.Saturation
	}

	gamma := 1.0
	if f.HasGamma {
		gamma = f.Gamma
	}

	var filters []string

	var eqParts []string
	if f.Brightness != 0 {
		eqParts = append(eqParts, fmt.Sprintf("brightness=%.2f", f.Brightness))
	}
	if contrast != 1.0 {
		eqParts = append(eqParts, fmt.Sprintf("contrast=%.2f", contrast))
	}
	if saturation != 1.0 {
		eqParts = append(eqParts, fmt.Sprintf("saturation=%.2f", saturation))
	}
	if gamma != 1.0 {
		eqParts = append(eqParts, fmt.Sprintf("gamma=%.2f", gamma))
	}
	if len(eqParts) > 0 {
		filters = append(filters, "eq="+strings.Join(eqParts, ":"))
	}

	if f.Blur > 0 {
		filters = append(filters, fmt.Sprintf("boxblur=%.1f:1", f.Blur))
	}

	if f.Sharpen > 0 {
		filters = append(filters, fmt.Sprintf("unsharp=3:3:%.1f", f.Sharpen))
	}

	if f.Flip {
		filters = append(filters, "hflip")
	}

	switch f.Rotate {
	case 90:
		filters = append(filters, "transpose=1")
	case 180:
		filters = append(filters, "transpose=2,transpose=2")
	case 270:
		filters = append(filters, "transpose=2")
	}

	return strings.Join(filters, ",")
}

// overlayWidthForResolution returns a logo width (in pixels) proportional
// to the output resolution so the overlay doesn't dominate small outputs
// or disappear on large ones.  The original 600 px at 1280×720 is preserved
// exactly; smaller resolutions scale linearly (960→450, 640→300).
func overlayWidthForResolution(res string) int {
	if res == "" {
		res = "1280x720"
	}
	parts := strings.Split(res, "x")
	if len(parts) != 2 {
		return 600
	}
	w, err := strconv.Atoi(parts[0])
	if err != nil {
		return 600
	}
	return w * 600 / 1280
}

func scaledSizeForResolution(res string, scale float64) (int, int) {
	if res == "" {
		res = "1280x720"
	}
	parts := strings.Split(res, "x")
	width, widthErr := strconv.Atoi(parts[0])
	height := 720
	var heightErr error
	if len(parts) == 2 {
		height, heightErr = strconv.Atoi(parts[1])
	}
	if len(parts) != 2 || widthErr != nil || heightErr != nil {
		width = 1280
		height = 720
	}
	scaledWidth := min(max(int(math.Round(float64(width)*scale)), 2), max(width-20, 2))
	scaledHeight := min(max(int(math.Round(float64(height)*scale)), 2), max(height-20, 2))
	if scaledWidth%2 != 0 {
		scaledWidth--
	}
	if scaledHeight%2 != 0 {
		scaledHeight--
	}
	return max(scaledWidth, 2), max(scaledHeight, 2)
}

func buildFilterComplex(
	cfg media.SessionConfig,
	webcamOverlayInput,
	staticLogoOverlayInput,
	annotationOverlayInput int,
) string {
	videoFilters := buildVideoFilters(cfg.Filters)
	hasSubtitles := cfg.BurnSubtitles && cfg.SubtitleFile != ""
	hasWebcamOverlay := webcamOverlayInput > 0
	hasStaticLogoOverlay := staticLogoOverlayInput > 0
	hasAnnotationOverlay := annotationOverlayInput > 0

	// Passthrough: no filters, overlays, or subtitles.  The encoder's -s
	// flag (if any) handles the output resolution.
	if videoFilters == "" && !hasWebcamOverlay && !hasStaticLogoOverlay && !hasAnnotationOverlay && !hasSubtitles {
		return ""
	}

	// Pre-scale video to the configured output resolution before any
	// filters, overlays, or subtitles.  This makes overlay positioning
	// and subtitle sizing consistent regardless of input resolution, and
	// reduces CPU load when the input is larger than the output.
	//
	// We preserve the original aspect ratio and pad with black bars
	// (letterboxing) so non-16:9 sources are not stretched or squashed.
	resolution := cfg.Encode.Resolution
	if resolution == "" {
		resolution = "1280x720"
	}
	resFfmpeg := strings.Replace(resolution, "x", ":", 1)
	scaleExpr := fmt.Sprintf(
		"scale=%s:force_original_aspect_ratio=decrease,pad=%s:(ow-iw)/2:(oh-ih)/2:black",
		resFfmpeg, resFfmpeg,
	)
	if hasWebcamOverlay {
		scaleExpr += ",setpts=PTS-STARTPTS"
	}

	current := "[v0]"
	labelIndex := 1
	var graph []string
	graph = append(graph, fmt.Sprintf("[0:v]%s%s", scaleExpr, current))

	if videoFilters != "" {
		next := fmt.Sprintf("[v%d]", labelIndex)
		labelIndex++
		graph = append(graph, current+videoFilters+next)
		current = next
	}

	if hasWebcamOverlay {
		xExpr, yExpr := webcamOverlayPositionExpressions(cfg.WebcamPosition)
		overlayW, overlayH := scaledSizeForResolution(cfg.Encode.Resolution, cfg.WebcamScale)
		next := fmt.Sprintf("[v%d]", labelIndex)
		labelIndex++
		graph = append(graph,
			fmt.Sprintf("[%d:v]setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,setsar=1[webcam]",
				webcamOverlayInput, overlayW, overlayH),
			fmt.Sprintf("%s[webcam]overlay=%s:%s:format=auto:shortest=1%s", current, xExpr, yExpr, next),
		)
		current = next
	}

	if hasStaticLogoOverlay {
		overlayW := overlayWidthForResolution(cfg.Encode.Resolution)
		next := fmt.Sprintf("[v%d]", labelIndex)
		labelIndex++
		graph = append(graph,
			fmt.Sprintf("[%d:v]format=rgba,scale=%d:-1[logo]", staticLogoOverlayInput, overlayW),
			fmt.Sprintf("%s[logo]overlay=W-w-10:10:format=auto%s", current, next),
		)
		current = next
	}

	if hasAnnotationOverlay {
		next := fmt.Sprintf("[v%d]", labelIndex)
		labelIndex++
		graph = append(graph,
			fmt.Sprintf("[%d:v]fps=%d,format=rgba[annotation]", annotationOverlayInput, media.AnnotationOverlayFPS),
			fmt.Sprintf("%s[annotation]overlay=0:0:format=auto:eof_action=repeat%s", current, next),
		)
		current = next
	}

	tail := buildSubtitleFilter(cfg, "format=yuv420p") + "[vout]"
	graph = append(graph, current+tail)
	return strings.Join(graph, ";")
}

func buildPipelineArgs(cfg media.SessionConfig, videoInput string, output string) []string {
	isDirectInput := cfg.IsDirectInput()

	args := []string{"-y"}
	if isDirectInput {
		args = appendDirectInputArgs(args, videoInput)
	} else {
		// Webcam input via MediaRecorder WebM chunks.
		// Preserve the recorder's timestamps so cameras delivering fewer than
		// 30 frames per second still advance at real time. +genpts fills only
		// timestamps that are missing.
		// thread_queue_size increases ffmpeg's internal input buffer for
		// live pipe sources where reader/writer speeds can momentarily diverge.
		args = append(args, "-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", videoInput)
	}

	webcamOverlayInput := 0
	staticLogoOverlayInput := 0
	annotationOverlayInput := 0
	nextInputIndex := 1

	if cfg.WebcamOverlay {
		args = append(args, "-f", "webm", "-thread_queue_size", "512", "-fflags", "+genpts", "-i", "pipe:0")
		webcamOverlayInput = nextInputIndex
		nextInputIndex++
	}

	if cfg.StaticLogoOverlay {
		args = append(args, "-i", "/app/assets/streamline-logo.png")
		staticLogoOverlayInput = nextInputIndex
		nextInputIndex++
	}

	if cfg.AnnotationOverlay {
		// The pipe format and codec are explicit, so avoid ffmpeg's multi-megabyte
		// default probe. Dense annotation PNGs would otherwise block webcam input
		// long enough to fill its stdin pipe before the output graph starts.
		args = append(args,
			"-framerate", fmt.Sprintf("%d", media.AnnotationOverlayFPS),
			"-f", "image2pipe",
			"-vcodec", "png",
			"-probesize", strconv.Itoa(annotationProbeSize),
			"-i", annotationInputPath(),
		)
		annotationOverlayInput = nextInputIndex
	}

	filterComplex := buildFilterComplex(cfg, webcamOverlayInput, staticLogoOverlayInput, annotationOverlayInput)
	if filterComplex != "" {
		args = append(args,
			"-filter_complex", filterComplex,
			"-map", "[vout]",
		)
		if !cfg.AnnotationOverlay || cfg.WebcamOverlay {
			args = append(args, "-map", "0:a?")
		}
	}

	return appendEncodeArgs(args, cfg, output)
}

// BuildArgs returns the full ffmpeg argument list for a media session.
func BuildArgs(cfg media.SessionConfig) ([]string, error) {
	output := cfg.DestinationURL
	if cfg.IsPreview() {
		output = "pipe:1"
	} else if output == "" {
		return nil, fmt.Errorf("RTMP output destination is not configured")
	}

	if cfg.SourceType == media.SourceTest {
		if cfg.StaticLogoOverlay || cfg.AnnotationOverlay || cfg.WebcamOverlay || cfg.WebcamScale != 0 ||
			cfg.WebcamPosition != "" || cfg.BurnSubtitles || cfg.SubtitleFile != "" ||
			buildVideoFilters(cfg.Filters) != "" || cfg.Encode != (media.EncodeConfig{}) {
			return nil, fmt.Errorf("test input does not support overlays, filters, subtitles, or encode parameters")
		}
		return buildTestArgs(cfg.IsPreview(), output), nil
	}

	input := "pipe:0"
	if cfg.IsDirectInput() {
		if cfg.SourceURL == "" {
			return nil, fmt.Errorf("direct input mode but no input URL configured")
		}
		input = cfg.SourceURL
	}

	return buildPipelineArgs(cfg, input, output), nil
}

func buildTestArgs(preview bool, output string) []string {
	args := []string{
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
	}
	if preview {
		return append(args,
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-min_frag_duration", "2000000",
			"pipe:1",
		)
	}
	return append(args, "-f", "flv", output)
}

// appendEncodeArgs appends video encode, audio encode, and output format args
// to the given slice based on the session config. Uses defaults when encode params are
// not explicitly set.
func appendEncodeArgs(args []string, cfg media.SessionConfig, output string) []string {
	e := cfg.Encode

	args = append(args, "-c:v", "libx264")

	// Preset (default: fast).
	preset := e.Preset
	if preset == "" {
		preset = "fast"
	}
	args = append(args, "-preset", preset)

	// Bitrate (default: 2M).
	bitrate := e.Bitrate
	if bitrate == "" {
		bitrate = defaultTranscodeBitrate
	}
	args = append(args, "-b:v", bitrate)

	isDirectInput := cfg.IsDirectInput()

	// GOP: smaller for interactive preview mode (~2s) for faster recovery
	// from transport stalls; use the larger default for all RTMP output for
	// better streaming efficiency.
	gop := strconv.Itoa(e.GOP)
	if e.GOP == 0 {
		if cfg.IsPreview() {
			gop = "30" // ~1s at 30fps, ~1.25s at 24fps
		} else {
			gop = defaultTranscodeGOP
		}
	}
	args = append(args, "-g", gop)

	// Disable B-frames for preview and RTMP output to avoid out-of-order frames.
	args = append(args, "-bf", "0")

	// Resolution (optional).
	if e.Resolution != "" {
		args = append(args, "-s", e.Resolution)
	}

	// FPS (optional).
	if e.FPS > 0 {
		args = append(args, "-r", fmt.Sprintf("%d", e.FPS))
	}

	args = append(args, "-pix_fmt", "yuv420p")

	// Output format.
	if cfg.IsPreview() {
		// Interactive preview: lower latency settings for WebSocket transport.
		// Disable audio for RTMP and webcam sources to reduce CPU load (RTMP
		// audio needs AAC transcoding; webcam Opus causes muxing issues).
		// CRF maintains quality, while the VBV ceiling prevents one-second fMP4
		// fragments from outrunning the relay and draining the viewer buffer.
		args = append(args,
			"-crf", "20",
			"-maxrate", bitrate,
			"-bufsize", bitrate,
		)
		// HLS sources keep audio since it's already AAC and adds minimal CPU.
		// All other sources (RTMP, webcam, unknown) use video-only for simplicity.
		if cfg.SourceType == media.SourceHLS {
			args = append(args,
				"-c:a", "aac",
				"-b:a", defaultAudioBitrate,
			)
		} else {
			args = append(args, "-an")
		}
		args = append(args,
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-level", h264Level(e.Resolution, e.FPS),
			"-flush_packets", "1",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"pipe:1",
		)
	} else {
		args = appendRTMPOutputArgs(args, isDirectInput, cfg.SourceType, e.Resolution, e.FPS, output)
	}

	return args
}

func h264Level(resolution string, fps int) string {
	parts := strings.Split(resolution, "x")
	if len(parts) != 2 {
		return "3.1"
	}
	width, widthErr := strconv.Atoi(parts[0])
	height, heightErr := strconv.Atoi(parts[1])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return "3.1"
	}
	if fps <= 0 {
		fps = 30
	}
	frameMacroblocks := ((width + 15) / 16) * ((height + 15) / 16)
	macroblocksPerSecond := frameMacroblocks * fps
	levels := []struct {
		name                    string
		maxFrameMacroblocks     int
		maxMacroblocksPerSecond int
	}{
		{name: "3.1", maxFrameMacroblocks: 3600, maxMacroblocksPerSecond: 108000},
		{name: "4.0", maxFrameMacroblocks: 8192, maxMacroblocksPerSecond: 245760},
		{name: "4.2", maxFrameMacroblocks: 8704, maxMacroblocksPerSecond: 522240},
		{name: "5.0", maxFrameMacroblocks: 22080, maxMacroblocksPerSecond: 589824},
		{name: "5.1", maxFrameMacroblocks: 36864, maxMacroblocksPerSecond: 983040},
		{name: "5.2", maxFrameMacroblocks: 36864, maxMacroblocksPerSecond: 2073600},
		{name: "6.0", maxFrameMacroblocks: 139264, maxMacroblocksPerSecond: 4177920},
		{name: "6.1", maxFrameMacroblocks: 139264, maxMacroblocksPerSecond: 8355840},
		{name: "6.2", maxFrameMacroblocks: 139264, maxMacroblocksPerSecond: 16711680},
	}
	for _, level := range levels {
		if frameMacroblocks <= level.maxFrameMacroblocks && macroblocksPerSecond <= level.maxMacroblocksPerSecond {
			return level.name
		}
	}
	return "6.2"
}

func appendRTMPOutputArgs(
	args []string,
	isDirectInput bool,
	sourceType media.SourceType,
	resolution string,
	fps int,
	output string,
) []string {
	// RTMP sources already have AAC audio; pass it through to avoid
	// unnecessary CPU overhead from re-encoding.
	if sourceType == media.SourceRTMP {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args,
			"-c:a", "aac",
			"-b:a", defaultAudioBitrate,
		)
	}
	args = append(args,
		"-max_muxing_queue_size", "1024",
		"-flvflags", "no_duration_filesize",
	)

	if isDirectInput {
		// Direct HLS/RTMP inputs keep the stricter live-ingest flags. Webcam RTMP
		// intentionally avoids zerolatency after it caused severe Stream artifacts.
		args = append(args,
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-level", h264Level(resolution, fps),
			"-flush_packets", "1",
		)
	}

	return append(args, "-f", "flv", output)
}

// buildSubtitleFilter returns a subtitle filter string if burn-in is enabled
// and a subtitle file is available. If baseFilter is non-empty, the subtitle
// filter is appended to it with a comma separator.
func buildSubtitleFilter(cfg media.SessionConfig, baseFilter string) string {
	if !cfg.BurnSubtitles || cfg.SubtitleFile == "" {
		return baseFilter
	}

	subFilter := fmt.Sprintf("subtitles=%s:force_style='BackColour=&H80000000,BorderStyle=3,Outline=4,Shadow=0'", cfg.SubtitleFile)
	if baseFilter != "" {
		return baseFilter + "," + subFilter
	}
	return subFilter
}
