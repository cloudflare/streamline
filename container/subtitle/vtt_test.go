package subtitle

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseWebVTT(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		expectedLen  int
		expectedCues []Cue
	}{
		{
			name: "basic WebVTT with full timestamps",
			content: `WEBVTT

00:00:01.000 --> 00:00:03.000
Hello world

00:00:05.000 --> 00:00:07.000
Second cue
`,
			expectedLen: 2,
			expectedCues: []Cue{
				{Start: 1000000000, End: 3000000000, Text: "Hello world"},
				{Start: 5000000000, End: 7000000000, Text: "Second cue"},
			},
		},
		{
			name: "WebVTT with short timestamps",
			content: `WEBVTT

00:01.000 --> 00:03.000
Short timestamp
`,
			expectedLen: 1,
			expectedCues: []Cue{
				{Start: 1000000000, End: 3000000000, Text: "Short timestamp"},
			},
		},
		{
			name: "WebVTT with cue identifiers",
			content: `WEBVTT

1
00:00:01.000 --> 00:00:03.000
First cue

2
00:00:05.000 --> 00:00:07.000
Second cue
`,
			expectedLen: 2,
			expectedCues: []Cue{
				{Start: 1000000000, End: 3000000000, Text: "First cue"},
				{Start: 5000000000, End: 7000000000, Text: "Second cue"},
			},
		},
		{
			name: "WebVTT with multiline text",
			content: `WEBVTT

00:00:01.000 --> 00:00:03.000
Line one
Line two
`,
			expectedLen: 1,
			expectedCues: []Cue{
				{Start: 1000000000, End: 3000000000, Text: "Line one\nLine two"},
			},
		},
		{
			name: "WebVTT with position settings",
			content: `WEBVTT

00:00:01.000 --> 00:00:03.000 align:start
Cue with position
`,
			expectedLen: 1,
			expectedCues: []Cue{
				{Start: 1000000000, End: 3000000000, Text: "Cue with position"},
			},
		},
		{
			name:         "empty WebVTT",
			content:      "WEBVTT\n",
			expectedLen:  0,
			expectedCues: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cues, err := ParseWebVTT(tt.content)
			require.NoError(t, err)
			require.Len(t, cues, tt.expectedLen)
			for i, expected := range tt.expectedCues {
				require.Equal(t, expected.Start, cues[i].Start)
				require.Equal(t, expected.End, cues[i].End)
				require.Equal(t, expected.Text, cues[i].Text)
			}
		})
	}
}

func TestParseWebVTTInvalidTimestamps(t *testing.T) {
	content := `WEBVTT

invalid --> timestamp
Should be skipped

00:00:01.000 --> 00:00:03.000
Valid cue
`
	cues, err := ParseWebVTT(content)
	require.NoError(t, err)
	require.Len(t, cues, 1)
	require.Equal(t, "Valid cue", cues[0].Text)
}

func TestWebVTTToSRT(t *testing.T) {
	cues := []Cue{
		{Start: 1000000000, End: 3000000000, Text: "Hello world"},
		{Start: 5000000000, End: 7000000000, Text: "Second cue\nwith newline"},
	}

	result := WebVTTToSRT(cues)
	expected := `1
00:00:01,000 --> 00:00:03,000
Hello world

2
00:00:05,000 --> 00:00:07,000
Second cue
with newline
`
	require.Equal(t, expected, result)
}

func TestConvertVTTToSRT(t *testing.T) {
	vtt := `WEBVTT

00:00:01.000 --> 00:00:03.000
Hello world
`
	srt, err := ConvertVTTToSRT(vtt)
	require.NoError(t, err)
	require.Contains(t, srt, "1")
	require.Contains(t, srt, "00:00:01,000 --> 00:00:03,000")
	require.Contains(t, srt, "Hello world")
}

func TestParseVTTTimestamp(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64 // nanoseconds
	}{
		{"full format", "00:00:01.000", 1000000000},
		{"short format", "00:01.000", 1000000000},
		{"with comma", "00:00:01,000", 1000000000},
		{"hours", "01:00:00.000", 3600000000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseVTTTimestamp(tt.input)
			require.NoError(t, err)
			require.Equal(t, tt.expected, int64(result))
		})
	}
}

func TestParseVTTTimestampInvalid(t *testing.T) {
	_, err := parseVTTTimestamp("not-a-timestamp")
	require.Error(t, err)
}
