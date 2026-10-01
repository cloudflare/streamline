// Package subtitle handles fetching, parsing, and converting WebVTT subtitle
// tracks from Cloudflare Stream videos for ffmpeg burn-in.
package subtitle

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	subtitleHTTPTimeout       = 10 * time.Second
	maxSubtitleResponseBytes  = 4 * 1024 * 1024
	maxSubtitleAggregateBytes = 16 * 1024 * 1024
)

// FetchResult contains the outcome of a subtitle fetch attempt.
type FetchResult struct {
	SubtitleFile string // Path to converted SRT file, empty if not found
	Language     string // Detected language code
	CueCount     int    // Number of subtitle cues
	Warning      string // User-facing warning if no subtitles found
}

// FetchAndConvertContext downloads and converts subtitles with caller-controlled cancellation.
func FetchAndConvertContext(ctx context.Context, videoID string) (*FetchResult, error) {
	// Try English VTT first.
	vttURL := fmt.Sprintf("https://videodelivery.net/%s/captions/en.vtt", videoID)
	log.Printf("[subtitle] Trying English subtitle track")
	vttData, err := fetchURL(ctx, vttURL)
	if err != nil {
		log.Printf("[subtitle] English VTT not found")
		// English not found — try to discover available tracks via manifest.
		vttURL, err = discoverSubtitleTrack(ctx, videoID)
		if err != nil {
			log.Printf("[subtitle] Discover failed")
			return &FetchResult{
				Warning: "No subtitles found for this video. Streaming without burn-in.",
			}, nil
		}
		log.Printf("[subtitle] Discovered subtitle track")
		vttData, err = fetchURL(ctx, vttURL)
		if err != nil {
			log.Printf("[subtitle] Discovered track fetch failed")
			return &FetchResult{
				Warning: "No subtitles found for this video. Streaming without burn-in.",
			}, nil
		}
	}
	log.Printf("[subtitle] Downloaded %d bytes", len(vttData))

	// If the data looks like an HLS manifest, download and concatenate segments.
	if strings.HasPrefix(string(vttData), "#EXTM3U") {
		log.Printf("[subtitle] Track is an HLS manifest, fetching segments...")
		vttData, err = fetchHLSSubtitleSegments(ctx, vttURL, vttData)
		if err != nil {
			log.Printf("[subtitle] Segment fetch failed")
			return &FetchResult{
				Warning: "Could not download subtitle segments. Streaming without burn-in.",
			}, nil
		}
		log.Printf("[subtitle] Concatenated %d bytes of VTT", len(vttData))
	}

	// Parse WebVTT to count cues and validate.
	cues, err := ParseWebVTT(string(vttData))
	if err != nil {
		return nil, fmt.Errorf("failed to parse WebVTT: %w", err)
	}

	// Convert to SRT.
	srtFile, err := os.CreateTemp("", "streamline-subtitle-*.srt")
	if err != nil {
		return nil, fmt.Errorf("failed to create SRT file: %w", err)
	}
	srtPath := srtFile.Name()
	srtData := WebVTTToSRT(cues)
	if _, err := srtFile.WriteString(srtData); err != nil {
		srtFile.Close()
		os.Remove(srtPath)
		return nil, fmt.Errorf("failed to write SRT file: %w", err)
	}
	if err := srtFile.Close(); err != nil {
		os.Remove(srtPath)
		return nil, fmt.Errorf("failed to close SRT file: %w", err)
	}

	// Extract language from URL.
	lang := "en"
	if idx := strings.LastIndex(vttURL, "/captions/"); idx != -1 {
		rest := vttURL[idx+len("/captions/"):]
		if dot := strings.Index(rest, "."); dot != -1 {
			lang = rest[:dot]
		}
	}

	return &FetchResult{
		SubtitleFile: srtPath,
		Language:     lang,
		CueCount:     len(cues),
	}, nil
}

// httpClient returns an HTTP client, optionally disabling TLS verification
// for local development when INSECURE_SKIP_VERIFY=1 is set.
func httpClient() *http.Client {
	if os.Getenv("INSECURE_SKIP_VERIFY") == "1" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		return &http.Client{
			Transport: transport,
			Timeout:   subtitleHTTPTimeout,
		}
	}
	return &http.Client{Timeout: subtitleHTTPTimeout}
}

// fetchURL performs an HTTP GET and returns the response body.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxSubtitleResponseBytes {
		return nil, fmt.Errorf("subtitle response exceeds %d bytes", maxSubtitleResponseBytes)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSubtitleResponseBytes {
		return nil, fmt.Errorf("subtitle response exceeds %d bytes", maxSubtitleResponseBytes)
	}
	return data, nil
}

// discoverSubtitleTrack attempts to find a subtitle track by inspecting the
// HLS manifest for #EXT-X-MEDIA:TYPE=SUBTITLES entries.
func discoverSubtitleTrack(ctx context.Context, videoID string) (string, error) {
	manifestURL := fmt.Sprintf("https://videodelivery.net/%s/manifest/video.m3u8", videoID)
	data, err := fetchURL(ctx, manifestURL)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	baseURL := baseOf(manifestURL)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		if !strings.Contains(line, "TYPE=SUBTITLES") {
			continue
		}

		// Extract URI from the media tag.
		// Format: #EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",URI="captions/en.m3u8"
		uriStart := strings.Index(line, `URI="`)
		if uriStart == -1 {
			continue
		}
		uriStart += len(`URI="`)
		uriEnd := strings.Index(line[uriStart:], `"`)
		if uriEnd == -1 {
			continue
		}
		uri := line[uriStart : uriStart+uriEnd]

		// Resolve relative URI against manifest base URL.
		return resolveURL(baseURL, uri), nil
	}

	return "", fmt.Errorf("no subtitle tracks found in manifest")
}

// fetchHLSSubtitleSegments downloads and concatenates all WebVTT segments from
// an HLS subtitle manifest. The result is a single WebVTT document.
func fetchHLSSubtitleSegments(ctx context.Context, manifestURL string, manifestData []byte) ([]byte, error) {
	baseURL := baseOf(manifestURL)
	lines := strings.Split(string(manifestData), "\n")

	var output strings.Builder
	output.WriteString("WEBVTT\n\n")

	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Non-comment, non-empty line is a segment URI.
		segURL := resolveURL(baseURL, line)
		log.Printf("[subtitle] Downloading subtitle segment")
		segData, err := fetchURL(ctx, segURL)
		if err != nil {
			return nil, fmt.Errorf("subtitle segment fetch failed: %w", err)
		}
		// Strip WEBVTT header from segments (keep only cues).
		segText := string(segData)
		if strings.HasPrefix(segText, "WEBVTT") {
			// Find first blank line after header.
			if idx := strings.Index(segText, "\n\n"); idx != -1 {
				segText = segText[idx+2:]
			}
		}
		if output.Len()+len(segText)+1 > maxSubtitleAggregateBytes {
			return nil, fmt.Errorf("subtitle segments exceed %d bytes", maxSubtitleAggregateBytes)
		}
		output.WriteString(segText)
		output.WriteByte('\n')
	}

	return []byte(output.String()), nil
}

// baseOf returns the base URL (scheme+host+path dir) of a URL.
func baseOf(url string) string {
	if idx := strings.LastIndex(url, "/"); idx != -1 {
		return url[:idx+1]
	}
	return url + "/"
}

// resolveURL resolves a potentially relative URI against a base URL.
func resolveURL(base, uri string) string {
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		return uri
	}
	if strings.HasPrefix(uri, "/") {
		// Absolute path — need host from base.
		if schemeEnd := strings.Index(base, "://"); schemeEnd != -1 {
			scheme := base[:schemeEnd+3]
			rest := base[schemeEnd+3:]
			if hostEnd := strings.Index(rest, "/"); hostEnd != -1 {
				return scheme + rest[:hostEnd] + uri
			}
		}
	}
	// Relative path.
	return base + uri
}
