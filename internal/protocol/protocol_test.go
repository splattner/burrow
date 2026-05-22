package protocol

import (
	"errors"
	"testing"
)

func TestEncodeDecodeControlWireFrame(t *testing.T) {
	encoded, err := EncodeControlFrame(42, ControlFrame{
		Type:     FrameOpen,
		StreamID: 42,
		ClientID: "client-a",
		Target:   "127.0.0.1:5432",
	})
	if err != nil {
		t.Fatalf("encode control frame: %v", err)
	}

	wf, err := DecodeWireFrame(encoded)
	if err != nil {
		t.Fatalf("decode wire frame: %v", err)
	}
	if wf.Kind != KindControl {
		t.Fatalf("expected control kind, got %v", wf.Kind)
	}
	if wf.StreamID != 42 {
		t.Fatalf("expected stream ID 42, got %d", wf.StreamID)
	}

	cf, err := DecodeControlFromWire(wf)
	if err != nil {
		t.Fatalf("decode control payload: %v", err)
	}
	if cf.Type != FrameOpen || cf.ClientID != "client-a" || cf.Target != "127.0.0.1:5432" {
		t.Fatalf("unexpected control payload: %#v", cf)
	}
}

func TestEncodeDecodeDataWireFrame(t *testing.T) {
	payload := []byte("hello tunnel")
	encoded, err := EncodeDataFrame(9, payload)
	if err != nil {
		t.Fatalf("encode data frame: %v", err)
	}

	wf, err := DecodeWireFrame(encoded)
	if err != nil {
		t.Fatalf("decode wire frame: %v", err)
	}
	if wf.Kind != KindData {
		t.Fatalf("expected data kind, got %v", wf.Kind)
	}
	if wf.StreamID != 9 {
		t.Fatalf("expected stream ID 9, got %d", wf.StreamID)
	}
	if string(wf.Payload) != "hello tunnel" {
		t.Fatalf("unexpected payload: %q", string(wf.Payload))
	}

	payload[0] = 'X'
	if string(wf.Payload) != "hello tunnel" {
		t.Fatalf("expected payload copy isolation")
	}
}

func TestDecodeWireFrameRejectsLengthMismatch(t *testing.T) {
	encoded, err := EncodeDataFrame(1, []byte("abc"))
	if err != nil {
		t.Fatalf("encode data frame: %v", err)
	}
	truncated := encoded[:len(encoded)-1]

	_, err = DecodeWireFrame(truncated)
	if !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("expected ErrLengthMismatch, got %v", err)
	}
}
