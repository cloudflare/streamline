package subtitle

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cue represents a single subtitle entry with timing and text.
type Cue struct {
	Start time.Duration
	End   time.Duration
	Text  string
}

// ParseWebVTT parses WebVTT content and returns a slice of cues.
// It handles the basic WebVTT format (not all edge cases, but sufficient
// for Cloudflare Stream captions).
func ParseWebVTT(content string) ([]Cue, error) {
	lines := strings.Split(content, "\n")
	var cues []Cue
	var inCues bool

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])

		// Skip WEBVTT header and blank lines before cues.
		if !inCues {
			if strings.HasPrefix(line, "WEBVTT") {
				inCues = true
				continue
			}
			if line == "" {
				continue
			}
			// First non-empty line after header might be a cue identifier
			// or the first cue timing.
			inCues = true
		}

		if line == "" {
			continue
		}

		// Check if this line contains cue timing.
		// Formats: "00:00:01.000 --> 00:00:03.000" or "00:01.000 --> 00:03.000"
		if strings.Contains(line, "-->") {
			start, end, err := parseVTTTiming(line)
			if err != nil {
				continue // Skip malformed cues
			}

			// Collect text lines until blank line or next cue.
			var textLines []string
			for j := i + 1; j < len(lines); j++ {
				textLine := strings.TrimSpace(lines[j])
				if textLine == "" {
					break
				}
				// Stop if next line is a new cue timing.
				if strings.Contains(textLine, "-->") {
					break
				}
				textLines = append(textLines, textLine)
				i = j
			}

			if len(textLines) > 0 {
				cues = append(cues, Cue{
					Start: start,
					End:   end,
					Text:  strings.Join(textLines, "\n"),
				})
			}
		}
	}

	return cues, nil
}

// parseVTTTiming parses a WebVTT timing line.
// Formats supported: "HH:MM:SS.mmm --> HH:MM:SS.mmm" and "MM:SS.mmm --> MM:SS.mmm"
func parseVTTTiming(line string) (time.Duration, time.Duration, error) {
	parts := strings.Split(line, "-->")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid timing line: %s", line)
	}

	start, err := parseVTTTimestamp(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}

	end, err := parseVTTTimestamp(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}

	return start, end, nil
}

// parseVTTTimestamp parses a WebVTT timestamp string.
func parseVTTTimestamp(ts string) (time.Duration, error) {
	// Remove any trailing position settings.
	if idx := strings.Index(ts, " "); idx != -1 {
		ts = ts[:idx]
	}

	// Try full format first.
	t, err := time.Parse("15:04:05.000", ts)
	if err == nil {
		// time.Parse uses reference time, so compute offset from midnight.
		ref := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
		return t.Sub(ref), nil
	}

	// Try short format (MM:SS.mmm).
	t, err = time.Parse("04:05.000", ts)
	if err == nil {
		ref := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
		return t.Sub(ref), nil
	}

	// Try with comma instead of dot.
	tsComma := strings.Replace(ts, ",", ".", 1)
	t, err = time.Parse("15:04:05.000", tsComma)
	if err == nil {
		ref := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
		return t.Sub(ref), nil
	}

	t, err = time.Parse("04:05.000", tsComma)
	if err == nil {
		ref := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
		return t.Sub(ref), nil
	}

	return 0, fmt.Errorf("unrecognized timestamp format: %s", ts)
}

// formatSRTDuration formats a duration as SRT timestamp: HH:MM:SS,mmm
func formatSRTDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60
	millis := int(d.Milliseconds()) % 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, seconds, millis)
}

// WebVTTToSRT converts parsed WebVTT cues to SRT format.
func WebVTTToSRT(cues []Cue) string {
	var b strings.Builder
	for i, cue := range cues {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("\n")
		b.WriteString(formatSRTDuration(cue.Start))
		b.WriteString(" --> ")
		b.WriteString(formatSRTDuration(cue.End))
		b.WriteString("\n")
		b.WriteString(cue.Text)
		b.WriteString("\n")
	}
	return b.String()
}
