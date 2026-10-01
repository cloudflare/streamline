// Package protocol defines the current HTTP configuration and output framing types.
package protocol

import (
	"bytes"
	"encoding/json"
)

// InputTransform describes how a non-primary input is composited.
type InputTransform struct {
	Scale    float64 `json:"scale"`
	Position string  `json:"position"`
}

// InputConfig describes one media input source.
type InputConfig struct {
	Type      string          `json:"type"`
	URL       string          `json:"url,omitempty"`
	Key       string          `json:"key,omitempty"`
	Transform *InputTransform `json:"transform,omitempty"`
}

// RelayConfig identifies the preview relay publisher endpoint and its single-session capability.
// Access credentials are injected by the outbound Worker and are not part of the container protocol.
type RelayConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (r *RelayConfig) UnmarshalJSON(data []byte) error {
	type rawRelayConfig RelayConfig
	var decoded rawRelayConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}

	*r = RelayConfig(decoded)
	return nil
}

// OutputConfig describes the media output destination.
type OutputConfig struct {
	Mode         string       `json:"mode"`
	Key          string       `json:"key,omitempty"`
	Format       string       `json:"format,omitempty"`
	Relay        *RelayConfig `json:"relay,omitempty"`
	relayPresent bool
}

func (o *OutputConfig) UnmarshalJSON(data []byte) error {
	type rawOutputConfig OutputConfig
	var decoded rawOutputConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*o = OutputConfig(decoded)
	_, o.relayPresent = fields["relay"]
	return nil
}

// HasRelay reports whether relay was explicitly configured, including as JSON null.
func (o OutputConfig) HasRelay() bool {
	return o.relayPresent || o.Relay != nil
}

// Operation represents one media pipeline operation. Params are validated against an exact
// operation-specific allowlist, including fixed H.264 encoding and overlay choices, before
// the media configuration is built.
type Operation struct {
	Op     string                 `json:"op"`
	Params map[string]interface{} `json:"params,omitempty"`
}
