// Package media defines engine-neutral configuration for a media session.
package media

// SourceType identifies the primary media input.
type SourceType string

const (
	SourceWebcam SourceType = "webcam"
	SourceHLS    SourceType = "hls"
	SourceRTMP   SourceType = "rtmp"
	SourceTest   SourceType = "test"
)

// OutputMode identifies where processed media is delivered.
type OutputMode string

const (
	OutputPreview OutputMode = "preview"
	OutputRTMP    OutputMode = "rtmp"
)

// AnnotationOverlayFPS is the rate used to feed annotation frames to the media engine.
const AnnotationOverlayFPS = 5

// SessionConfig describes one media processing session.
type SessionConfig struct {
	Diagnostics bool

	SourceType        SourceType
	SourceURL         string
	DestinationURL    string
	OutputMode        OutputMode
	StaticLogoOverlay bool

	BurnSubtitles bool
	SubtitleFile  string

	AnnotationOverlay bool
	WebcamOverlay     bool
	WebcamScale       float64
	WebcamPosition    string

	Filters FilterConfig
	Encode  EncodeConfig
}

// IsDirectInput reports whether the media engine reads the primary input itself.
func (c SessionConfig) IsDirectInput() bool {
	return c.SourceType == SourceHLS || c.SourceType == SourceRTMP
}

// HasWebcamInput reports whether browser WebM is one of the session inputs.
func (c SessionConfig) HasWebcamInput() bool {
	return c.SourceType == SourceWebcam || c.WebcamOverlay
}

// IsPreview reports whether output is returned as a browser preview.
func (c SessionConfig) IsPreview() bool {
	return c.OutputMode == OutputPreview
}

// EncodeConfig holds configurable H.264 encoding parameters.
type EncodeConfig struct {
	Preset     string
	Bitrate    string
	Resolution string
	FPS        int
	GOP        int
}

// FilterConfig holds adjustable video filter values.
type FilterConfig struct {
	Blur          float64
	Brightness    float64
	Contrast      float64
	Saturation    float64
	Gamma         float64
	HasContrast   bool
	HasSaturation bool
	HasGamma      bool
	Sharpen       float64
	Flip          bool
	Rotate        int
}
