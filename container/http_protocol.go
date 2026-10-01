package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/cloudflare/streamline/container/media"
	"github.com/cloudflare/streamline/container/protocol"
	"github.com/cloudflare/streamline/container/subtitle"
)

type fetchSubtitlesFunc func(context.Context, string) (*subtitle.FetchResult, error)

type httpStartRequest struct {
	Input       *protocol.InputConfig  `json:"input,omitempty"`
	Inputs      []protocol.InputConfig `json:"inputs,omitempty"`
	Pipeline    []protocol.Operation   `json:"pipeline"`
	Output      *protocol.OutputConfig `json:"output"`
	SessionID   string                 `json:"session_id"`
	Diagnostics bool                   `json:"diagnostics"`
}

type httpStartOutput struct {
	Mode   string `json:"mode"`
	Format string `json:"format,omitempty"`
}

type httpStartResponse struct {
	Status   string                `json:"status"`
	Mode     string                `json:"mode"`
	Output   httpStartOutput       `json:"output"`
	Subtitle *HTTPSubtitleMetadata `json:"subtitle,omitempty"`
}

const (
	subtitleUnsupportedWarning = "Subtitle burn-in is only available for direct Cloudflare Stream VOD/HLS sources. Streaming without burn-in."
	subtitlePreparationWarning = "Could not prepare subtitles. Streaming without burn-in."
	maxPipelineOperations      = 16
)

func decodeHTTPStartRequest(r io.Reader) (httpStartRequest, error) {
	var req httpStartRequest
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid JSON body: %w", err)
	}

	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return req, fmt.Errorf("invalid JSON body: %w", err)
		}
		return req, fmt.Errorf("request body must contain one JSON object")
	}
	return req, nil
}

func normalizeHTTPStartRequest(req *httpStartRequest) error {
	if (req.Input == nil) == (req.Inputs == nil) {
		return fmt.Errorf("exactly one of input or inputs is required")
	}
	if req.Input != nil {
		if err := normalizeHTTPInput(req.Input, "input"); err != nil {
			return err
		}
		if req.Input.Transform != nil {
			return fmt.Errorf("input.transform is not supported for the primary input")
		}
	} else {
		if len(req.Inputs) != 2 {
			return fmt.Errorf("inputs must contain an HLS or RTMP primary input and one webcam input")
		}
		for index := range req.Inputs {
			if err := normalizeHTTPInput(&req.Inputs[index], fmt.Sprintf("inputs[%d]", index)); err != nil {
				return err
			}
		}
		if (req.Inputs[0].Type != "hls" && req.Inputs[0].Type != "rtmp") ||
			req.Inputs[0].Transform != nil || req.Inputs[1].Type != "webcam" {
			return fmt.Errorf("inputs must contain an HLS or RTMP primary input followed by one webcam input")
		}
		transform := req.Inputs[1].Transform
		if transform == nil {
			return fmt.Errorf("inputs[1].transform is required")
		}
		transform.Position = strings.TrimSpace(transform.Position)
		if transform.Scale <= 0 || transform.Scale > 1 || math.IsNaN(transform.Scale) || math.IsInf(transform.Scale, 0) {
			return fmt.Errorf("inputs[1].transform.scale must be between 0 and 1")
		}
		if transform.Position != "top-left" && transform.Position != "top-right" &&
			transform.Position != "bottom-left" && transform.Position != "bottom-right" {
			return fmt.Errorf("inputs[1].transform.position is invalid")
		}
	}

	if req.Output == nil {
		return fmt.Errorf("output is required")
	}
	return normalizeHTTPOutput(req.Output)
}

func normalizeHTTPInput(input *protocol.InputConfig, field string) error {
	input.Type = strings.TrimSpace(input.Type)
	input.URL = strings.TrimSpace(input.URL)
	input.Key = strings.TrimSpace(input.Key)
	switch input.Type {
	case "webcam", "test":
		if input.URL != "" {
			return fmt.Errorf("%s.url is not supported for %s input", field, input.Type)
		}
		if input.Key != "" {
			return fmt.Errorf("%s.key is not supported for %s input", field, input.Type)
		}
	case "hls":
		if input.Key != "" {
			return fmt.Errorf("%s.key is not supported for hls input", field)
		}
		if err := validateInputURL(input.URL, "hls", "http", "https"); err != nil {
			return err
		}
	case "rtmp":
		if input.URL != "" {
			return fmt.Errorf("%s.url is not supported for rtmp input", field)
		}
		if err := validateStreamRtmpKey(input.Key, "rtmp input"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s.type must be webcam, hls, rtmp, or test", field)
	}
	return nil
}

func normalizeHTTPOutput(output *protocol.OutputConfig) error {
	switch output.Mode {
	case "websocket":
		if output.Key != "" {
			return fmt.Errorf("output.key is not supported for websocket output")
		}
		if output.Format == "" {
			output.Format = "fmp4"
		}
		if output.Format != "fmp4" {
			return fmt.Errorf("websocket output format must be fmp4")
		}
	case "rtmp":
		if output.Format != "" {
			return fmt.Errorf("output.format is not supported for RTMP output")
		}
		output.Key = strings.TrimSpace(output.Key)
		if err := validateStreamRtmpKey(output.Key, "RTMP output"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("output mode must be websocket or rtmp")
	}

	if output.HasRelay() {
		if output.Mode == "rtmp" {
			return fmt.Errorf("output.relay is not supported for RTMP output")
		}
		if output.Relay == nil {
			return fmt.Errorf("output.relay must be an object")
		}
		hasRelayURL := output.Relay.URL != ""
		hasRelayToken := output.Relay.Token != ""
		if !hasRelayURL || !hasRelayToken {
			return fmt.Errorf("output.relay.url and output.relay.token are required")
		}
		parsed, err := url.Parse(output.Relay.URL)
		if err != nil || parsed.Host == "" || parsed.Scheme != "wss" {
			return fmt.Errorf("output.relay.url must use wss:// and include a host")
		}
	}
	return nil
}

func validateInputURL(rawURL, inputType string, allowedSchemes ...string) error {
	if rawURL == "" {
		return fmt.Errorf("%s input requires url", inputType)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%s input url is invalid", inputType)
	}
	for _, scheme := range allowedSchemes {
		if parsed.Scheme == scheme {
			return nil
		}
	}
	return fmt.Errorf("%s input url must use %s", inputType, strings.Join(allowedSchemes, ":// or ")+"://")
}

func validateStreamRtmpKey(key, field string) error {
	if key == "" {
		return fmt.Errorf("%s requires key", field)
	}
	if len(key) > 2011 {
		return fmt.Errorf("%s key is invalid", field)
	}
	for _, char := range key {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("-._~", char) {
			continue
		}
		return fmt.Errorf("%s key is invalid", field)
	}
	return nil
}

func stringPipelineParam(params map[string]interface{}, key string) (string, bool, error) {
	raw, ok := params[key]
	if !ok {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return value, true, nil
}

func numberPipelineParam(params map[string]interface{}, key string) (float64, bool, error) {
	raw, ok := params[key]
	if !ok {
		return 0, false, nil
	}
	value, ok := raw.(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, true, fmt.Errorf("%s must be a finite number", key)
	}
	return value, true, nil
}

func validatePipelineParamKeys(params map[string]interface{}, allowed ...string) error {
	for key := range params {
		supported := false
		for _, allowedKey := range allowed {
			if key == allowedKey {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("%s is not supported", key)
		}
	}
	return nil
}

func validBitrate(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	number := value
	last := value[len(value)-1]
	if last == 'k' || last == 'K' || last == 'm' || last == 'M' {
		number = value[:len(value)-1]
	}
	parsed, err := strconv.ParseFloat(number, 64)
	return err == nil && parsed > 0 && !math.IsInf(parsed, 0)
}

func validResolution(value string) bool {
	parts := strings.Split(value, "x")
	if len(parts) != 2 {
		return false
	}
	width, widthErr := strconv.Atoi(parts[0])
	height, heightErr := strconv.Atoi(parts[1])
	return widthErr == nil && heightErr == nil && width > 0 && height > 0 && width <= 3840 && height <= 2160 &&
		width%2 == 0 && height%2 == 0
}

func validateHTTPPipeline(req httpStartRequest) error {
	if len(req.Pipeline) == 0 || len(req.Pipeline) > maxPipelineOperations {
		return fmt.Errorf("pipeline must contain between 1 and %d operations", maxPipelineOperations)
	}

	primaryInput := protocol.InputConfig{}
	if req.Input != nil {
		primaryInput = *req.Input
	} else if len(req.Inputs) > 0 {
		primaryInput = req.Inputs[0]
	}
	if primaryInput.Type == "test" {
		if len(req.Pipeline) != 1 || req.Pipeline[0].Op != "encode" || len(req.Pipeline[0].Params) != 0 {
			return fmt.Errorf("test input supports only one encode operation with no parameters")
		}
		return nil
	}
	encodeOperations := 0
	for index, operation := range req.Pipeline {
		params := operation.Params
		operationError := func(err error) error {
			return fmt.Errorf("pipeline[%d] %s: %w", index, operation.Op, err)
		}

		switch operation.Op {
		case "overlay":
			if err := validatePipelineParamKeys(params, "image", "position"); err != nil {
				return operationError(err)
			}
			image, ok, err := stringPipelineParam(params, "image")
			if err != nil {
				return operationError(err)
			}
			if !ok || image == "" {
				return operationError(fmt.Errorf("image is required"))
			}
			if image != "annotation" && image != "/app/assets/streamline-logo.png" {
				return operationError(fmt.Errorf("image must be annotation or /app/assets/streamline-logo.png"))
			}
			position, ok, err := stringPipelineParam(params, "position")
			if err != nil {
				return operationError(err)
			}
			if !ok || (image == "/app/assets/streamline-logo.png" && position != "top-right") ||
				(image == "annotation" && position != "full") {
				return operationError(fmt.Errorf("overlay must use the fixed logo at top-right or annotation at full"))
			}

		case "filter":
			if err := validatePipelineParamKeys(params, "preset", "amount", "degrees"); err != nil {
				return operationError(err)
			}
			preset, ok, err := stringPipelineParam(params, "preset")
			if err != nil {
				return operationError(err)
			}
			if !ok {
				return operationError(fmt.Errorf("preset is required"))
			}
			switch preset {
			case "blur", "brightness", "contrast", "saturation", "gamma", "sharpen":
				if _, hasDegrees := params["degrees"]; hasDegrees {
					return operationError(fmt.Errorf("degrees is not supported"))
				}
				amount, hasAmount, err := numberPipelineParam(params, "amount")
				if err != nil {
					return operationError(err)
				}
				if !hasAmount {
					return operationError(fmt.Errorf("amount is required"))
				}
				switch preset {
				case "blur":
					if amount < 0 || amount > 10 {
						return operationError(fmt.Errorf("amount must be between 0 and 10"))
					}
				case "brightness":
					if amount < -1 || amount > 1 {
						return operationError(fmt.Errorf("amount must be between -1 and 1"))
					}
				case "contrast", "saturation":
					if amount < 0 || amount > 2 {
						return operationError(fmt.Errorf("amount must be between 0 and 2"))
					}
				case "gamma":
					if amount < 0.1 || amount > 3 {
						return operationError(fmt.Errorf("amount must be between 0.1 and 3"))
					}
				case "sharpen":
					if amount < 0 || amount > 5 {
						return operationError(fmt.Errorf("amount must be between 0 and 5"))
					}
				}
			case "flip":
				if _, hasAmount := params["amount"]; hasAmount {
					return operationError(fmt.Errorf("amount is not supported"))
				}
				if _, hasDegrees := params["degrees"]; hasDegrees {
					return operationError(fmt.Errorf("degrees is not supported"))
				}
			case "rotate":
				if _, hasAmount := params["amount"]; hasAmount {
					return operationError(fmt.Errorf("amount is not supported"))
				}
				degrees, hasDegrees, err := numberPipelineParam(params, "degrees")
				if err != nil {
					return operationError(err)
				}
				if !hasDegrees {
					return operationError(fmt.Errorf("degrees is required"))
				}
				if degrees != 0 && degrees != 90 && degrees != 180 && degrees != 270 {
					return operationError(fmt.Errorf("degrees must be 0, 90, 180, or 270"))
				}
			default:
				return operationError(fmt.Errorf("preset is unsupported"))
			}

		case "subtitle":
			if err := validatePipelineParamKeys(params, "source"); err != nil {
				return operationError(err)
			}
			source, exists, err := stringPipelineParam(params, "source")
			if err != nil {
				return operationError(err)
			}
			if exists && source != "auto" {
				return operationError(fmt.Errorf("only source auto is supported"))
			}

		case "encode":
			encodeOperations++
			if err := validatePipelineParamKeys(params, "codec", "preset", "bitrate", "resolution", "fps", "gop"); err != nil {
				return operationError(err)
			}
			codec, hasCodec, err := stringPipelineParam(params, "codec")
			if err != nil {
				return operationError(err)
			}
			if hasCodec && codec != "h264" {
				return operationError(fmt.Errorf("codec must be h264"))
			}
			preset, hasPreset, err := stringPipelineParam(params, "preset")
			if err != nil {
				return operationError(err)
			}
			if hasPreset {
				supported := preset == "ultrafast" || preset == "superfast" || preset == "veryfast" ||
					preset == "faster" || preset == "fast" || preset == "medium" || preset == "slow" ||
					preset == "slower" || preset == "veryslow"
				if !supported {
					return operationError(fmt.Errorf("preset is unsupported"))
				}
			}
			bitrate, hasBitrate, err := stringPipelineParam(params, "bitrate")
			if err != nil {
				return operationError(err)
			}
			if hasBitrate && !validBitrate(bitrate) {
				return operationError(fmt.Errorf("bitrate is invalid"))
			}
			resolution, hasResolution, err := stringPipelineParam(params, "resolution")
			if err != nil {
				return operationError(err)
			}
			if hasResolution && !validResolution(resolution) {
				return operationError(fmt.Errorf("resolution must be positive even WIDTHxHEIGHT up to 3840x2160"))
			}
			fps, hasFPS, err := numberPipelineParam(params, "fps")
			if err != nil {
				return operationError(err)
			}
			if hasFPS && (fps != math.Trunc(fps) || fps <= 0 || fps > 240) {
				return operationError(fmt.Errorf("fps must be an integer between 1 and 240"))
			}
			gop, hasGOP, err := numberPipelineParam(params, "gop")
			if err != nil {
				return operationError(err)
			}
			if hasGOP && (gop != math.Trunc(gop) || gop <= 0 || gop > 10000) {
				return operationError(fmt.Errorf("gop must be an integer between 1 and 10000"))
			}

		default:
			return operationError(fmt.Errorf("operation is unsupported"))
		}
	}
	if encodeOperations != 1 {
		return fmt.Errorf("pipeline must contain exactly one encode operation")
	}
	return nil
}

type subtitleFetchOutcome struct {
	result *subtitle.FetchResult
	err    error
}

func fetchSubtitlesBounded(
	ctx context.Context,
	fetcher fetchSubtitlesFunc,
	videoID string,
) (*subtitle.FetchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outcomes := make(chan subtitleFetchOutcome)
	go func() {
		result, err := fetcher(ctx, videoID)
		outcome := subtitleFetchOutcome{result: result, err: err}
		select {
		case outcomes <- outcome:
		case <-ctx.Done():
			if result != nil && result.SubtitleFile != "" {
				_ = os.Remove(result.SubtitleFile)
			}
		}
	}()

	select {
	case outcome := <-outcomes:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) prepareHTTPSubtitles(ctx context.Context, cfg *media.SessionConfig) *HTTPSubtitleMetadata {
	if !cfg.BurnSubtitles {
		return nil
	}

	parsed, err := url.Parse(cfg.SourceURL)
	videoID := extractVideoID(cfg.SourceURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		cfg.SourceType != media.SourceHLS || videoID == "" {
		return &HTTPSubtitleMetadata{State: "unavailable", Warning: subtitleUnsupportedWarning}
	}

	fetcher := s.subtitleFetcher
	if fetcher == nil {
		fetcher = subtitle.FetchAndConvertContext
	}
	result, err := fetchSubtitlesBounded(ctx, fetcher, videoID)
	if err != nil || result == nil {
		log.Printf("[http] Subtitle preparation failed; starting without burn-in")
		return &HTTPSubtitleMetadata{State: "unavailable", Warning: subtitlePreparationWarning}
	}
	if result.Warning != "" {
		if result.SubtitleFile != "" {
			_ = os.Remove(result.SubtitleFile)
		}
		return &HTTPSubtitleMetadata{State: "unavailable", Warning: result.Warning}
	}
	if result.SubtitleFile == "" {
		return &HTTPSubtitleMetadata{State: "unavailable", Warning: subtitlePreparationWarning}
	}

	cfg.SubtitleFile = result.SubtitleFile
	language := result.Language
	cueCount := result.CueCount
	return &HTTPSubtitleMetadata{
		State:    "ready",
		Language: &language,
		CueCount: &cueCount,
	}
}
