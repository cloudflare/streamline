package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var errOutputNotRunning = errors.New("stream is not running")

const (
	maxStreamBufferBytes  = 8 * 1024 * 1024
	outputSubscriberQueue = 32
	outputWriteTimeout    = 10 * time.Second
	outputPingInterval    = 20 * time.Second
	maxOutputFrameBytes   = 256 * 1024
)

func (h *HTTPStreamHandler) drainRelayBuffer() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	data := append([]byte(nil), h.streamBuf...)
	h.streamBuf = h.streamBuf[:0]
	return data
}

// SubscribeOutput attaches one output consumer to the running ffmpeg session.
// Bytes accumulated between subscribers are delivered first to make a quick
// disconnect/reconnect handoff continuous.
func (h *HTTPStreamHandler) SubscribeOutput() (<-chan []byte, func([]byte), error) {
	return h.SubscribeOutputForSessionID("")
}

func (h *HTTPStreamHandler) SubscribeOutputForSessionID(
	sessionID string,
) (<-chan []byte, func([]byte), error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if sessionID != h.relaySessionID {
		return nil, nil, errSessionSuperseded
	}
	if !h.config.IsPreview() {
		return nil, nil, fmt.Errorf("output subscription is not available for RTMP output")
	}
	if h.outputSubscriber != nil {
		return nil, nil, fmt.Errorf("output subscriber already attached")
	}
	if h.streamBufOverflowed {
		return nil, nil, fmt.Errorf("output reconnect buffer overflowed; restart the stream")
	}
	sid := h.sessionID

	if !h.isRunning || h.ffmpeg == nil {
		if len(h.streamBuf) == 0 {
			return nil, nil, errOutputNotRunning
		}
		subscriber := make(chan []byte, 1)
		subscriber <- append([]byte(nil), h.streamBuf...)
		close(subscriber)
		h.streamBuf = h.streamBuf[:0]
		var once sync.Once
		cancel := func(unwritten []byte) {
			once.Do(func() {
				pending := append([]byte(nil), unwritten...)
				for chunk := range subscriber {
					pending = append(pending, chunk...)
				}
				h.mu.Lock()
				defer h.mu.Unlock()
				if h.sessionID == sid {
					h.prependOutputLocked(pending)
				}
			})
		}
		return subscriber, cancel, nil
	}

	subscriber := make(chan []byte, outputSubscriberQueue)
	if len(h.streamBuf) > 0 {
		backlog := append([]byte(nil), h.streamBuf...)
		subscriber <- backlog
		h.streamBuf = h.streamBuf[:0]
	}
	h.outputSubscriber = subscriber

	var once sync.Once
	var cancelMu sync.Mutex
	cancel := func(unwritten []byte) {
		cancelMu.Lock()
		defer cancelMu.Unlock()
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			isCurrentSubscriber := h.outputSubscriber == subscriber
			if isCurrentSubscriber && !h.outputClosing {
				close(subscriber)
			}
			pending := append([]byte(nil), unwritten...)
			for chunk := range subscriber {
				pending = append(pending, chunk...)
			}
			if h.sessionID != sid {
				return
			}
			if isCurrentSubscriber {
				h.outputSubscriber = nil
				h.outputClosing = false
			}
			h.prependOutputLocked(pending)
		})
	}

	return subscriber, cancel, nil
}

func (h *HTTPStreamHandler) publishOutputLocked(chunk []byte) {
	h.lastOutputAt = time.Now()
	data := append([]byte(nil), chunk...)
	if h.outputSubscriber == nil {
		h.appendStreamBufferLocked(data)
		return
	}
	if h.outputClosing {
		h.appendStreamBufferLocked(data)
		return
	}

	select {
	case h.outputSubscriber <- data:
		return
	default:
		h.logger.Printf("Output subscriber fell behind; closing connection")
		close(h.outputSubscriber)
		h.outputClosing = true
		h.appendStreamBufferLocked(data)
	}
}

// prependOutputLocked restores bytes removed from a subscriber ahead of output
// produced while that subscriber was closing.
func (h *HTTPStreamHandler) prependOutputLocked(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if h.streamBufOverflowed {
		return
	}
	if len(h.streamBuf)+len(chunk) > maxStreamBufferBytes {
		h.streamBuf = h.streamBuf[:0]
		h.streamBufOverflowed = true
		h.logger.Printf("Output reconnect buffer exceeded %d bytes", maxStreamBufferBytes)
		return
	}

	buffered := make([]byte, 0, len(chunk)+len(h.streamBuf))
	buffered = append(buffered, chunk...)
	buffered = append(buffered, h.streamBuf...)
	h.streamBuf = buffered
}

func (h *HTTPStreamHandler) appendStreamBufferLocked(chunk []byte) {
	if h.streamBufOverflowed {
		return
	}
	if len(h.streamBuf)+len(chunk) > maxStreamBufferBytes {
		h.streamBuf = h.streamBuf[:0]
		h.streamBufOverflowed = true
		h.logger.Printf("Output reconnect buffer exceeded %d bytes", maxStreamBufferBytes)
		return
	}
	h.streamBuf = append(h.streamBuf, chunk...)
}

func (h *HTTPStreamHandler) closeOutputSubscriberLocked() {
	if h.outputSubscriber == nil {
		return
	}
	if !h.outputClosing {
		close(h.outputSubscriber)
	}
	h.outputSubscriber = nil
	h.outputClosing = false
}

// parseFfmpegOutput reads ffmpeg stdout and extracts complete fMP4 units.
func (h *HTTPStreamHandler) parseFfmpegOutput(r io.Reader, sid int) {
	defer func() {
		if r := recover(); r != nil {
			h.logger.Printf("PANIC in parseFfmpegOutput: %v", r)
		}
	}()

	h.logger.Printf("parseFfmpegOutput: started for session %d", sid)
	buf := make([]byte, 65536)

	for {
		n, err := r.Read(buf)
		if err != nil {
			if err != io.EOF {
				h.logger.Printf("ffmpeg stdout read error: %v", err)
			} else {
				h.logger.Printf("ffmpeg stdout EOF")
			}
			return
		}

		if n > 0 {
			h.mu.Lock()
			currentSessionID := h.sessionID
			if currentSessionID != sid {
				h.mu.Unlock()
				h.logger.Printf("parseFfmpegOutput: session %d stale (current=%d), discarding %d bytes",
					sid, currentSessionID, n)
				return
			}
			h.stdoutBuf = append(h.stdoutBuf, buf[:n]...)
			h.processBoxes()
			h.mu.Unlock()
		}
	}
}

// processBoxes scans the stdout buffer for complete MP4 boxes.
func (h *HTTPStreamHandler) processBoxes() {
	defer func() {
		if r := recover(); r != nil {
			h.logger.Printf("PANIC in processBoxes: %v", r)
		}
	}()

	offset := 0
	for offset+8 <= len(h.stdoutBuf) {
		size := int(binary.BigEndian.Uint32(h.stdoutBuf[offset:]))
		boxType := string(h.stdoutBuf[offset+4 : offset+8])

		if size == 0 || size == 1 {
			// Invalid or extended size - stop processing
			h.logger.Printf("processBoxes: invalid size %d at offset %d", size, offset)
			break
		}

		if offset+size > len(h.stdoutBuf) {
			// Incomplete box - stop and wait for more data
			break
		}

		switch boxType {
		case "ftyp":
			// Wait for moov so the initialization segment is delivered atomically.
			h.initSegment = append(h.initSegment[:0], h.stdoutBuf[offset:offset+size]...)
			offset += size
		case "moov":
			h.initSegment = append(h.initSegment, h.stdoutBuf[offset:offset+size]...)
			offset += size
			h.publishOutputLocked(h.initSegment)

		case "moof":
			// Look for mdat after moof
			mdatOffset := offset + size
			if mdatOffset+8 > len(h.stdoutBuf) {
				// Incomplete mdat header - stop and wait for more data
				goto done
			}

			mdatSize := int(binary.BigEndian.Uint32(h.stdoutBuf[mdatOffset:]))
			mdatType := string(h.stdoutBuf[mdatOffset+4 : mdatOffset+8])

			if mdatType != "mdat" {
				// Not a valid segment, skip this moof
				offset += size
				continue
			}

			if mdatOffset+mdatSize > len(h.stdoutBuf) {
				// Incomplete mdat - stop and wait for more data
				goto done
			}

			// Complete moof+mdat segment
			segData := make([]byte, size+mdatSize)
			copy(segData, h.stdoutBuf[offset:mdatOffset+mdatSize])

			h.publishOutputLocked(segData)

			// Advance past this segment
			offset = mdatOffset + mdatSize

		default:
			// Unknown box, skip
			h.logger.Printf("processBoxes: unknown box type '%s' at offset %d (size: %d)", boxType, offset, size)
			offset += size
		}
	}

done:
	// Keep unprocessed data
	if offset > 0 {
		h.stdoutBuf = h.stdoutBuf[offset:]
	}
}

func writeOutputFrames(conn *websocket.Conn, data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		frameSize := min(len(data), maxOutputFrameBytes)
		frame := data[:frameSize]
		if err := conn.SetWriteDeadline(time.Now().Add(outputWriteTimeout)); err != nil {
			return written, err
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return written, err
		}
		data = data[frameSize:]
		written += frameSize
	}
	return written, nil
}
