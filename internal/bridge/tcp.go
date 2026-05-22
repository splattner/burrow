package bridge

import "io"

type TCPBridge struct{}

func NewTCPBridge() *TCPBridge {
	return &TCPBridge{}
}

func (b *TCPBridge) Relay(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}
