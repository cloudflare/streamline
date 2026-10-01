package ffmpeg

import (
	"regexp"
	"strings"
)

var logURLPattern = regexp.MustCompile(`(?i)(?:https?|rtmps?)://[^\s"'<>]+`)

func RedactLogLine(line string, sensitiveURLs ...string) string {
	for _, sensitiveURL := range sensitiveURLs {
		if sensitiveURL != "" {
			line = strings.ReplaceAll(line, sensitiveURL, "[redacted-url]")
		}
	}
	return logURLPattern.ReplaceAllString(line, "[redacted-url]")
}
