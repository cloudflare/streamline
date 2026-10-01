package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/streamline/container/ffmpeg"
	"github.com/cloudflare/streamline/container/media"
)

const (
	rtmpRestartDelay    = 500 * time.Millisecond
	rtmpOutputTimeout   = 10 * time.Second
	rtmpStartupTimeout  = 20 * time.Second
	webcamIngestTimeout = 10 * time.Second
)

// startFfmpeg spawns ffmpeg with stdout only for fMP4 preview output.
func (h *HTTPStreamHandler) startFfmpeg(sid int) error {
	h.logger.Printf("startFfmpeg: sid=%d test=%v direct=%v webcam=%v preview=%v sourceType=%s",
		sid, h.config.SourceType == media.SourceTest, h.config.IsDirectInput(), h.config.HasWebcamInput(),
		h.config.IsPreview(), h.config.SourceType)

	args, err := ffmpeg.BuildArgs(h.config)
	if err != nil {
		return err
	}

	globalArgs := []string{"-nostats"}
	if h.config.Diagnostics || !h.config.IsPreview() {
		globalArgs = append(globalArgs, "-stats_period", "1", "-progress", "pipe:2")
	}
	args = append(globalArgs, args...)
	cmd := exec.Command("ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture stderr for diagnostics
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// RTMP output is written directly by ffmpeg and must not create a stdout pipe.
	var stdoutPipe io.ReadCloser
	if h.config.IsPreview() {
		stdoutPipe, err = cmd.StdoutPipe()
		if err != nil {
			stderrPipe.Close()
			return fmt.Errorf("failed to create stdout pipe: %w", err)
		}
	}

	// Get stdin whenever browser WebM is one of the media inputs.
	var stdinPipe io.WriteCloser
	if h.config.HasWebcamInput() {
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			stderrPipe.Close()
			if stdoutPipe != nil {
				stdoutPipe.Close()
			}
			return fmt.Errorf("failed to create stdin pipe: %w", err)
		}
	}

	var annotationRead *os.File
	var annotationReady chan error
	if h.config.AnnotationOverlay {
		annotationRead, h.annotationPipe, err = os.Pipe()
		if err != nil {
			stderrPipe.Close()
			if stdoutPipe != nil {
				stdoutPipe.Close()
			}
			if stdinPipe != nil {
				stdinPipe.Close()
			}
			return fmt.Errorf("failed to create annotation pipe: %w", err)
		}
		cmd.ExtraFiles = []*os.File{annotationRead}
		h.annotationStop = make(chan struct{})
		h.annotationDone = make(chan error, 1)
		annotationReady = make(chan error, 1)
		annotationWrite := h.annotationPipe
		go func(pipe *os.File, stop <-chan struct{}, ready chan<- error, done chan<- error) {
			done <- replayAnnotationFrames(h.logger, pipe, h.annotationFrame, stop, ready)
		}(annotationWrite, h.annotationStop, annotationReady, h.annotationDone)
	}

	if err := cmd.Start(); err != nil {
		if annotationRead != nil {
			annotationRead.Close()
		}
		h.stopAnnotationWriterLocked(false)
		stderrPipe.Close()
		if stdoutPipe != nil {
			stdoutPipe.Close()
		}
		if stdinPipe != nil {
			stdinPipe.Close()
		}
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}
	stderrProgressReady := make(chan struct{})
	stderrDone := make(chan struct{})
	sourceURL := h.config.SourceURL
	destinationURL := h.config.DestinationURL
	go func() {
		defer close(stderrDone)
		h.readFFmpegStderr(stderrPipe, sid, stderrProgressReady, sourceURL, destinationURL)
	}()
	if annotationRead != nil {
		annotationRead.Close()
		var annotationErr error
		select {
		case annotationErr = <-annotationReady:
		case <-time.After(annotationPrimeTimeout):
			annotationErr = errors.New("timed out priming the initial annotation frame")
		}
		if annotationErr != nil {
			h.stopAnnotationWriterLocked(false)
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			<-stderrDone
			if stdoutPipe != nil {
				stdoutPipe.Close()
			}
			if stdinPipe != nil {
				stdinPipe.Close()
			}
			return fmt.Errorf("failed to prime annotation pipe: %w", annotationErr)
		}
	}

	h.logger.Printf("startFfmpeg: ffmpeg started (pid=%d)", cmd.Process.Pid)

	h.ffmpeg = cmd
	h.stdin = stdinPipe
	h.stdout = stdoutPipe
	h.isRunning = true
	h.processStartedAt = time.Now()
	h.lastOutputAt = time.Time{}
	h.progress = FFmpegProgress{
		State:     "starting",
		UpdatedAt: h.processStartedAt,
	}
	close(stderrProgressReady)

	// Parse stdout only for fMP4 preview output.
	outputDone := make(chan struct{})
	if stdoutPipe == nil {
		close(outputDone)
	} else {
		go func() {
			defer close(outputDone)
			defer func() {
				if r := recover(); r != nil {
					h.logger.Printf("PANIC in fMP4 parser: %v", r)
				}
			}()
			h.parseFfmpegOutput(stdoutPipe, sid)
		}()
	}

	// Monitor ffmpeg exit
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.logger.Printf("PANIC in ffmpeg monitor: %v", r)
			}
		}()
		err := cmd.Wait()
		<-outputDone
		<-stderrDone
		h.mu.Lock()
		currentSessionID := h.sessionID
		if currentSessionID != sid || h.ffmpeg != cmd {
			h.mu.Unlock()
			h.logger.Printf("ffmpeg monitor: ignoring exit for stale session %d (current=%d)", sid, currentSessionID)
			return
		}
		autoRestart := h.sessionActive && h.config.SourceType == media.SourceRTMP && !h.config.HasWebcamInput()
		h.stopAnnotationWriterLocked(!autoRestart)
		h.isRunning = false
		h.ffmpeg = nil
		h.stdin = nil
		h.stdout = nil
		h.processStartedAt = time.Time{}
		if err != nil {
			h.lastFFmpegExitErr = err.Error()
		} else {
			h.lastFFmpegExitErr = ""
		}
		if autoRestart {
			// A killed process may leave an incomplete MP4 box in stdoutBuf. Do
			// not splice that tail into the restarted process's initialization.
			h.initSegment = nil
			h.stdoutBuf = h.stdoutBuf[:0]
			h.progress.State = "restarting"
			h.progress.UpdatedAt = time.Now()
		} else {
			h.sessionActive = false
			h.closeOutputSubscriberLocked()
			h.cleanupSubtitleFileLocked()
		}
		h.mu.Unlock()

		if err != nil {
			h.logger.Printf("ffmpeg exited with error: %v", err)
		} else {
			h.logger.Printf("ffmpeg exited cleanly")
		}
		if autoRestart {
			go h.restartRTMPFfmpeg(sid)
		}
	}()

	if h.config.SourceType == media.SourceRTMP && h.config.IsPreview() {
		go h.watchRTMPOutput(cmd, sid)
	} else if !h.config.IsPreview() {
		go h.watchRTMPDestination(cmd, sid)
	}

	return nil
}

func (h *HTTPStreamHandler) restartRTMPFfmpeg(sid int) {
	timer := time.NewTimer(rtmpRestartDelay)
	defer timer.Stop()
	<-timer.C

	h.mu.Lock()
	if h.sessionID != sid || !h.sessionActive || h.config.SourceType != media.SourceRTMP ||
		h.config.HasWebcamInput() || h.ffmpeg != nil {
		h.mu.Unlock()
		return
	}
	h.restartCount++
	h.lastRestartAt = time.Now()
	if err := h.startFfmpeg(sid); err != nil {
		restartCount := h.restartCount
		h.progress.State = "restart-failed"
		h.progress.UpdatedAt = time.Now()
		h.lastFFmpegExitErr = err.Error()
		h.mu.Unlock()
		h.logger.Printf("ffmpeg restart %d failed: %v", restartCount, err)
		go h.restartRTMPFfmpeg(sid)
		return
	}
	restartCount := h.restartCount
	h.mu.Unlock()
	h.logger.Printf("ffmpeg restart %d started", restartCount)
}

func (h *HTTPStreamHandler) watchRTMPOutput(cmd *exec.Cmd, sid int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		h.mu.Lock()
		if h.sessionID != sid || h.ffmpeg != cmd || !h.isRunning {
			h.mu.Unlock()
			return
		}
		reference := h.processStartedAt
		timeout := rtmpStartupTimeout
		if !h.lastOutputAt.IsZero() {
			reference = h.lastOutputAt
			timeout = rtmpOutputTimeout
		}
		stalledFor := time.Since(reference)
		if stalledFor <= timeout {
			h.mu.Unlock()
			continue
		}

		h.progress.State = "stalled"
		h.progress.UpdatedAt = time.Now()
		process := cmd.Process
		h.mu.Unlock()

		h.logger.Printf("ffmpeg output stalled for %s; restarting RTMP input", stalledFor.Round(time.Second))
		if process != nil {
			_ = process.Kill()
		}
		return
	}
}

func (h *HTTPStreamHandler) watchRTMPDestination(cmd *exec.Cmd, sid int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		h.mu.Lock()
		if h.sessionID != sid || h.ffmpeg != cmd || !h.isRunning {
			h.mu.Unlock()
			return
		}
		if h.config.HasWebcamInput() && h.ingestWriting && !h.ingestWriteStartedAt.IsZero() {
			stalledFor := time.Since(h.ingestWriteStartedAt)
			if stalledFor > rtmpOutputTimeout {
				h.progress.State = "stalled"
				h.progress.UpdatedAt = time.Now()
				process := cmd.Process
				h.mu.Unlock()

				h.logger.Printf("ffmpeg webcam ingest write stalled for %s; stopping RTMP output process",
					stalledFor.Round(time.Second))
				if process != nil {
					_ = process.Kill()
				}
				return
			}
		}

		reference := h.processStartedAt
		timeout := rtmpStartupTimeout
		if h.progress.State == "continue" || h.progress.OutTimeUS > 0 {
			reference = h.progress.UpdatedAt
			timeout = rtmpOutputTimeout
		}
		if h.config.WebcamOverlay && !h.ingestWriting &&
			!h.processStartedAt.IsZero() {
			ingestReference := h.lastIngestAt
			ingestTimeout := webcamIngestTimeout
			if ingestReference.IsZero() {
				ingestReference = h.processStartedAt
				ingestTimeout = rtmpStartupTimeout
			}
			if stalledFor := time.Since(ingestReference); stalledFor > ingestTimeout {
				h.progress.State = "stalled"
				h.progress.UpdatedAt = time.Now()
				process := cmd.Process
				h.mu.Unlock()

				h.logger.Printf("ffmpeg webcam ingest stalled for %s; stopping RTMP output process",
					stalledFor.Round(time.Second))
				if process != nil {
					_ = process.Kill()
				}
				return
			}
		}
		// Standalone webcam sessions have historically tolerated producer gaps.
		// Mixed sessions cannot because the direct background keeps ffmpeg alive
		// while its browser input is permanently starved.
		if !h.config.IsDirectInput() && !h.ingestWriting && time.Since(h.lastIngestAt) > 2*time.Second {
			h.mu.Unlock()
			continue
		}
		stalledFor := time.Since(reference)
		if stalledFor <= timeout {
			h.mu.Unlock()
			continue
		}

		h.progress.State = "stalled"
		h.progress.UpdatedAt = time.Now()
		process := cmd.Process
		h.mu.Unlock()

		h.logger.Printf("ffmpeg RTMP destination stalled for %s; stopping process", stalledFor.Round(time.Second))
		if process != nil {
			_ = process.Kill()
		}
		return
	}
}

func (h *HTTPStreamHandler) readFFmpegStderr(
	r io.Reader,
	sid int,
	progressReady <-chan struct{},
	sensitiveURLs ...string,
) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		select {
		case <-progressReady:
			if h.updateFFmpegProgress(sid, line) {
				continue
			}
		default:
		}
		if line != "" {
			h.logger.Printf("[ffmpeg stderr] %s", ffmpeg.RedactLogLine(line, sensitiveURLs...))
		}
	}
	if err := scanner.Err(); err != nil {
		h.logger.Printf("ffmpeg stderr read error: %v", err)
		_, _ = io.Copy(io.Discard, r)
	}
}

func (h *HTTPStreamHandler) updateFFmpegProgress(sid int, line string) bool {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessionID != sid {
		return true
	}

	parseInt := func(destination *int64) {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			*destination = parsed
		}
	}
	parseFloat := func(destination *float64, raw string) {
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			*destination = parsed
		}
	}

	switch key {
	case "frame":
		parseInt(&h.progress.Frame)
	case "fps":
		parseFloat(&h.progress.FPS, value)
	case "bitrate":
		h.progress.Bitrate = strings.TrimSpace(value)
	case "total_size":
		parseInt(&h.progress.TotalSize)
	case "out_time_us":
		parseInt(&h.progress.OutTimeUS)
	case "dup_frames":
		parseInt(&h.progress.DupFrames)
	case "drop_frames":
		parseInt(&h.progress.DropFrames)
	case "speed":
		parseFloat(&h.progress.Speed, strings.TrimSuffix(strings.TrimSpace(value), "x"))
	case "progress":
		h.progress.State = strings.TrimSpace(value)
		h.progress.UpdatedAt = time.Now()
	default:
		return strings.HasPrefix(key, "stream_") || key == "out_time" || key == "out_time_ms"
	}
	return true
}
