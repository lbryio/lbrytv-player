package player

import (
	"encoding/hex"
	"errors"
	"io"
	"math"
	"time"

	"github.com/lbryio/lbrytv-player/internal/metrics"

	ljsonrpc "github.com/lbryio/lbry.go/v2/extras/jsonrpc"
	"github.com/lbryio/lbry.go/v2/stream"
	pb "github.com/lbryio/types/v2/go"
)

// Stream provides an io.ReadSeeker interface to a stream of blobs to be used by standard http library for range requests,
// as well as some stream metadata.
type Stream struct {
	URI         string
	Size        uint64
	ContentType string

	chunkGetter chunkGetter

	player         *Player
	claim          *ljsonrpc.Claim
	resolvedStream *pb.Stream
	hash           string
	sdBlob         *stream.SDBlob
	seekOffset     int64
}

func NewStream(p *Player, uri string, claim *ljsonrpc.Claim) *Stream {
	stream := claim.Value.GetStream()
	return &Stream{
		URI:         uri,
		ContentType: patchMediaType(stream.Source.MediaType),
		Size:        stream.GetSource().GetSize(),

		player:         p,
		claim:          claim,
		resolvedStream: stream,
		hash:           hex.EncodeToString(stream.Source.SdHash),
	}
}

func (s *Stream) Filename() string {
	filename := s.claim.Value.GetStream().GetSource().GetName()
	if filename == "" {
		return "video.mp4"
	}
	return filename
}

// PrepareForReading downloads stream description from the reflector and tries to determine stream size
// using several methods, including legacy ones for streams that do not have metadata.
func (s *Stream) PrepareForReading() error {
	sdBlob, err := s.player.hotCache.GetSDBlob(s.hash)
	if err != nil {
		return err
	}

	s.sdBlob = &sdBlob

	s.setSize(sdBlob.BlobInfos)

	s.chunkGetter = chunkGetter{
		hotCache: s.player.hotCache,
		sdBlob:   &sdBlob,
		prefetch: s.player.enablePrefetch,
	}

	return nil

}

func (s *Stream) setSize(blobs []stream.BlobInfo) {
	if s.Size > 0 {
		return
	}

	if s.claim.Value.GetStream().GetSource().GetSize() > 0 {
		s.Size = s.claim.Value.GetStream().GetSource().GetSize()
	}

	size, err := s.claim.GetStreamSizeByMagic()

	if err != nil {
		Logger.Infof("couldn't figure out stream %v size by magic: %v", s.URI, err)
		for _, blob := range blobs {
			if blob.Length == stream.MaxBlobSize {
				size += MaxChunkSize
			} else {
				size += uint64(blob.Length - 1)
			}
		}
		// last padding is unguessable
		size -= 16
	}

	s.Size = size
}

// Timestamp returns stream creation timestamp, used in HTTP response header.
func (s *Stream) Timestamp() time.Time {
	return time.Unix(int64(s.claim.Timestamp), 0)
}

// Seek implements io.ReadSeeker interface and is meant to be called by http.ServeContent.
func (s *Stream) Seek(offset int64, whence int) (int64, error) {
	var newOffset int64

	if s.Size == 0 {
		return 0, errStreamSizeZero
	} else if uint64(math.Abs(float64(offset))) > s.Size {
		return 0, errOutOfBounds
	}

	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = s.seekOffset + offset
	case io.SeekEnd:
		newOffset = int64(s.Size) - offset
	default:
		return 0, errors.New("invalid seek whence argument")
	}

	if newOffset < 0 {
		return 0, errSeekingBeforeStart
	}

	s.seekOffset = newOffset
	return newOffset, nil
}

// Read implements io.ReadSeeker interface and is meant to be called by http.ServeContent.
// Actual chunk retrieval and delivery happens in s.readFromChunks().
func (s *Stream) Read(dest []byte) (n int, err error) {
	n, err = s.readFromChunks(getRange(s.seekOffset, len(dest)), dest)
	s.seekOffset += int64(n)

	metrics.OutBytes.Add(float64(n))

	if err != nil {
		Logger.Errorf("failed to read from stream %v at offset %v: %v", s.URI, s.seekOffset, err)
	}

	return n, err
}

func (s *Stream) readFromChunks(sr streamRange, dest []byte) (int, error) {
	var read int

	for i := sr.FirstChunkIdx; i < sr.LastChunkIdx+1; i++ {
		offset, readLen := sr.ByteRangeForChunk(i)

		b, err := s.chunkGetter.Get(int(i))
		if err != nil {
			return read, err
		}

		n, err := b.Read(offset, readLen, dest[read:])
		read += n
		if err != nil {
			return read, err
		}
	}

	return read, nil
}