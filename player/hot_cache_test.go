package player

import (
	"testing"

	"github.com/lbryio/lbry.go/v2/extras/errors"
	"github.com/lbryio/lbry.go/v2/stream"
	"github.com/lbryio/reflector.go/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHotCache_BlobNotFound(t *testing.T) {
	origin := store.NewMemStore()
	hc := NewHotCache(origin, 1000, 1000)
	assert.NotNil(t, hc)

	_, err := hc.GetSDBlob("test")
	assert.True(t, errors.Is(err, store.ErrBlobNotFound))
}

func TestHotCache_Stream(t *testing.T) {
	origin := store.NewMemStore()

	data := randomString(MaxChunkSize * 3)
	s, err := stream.New([]byte(data))
	require.NoError(t, err)
	require.Equal(t, 4, len(s)) // make sure we got an sd blob plus 3 content blobs

	for _, b := range s {
		origin.Put(b.HashHex(), b)
	}

	hc := NewHotCache(origin, 1000, 1000)

	var streamSDBlob stream.SDBlob
	err = streamSDBlob.FromBlob(s[0])
	require.NoError(t, err)

	storedSDBlob, err := hc.GetSDBlob(s[0].HashHex())
	require.NoError(t, err)
	assert.EqualValues(t, streamSDBlob, storedSDBlob)

	// check the first chunk matches the stream data
	chunkIdx := 0
	chunk, err := hc.GetChunk(s[chunkIdx+1].HashHex(), streamSDBlob.Key, streamSDBlob.BlobInfos[chunkIdx].IV)
	require.NoError(t, err)
	assert.EqualValues(t, data[:20], chunk[:20])
}