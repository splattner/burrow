package bridge

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelay_CopiesBytes(t *testing.T) {
	b := NewTCPBridge()
	src := strings.NewReader("hello world")
	var dst bytes.Buffer

	n, err := b.Relay(&dst, src)
	require.NoError(t, err)
	assert.Equal(t, int64(11), n)
	assert.Equal(t, "hello world", dst.String())
}

func TestRelay_EmptyInput(t *testing.T) {
	b := NewTCPBridge()
	src := strings.NewReader("")
	var dst bytes.Buffer

	n, err := b.Relay(&dst, src)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	assert.Empty(t, dst.String())
}

func TestRelay_LargePayload(t *testing.T) {
	b := NewTCPBridge()
	payload := bytes.Repeat([]byte("x"), 1<<20) // 1 MiB
	var dst bytes.Buffer

	n, err := b.Relay(&dst, bytes.NewReader(payload))
	require.NoError(t, err)
	assert.Equal(t, int64(1<<20), n)
	assert.Equal(t, payload, dst.Bytes())
}
