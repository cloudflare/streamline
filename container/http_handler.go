package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/cloudflare/streamline/container/media"
	"github.com/cloudflare/streamline/container/protocol"
	"github.com/gorilla/websocket"
)

const (
	maxHTTPStartBodyBytes             = 64 * 1024
	maxHTTPIngestBodyBytes            = 1 * 1024 * 1024
	httpStartBodyReadTimeout          = 10 * time.Second
	httpIngestBodyReadTimeout         = 10 * time.Second
	httpIngestSlowLogThreshold        = 2 * time.Second
	defaultSubtitlePreparationTimeout = 10 * time.Second
)

// handleHTTPStart handles POST /start
func (s *Server) handleHTTPStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.ContentLength > maxHTTPStartBodyBytes {
		http.Error(w, "Request body exceeds 64 KiB", http.StatusRequestEntityTooLarge)
		return
	}
	responseController := http.NewResponseController(w)
	_ = responseController.SetReadDeadline(time.Now().Add(httpStartBodyReadTimeout))
	defer responseController.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPStartBodyBytes)
	req, err := decodeHTTPStartRequest(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "Request body exceeds 64 KiB", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = responseController.SetReadDeadline(time.Time{})
	if err := normalizeHTTPStartRequest(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateHTTPPipeline(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	primaryInput := req.Input
	var webcamTransform *protocol.InputTransform
	if primaryInput == nil {
		primaryInput = &req.Inputs[0]
		webcamTransform = req.Inputs[1].Transform
	}
	sourceType := media.SourceType(primaryInput.Type)
	sourceURL := primaryInput.URL
	if primaryInput.Type == "rtmp" {
		sourceURL = streamRtmpURL(primaryInput.Key)
	}
	cfg := buildMediaConfig(sourceType, sourceURL, webcamTransform, req.Pipeline, *req.Output)
	cfg.Diagnostics = req.Diagnostics

	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	subtitleTimeout := s.subtitleTimeout
	if subtitleTimeout <= 0 {
		subtitleTimeout = defaultSubtitlePreparationTimeout
	}
	subtitleContext, cancelSubtitle := context.WithTimeout(r.Context(), subtitleTimeout)
	subtitleMetadata := s.prepareHTTPSubtitles(subtitleContext, &cfg)
	cancelSubtitle()
	if r.Context().Err() != nil {
		if cfg.SubtitleFile != "" {
			_ = os.Remove(cfg.SubtitleFile)
		}
		return
	}

	// Get or create handler for this session
	if s.httpHandler == nil {
		s.httpHandler = NewHTTPStreamHandler()
	}

	s.stopRelayPublisher()
	s.setActiveSessionID("")
	s.cancelSessionTimerLocked()

	// Start ffmpeg (blocks briefly while process spawns)
	if err := s.httpHandler.StartWithSessionID(cfg, req.SessionID); err != nil {
		log.Printf("[http] Failed to start ffmpeg: %v", err)
		http.Error(w, "Failed to start stream", http.StatusInternalServerError)
		return
	}
	s.setActiveSessionID(req.SessionID)
	s.httpHandler.setSubtitleMetadata(subtitleMetadata)
	if req.Output.Relay != nil {
		s.startRelayPublisher(*req.Output.Relay, req.SessionID)
	}
	s.scheduleSessionTimerLocked()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(httpStartResponse{
		Status:   "started",
		Mode:     httpStartResponseMode(cfg.SourceType),
		Output:   httpStartOutput{Mode: req.Output.Mode, Format: req.Output.Format},
		Subtitle: subtitleMetadata,
	})
}

func httpStartResponseMode(sourceType media.SourceType) string {
	if sourceType == media.SourceHLS || sourceType == media.SourceRTMP {
		return "direct"
	}
	return string(sourceType)
}

// handleHTTPAnnotation handles PUT /api/annotation with a raw PNG request body.
func (s *Server) handleHTTPAnnotation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "image/png" {
		http.Error(w, "Content-Type must be image/png", http.StatusUnsupportedMediaType)
		return
	}
	if s.httpHandler != nil && s.httpHandler.IsSessionActive() &&
		!s.matchesActiveSessionID(r.Header.Get("X-Streamline-Session-ID")) {
		http.Error(w, "Relay session is no longer current", http.StatusConflict)
		return
	}
	if r.ContentLength > maxAnnotationImageBytes {
		http.Error(w, "Annotation image exceeds the 5MB limit", http.StatusRequestEntityTooLarge)
		return
	}
	responseController := http.NewResponseController(w)
	_ = responseController.SetReadDeadline(time.Now().Add(httpIngestBodyReadTimeout))
	defer responseController.SetReadDeadline(time.Time{})
	update, err := s.httpHandler.beginAnnotationUpdateForSessionID(r.Header.Get("X-Streamline-Session-ID"))
	if err != nil {
		if errors.Is(err, errSessionSuperseded) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeAnnotationError(w, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAnnotationImageBytes)
	frame, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "Annotation image exceeds the 5MB limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read annotation image", http.StatusBadRequest)
		return
	}
	if err := s.httpHandler.commitAnnotationUpdate(update, frame); err != nil {
		writeAnnotationError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func writeAnnotationError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var annotationErr *annotationError
	if errors.As(err, &annotationErr) {
		switch annotationErr.code {
		case "ANNOTATION_NOT_RUNNING", "ANNOTATION_NOT_ENABLED":
			status = http.StatusConflict
		case "ANNOTATION_RATE_LIMITED", "ANNOTATION_BUSY":
			status = http.StatusTooManyRequests
		case "ANNOTATION_SUPERSEDED":
			status = http.StatusConflict
		case "ANNOTATION_TOO_LARGE":
			status = http.StatusRequestEntityTooLarge
		}
	}
	http.Error(w, err.Error(), status)
}

// handleHTTPIngest handles POST /ingest
func (s *Server) handleHTTPIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.httpHandler == nil {
		http.Error(w, "Not started", http.StatusServiceUnavailable)
		return
	}
	if !s.matchesActiveSessionID(r.Header.Get("X-Streamline-Session-ID")) {
		http.Error(w, "Relay session is no longer current", http.StatusConflict)
		return
	}

	if r.ContentLength > maxHTTPIngestBodyBytes {
		http.Error(w, "Ingest request exceeds the 1MiB limit", http.StatusRequestEntityTooLarge)
		return
	}
	responseController := http.NewResponseController(w)
	_ = responseController.SetReadDeadline(time.Now().Add(httpIngestBodyReadTimeout))
	defer responseController.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPIngestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "Ingest request exceeds the 1MiB limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	requestID := validIngestRequestID(r.Header.Get("X-Ingest-Request-ID"))
	if requestID != "" {
		w.Header().Set("X-Ingest-Request-ID", requestID)
	}
	startedAt := time.Now()
	n, err := s.httpHandler.IngestChunkForSessionID(body, r.Header.Get("X-Streamline-Session-ID"))
	duration := time.Since(startedAt)
	if err != nil || duration >= httpIngestSlowLogThreshold {
		log.Printf("[http] Ingest request id=%q bytes=%d duration=%s error=%v",
			requestID, len(body), duration.Round(time.Millisecond), err)
	}
	if err != nil {
		if errors.Is(err, errSessionSuperseded) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err.Error() == "invalid WebM header" {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "chunk received",
		"bytes":  n,
	})
}

func validIngestRequestID(value string) string {
	if len(value) == 0 || len(value) > 64 {
		return ""
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' {
			return ""
		}
	}
	return value
}

// handleHTTPStop handles POST /stop
func (s *Server) handleHTTPStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	log.Printf("[http] Stop request id=%q session_id=%q", r.Header.Get("X-Stop-Request-ID"),
		r.Header.Get("X-Streamline-Session-ID"))
	if s.httpHandler != nil && s.httpHandler.IsSessionActive() &&
		!s.matchesActiveSessionID(r.Header.Get("X-Streamline-Session-ID")) {
		http.Error(w, "Relay session is no longer current", http.StatusConflict)
		return
	}
	s.cancelSessionTimerLocked()

	if s.httpHandler != nil {
		s.stopRelayPublisher()
		s.httpHandler.Stop()
	}
	s.setActiveSessionID("")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "stopped",
	})
}

func (s *Server) scheduleSessionTimerLocked() {
	if s.maxSessionDuration == 0 || s.httpHandler == nil {
		return
	}
	sessionID, active := s.httpHandler.activeSessionID()
	if !active {
		return
	}
	s.sessionTimerID = sessionID
	s.sessionTimer = time.AfterFunc(s.maxSessionDuration, func() {
		s.expireSession(sessionID)
	})
}

func (s *Server) cancelSessionTimerLocked() {
	if s.sessionTimer != nil {
		s.sessionTimer.Stop()
	}
	s.sessionTimer = nil
	s.sessionTimerID = 0
}

func (s *Server) expireSession(sessionID int) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.sessionTimerID != sessionID {
		return
	}
	s.cancelSessionTimerLocked()
	if s.httpHandler == nil || !s.httpHandler.isSessionActiveForID(sessionID) {
		return
	}

	log.Printf("[http] Maximum session duration reached for session %d", sessionID)
	s.stopRelayPublisher()
	s.httpHandler.Stop()
	s.setActiveSessionID("")
}

func (s *Server) handleHTTPMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.matchesActiveSessionID(r.Header.Get("X-Streamline-Session-ID")) {
		http.Error(w, "Relay session is no longer current", http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.httpHandler.Metrics())
}

// handleHTTPOutput attaches a replaceable WebSocket subscriber to an ffmpeg
// session started by /start. Closing this socket does not stop ffmpeg.
func (s *Server) handleHTTPOutput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "Expected WebSocket upgrade", http.StatusBadRequest)
		return
	}
	if s.httpHandler == nil {
		http.Error(w, "Not started", http.StatusServiceUnavailable)
		return
	}
	requestedSessionID := r.URL.Query().Get("session_id")
	if !s.matchesActiveSessionID(requestedSessionID) {
		http.Error(w, "Relay session is no longer current", http.StatusConflict)
		return
	}

	chunks, cancel, err := s.httpHandler.SubscribeOutputForSessionID(requestedSessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	var unwritten []byte
	defer func() {
		cancel(unwritten)
	}()

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[output] WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(relayReadLimit)

	peerClosed := make(chan struct{})
	go func() {
		defer close(peerClosed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	pingTicker := time.NewTicker(outputPingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				closeCode := websocket.CloseNormalClosure
				closeReason := "output ended"
				if !s.matchesActiveSessionID(requestedSessionID) {
					closeCode = websocket.CloseServiceRestart
					closeReason = "output session replaced"
				} else if s.httpHandler.IsSessionActive() {
					closeCode = websocket.CloseTryAgainLater
					closeReason = "output subscriber fell behind"
				}
				_ = conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(closeCode, closeReason),
					time.Now().Add(outputWriteTimeout),
				)
				return
			}
			written, err := writeOutputFrames(conn, chunk)
			if err != nil {
				unwritten = chunk[written:]
				log.Printf("[output] WebSocket write failed: %v", err)
				return
			}
		case <-peerClosed:
			return
		case <-pingTicker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(outputWriteTimeout)); err != nil {
				return
			}
		}
	}
}

func (s *Server) matchesActiveSessionID(sessionID string) bool {
	s.sessionIDMu.RLock()
	defer s.sessionIDMu.RUnlock()
	return sessionID == s.activeSessionID
}

func (s *Server) setActiveSessionID(sessionID string) {
	s.sessionIDMu.Lock()
	s.activeSessionID = sessionID
	s.sessionIDMu.Unlock()
}

// handleHTTPTestStream is a diagnostic endpoint that emits chunks every 500ms
// for 5 seconds to verify chunked streaming plumbing end-to-end.
func (s *Server) handleHTTPTestStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering if behind a proxy
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Write([]byte("streaming not supported\n"))
		return
	}

	for i := 0; i < 10; i++ {
		// Prefix with chunk number and newline so client can split on it
		chunk := fmt.Sprintf("CHUNK-%d\n", i)
		w.Write([]byte(chunk))
		flusher.Flush()
		log.Printf("[test-stream] flushed chunk %d at %v", i, time.Now().Format("15:04:05.000"))
		time.Sleep(500 * time.Millisecond)
	}
	w.Write([]byte("done\n"))
	flusher.Flush()
	log.Printf("[test-stream] flushed done at %v", time.Now().Format("15:04:05.000"))
}
