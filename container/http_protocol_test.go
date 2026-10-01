package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/media"
	"github.com/cloudflare/streamline/container/protocol"
	"github.com/cloudflare/streamline/container/subtitle"
	"github.com/stretchr/testify/require"
)

func TestHTTPStartValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "malformed JSON", body: `{`, want: "invalid JSON body"},
		{name: "empty body", body: ``, want: "invalid JSON body"},
		{name: "trailing JSON", body: `{"input":{"type":"webcam"}}{}`, want: "one JSON object"},
		{name: "missing input", body: `{}`, want: "exactly one of input or inputs is required"},
		{name: "invalid input type", body: `{"input":{"type":"other"}}`, want: "input.type must be"},
		{
			name: "singular and plural inputs",
			body: `{"input":{"type":"rtmp","key":"input-key"},` +
				`"inputs":[{"type":"rtmp","key":"input-key"},{"type":"webcam",` +
				`"transform":{"scale":0.25,"position":"top-right"}}]}`,
			want: "exactly one of input or inputs is required",
		},
		{name: "empty plural inputs", body: `{"inputs":[]}`, want: "inputs must contain an HLS or RTMP primary input"},
		{
			name: "reversed plural inputs",
			body: `{"inputs":[{"type":"webcam"},{"type":"rtmp","key":"input-key",` +
				`"transform":{"scale":0.25,"position":"top-right"}}]}`,
			want: "HLS or RTMP primary input followed by one webcam input",
		},
		{
			name: "primary transform",
			body: `{"inputs":[{"type":"rtmp","key":"input-key",` +
				`"transform":{"scale":1,"position":"top-left"}},{"type":"webcam",` +
				`"transform":{"scale":0.25,"position":"top-right"}}]}`,
			want: "HLS or RTMP primary input followed by one webcam input",
		},
		{
			name: "missing secondary transform",
			body: `{"inputs":[{"type":"rtmp","key":"input-key"},{"type":"webcam"}]}`,
			want: "inputs[1].transform is required",
		},
		{
			name: "invalid secondary scale",
			body: `{"inputs":[{"type":"rtmp","key":"input-key"},{"type":"webcam",` +
				`"transform":{"scale":0,"position":"top-right"}}]}`,
			want: "transform.scale must be between 0 and 1",
		},
		{
			name: "invalid secondary position",
			body: `{"inputs":[{"type":"rtmp","key":"input-key"},{"type":"webcam",` +
				`"transform":{"scale":0.25,"position":"center"}}]}`,
			want: "transform.position is invalid",
		},
		{
			name: "unknown secondary transform field",
			body: `{"inputs":[{"type":"rtmp","key":"input-key"},{"type":"webcam",` +
				`"transform":{"scale":0.25,"position":"top-right","url":"https://example.test"}}]}`,
			want: `unknown field "url"`,
		},
		{name: "missing output", body: `{"input":{"type":"webcam"}}`, want: "output is required"},
		{name: "null output", body: `{"input":{"type":"webcam"},"output":null}`, want: "output is required"},
		{
			name: "malformed output", body: `{"input":{"type":"webcam"},"output":"websocket"}`,
			want: "invalid JSON body",
		},
		{
			name: "legacy schema", body: `{"mode":"webcam"}`,
			want: `unknown field "mode"`,
		},
		{
			name: "mixed schema",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},"source_url":"https://example.test"}`,
			want: `unknown field "source_url"`,
		},
		{name: "missing HLS URL", body: `{"input":{"type":"hls"}}`, want: "hls input requires url"},
		{
			name: "relative HLS URL", body: `{"input":{"type":"hls","url":"/video.m3u8"}}`,
			want: "hls input url is invalid",
		},
		{
			name: "HLS URL with unsupported scheme",
			body: `{"input":{"type":"hls","url":"rtmp://example.test/live/input"}}`,
			want: "hls input url must use http:// or https://",
		},
		{
			name: "missing RTMP key", body: `{"input":{"type":"rtmp"}}`,
			want: "rtmp input requires key",
		},
		{
			name: "legacy RTMP URL",
			body: `{"input":{"type":"rtmp","url":"rtmps://live.cloudflare.com:443/live/input"}}`,
			want: "input.url is not supported for rtmp input",
		},
		{
			name: "invalid RTMP key",
			body: `{"input":{"type":"rtmp","key":"path/segment"}}`,
			want: "rtmp input key is invalid",
		},
		{
			name: "URL with webcam input", body: `{"input":{"type":"webcam","url":"https://example.test"}}`,
			want: "input.url is not supported for webcam input",
		},
		{
			name: "key with webcam input", body: `{"input":{"type":"webcam","key":"secret"}}`,
			want: "input.key is not supported for webcam input",
		},
		{
			name: "missing RTMP output key", body: `{"input":{"type":"webcam"},"output":{"mode":"rtmp"}}`,
			want: "RTMP output requires key",
		},
		{
			name: "legacy RTMP destination",
			body: `{"input":{"type":"webcam"},"output":{"mode":"rtmp","destination":"https://example.test/live"}}`,
			want: `unknown field "destination"`,
		},
		{
			name: "invalid RTMP output key",
			body: `{"input":{"type":"webcam"},"output":{"mode":"rtmp","key":"key?query"}}`,
			want: "RTMP output key is invalid",
		},
		{
			name: "partial relay URL",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"websocket","relay":{"url":"wss://relay.example.test"}}}`,
			want: "output.relay.url and output.relay.token are required",
		},
		{
			name: "partial relay token",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"websocket","relay":{"token":"secret"}}}`,
			want: "output.relay.url and output.relay.token are required",
		},
		{
			name: "relay URL requires websocket scheme",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"websocket",` +
				`"relay":{"url":"https://relay.example.test","token":"secret"}}}`,
			want: "output.relay.url must use wss://",
		},
		{
			name: "relay URL requires host",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"websocket","relay":{"url":"wss:///relay","token":"secret"}}}`,
			want: "output.relay.url must use wss://",
		},
		{
			name: "relay capability requires TLS",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket",` +
				`"relay":{"url":"ws://relay.example.test","token":"secret"}}}`,
			want: "output.relay.url must use wss://",
		},
		{
			name: "relay Access credentials are not in the container protocol",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket",` +
				`"relay":{"url":"wss://relay.example.test","token":"secret","access":{}}}}`,
			want: `unknown field "access"`,
		},
		{
			name: "relay with RTMP output",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"rtmp","key":"output-key",` +
				`"relay":{"url":"wss://relay.example.test","token":"secret"}}}`,
			want: "not supported for RTMP output",
		},
		{
			name: "null relay with RTMP output",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"rtmp","key":"output-key","relay":null}}`,
			want: "not supported for RTMP output",
		},
		{
			name: "null relay with websocket output",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket","relay":null}}`,
			want: "output.relay must be an object",
		},
		{
			name: "unsupported websocket format",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket","format":"mpegts"}}`,
			want: "format must be fmp4",
		},
		{
			name: "format with RTMP output",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"rtmp","key":"output-key","format":"fmp4"}}`,
			want: "output.format is not supported for RTMP output",
		},
		{
			name: "URL in test input",
			body: `{"input":{"type":"test","url":"https://example.test/video.m3u8"}}`,
			want: "input.url is not supported for test input",
		},
		{
			name: "subtitle in test mode",
			body: `{"input":{"type":"test"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"subtitle"}]}`,
			want: "test input supports only one encode operation with no parameters",
		},
		{
			name: "unsupported operation",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"unsupported"}]}`,
			want: "operation is unsupported",
		},
		{
			name: "malformed overlay",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"overlay","params":{"image":42}}]}`,
			want: "image must be a string",
		},
		{
			name: "unapproved overlay path",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"overlay","params":{"image":"/tmp/untrusted.png"}}]}`,
			want: "image must be annotation or /app/assets/streamline-logo.png",
		},
		{
			name: "invalid filter range",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"filter","params":{"preset":"rotate","degrees":45}}]}`,
			want: "degrees must be 0, 90, 180, or 270",
		},
		{
			name: "invalid encode type",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"encode","params":{"fps":"30"}}]}`,
			want: "fps must be a finite number",
		},
		{
			name: "invalid encode resolution",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"encode","params":{"resolution":"1279x720"}}]}`,
			want: "positive even WIDTHxHEIGHT",
		},
		{
			name: "oversized encode resolution",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"encode","params":{"resolution":"7680x4320"}}]}`,
			want: "up to 3840x2160",
		},
		{
			name: "unsupported subtitle styling",
			body: `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"subtitle","params":{"source":"auto","fontSize":24}}]}`,
			want: "fontSize is not supported",
		},
		{
			name: "non h264 encode",
			body: `{"input":{"type":"webcam"},` +
				`"output":{"mode":"websocket"},` +
				`"pipeline":[{"op":"encode","params":{"codec":"h265"}}]}`,
			want: "codec must be h264",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer()
			request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(tt.body))
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Contains(t, response.Body.String(), tt.want)
			require.False(t, server.httpHandler.IsSessionActive())
		})
	}
}

func TestHTTPStartBodyLimitIsCheckedBeforeSessionLock(t *testing.T) {
	server := NewServer()
	server.sessionMu.Lock()
	defer server.sessionMu.Unlock()
	body := `{"input":{"type":"webcam"},"padding":"` + strings.Repeat("x", maxHTTPStartBodyBytes) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
	request.ContentLength = -1
	response := httptest.NewRecorder()
	done := make(chan struct{})

	go func() {
		server.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-done:
		require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	case <-time.After(time.Second):
		t.Fatal("oversized request waited for the session lock")
	}
}

func TestHTTPStartMapsInputType(t *testing.T) {
	tests := []struct {
		name       string
		inputType  string
		inputField string
		inputValue string
		sourceType media.SourceType
	}{
		{
			name: "HLS remains HLS", inputType: "hls", inputField: "url",
			inputValue: "https://example.test/video.m3u8", sourceType: media.SourceHLS,
		},
		{
			name: "RTMP remains RTMP", inputType: "rtmp", inputField: "key", inputValue: "input-key",
			sourceType: media.SourceRTMP,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installFakeFFmpeg(t, "exec sleep 30\n")
			server := NewServer()
			t.Cleanup(server.httpHandler.Stop)
			body := fmt.Sprintf(
				`{"input":{"type":%q,%q:%q},"output":{"mode":"websocket"},`+
					`"pipeline":[{"op":"encode"}]}`,
				tt.inputType,
				tt.inputField,
				tt.inputValue,
			)
			request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result httpStartResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.Equal(t, "direct", result.Mode)
			require.Equal(t, tt.sourceType, server.httpHandler.config.SourceType)
			if tt.inputType == "rtmp" {
				require.Equal(t, "rtmps://live.cloudflare.com:443/live/input-key", server.httpHandler.config.SourceURL)
			}
		})
	}
}

func TestHTTPStartOutputContract(t *testing.T) {
	t.Run("explicit test input uses default preview output", func(t *testing.T) {
		installFakeFFmpeg(t, "exec sleep 30\n")
		server := NewServer()
		t.Cleanup(server.httpHandler.Stop)
		request := httptest.NewRequest(
			http.MethodPost,
			"/start",
			strings.NewReader(`{"input":{"type":"test"},"output":{"mode":"websocket"},`+
				`"pipeline":[{"op":"encode"}]}`),
		)
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		var result httpStartResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, "test", result.Mode)
		require.Equal(t, httpStartOutput{Mode: "websocket", Format: "fmp4"}, result.Output)
	})

	t.Run("websocket output defaults to fmp4 and maps the protocol pipeline", func(t *testing.T) {
		server := NewServer()
		t.Cleanup(server.httpHandler.Stop)
		body := `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
			`"pipeline":[{"op":"encode","params":{"fps":60,"gop":120}}]}`
		request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		var result httpStartResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, "started", result.Status)
		require.Equal(t, "webcam", result.Mode)
		require.Equal(t, httpStartOutput{Mode: "websocket", Format: "fmp4"}, result.Output)
		require.Nil(t, result.Subtitle)
		require.Equal(t, media.OutputPreview, server.httpHandler.config.OutputMode)
		require.Equal(t, 60, server.httpHandler.config.Encode.FPS)
		require.Equal(t, 120, server.httpHandler.config.Encode.GOP)
		require.Equal(t, "websocket", server.httpHandler.Metrics().OutputMode)
	})

	t.Run("RTMP output URL is assembled without echoing its key", func(t *testing.T) {
		key := "secret-key"
		server := NewServer()
		t.Cleanup(server.httpHandler.Stop)
		request := httptest.NewRequest(
			http.MethodPost,
			"/start",
			strings.NewReader(`{"input":{"type":"webcam"},"output":{"mode":"rtmp","key":"`+key+`"},`+
				`"pipeline":[{"op":"encode"}]}`),
		)
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		var result httpStartResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, httpStartOutput{Mode: "rtmp"}, result.Output)
		require.NotContains(t, response.Body.String(), key)
		require.Equal(t, "rtmps://live.cloudflare.com:443/live/"+key, server.httpHandler.config.DestinationURL)
		require.Equal(t, media.OutputRTMP, server.httpHandler.config.OutputMode)
		require.Equal(t, "rtmp", server.httpHandler.Metrics().OutputMode)
	})
}

func TestNormalizeHTTPStartRequestAcceptsPreviewRelay(t *testing.T) {
	relay := &protocol.RelayConfig{
		URL:   "wss://relay.example.test/publish",
		Token: "secret",
	}
	req := httpStartRequest{
		Input: &protocol.InputConfig{Type: "webcam"},
		Output: &protocol.OutputConfig{
			Mode:   "websocket",
			Format: "fmp4",
			Relay:  relay,
		},
	}

	require.NoError(t, normalizeHTTPStartRequest(&req))
	require.Equal(t, "websocket", req.Output.Mode)
	require.Equal(t, "fmp4", req.Output.Format)
	require.Same(t, relay, req.Output.Relay)
}

func TestHTTPStartConfiguresRTMPWebcamPiP(t *testing.T) {
	installFakeFFmpeg(t, "exec sleep 30\n")
	server := NewServer()
	t.Cleanup(server.httpHandler.Stop)
	body := `{"inputs":[{"type":"rtmp","key":"input-key"},{"type":"webcam",` +
		`"transform":{"scale":0.25,"position":"top-right"}}],` +
		`"pipeline":[{"op":"encode","params":{"codec":"h264","resolution":"1280x720"}}],` +
		`"output":{"mode":"websocket"},"session_id":"session-1"}`
	request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, media.SourceRTMP, server.httpHandler.config.SourceType)
	require.Equal(t, streamRtmpURL("input-key"), server.httpHandler.config.SourceURL)
	require.True(t, server.httpHandler.config.WebcamOverlay)
	require.Equal(t, 0.25, server.httpHandler.config.WebcamScale)
	require.Equal(t, "top-right", server.httpHandler.config.WebcamPosition)
	require.True(t, server.httpHandler.config.HasWebcamInput())
	require.NotNil(t, server.httpHandler.stdin)

	ingestRequest := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader([]byte{0x1A, 0x45, 0xDF, 0xA3}))
	ingestRequest.Header.Set("X-Streamline-Session-ID", "session-1")
	ingestResponse := httptest.NewRecorder()
	server.ServeHTTP(ingestResponse, ingestRequest)

	require.Equal(t, http.StatusOK, ingestResponse.Code)
	require.True(t, server.httpHandler.ingestInitialized)
}

func TestHTTPStartConfiguresHLSWebcamPiP(t *testing.T) {
	installFakeFFmpeg(t, "exec sleep 30\n")
	server := NewServer()
	t.Cleanup(server.httpHandler.Stop)
	body := `{"inputs":[{"type":"hls","url":"https://videodelivery.net/video-id/manifest/video.m3u8"},{"type":"webcam",` +
		`"transform":{"scale":0.25,"position":"top-right"}}],` +
		`"pipeline":[{"op":"encode","params":{"codec":"h264","resolution":"1280x720"}}],` +
		`"output":{"mode":"websocket"},"session_id":"session-1"}`
	request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, media.SourceHLS, server.httpHandler.config.SourceType)
	require.Equal(t, "https://videodelivery.net/video-id/manifest/video.m3u8", server.httpHandler.config.SourceURL)
	require.True(t, server.httpHandler.config.WebcamOverlay)
	require.True(t, server.httpHandler.config.HasWebcamInput())
	require.NotNil(t, server.httpHandler.stdin)
}

func TestStreamRtmpKeyBounds(t *testing.T) {
	maximumKey := strings.Repeat("x", 2011)
	require.NoError(t, validateStreamRtmpKey(maximumKey, "RTMP output"))
	require.Len(t, streamRtmpURL(maximumKey), 2048)
	require.Error(t, validateStreamRtmpKey(maximumKey+"x", "RTMP output"))
}

func TestValidateHTTPPipelineAllowsOnlySupportedOverlayImages(t *testing.T) {
	tests := []struct {
		image    string
		position string
		wantErr  bool
	}{
		{image: "annotation", position: "full"},
		{image: "/app/assets/streamline-logo.png", position: "top-right"},
		{image: "https://example.test/image.png", position: "top-right", wantErr: true},
		{image: "/app/assets/other.png", position: "top-right", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			req := httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{
					{Op: "overlay", Params: map[string]interface{}{"image": tt.image, "position": tt.position}},
					{Op: "encode"},
				},
			}
			err := validateHTTPPipeline(req)
			if tt.wantErr {
				require.ErrorContains(t, err, "image must be annotation or /app/assets/streamline-logo.png")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateHTTPPipelineOperationCounts(t *testing.T) {
	maximum := make([]protocol.Operation, maxPipelineOperations)
	for index := 0; index < maxPipelineOperations-1; index++ {
		maximum[index] = protocol.Operation{Op: "filter", Params: map[string]interface{}{"preset": "flip"}}
	}
	maximum[maxPipelineOperations-1] = protocol.Operation{Op: "encode"}
	overMaximum := append(append([]protocol.Operation(nil), maximum...), protocol.Operation{
		Op: "filter", Params: map[string]interface{}{"preset": "flip"},
	})

	tests := []struct {
		name       string
		operations []protocol.Operation
		want       string
	}{
		{name: "empty", want: "pipeline must contain between 1 and 16 operations"},
		{name: "one encode", operations: []protocol.Operation{{Op: "encode"}}},
		{name: "sixteen operations", operations: maximum},
		{name: "seventeen operations", operations: overMaximum, want: "pipeline must contain between 1 and 16 operations"},
		{
			name: "missing encode",
			operations: []protocol.Operation{{
				Op: "filter", Params: map[string]interface{}{"preset": "flip"},
			}},
			want: "pipeline must contain exactly one encode operation",
		},
		{
			name:       "duplicate encode",
			operations: []protocol.Operation{{Op: "encode"}, {Op: "encode"}},
			want:       "pipeline must contain exactly one encode operation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:    &protocol.InputConfig{Type: "webcam"},
				Output:   &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: tt.operations,
			})
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

func TestValidateHTTPPipelineRejectsUnknownParameters(t *testing.T) {
	tests := []protocol.Operation{
		{Op: "overlay", Params: map[string]interface{}{"image": "annotation", "unexpected": true}},
		{Op: "filter", Params: map[string]interface{}{"preset": "flip", "unexpected": true}},
		{Op: "subtitle", Params: map[string]interface{}{"source": "auto", "unexpected": true}},
		{Op: "encode", Params: map[string]interface{}{"codec": "h264", "unexpected": true}},
	}

	for _, operation := range tests {
		t.Run(operation.Op, func(t *testing.T) {
			pipeline := []protocol.Operation{operation}
			if operation.Op != "encode" {
				pipeline = append(pipeline, protocol.Operation{Op: "encode"})
			}
			err := validateHTTPPipeline(httpStartRequest{
				Input:    &protocol.InputConfig{Type: "webcam"},
				Output:   &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: pipeline,
			})
			require.ErrorContains(t, err, "unexpected is not supported")
		})
	}
}

func TestValidateHTTPPipelineRejectsIgnoredOverlayParameters(t *testing.T) {
	for _, parameter := range []string{"padding", "x", "y", "scale", "opacity"} {
		t.Run(parameter, func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{
					{Op: "overlay", Params: map[string]interface{}{
						"image": "/app/assets/streamline-logo.png", parameter: float64(1),
					}},
					{Op: "encode"},
				},
			})
			require.ErrorContains(t, err, parameter+" is not supported")
		})
	}
}

func TestValidateHTTPPipelineOverlayPositions(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		position string
		wantErr  bool
	}{
		{name: "annotation full", image: "annotation", position: "full"},
		{name: "logo top right", image: "/app/assets/streamline-logo.png", position: "top-right"},
		{name: "annotation position required", image: "annotation", wantErr: true},
		{name: "annotation top right", image: "annotation", position: "top-right", wantErr: true},
		{name: "logo position required", image: "/app/assets/streamline-logo.png", wantErr: true},
		{name: "logo top left", image: "/app/assets/streamline-logo.png", position: "top-left", wantErr: true},
		{name: "logo full", image: "/app/assets/streamline-logo.png", position: "full", wantErr: true},
		{name: "annotation center", image: "annotation", position: "center", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]interface{}{"image": tt.image}
			if tt.position != "" {
				params["position"] = tt.position
			}
			err := validateHTTPPipeline(httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{
					{Op: "overlay", Params: params},
					{Op: "encode"},
				},
			})
			if tt.wantErr {
				require.ErrorContains(t, err, "overlay must use the fixed logo at top-right or annotation at full")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateHTTPPipelineSupportsOnlyH264(t *testing.T) {
	for _, codec := range []string{"h265", "vp8", "vp9", "av1"} {
		t.Run(codec, func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{{
					Op: "encode", Params: map[string]interface{}{"codec": codec},
				}},
			})

			require.ErrorContains(t, err, "codec must be h264")
		})
	}

	require.NoError(t, validateHTTPPipeline(httpStartRequest{
		Input:  &protocol.InputConfig{Type: "webcam"},
		Output: &protocol.OutputConfig{Mode: "websocket"},
		Pipeline: []protocol.Operation{{
			Op: "encode", Params: map[string]interface{}{"codec": "h264"},
		}},
	}))
}

func TestValidateHTTPPipelineRestrictsSyntheticInputToCanonicalEncode(t *testing.T) {
	tests := []protocol.Operation{
		{Op: "overlay", Params: map[string]interface{}{"image": "/app/assets/streamline-logo.png", "position": "top-right"}},
		{Op: "filter", Params: map[string]interface{}{"preset": "flip"}},
		{Op: "subtitle", Params: map[string]interface{}{"source": "auto"}},
		{Op: "encode", Params: map[string]interface{}{"codec": "h264"}},
		{Op: "encode", Params: map[string]interface{}{"preset": "ultrafast"}},
		{Op: "encode", Params: map[string]interface{}{"bitrate": "2M"}},
		{Op: "encode", Params: map[string]interface{}{"resolution": "160x120"}},
		{Op: "encode", Params: map[string]interface{}{"fps": float64(10)}},
		{Op: "encode", Params: map[string]interface{}{"gop": float64(30)}},
	}

	for _, operation := range tests {
		name := operation.Op
		for key := range operation.Params {
			name += " " + key
		}
		t.Run(name, func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:    &protocol.InputConfig{Type: "test"},
				Output:   &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{operation},
			})

			require.EqualError(t, err, "test input supports only one encode operation with no parameters")
		})
	}

	require.NoError(t, validateHTTPPipeline(httpStartRequest{
		Input:    &protocol.InputConfig{Type: "test"},
		Output:   &protocol.OutputConfig{Mode: "websocket"},
		Pipeline: []protocol.Operation{{Op: "encode"}},
	}))
}

func TestValidateHTTPPipelineFilterParameters(t *testing.T) {
	for _, preset := range []string{"blur", "brightness", "contrast", "saturation", "gamma", "sharpen"} {
		t.Run(preset+" accepts amount", func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{
					{Op: "filter", Params: map[string]interface{}{"preset": preset, "amount": float64(1)}},
					{Op: "encode"},
				},
			})
			require.NoError(t, err)
		})

		t.Run(preset+" requires amount", func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{
					{Op: "filter", Params: map[string]interface{}{"preset": preset}},
					{Op: "encode"},
				},
			})
			require.ErrorContains(t, err, "amount is required")
		})

		t.Run(preset+" rejects degrees", func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{
					{Op: "filter", Params: map[string]interface{}{
						"preset": preset, "amount": float64(1), "degrees": float64(90),
					}},
					{Op: "encode"},
				},
			})
			require.ErrorContains(t, err, "degrees is not supported")
		})
	}

	tests := []struct {
		name   string
		params map[string]interface{}
		want   string
	}{
		{name: "flip", params: map[string]interface{}{"preset": "flip"}},
		{name: "flip rejects amount", params: map[string]interface{}{"preset": "flip", "amount": float64(1)}, want: "amount is not supported"},
		{name: "flip rejects degrees", params: map[string]interface{}{"preset": "flip", "degrees": float64(90)}, want: "degrees is not supported"},
		{name: "rotate", params: map[string]interface{}{"preset": "rotate", "degrees": float64(90)}},
		{name: "rotate requires degrees", params: map[string]interface{}{"preset": "rotate"}, want: "degrees is required"},
		{
			name: "rotate rejects amount",
			params: map[string]interface{}{
				"preset": "rotate", "degrees": float64(90), "amount": float64(1),
			},
			want: "amount is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHTTPPipeline(httpStartRequest{
				Input:  &protocol.InputConfig{Type: "webcam"},
				Output: &protocol.OutputConfig{Mode: "websocket"},
				Pipeline: []protocol.Operation{
					{Op: "filter", Params: tt.params},
					{Op: "encode"},
				},
			})
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

func TestHTTPStartSubtitleMetadata(t *testing.T) {
	t.Run("ready subtitles are configured, reported, and cleaned on stop", func(t *testing.T) {
		installFakeFFmpeg(t, "exec sleep 30\n")
		subtitleFile := filepath.Join(t.TempDir(), "captions.srt")
		require.NoError(t, os.WriteFile(subtitleFile, []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n"), 0o600))
		server := NewServer()
		server.subtitleFetcher = func(_ context.Context, videoID string) (*subtitle.FetchResult, error) {
			require.Equal(t, "video-id", videoID)
			return &subtitle.FetchResult{SubtitleFile: subtitleFile, Language: "en", CueCount: 1}, nil
		}
		body := `{"input":{"type":"hls",` +
			`"url":"https://videodelivery.net/video-id/manifest/video.m3u8?token=source-secret"},` +
			`"output":{"mode":"websocket"},` +
			`"pipeline":[{"op":"subtitle"},{"op":"encode"}]}`
		request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		var result httpStartResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, "ready", result.Subtitle.State)
		require.Equal(t, "en", *result.Subtitle.Language)
		require.Equal(t, 1, *result.Subtitle.CueCount)
		require.NotContains(t, response.Body.String(), "source-secret")
		require.Equal(t, subtitleFile, server.httpHandler.config.SubtitleFile)
		require.True(t, server.httpHandler.config.BurnSubtitles)
		require.Equal(t, result.Subtitle, server.httpHandler.Metrics().Subtitle)

		server.httpHandler.Stop()
		require.NoFileExists(t, subtitleFile)
	})

	t.Run("unsupported sources report unavailable without fetching", func(t *testing.T) {
		server := NewServer()
		t.Cleanup(server.httpHandler.Stop)
		server.subtitleFetcher = func(context.Context, string) (*subtitle.FetchResult, error) {
			t.Fatal("subtitle fetcher should not be called")
			return nil, nil
		}
		body := `{"input":{"type":"webcam"},"output":{"mode":"websocket"},` +
			`"pipeline":[{"op":"subtitle"},{"op":"encode"}]}`
		request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		var result httpStartResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, "unavailable", result.Subtitle.State)
		require.Equal(t, subtitleUnsupportedWarning, result.Subtitle.Warning)
		require.Empty(t, server.httpHandler.config.SubtitleFile)
	})

	t.Run("fetch failure reports unavailable and still starts", func(t *testing.T) {
		installFakeFFmpeg(t, "exec sleep 30\n")
		server := NewServer()
		t.Cleanup(server.httpHandler.Stop)
		server.subtitleFetcher = func(context.Context, string) (*subtitle.FetchResult, error) {
			return nil, fmt.Errorf("fetch failed")
		}
		body := `{"input":{"type":"hls",` +
			`"url":"https://videodelivery.net/video-id/manifest/video.m3u8"},` +
			`"output":{"mode":"websocket"},` +
			`"pipeline":[{"op":"subtitle"},{"op":"encode"}]}`
		request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		var result httpStartResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, &HTTPSubtitleMetadata{
			State: "unavailable", Warning: subtitlePreparationWarning,
		}, result.Subtitle)
		require.Empty(t, server.httpHandler.config.SubtitleFile)
		require.True(t, server.httpHandler.IsSessionActive())
	})

	t.Run("start failure removes prepared subtitles", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		subtitleFile := filepath.Join(t.TempDir(), "captions.srt")
		require.NoError(t, os.WriteFile(subtitleFile, []byte("captions"), 0o600))
		server := NewServer()
		server.subtitleFetcher = func(context.Context, string) (*subtitle.FetchResult, error) {
			return &subtitle.FetchResult{SubtitleFile: subtitleFile, Language: "en", CueCount: 1}, nil
		}
		body := `{"input":{"type":"hls",` +
			`"url":"https://videodelivery.net/video-id/manifest/video.m3u8"},` +
			`"output":{"mode":"websocket"},` +
			`"pipeline":[{"op":"subtitle"},{"op":"encode"}]}`
		request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(body))
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusInternalServerError, response.Code)
		require.NoFileExists(t, subtitleFile)
		require.NotContains(t, response.Body.String(), subtitleFile)
	})
}

func TestHTTPStartSubtitleTimeoutBoundsStop(t *testing.T) {
	installFakeFFmpeg(t, "exec sleep 30\n")
	server := NewServer()
	server.subtitleTimeout = 50 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	lateFinished := make(chan struct{})
	lateSubtitleFile := filepath.Join(t.TempDir(), "late.srt")
	server.subtitleFetcher = func(context.Context, string) (*subtitle.FetchResult, error) {
		close(entered)
		<-release
		if err := os.WriteFile(lateSubtitleFile, []byte("late"), 0o600); err != nil {
			return nil, err
		}
		close(lateFinished)
		return &subtitle.FetchResult{SubtitleFile: lateSubtitleFile, Language: "en", CueCount: 1}, nil
	}
	startRequest := httptest.NewRequest(
		http.MethodPost,
		"/start",
		strings.NewReader(`{"input":{"type":"hls",`+
			`"url":"https://videodelivery.net/video-id/manifest/video.m3u8"},`+
			`"output":{"mode":"websocket"},`+
			`"pipeline":[{"op":"subtitle"},{"op":"encode"}]}`),
	)
	startResponse := httptest.NewRecorder()
	startDone := make(chan struct{})
	go func() {
		server.ServeHTTP(startResponse, startRequest)
		close(startDone)
	}()
	<-entered

	stopRequest := httptest.NewRequest(http.MethodPost, "/stop", nil)
	stopResponse := httptest.NewRecorder()
	stopDone := make(chan struct{})
	go func() {
		server.ServeHTTP(stopResponse, stopRequest)
		close(stopDone)
	}()

	select {
	case <-stopDone:
		require.Equal(t, http.StatusOK, stopResponse.Code)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("stop remained blocked by subtitle preparation")
	}
	close(release)
	select {
	case <-startDone:
		require.Equal(t, http.StatusOK, startResponse.Code)
	case <-time.After(time.Second):
		t.Fatal("start did not finish after subtitle timeout")
	}
	<-lateFinished
	require.Eventually(t, func() bool {
		_, err := os.Stat(lateSubtitleFile)
		return errors.Is(err, os.ErrNotExist)
	}, time.Second, 10*time.Millisecond)
}
