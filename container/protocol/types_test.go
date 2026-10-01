package protocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOutputConfigTracksRelayPresence(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		hasRelay   bool
		relayIsNil bool
	}{
		{name: "omitted", body: `{"mode":"websocket"}`, relayIsNil: true},
		{name: "null", body: `{"mode":"websocket","relay":null}`, hasRelay: true, relayIsNil: true},
		{name: "configured", body: `{"mode":"websocket","relay":{"url":"wss://relay","token":"secret"}}`, hasRelay: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output OutputConfig
			require.NoError(t, json.Unmarshal([]byte(tt.body), &output))
			require.Equal(t, tt.hasRelay, output.HasRelay())
			require.Equal(t, tt.relayIsNil, output.Relay == nil)
		})
	}
}

func TestOutputConfigRejectsUnknownFields(t *testing.T) {
	var output OutputConfig
	require.ErrorContains(t, json.Unmarshal([]byte(`{"mode":"websocket","unknown":true}`), &output), "unknown field")
	require.ErrorContains(t, json.Unmarshal([]byte(
		`{"mode":"rtmp","destination":"rtmps://live.cloudflare.com:443/live/secret"}`,
	), &output), "unknown field")
	require.ErrorContains(t, json.Unmarshal([]byte(
		`{"mode":"websocket","relay":{"url":"wss://relay","token":"secret","unknown":true}}`,
	), &output), "unknown field")
}

func TestOutputConfigAcceptsRTMPKey(t *testing.T) {
	var output OutputConfig
	require.NoError(t, json.Unmarshal([]byte(`{"mode":"rtmp","key":"stream-key"}`), &output))
	require.Equal(t, "stream-key", output.Key)
}

func TestRelayConfigRejectsAccess(t *testing.T) {
	tests := []string{
		`{"url":"wss://relay","token":"secret","access":null}`,
		`{"url":"wss://relay","token":"secret","access":{}}`,
	}

	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			var relay RelayConfig
			require.ErrorContains(t, json.Unmarshal([]byte(body), &relay), `unknown field "access"`)
		})
	}
}
