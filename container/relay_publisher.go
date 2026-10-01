package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/cloudflare/streamline/container/protocol"
	"github.com/gorilla/websocket"
)

const (
	relayReconnectDelay = 500 * time.Millisecond
	relayPingInterval   = 30 * time.Second
	relayReadLimit      = 1024
)

func (s *Server) startRelayPublisher(relay protocol.RelayConfig, sessionID string) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.relayMu.Lock()
	if s.relayCancel != nil {
		s.relayCancel()
	}
	if s.relayDone != nil {
		<-s.relayDone
	}
	s.relayCancel = cancel
	s.relayDone = done
	s.relayMu.Unlock()

	go func() {
		defer close(done)
		if err := s.publishOutput(ctx, relay, sessionID); err != nil && ctx.Err() == nil {
			log.Printf("[relay] Publisher stopped: %v", err)
		}
	}()
}

func (s *Server) stopRelayPublisher() {
	s.relayMu.Lock()
	if s.relayCancel != nil {
		s.relayCancel()
	}
	if s.relayDone != nil {
		<-s.relayDone
	}
	s.relayCancel = nil
	s.relayDone = nil
	s.relayMu.Unlock()
}

func (s *Server) publishOutput(ctx context.Context, relay protocol.RelayConfig, sessionID string) error {
	var terminalFrames []byte
	terminalPending := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		headers := http.Header{"Authorization": []string{"Bearer " + relay.Token}}
		conn, response, err := websocket.DefaultDialer.DialContext(ctx, relay.URL, headers)
		if err != nil {
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			if !waitForRelayRetry(ctx) {
				return ctx.Err()
			}
			continue
		}
		conn.SetReadLimit(relayReadLimit)
		if terminalPending {
			remaining, err := writeTerminalOutput(conn, terminalFrames)
			conn.Close()
			if err == nil {
				return nil
			}
			terminalFrames = remaining
			if !waitForRelayRetry(ctx) {
				return ctx.Err()
			}
			continue
		}

		chunks, cancelSubscription, err := s.httpHandler.SubscribeOutputForSessionID(sessionID)
		if err != nil {
			if errors.Is(err, errOutputNotRunning) && !s.httpHandler.IsSessionActive() {
				_, terminalErr := writeTerminalOutput(conn, nil)
				conn.Close()
				if terminalErr == nil {
					return nil
				}
				terminalPending = true
			} else {
				conn.Close()
			}
			if errors.Is(err, errOutputNotRunning) {
				if !waitForRelayRetry(ctx) {
					return ctx.Err()
				}
				continue
			}
			return fmt.Errorf("subscribe to ffmpeg output: %w", err)
		}

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		pingTicker := time.NewTicker(relayPingInterval)
		var unwritten []byte
		connected := true
		for connected {
			select {
			case <-ctx.Done():
				connected = false
			case <-readDone:
				connected = false
			case chunk, ok := <-chunks:
				if !ok {
					connected = false
					break
				}
				batch, subscriberClosed := coalesceOutputChunks(chunk, chunks)
				written, err := writeOutputFrames(conn, batch)
				if err != nil {
					unwritten = batch[written:]
					connected = false
				} else if subscriberClosed {
					connected = false
				}
			case <-pingTicker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(outputWriteTimeout)); err != nil {
					connected = false
				}
			}
		}

		pingTicker.Stop()
		cancelSubscription(unwritten)
		if ctx.Err() != nil {
			conn.Close()
			return ctx.Err()
		}
		if !s.httpHandler.IsSessionActive() {
			terminalFrames = s.httpHandler.drainRelayBuffer()
			remaining, terminalErr := writeTerminalOutput(conn, terminalFrames)
			conn.Close()
			if terminalErr == nil {
				return nil
			}
			terminalFrames = remaining
			terminalPending = true
		} else {
			conn.Close()
		}
		if !waitForRelayRetry(ctx) {
			return ctx.Err()
		}
	}
}

func writeTerminalOutput(conn *websocket.Conn, frames []byte) ([]byte, error) {
	written, err := writeOutputFrames(conn, frames)
	if err != nil {
		return frames[written:], err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(outputWriteTimeout)); err != nil {
		return nil, err
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"eos"}`)); err != nil {
		return nil, err
	}
	if err := conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "output ended"),
		time.Now().Add(outputWriteTimeout),
	); err != nil {
		return nil, err
	}
	return nil, nil
}

func coalesceOutputChunks(first []byte, chunks <-chan []byte) ([]byte, bool) {
	batch := first
	for len(batch) < maxOutputFrameBytes {
		select {
		case next, ok := <-chunks:
			if !ok {
				return batch, true
			}
			batch = append(batch, next...)
		default:
			return batch, false
		}
	}
	return batch, false
}

func waitForRelayRetry(ctx context.Context) bool {
	timer := time.NewTimer(relayReconnectDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
