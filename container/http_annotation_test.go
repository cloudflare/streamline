package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHTTPStreamHandlerUpdatesAnnotation(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.AnnotationOverlay = true
	h.config.Encode.Resolution = "1x1"
	h.sessionActive = true
	frame := decodeTestAnnotationPNG(t)

	require.NoError(t, h.UpdateAnnotation(frame))
	require.Equal(t, frame, h.annotationFrame())

	err := h.UpdateAnnotation(frame)
	var annotationErr *annotationError
	require.ErrorAs(t, err, &annotationErr)
	require.Equal(t, "ANNOTATION_RATE_LIMITED", annotationErr.code)
}

func TestHTTPStreamHandlerRejectsAnnotationFromPreviousSession(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.AnnotationOverlay = true
	h.config.Encode.Resolution = "1x1"
	h.sessionActive = true
	h.sessionID = 1
	update, err := h.beginAnnotationUpdate()
	require.NoError(t, err)

	h.sessionID++
	err = h.commitAnnotationUpdate(update, decodeTestAnnotationPNG(t))
	var annotationErr *annotationError
	require.ErrorAs(t, err, &annotationErr)
	require.Equal(t, "ANNOTATION_NOT_RUNNING", annotationErr.code)
	require.Empty(t, h.annotationFrame())
}

func TestHTTPStreamHandlerRejectsSupersededAnnotationUpdate(t *testing.T) {
	h := NewHTTPStreamHandler()
	h.config.AnnotationOverlay = true
	h.config.Encode.Resolution = "1x1"
	h.sessionActive = true
	first, err := h.beginAnnotationUpdate()
	require.NoError(t, err)
	h.lastAnnotation = time.Time{}
	second, err := h.beginAnnotationUpdate()
	require.NoError(t, err)
	frame := decodeTestAnnotationPNG(t)
	require.NoError(t, h.commitAnnotationUpdate(second, frame))

	err = h.commitAnnotationUpdate(first, frame)
	var annotationErr *annotationError
	require.ErrorAs(t, err, &annotationErr)
	require.Equal(t, "ANNOTATION_SUPERSEDED", annotationErr.code)
}

func TestHTTPAnnotationEndpoint(t *testing.T) {
	frame := decodeTestAnnotationPNG(t)

	t.Run("accepts a PNG for an active annotation session", func(t *testing.T) {
		server := NewServer()
		server.httpHandler.config.AnnotationOverlay = true
		server.httpHandler.config.Encode.Resolution = "1x1"
		server.httpHandler.sessionActive = true
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", bytes.NewReader(frame))
		request.Header.Set("Content-Type", "image/png")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusNoContent, response.Code)
		require.Equal(t, frame, server.httpHandler.annotationFrame())
	})

	t.Run("rejects unsupported content", func(t *testing.T) {
		server := NewServer()
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", strings.NewReader("not a png"))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusUnsupportedMediaType, response.Code)
	})

	t.Run("rejects an inactive session", func(t *testing.T) {
		server := NewServer()
		server.httpHandler.config.AnnotationOverlay = true
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", bytes.NewReader(frame))
		request.Header.Set("Content-Type", "image/png")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusConflict, response.Code)
	})

	t.Run("rejects an oversized image", func(t *testing.T) {
		server := NewServer()
		server.httpHandler.config.AnnotationOverlay = true
		server.httpHandler.sessionActive = true
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", bytes.NewReader(make([]byte, maxAnnotationImageBytes+2)))
		request.Header.Set("Content-Type", "image/png")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	})

	t.Run("rejects a truncated PNG", func(t *testing.T) {
		server := NewServer()
		server.httpHandler.config.AnnotationOverlay = true
		server.httpHandler.config.Encode.Resolution = "1x1"
		server.httpHandler.sessionActive = true
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", bytes.NewReader(frame[:len(frame)-8]))
		request.Header.Set("Content-Type", "image/png")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusBadRequest, response.Code)
	})

	t.Run("rejects data after the PNG", func(t *testing.T) {
		server := NewServer()
		server.httpHandler.config.AnnotationOverlay = true
		server.httpHandler.config.Encode.Resolution = "1x1"
		server.httpHandler.sessionActive = true
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", bytes.NewReader(append(frame, 0)))
		request.Header.Set("Content-Type", "image/png")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusBadRequest, response.Code)
	})

	t.Run("rejects oversized dimensions before full decode", func(t *testing.T) {
		oversized := append([]byte(nil), frame...)
		binary.BigEndian.PutUint32(oversized[16:20], maxAnnotationCanvasWidth+1)
		binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
		server := NewServer()
		server.httpHandler.config.AnnotationOverlay = true
		server.httpHandler.config.Encode.Resolution = "1x1"
		server.httpHandler.sessionActive = true
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", bytes.NewReader(oversized))
		request.Header.Set("Content-Type", "image/png")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusBadRequest, response.Code)
	})

	t.Run("rejects a PNG that does not match the output resolution", func(t *testing.T) {
		server := NewServer()
		server.httpHandler.config.AnnotationOverlay = true
		server.httpHandler.config.Encode.Resolution = "16x16"
		server.httpHandler.sessionActive = true
		request := httptest.NewRequest(http.MethodPut, "/api/annotation", bytes.NewReader(frame))
		request.Header.Set("Content-Type", "image/png")
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusBadRequest, response.Code)
	})

	t.Run("rejects annotation in test mode", func(t *testing.T) {
		server := NewServer()
		body := strings.NewReader(`{"input":{"type":"test"},` +
			`"output":{"mode":"websocket"},` +
			`"pipeline":[{"op":"overlay","params":{"image":"annotation"}},{"op":"encode"}]}`)
		request := httptest.NewRequest(http.MethodPost, "/start", body)
		response := httptest.NewRecorder()

		server.ServeHTTP(response, request)

		require.Equal(t, http.StatusBadRequest, response.Code)
	})
}
