package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	Version1   byte = 1
	headerSize      = 14
)

var (
	ErrFrameTooShort      = errors.New("frame too short")
	ErrUnsupportedVersion = errors.New("unsupported frame version")
	ErrInvalidFrameKind   = errors.New("invalid frame kind")
	ErrLengthMismatch     = errors.New("frame payload length mismatch")
)

type FrameType string

type FrameKind byte

const (
	FrameRegister     FrameType = "register"
	FrameRegisterAck  FrameType = "register_ack"
	FrameHeartbeat    FrameType = "heartbeat"
	FrameHeartbeatAck FrameType = "heartbeat_ack"
	FrameOpen         FrameType = "open"
	FrameOpenAck      FrameType = "open_ack"
	FrameClose        FrameType = "close"
	FrameError        FrameType = "error"
	FrameData         FrameType = "data"

	KindControl FrameKind = 1
	KindData    FrameKind = 2
)

type ControlFrame struct {
	Type              FrameType       `json:"type"`
	StreamID          uint64          `json:"stream_id,omitempty"`
	ClientID          string          `json:"client_id,omitempty"`
	Target            string          `json:"target,omitempty"`
	Message           string          `json:"message,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	PreviousSessionID string          `json:"previous_session_id,omitempty"`
	Meta              json.RawMessage `json:"meta,omitempty"`
}

type DataFrame struct {
	StreamID uint64
	Payload  []byte
}

type WireFrame struct {
	Version  byte
	Kind     FrameKind
	StreamID uint64
	Payload  []byte
}

func EncodeControl(f ControlFrame) ([]byte, error) {
	return json.Marshal(f)
}

func DecodeControl(b []byte) (ControlFrame, error) {
	var f ControlFrame
	err := json.Unmarshal(b, &f)
	return f, err
}

func EncodeWireFrame(frame WireFrame) ([]byte, error) {
	if frame.Kind != KindControl && frame.Kind != KindData {
		return nil, ErrInvalidFrameKind
	}

	buf := make([]byte, headerSize+len(frame.Payload))
	buf[0] = Version1
	buf[1] = byte(frame.Kind)
	binary.BigEndian.PutUint64(buf[2:10], frame.StreamID)
	binary.BigEndian.PutUint32(buf[10:14], uint32(len(frame.Payload)))
	copy(buf[14:], frame.Payload)
	return buf, nil
}

func EncodeControlFrame(streamID uint64, frame ControlFrame) ([]byte, error) {
	payload, err := EncodeControl(frame)
	if err != nil {
		return nil, fmt.Errorf("encode control payload: %w", err)
	}

	return EncodeWireFrame(WireFrame{
		Kind:     KindControl,
		StreamID: streamID,
		Payload:  payload,
	})
}

func EncodeDataFrame(streamID uint64, payload []byte) ([]byte, error) {
	dup := make([]byte, len(payload))
	copy(dup, payload)

	return EncodeWireFrame(WireFrame{
		Kind:     KindData,
		StreamID: streamID,
		Payload:  dup,
	})
}

func DecodeWireFrame(b []byte) (WireFrame, error) {
	if len(b) < headerSize {
		return WireFrame{}, ErrFrameTooShort
	}
	if b[0] != Version1 {
		return WireFrame{}, ErrUnsupportedVersion
	}

	kind := FrameKind(b[1])
	if kind != KindControl && kind != KindData {
		return WireFrame{}, ErrInvalidFrameKind
	}

	streamID := binary.BigEndian.Uint64(b[2:10])
	payloadLen := binary.BigEndian.Uint32(b[10:14])
	if len(b) != headerSize+int(payloadLen) {
		return WireFrame{}, ErrLengthMismatch
	}

	payload := make([]byte, payloadLen)
	copy(payload, b[headerSize:])

	return WireFrame{
		Version:  b[0],
		Kind:     kind,
		StreamID: streamID,
		Payload:  payload,
	}, nil
}

func DecodeControlFromWire(wf WireFrame) (ControlFrame, error) {
	if wf.Kind != KindControl {
		return ControlFrame{}, ErrInvalidFrameKind
	}
	return DecodeControl(wf.Payload)
}
