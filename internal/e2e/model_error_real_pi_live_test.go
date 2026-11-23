//go:build e2e && live

package e2e

import "testing"

func TestT242RealPiErrorUIIsRecognized(t *testing.T) {
	t242RealPiErrorUI(t, "stream error: stream disconnected before completion: stream closed before response.completed", "model_stream_error")
}
