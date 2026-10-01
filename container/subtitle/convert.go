package subtitle

// This file exists to provide a clean package entry point for subtitle
// conversion. The actual conversion logic lives in vtt.go.

// ConvertVTTToSRT is a convenience wrapper around ParseWebVTT + WebVTTToSRT.
func ConvertVTTToSRT(vttContent string) (string, error) {
	cues, err := ParseWebVTT(vttContent)
	if err != nil {
		return "", err
	}
	return WebVTTToSRT(cues), nil
}
