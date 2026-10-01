package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cloudflare/streamline/container/media"
)

const minWebMHeader = 4

type annotationUpdate struct {
	sessionID int
	width     int
	height    int
	sequence  uint64
}

// IngestChunk processes a single WebM chunk.
func (h *HTTPStreamHandler) IngestChunk(chunk []byte) (int, error) {
	return h.ingestChunk(chunk, "")
}

func (h *HTTPStreamHandler) IngestChunkForSessionID(chunk []byte, sessionID string) (int, error) {
	return h.ingestChunk(chunk, sessionID)
}

func (h *HTTPStreamHandler) ingestChunk(chunk []byte, sessionID string) (int, error) {
	h.ingestMu.Lock()
	defer h.ingestMu.Unlock()

	h.mu.Lock()

	if sessionID != h.relaySessionID {
		h.mu.Unlock()
		return 0, errSessionSuperseded
	}
	if !h.sessionActive {
		h.mu.Unlock()
		return 0, fmt.Errorf("stream session is not running")
	}
	if h.config.SourceType == media.SourceTest || !h.config.HasWebcamInput() {
		h.mu.Unlock()
		return 0, fmt.Errorf("ingest not available in this mode")
	}
	h.lastIngestAt = time.Now()

	// Buffer through the first WebM header. Standalone webcam sessions also
	// start ffmpeg here; mixed sessions already have a process waiting on stdin.
	if !h.ingestInitialized {
		h.pendingChunks = append(h.pendingChunks, chunk...)
		h.logger.Printf("IngestChunk: buffered %d bytes (total: %d)", len(chunk), len(h.pendingChunks))

		if h.ffmpeg == nil && h.isStarting {
			h.mu.Unlock()
			return len(chunk), nil
		}

		// Wait until we have enough data for a valid WebM header
		if len(h.pendingChunks) < minWebMHeader {
			h.mu.Unlock()
			return len(chunk), nil
		}

		// Check for WebM EBML header (0x1A 0x45 0xDF 0xA3)
		if !(h.pendingChunks[0] == 0x1A && h.pendingChunks[1] == 0x45 &&
			h.pendingChunks[2] == 0xDF && h.pendingChunks[3] == 0xA3) {
			h.logger.Printf("IngestChunk: invalid header bytes: %02x %02x %02x %02x",
				h.pendingChunks[0], h.pendingChunks[1], h.pendingChunks[2], h.pendingChunks[3])
			// Not a valid WebM header yet - might be partial data or post-stop chunks
			// Just buffer and wait; don't return error
			if len(h.pendingChunks) > 1024*1024 {
				// Too much buffered without valid header - clear it
				h.pendingChunks = h.pendingChunks[len(h.pendingChunks)/2:]
			}
			h.mu.Unlock()
			return len(chunk), nil
		}

		sid := h.sessionID
		if h.ffmpeg == nil {
			h.isStarting = true
			h.logger.Printf("Starting ffmpeg with %d bytes buffered", len(h.pendingChunks))
			err := h.startFfmpeg(sid)
			h.isStarting = false

			if err != nil {
				h.cleanupSubtitleFileLocked()
				h.mu.Unlock()
				return 0, fmt.Errorf("failed to start ffmpeg: %w", err)
			}
		}

		// Discard buffered data if session startup replaced this session ID.
		if h.sessionID != sid {
			h.pendingChunks = h.pendingChunks[:0]
			h.mu.Unlock()
			return 0, fmt.Errorf("session changed during ffmpeg startup")
		}

		if h.ffmpeg == nil || h.stdin == nil {
			h.mu.Unlock()
			return 0, fmt.Errorf("ffmpeg not running after startup")
		}

		stdin := h.stdin
		pendingChunks := append([]byte(nil), h.pendingChunks...)
		h.beginIngestWriteLocked(len(pendingChunks))
		h.pendingChunks = h.pendingChunks[:0]
		h.ingestInitialized = true
		h.mu.Unlock()

		// Do not hold the state lock while writing. Closing stdin during stop
		// must be able to interrupt a stalled ffmpeg input.
		_, err := stdin.Write(pendingChunks)
		h.finishIngestWrite(sid)
		if err != nil {
			return 0, fmt.Errorf("failed to write buffered chunks: %w", err)
		}

		return len(chunk), nil
	}

	// ffmpeg is running and the WebM initialization header was accepted.
	if h.stdin == nil {
		h.mu.Unlock()
		return 0, fmt.Errorf("ffmpeg stdin not available")
	}

	stdin := h.stdin
	sid := h.sessionID
	h.beginIngestWriteLocked(len(chunk))
	h.mu.Unlock()
	_, err := stdin.Write(chunk)
	h.finishIngestWrite(sid)
	if err != nil {
		return 0, fmt.Errorf("failed to write chunk: %w", err)
	}

	return len(chunk), nil
}

func (h *HTTPStreamHandler) finishIngestWrite(sid int) {
	h.mu.Lock()
	if h.sessionID == sid {
		if !h.ingestWriteStartedAt.IsZero() {
			h.lastIngestWriteDuration = time.Since(h.ingestWriteStartedAt)
		}
		h.ingestWriting = false
		h.ingestWriteStartedAt = time.Time{}
		h.ingestWriteBytes = 0
	}
	h.mu.Unlock()
}

func (h *HTTPStreamHandler) beginIngestWriteLocked(size int) {
	h.ingestWriting = true
	h.ingestWriteStartedAt = time.Now()
	h.ingestWriteBytes = size
}

func (h *HTTPStreamHandler) beginAnnotationUpdate() (annotationUpdate, error) {
	return h.beginAnnotationUpdateForSessionID("")
}

func (h *HTTPStreamHandler) beginAnnotationUpdateForSessionID(sessionID string) (annotationUpdate, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sessionID != h.relaySessionID {
		return annotationUpdate{}, errSessionSuperseded
	}
	if !h.sessionActive {
		return annotationUpdate{}, &annotationError{
			code: "ANNOTATION_NOT_RUNNING", message: "Stream session is not running",
		}
	}
	if !h.config.AnnotationOverlay {
		return annotationUpdate{}, &annotationError{
			code:    "ANNOTATION_NOT_ENABLED",
			message: "Annotation overlay is not enabled for this stream",
		}
	}

	now := time.Now()
	if !h.lastAnnotation.IsZero() && now.Sub(h.lastAnnotation) < annotationMinInterval {
		return annotationUpdate{}, &annotationError{
			code:    "ANNOTATION_RATE_LIMITED",
			message: "Annotation updates are limited to 20 per second",
		}
	}
	h.lastAnnotation = now
	h.annotationSeq++
	width, height := annotationDimensions(h.config.Encode.Resolution)
	return annotationUpdate{
		sessionID: h.sessionID,
		width:     width,
		height:    height,
		sequence:  h.annotationSeq,
	}, nil
}

func (h *HTTPStreamHandler) commitAnnotationUpdate(update annotationUpdate, frame []byte) error {
	width, height, err := validateAnnotationPNG(frame)
	if err != nil {
		return err
	}
	if width != update.width || height != update.height {
		return &annotationError{
			code: "INVALID_ANNOTATION_DIMENSIONS",
			message: fmt.Sprintf("Annotation canvas must match the configured output resolution (%dx%d)",
				update.width, update.height),
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sessionActive || h.sessionID != update.sessionID || !h.config.AnnotationOverlay {
		return &annotationError{code: "ANNOTATION_NOT_RUNNING", message: "Stream session is no longer running"}
	}
	if update.sequence <= h.committedSeq {
		return &annotationError{code: "ANNOTATION_SUPERSEDED", message: "Annotation update was superseded"}
	}
	h.committedSeq = update.sequence
	h.setAnnotationFrame(frame)
	return nil
}

// UpdateAnnotation replaces the PNG replayed into an active annotation overlay.
func (h *HTTPStreamHandler) UpdateAnnotation(frame []byte) error {
	update, err := h.beginAnnotationUpdate()
	if err != nil {
		return err
	}
	return h.commitAnnotationUpdate(update, frame)
}

func (h *HTTPStreamHandler) seedAnnotationFrameLocked(cfg media.SessionConfig) error {
	h.lastAnnotation = time.Time{}
	h.committedSeq = h.annotationSeq
	if !cfg.AnnotationOverlay {
		h.setAnnotationFrame(nil)
		return nil
	}

	frame, err := transparentAnnotationFrame(cfg.Encode.Resolution)
	if err != nil {
		return err
	}
	h.setAnnotationFrame(frame)
	return nil
}

func (h *HTTPStreamHandler) stopAnnotationWriterLocked(clearFrame bool) {
	stop := h.annotationStop
	done := h.annotationDone
	pipe := h.annotationPipe
	h.annotationStop = nil
	h.annotationDone = nil
	h.annotationPipe = nil

	if stop != nil {
		close(stop)
	}
	if done != nil {
		if err := <-done; err != nil && !errors.Is(err, os.ErrClosed) {
			h.logger.Printf("[annotation] Writer stopped with error: %v", err)
		}
	}
	if pipe != nil {
		_ = pipe.Close()
	}
	if clearFrame {
		h.lastAnnotation = time.Time{}
		h.setAnnotationFrame(nil)
	}
}

func (h *HTTPStreamHandler) setAnnotationFrame(frame []byte) {
	h.annotationMu.Lock()
	defer h.annotationMu.Unlock()
	h.annotationPNG = append([]byte(nil), frame...)
}

func (h *HTTPStreamHandler) annotationFrame() []byte {
	h.annotationMu.RLock()
	defer h.annotationMu.RUnlock()
	return h.annotationPNG
}
