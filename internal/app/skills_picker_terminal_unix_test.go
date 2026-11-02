//go:build !windows

package app

import (
	"os"
	"testing"
	"time"
)

func TestWaitForPickerByteTimesOutAndDetectsInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	ready, err := waitForPickerByte(reader.Fd(), 20*time.Millisecond)
	if err != nil || ready {
		t.Fatalf("wait without input = %v, %v; want timeout", ready, err)
	}
	if _, err := writer.Write([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}
	ready, err = waitForPickerByte(reader.Fd(), 20*time.Millisecond)
	if err != nil || !ready {
		t.Fatalf("wait with input = %v, %v; want ready", ready, err)
	}
}
