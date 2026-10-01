package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/cloudflare/streamline/container/media"
)

var errSessionSuperseded = errors.New("relay session is no longer current")

// HTTPStreamHandler manages HTTP-based streaming sessions.
type HTTPStreamHandler struct {
	logger        *log.Logger
	mu            sync.RWMutex
	ingestMu      sync.Mutex
	ffmpeg        *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	isRunning     bool
	sessionActive bool

	config               media.SessionConfig
	subtitleFile         string
	subtitleMetadata     *HTTPSubtitleMetadata
	configuredOutputMode string

	annotationPipe *os.File
	annotationStop chan struct{}
	annotationDone chan error
	annotationMu   sync.RWMutex
	annotationPNG  []byte
	lastAnnotation time.Time
	annotationSeq  uint64
	committedSeq   uint64

	// fMP4 parser state. initSegment accumulates ftyp+moov so they can be
	// published atomically; stdoutBuf retains incomplete boxes between reads.
	initSegment []byte
	stdoutBuf   []byte

	// Raw fMP4 output is delivered to one replaceable subscriber. When no
	// subscriber is attached, streamBuf holds a short reconnect backlog.
	streamBuf           []byte
	streamBufOverflowed bool
	outputSubscriber    chan []byte
	outputClosing       bool

	// Initial WebM buffering and active ingest state.
	pendingChunks           []byte
	isStarting              bool
	ingestInitialized       bool
	ingestWriting           bool
	ingestWriteStartedAt    time.Time
	ingestWriteBytes        int
	lastIngestWriteDuration time.Duration

	// Session isolation: goroutines from previous sessions ignore state
	// mutations when sessionID has changed.
	sessionID         int
	progress          FFmpegProgress
	processStartedAt  time.Time
	lastIngestAt      time.Time
	lastOutputAt      time.Time
	restartCount      int
	lastRestartAt     time.Time
	lastFFmpegExitErr string
	relaySessionID    string
}

// FFmpegProgress is the latest machine-readable progress emitted by ffmpeg.
type FFmpegProgress struct {
	Frame      int64     `json:"frame"`
	FPS        float64   `json:"fps"`
	Bitrate    string    `json:"bitrate"`
	TotalSize  int64     `json:"totalSize"`
	OutTimeUS  int64     `json:"outTimeUs"`
	DupFrames  int64     `json:"dupFrames"`
	DropFrames int64     `json:"dropFrames"`
	Speed      float64   `json:"speed"`
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// HTTPSubtitleMetadata describes subtitle preparation without exposing media URLs.
type HTTPSubtitleMetadata struct {
	State    string  `json:"state"`
	Language *string `json:"language,omitempty"`
	CueCount *int    `json:"cueCount,omitempty"`
	Warning  string  `json:"warning,omitempty"`
}

// HTTPStreamMetrics describes producer and relay state for short polling diagnostics.
type HTTPStreamMetrics struct {
	Running              bool                  `json:"running"`
	SessionActive        bool                  `json:"sessionActive"`
	OutputMode           string                `json:"outputMode,omitempty"`
	Subtitle             *HTTPSubtitleMetadata `json:"subtitle,omitempty"`
	Progress             FFmpegProgress        `json:"ffmpeg"`
	ProgressAgeMS        int64                 `json:"progressAgeMs"`
	LastOutputAt         time.Time             `json:"lastOutputAt"`
	OutputAgeMS          int64                 `json:"outputAgeMs"`
	RestartCount         int                   `json:"restartCount"`
	LastRestartAt        time.Time             `json:"lastRestartAt"`
	LastFFmpegExitError  string                `json:"lastFfmpegExitError,omitempty"`
	OutputSubscriber     bool                  `json:"outputSubscriber"`
	OutputQueueDepth     int                   `json:"outputQueueDepth"`
	ReconnectBufferBytes int                   `json:"reconnectBufferBytes"`
	ReconnectOverflowed  bool                  `json:"reconnectOverflowed"`
	IngestWriting        bool                  `json:"ingestWriting"`
	IngestWriteAgeMS     int64                 `json:"ingestWriteAgeMs"`
	IngestWriteBytes     int                   `json:"ingestWriteBytes"`
	LastIngestWriteMS    int64                 `json:"lastIngestWriteMs"`
}

// NewHTTPStreamHandler creates a new HTTP-based stream handler.
func NewHTTPStreamHandler() *HTTPStreamHandler {
	return &HTTPStreamHandler{
		logger: log.New(os.Stderr, "[http] ", log.LstdFlags|log.Lmsgprefix),
	}
}

// Start begins a new streaming session.
func (h *HTTPStreamHandler) Start(cfg media.SessionConfig) error {
	return h.start(cfg, "")
}

func (h *HTTPStreamHandler) StartWithSessionID(cfg media.SessionConfig, sessionID string) error {
	return h.start(cfg, sessionID)
}

func (h *HTTPStreamHandler) start(cfg media.SessionConfig, sessionID string) error {
	switch cfg.SourceType {
	case media.SourceWebcam, media.SourceHLS, media.SourceRTMP, media.SourceTest:
	default:
		return fmt.Errorf("unsupported media source type %q", cfg.SourceType)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Stop existing session
	h.stopAnnotationWriterLocked(true)
	h.closeOutputSubscriberLocked()
	if h.ffmpeg != nil && h.ffmpeg.Process != nil {
		h.logger.Printf("Stopping existing ffmpeg process")
		if h.stdin != nil {
			h.stdin.Close()
		}
		h.ffmpeg.Process.Kill()
	}
	// Always clear references after stopping (or if process already dead)
	h.ffmpeg = nil
	h.stdin = nil
	h.stdout = nil
	h.cleanupSubtitleFileLocked()

	// Increment session ID to invalidate any goroutines from previous sessions.
	h.sessionID++
	sid := h.sessionID

	// Reset state
	h.isRunning = false
	h.sessionActive = false
	h.initSegment = nil
	h.stdoutBuf = h.stdoutBuf[:0]
	h.streamBuf = h.streamBuf[:0]
	h.streamBufOverflowed = false
	h.progress = FFmpegProgress{}
	h.processStartedAt = time.Time{}
	h.lastIngestAt = time.Time{}
	h.lastOutputAt = time.Time{}
	h.restartCount = 0
	h.lastRestartAt = time.Time{}
	h.lastFFmpegExitErr = ""
	h.pendingChunks = h.pendingChunks[:0]
	h.isStarting = false
	h.ingestInitialized = false
	h.ingestWriting = false
	h.ingestWriteStartedAt = time.Time{}
	h.ingestWriteBytes = 0
	h.lastIngestWriteDuration = 0
	h.config = cfg
	h.relaySessionID = sessionID
	h.subtitleFile = cfg.SubtitleFile
	h.subtitleMetadata = nil
	if cfg.IsPreview() {
		h.configuredOutputMode = "websocket"
	} else {
		h.configuredOutputMode = "rtmp"
	}
	if err := h.seedAnnotationFrameLocked(cfg); err != nil {
		h.cleanupSubtitleFileLocked()
		h.configuredOutputMode = ""
		return fmt.Errorf("failed to seed annotation frame: %w", err)
	}

	if cfg.SourceType == media.SourceTest || cfg.IsDirectInput() {
		if err := h.startFfmpeg(sid); err != nil {
			h.cleanupSubtitleFileLocked()
			h.configuredOutputMode = ""
			return err
		}
		h.sessionActive = true
		return nil
	}

	// For webcam mode, ffmpeg starts lazily on first chunk
	h.sessionActive = true
	return nil
}

// Stop terminates the current streaming session.
func (h *HTTPStreamHandler) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.stopAnnotationWriterLocked(true)
	if h.ffmpeg != nil && h.ffmpeg.Process != nil {
		if h.stdin != nil {
			h.stdin.Close()
		}
		h.ffmpeg.Process.Kill()
		h.ffmpeg = nil
	}
	h.stdin = nil
	h.stdout = nil
	h.cleanupSubtitleFileLocked()

	// Increment session ID so in-flight goroutines know they're stale.
	h.sessionID++

	h.isRunning = false
	h.sessionActive = false
	h.initSegment = nil
	h.stdoutBuf = h.stdoutBuf[:0]
	h.streamBuf = h.streamBuf[:0]
	h.streamBufOverflowed = false
	h.progress = FFmpegProgress{}
	h.processStartedAt = time.Time{}
	h.lastIngestAt = time.Time{}
	h.lastOutputAt = time.Time{}
	h.restartCount = 0
	h.lastRestartAt = time.Time{}
	h.lastFFmpegExitErr = ""
	h.closeOutputSubscriberLocked()
	h.pendingChunks = h.pendingChunks[:0]
	h.isStarting = false
	h.ingestInitialized = false
	h.ingestWriting = false
	h.ingestWriteStartedAt = time.Time{}
	h.ingestWriteBytes = 0
	h.lastIngestWriteDuration = 0
	h.subtitleMetadata = nil
	h.configuredOutputMode = ""
	h.relaySessionID = ""
}

func (h *HTTPStreamHandler) cleanupSubtitleFileLocked() {
	if h.subtitleFile == "" {
		return
	}
	if err := os.Remove(h.subtitleFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.logger.Printf("Failed to remove generated subtitle file: %v", err)
	}
	h.subtitleFile = ""
	h.config.SubtitleFile = ""
}

func (h *HTTPStreamHandler) setSubtitleMetadata(metadata *HTTPSubtitleMetadata) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if metadata == nil || !h.sessionActive {
		return
	}
	copy := *metadata
	h.subtitleMetadata = &copy
}

// IsRunning returns true if ffmpeg is active.
func (h *HTTPStreamHandler) IsRunning() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.ffmpeg != nil && h.isRunning
}

// IsSessionActive reports whether the current session should continue across
// a transient ffmpeg restart.
func (h *HTTPStreamHandler) IsSessionActive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.sessionActive
}

func (h *HTTPStreamHandler) activeSessionID() (int, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessionID, h.sessionActive
}

func (h *HTTPStreamHandler) isSessionActiveForID(sessionID int) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessionActive && h.sessionID == sessionID
}

// Metrics returns a point-in-time snapshot without exposing source URLs or relay credentials.
func (h *HTTPStreamHandler) Metrics() HTTPStreamMetrics {
	h.mu.RLock()
	defer h.mu.RUnlock()

	queueDepth := 0
	if h.outputSubscriber != nil {
		queueDepth = len(h.outputSubscriber)
	}
	now := time.Now()
	ageMS := func(timestamp time.Time) int64 {
		if timestamp.IsZero() {
			return 0
		}
		return max(now.Sub(timestamp).Milliseconds(), 0)
	}
	outputReference := h.lastOutputAt
	if outputReference.IsZero() {
		outputReference = h.processStartedAt
	}
	var subtitleMetadata *HTTPSubtitleMetadata
	if h.subtitleMetadata != nil {
		copy := *h.subtitleMetadata
		subtitleMetadata = &copy
	}
	return HTTPStreamMetrics{
		Running:              h.isRunning,
		SessionActive:        h.sessionActive,
		OutputMode:           h.configuredOutputMode,
		Subtitle:             subtitleMetadata,
		Progress:             h.progress,
		ProgressAgeMS:        ageMS(h.progress.UpdatedAt),
		LastOutputAt:         h.lastOutputAt,
		OutputAgeMS:          ageMS(outputReference),
		RestartCount:         h.restartCount,
		LastRestartAt:        h.lastRestartAt,
		LastFFmpegExitError:  h.lastFFmpegExitErr,
		OutputSubscriber:     h.outputSubscriber != nil,
		OutputQueueDepth:     queueDepth,
		ReconnectBufferBytes: len(h.streamBuf),
		ReconnectOverflowed:  h.streamBufOverflowed,
		IngestWriting:        h.ingestWriting,
		IngestWriteAgeMS:     ageMS(h.ingestWriteStartedAt),
		IngestWriteBytes:     h.ingestWriteBytes,
		LastIngestWriteMS:    max(h.lastIngestWriteDuration.Milliseconds(), 0),
	}
}
