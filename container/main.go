package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	serverReadHeaderTimeout = 5 * time.Second
	serverIdleTimeout       = 2 * time.Minute
)

func main() {
	port := flag.String("port", "8080", "HTTP server port")
	flag.Parse()

	maxSessionDuration, err := parseMaxSessionDuration(os.Getenv("MAX_SESSION_DURATION_SECONDS"))
	if err != nil {
		log.Fatalf("[server] Invalid MAX_SESSION_DURATION_SECONDS: %v", err)
	}
	server := newServer(maxSessionDuration)

	addr := fmt.Sprintf(":%s", *port)
	log.Printf("[server] Starting on %s", addr)
	if err := newHTTPServer(addr, server).ListenAndServe(); err != nil {
		log.Fatalf("[server] Failed to start: %v", err)
	}
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

// Server is the HTTP front-end for the media pass-through container.
type Server struct {
	upgrader           websocket.Upgrader
	httpHandler        *HTTPStreamHandler
	subtitleFetcher    fetchSubtitlesFunc
	subtitleTimeout    time.Duration
	sessionMu          sync.Mutex
	sessionIDMu        sync.RWMutex
	activeSessionID    string
	relayMu            sync.Mutex
	relayCancel        func()
	relayDone          <-chan struct{}
	maxSessionDuration time.Duration
	sessionTimer       *time.Timer
	sessionTimerID     int
}

// NewServer constructs a Server with all routes registered.
func NewServer() *Server {
	return newServer(0)
}

func newServer(maxSessionDuration time.Duration) *Server {
	allowedOrigins := getAllowedOrigins()

	return &Server{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if isOriginAllowed(origin, allowedOrigins) {
					return true
				}
				log.Printf("[server] Rejected WebSocket from origin: %s", origin)
				return false
			},
		},
		httpHandler:        NewHTTPStreamHandler(),
		maxSessionDuration: maxSessionDuration,
	}
}

func parseMaxSessionDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	seconds, err := strconv.ParseUint(raw, 10, 64)
	maxSeconds := uint64((1<<63 - 1) / time.Second)
	if err != nil || seconds == 0 || seconds > maxSeconds {
		return 0, fmt.Errorf("must be a positive integer number of seconds")
	}
	return time.Duration(seconds) * time.Second, nil
}

func isOriginAllowed(origin string, allowedOrigins []string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return true
	}

	for _, allowed := range allowedOrigins {
		allowed = strings.TrimSpace(allowed)
		if origin == allowed || (strings.HasSuffix(allowed, ":") && strings.HasPrefix(origin, allowed)) {
			return true
		}
	}

	return false
}

// getAllowedOrigins returns the list of allowed WebSocket origins.
// Defaults to localhost for development. Set ALLOWED_ORIGINS env var
// for production (comma-separated list).
func getAllowedOrigins() []string {
	if env := os.Getenv("ALLOWED_ORIGINS"); env != "" {
		return strings.Split(env, ",")
	}
	// Default: allow localhost on any port for development
	return []string{
		"http://localhost:",
		"https://localhost:",
		"http://127.0.0.1:",
		"https://127.0.0.1:",
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		s.handleHealth(w, r)
	case "/start":
		s.handleHTTPStart(w, r)
	case "/ingest":
		s.handleHTTPIngest(w, r)
	case "/api/annotation":
		s.handleHTTPAnnotation(w, r)
	case "/stop":
		s.handleHTTPStop(w, r)
	case "/metrics":
		s.handleHTTPMetrics(w, r)
	case "/output":
		s.handleHTTPOutput(w, r)
	case "/test-stream":
		s.handleHTTPTestStream(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
