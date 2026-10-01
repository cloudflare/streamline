package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/cloudflare/streamline/container/media"
)

var annotationDecode = make(chan struct{}, 1)

const (
	maxAnnotationImageBytes   = 5 * 1024 * 1024
	maxAnnotationCanvasWidth  = 3840
	maxAnnotationCanvasHeight = 2160
	annotationMinInterval     = 50 * time.Millisecond
	annotationPrimeTimeout    = 5 * time.Second
	annotationPrimeRetry      = 5 * time.Millisecond
	annotationReplayInterval  = time.Second / time.Duration(media.AnnotationOverlayFPS)
	defaultAnnotationWidth    = 1280
	defaultAnnotationHeight   = 720
)

type annotationError struct {
	code    string
	message string
}

func (e *annotationError) Error() string {
	return e.message
}

func validateAnnotationPNG(frame []byte) (int, int, error) {
	if len(frame) > maxAnnotationImageBytes {
		return 0, 0, &annotationError{
			code:    "ANNOTATION_TOO_LARGE",
			message: "Annotation image exceeds the 5MB limit",
		}
	}

	config, err := png.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		return 0, 0, &annotationError{
			code:    "INVALID_ANNOTATION",
			message: "Annotation image is not a valid PNG",
		}
	}
	width := config.Width
	height := config.Height
	if width <= 0 || height <= 0 || width > maxAnnotationCanvasWidth || height > maxAnnotationCanvasHeight {
		return 0, 0, &annotationError{
			code: "INVALID_ANNOTATION_DIMENSIONS",
			message: fmt.Sprintf("Annotation canvas must be between 1x1 and %dx%d",
				maxAnnotationCanvasWidth, maxAnnotationCanvasHeight),
		}
	}

	select {
	case annotationDecode <- struct{}{}:
	default:
		return 0, 0, &annotationError{
			code:    "ANNOTATION_BUSY",
			message: "Another annotation image is being decoded",
		}
	}
	defer func() { <-annotationDecode }()
	reader := bytes.NewReader(frame)
	if _, err := png.Decode(reader); err != nil || reader.Len() != 0 {
		return 0, 0, &annotationError{
			code:    "INVALID_ANNOTATION",
			message: "Annotation image is not a valid PNG",
		}
	}

	return width, height, nil
}

func transparentAnnotationFrame(resolution string) ([]byte, error) {
	width, height := annotationDimensions(resolution)
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		return nil, fmt.Errorf("failed to encode initial annotation PNG: %w", err)
	}
	return buf.Bytes(), nil
}

func annotationDimensions(resolution string) (int, int) {
	if resolution != "" {
		var width, height int
		if _, err := fmt.Sscanf(resolution, "%dx%d", &width, &height); err == nil {
			if width > 0 && width <= maxAnnotationCanvasWidth && height > 0 && height <= maxAnnotationCanvasHeight {
				return width, height
			}
		}
	}

	return defaultAnnotationWidth, defaultAnnotationHeight
}

func replayAnnotationFrames(
	logger *log.Logger,
	pipe *os.File,
	frame func() []byte,
	stop <-chan struct{},
	ready chan<- error,
) error {
	signalReady := func(err error) {
		if ready != nil {
			ready <- err
			close(ready)
			ready = nil
		}
	}
	if pipe == nil {
		signalReady(nil)
		return nil
	}

	// Non-blocking writes allow large PNGs to drain over multiple ticks without
	// stalling control requests or process cleanup. Startup is ready once the
	// first bytes enter the pipe; waiting for the whole PNG would deadlock webcam
	// ffmpeg, which reads its video input before draining the annotation input.
	fd := int(pipe.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		signalReady(err)
		return fmt.Errorf("failed to set annotation pipe non-blocking: %w", err)
	}

	var pending []byte
	initialWriteStarted := false
	loadFrame := func() {
		current := frame()
		if len(current) > 0 {
			pending = current
		}
	}
	writePending := func() error {
		if len(pending) == 0 {
			return nil
		}
		n, err := syscall.Write(fd, pending)
		if n > 0 {
			pending = pending[n:]
			if !initialWriteStarted {
				initialWriteStarted = true
				signalReady(nil)
			}
		}
		if errors.Is(err, syscall.EAGAIN) {
			return nil
		}
		return err
	}

	firstFrameAt := time.Now()
	loadFrame()
	if err := writePending(); err != nil {
		signalReady(err)
		return err
	}
	if len(pending) == 0 {
		signalReady(nil)
	}

	nextFrameAt := firstFrameAt.Add(annotationReplayInterval)
	nextFrameDelay := func() time.Duration {
		delay := time.Until(nextFrameAt)
		if delay < annotationPrimeRetry {
			return annotationPrimeRetry
		}
		return delay
	}
	timerInterval := nextFrameDelay()
	if len(pending) > 0 {
		timerInterval = annotationPrimeRetry
	}
	timer := time.NewTimer(timerInterval)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			signalReady(errors.New("annotation writer stopped before the initial frame was written"))
			return nil
		case <-timer.C:
			if len(pending) == 0 {
				loadFrame()
				nextFrameAt = nextFrameAt.Add(annotationReplayInterval)
			}
			if err := writePending(); err != nil {
				signalReady(err)
				return err
			}
			if len(pending) > 0 {
				timer.Reset(annotationPrimeRetry)
			} else {
				timer.Reset(nextFrameDelay())
			}
		}
	}
}
