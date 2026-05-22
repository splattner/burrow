package tunnel

import (
	"errors"
	"sync"
	"time"
)

var ErrStreamNotFound = errors.New("stream not found")
var ErrStreamExists = errors.New("stream already exists")
var ErrStreamClosed = errors.New("stream closed")
var ErrStreamBackpressure = errors.New("stream backpressure: inbound queue full")

const (
	defaultStreamBufferSize = 16
	defaultDeliveryTimeout  = 100 * time.Millisecond
)

type MultiplexerConfig struct {
	StreamBufferSize int
	DeliveryTimeout  time.Duration
}

type Stream struct {
	ID              uint64
	In              chan []byte
	closed          chan struct{}
	once            sync.Once
	deliveryTimeout time.Duration
}

func newStream(id uint64, bufferSize int, deliveryTimeout time.Duration) *Stream {
	if bufferSize <= 0 {
		bufferSize = defaultStreamBufferSize
	}
	if deliveryTimeout <= 0 {
		deliveryTimeout = defaultDeliveryTimeout
	}

	return &Stream{
		ID:              id,
		In:              make(chan []byte, bufferSize),
		closed:          make(chan struct{}),
		deliveryTimeout: deliveryTimeout,
	}
}

func (s *Stream) Push(payload []byte) error {
	dup := make([]byte, len(payload))
	copy(dup, payload)

	timer := time.NewTimer(s.deliveryTimeout)
	defer timer.Stop()

	select {
	case <-s.closed:
		return ErrStreamClosed
	case s.In <- dup:
		return nil
	case <-timer.C:
		return ErrStreamBackpressure
	}
}

func (s *Stream) Close() {
	s.once.Do(func() {
		close(s.closed)
		close(s.In)
	})
}

type Multiplexer struct {
	mu      sync.RWMutex
	streams map[uint64]*Stream
	config  MultiplexerConfig
}

func NewMultiplexer() *Multiplexer {
	return NewMultiplexerWithConfig(MultiplexerConfig{})
}

func NewMultiplexerWithConfig(cfg MultiplexerConfig) *Multiplexer {
	if cfg.StreamBufferSize <= 0 {
		cfg.StreamBufferSize = defaultStreamBufferSize
	}
	if cfg.DeliveryTimeout <= 0 {
		cfg.DeliveryTimeout = defaultDeliveryTimeout
	}

	return &Multiplexer{
		streams: make(map[uint64]*Stream),
		config:  cfg,
	}
}

func (m *Multiplexer) Open(id uint64) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.streams[id]; exists {
		return nil, ErrStreamExists
	}
	s := newStream(id, m.config.StreamBufferSize, m.config.DeliveryTimeout)
	m.streams[id] = s
	return s, nil
}

func (m *Multiplexer) Get(id uint64) (*Stream, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.streams[id]
	if !ok {
		return nil, ErrStreamNotFound
	}
	return s, nil
}

func (m *Multiplexer) Deliver(id uint64, payload []byte) error {
	m.mu.RLock()
	s, ok := m.streams[id]
	m.mu.RUnlock()
	if !ok {
		return ErrStreamNotFound
	}
	return s.Push(payload)
}

func (m *Multiplexer) Close(id uint64) bool {
	m.mu.Lock()
	s, ok := m.streams[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.streams, id)
	m.mu.Unlock()
	s.Close()
	return true
}

func (m *Multiplexer) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}

func (m *Multiplexer) CloseAll() {
	m.mu.Lock()
	toClose := make([]*Stream, 0, len(m.streams))
	for id, s := range m.streams {
		delete(m.streams, id)
		toClose = append(toClose, s)
	}
	m.mu.Unlock()

	for _, s := range toClose {
		s.Close()
	}
}
