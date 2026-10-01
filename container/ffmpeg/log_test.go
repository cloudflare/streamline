package ffmpeg

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactLogLine(t *testing.T) {
	source := "https://user:password@example.test/video.m3u8?token=source-secret"
	destination := "rtmps://example.test/live/destination-secret"
	redacted := RedactLogLine("Input "+source+" failed while writing "+destination, source, destination)

	require.NotContains(t, redacted, "password")
	require.NotContains(t, redacted, "source-secret")
	require.NotContains(t, redacted, "destination-secret")
	require.Equal(t, "Input [redacted-url] failed while writing [redacted-url]", redacted)
}
