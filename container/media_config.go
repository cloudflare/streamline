package main

import (
	"net/url"
	"strings"

	"github.com/cloudflare/streamline/container/media"
	"github.com/cloudflare/streamline/container/protocol"
)

const streamRtmpURLPrefix = "rtmps://live.cloudflare.com:443/live/"

func streamRtmpURL(key string) string {
	return streamRtmpURLPrefix + key
}

func stringParam(params map[string]interface{}, key string) (string, bool) {
	value, ok := params[key].(string)
	return value, ok
}

func floatParam(params map[string]interface{}, key string) (float64, bool) {
	value, ok := params[key].(float64)
	return value, ok
}

func buildMediaConfig(
	sourceType media.SourceType,
	sourceURL string,
	webcamTransform *protocol.InputTransform,
	pipeline []protocol.Operation,
	output protocol.OutputConfig,
) media.SessionConfig {
	streamURL := ""
	outputMode := media.OutputPreview
	if output.Mode == "rtmp" {
		streamURL = streamRtmpURL(output.Key)
		outputMode = media.OutputRTMP
	}
	result := media.SessionConfig{
		SourceType:     sourceType,
		SourceURL:      sourceURL,
		DestinationURL: streamURL,
		OutputMode:     outputMode,
		Filters: media.FilterConfig{
			Contrast:   1.0,
			Saturation: 1.0,
			Gamma:      1.0,
		},
	}
	if webcamTransform != nil {
		result.WebcamOverlay = true
		result.WebcamScale = webcamTransform.Scale
		result.WebcamPosition = webcamTransform.Position
	}

	for _, operation := range pipeline {
		switch operation.Op {
		case "overlay":
			if image, ok := stringParam(operation.Params, "image"); ok {
				if image == "annotation" {
					result.AnnotationOverlay = true
				} else if image == "/app/assets/streamline-logo.png" {
					result.StaticLogoOverlay = true
				}
			}
		case "filter":
			if preset, ok := stringParam(operation.Params, "preset"); ok {
				switch preset {
				case "blur":
					result.Filters.Blur, _ = floatParam(operation.Params, "amount")
				case "brightness":
					result.Filters.Brightness, _ = floatParam(operation.Params, "amount")
				case "contrast":
					if amount, ok := floatParam(operation.Params, "amount"); ok {
						result.Filters.Contrast = amount
						result.Filters.HasContrast = true
					}
				case "saturation":
					if amount, ok := floatParam(operation.Params, "amount"); ok {
						result.Filters.Saturation = amount
						result.Filters.HasSaturation = true
					}
				case "gamma":
					if amount, ok := floatParam(operation.Params, "amount"); ok {
						result.Filters.Gamma = amount
						result.Filters.HasGamma = true
					}
				case "sharpen":
					result.Filters.Sharpen, _ = floatParam(operation.Params, "amount")
				case "flip":
					result.Filters.Flip = true
				case "rotate":
					if degrees, ok := floatParam(operation.Params, "degrees"); ok {
						result.Filters.Rotate = int(degrees)
					}
				}
			}
		case "subtitle":
			result.BurnSubtitles = true
		case "encode":
			if preset, ok := stringParam(operation.Params, "preset"); ok {
				result.Encode.Preset = preset
			}
			if bitrate, ok := stringParam(operation.Params, "bitrate"); ok {
				result.Encode.Bitrate = bitrate
			}
			if resolution, ok := stringParam(operation.Params, "resolution"); ok {
				result.Encode.Resolution = resolution
			}
			if fps, ok := floatParam(operation.Params, "fps"); ok {
				result.Encode.FPS = int(fps)
			}
			if gop, ok := floatParam(operation.Params, "gop"); ok {
				result.Encode.GOP = int(gop)
			}
		}
	}
	return result
}

func extractVideoID(videoURL string) string {
	const prefix = "https://videodelivery.net/"
	if !strings.HasPrefix(videoURL, prefix) {
		parsed, err := url.Parse(videoURL)
		if err != nil || !strings.HasSuffix(parsed.Host, ".cloudflarestream.com") {
			return ""
		}
		rest := strings.TrimPrefix(parsed.Path, "/")
		if rest == "" {
			return ""
		}
		if index := strings.Index(rest, "/"); index != -1 {
			return rest[:index]
		}
		return rest
	}

	rest := videoURL[len(prefix):]
	if index := strings.Index(rest, "/"); index != -1 {
		return rest[:index]
	}
	return rest
}
