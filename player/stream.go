package player

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/lbryio/lbrytv-player/internal/metrics"
	"github.com/lbryio/lbrytv-player/pkg/mime"

	ljsonrpc "github.com/lbryio/lbry.go/v2/extras/jsonrpc"
	"github.com/lbryio/lbry.go/v2/stream"
	pb "github.com/lbryio/types/v2/go"
)

// Stream provides an io.ReadSeeker interface to a stream of blobs to be used by standard http library for range requests,
// as well as some stream metadata.
type Stream struct {
	URI              string
	Size             uint64
	ContentType      string
	hash             string
	prefetchedChunks map[int]bool

	player         *Player
	claim          *ljsonrpc.Claim
	source         *pb.Source
	resolvedStream *pb.Stream
	sdBlob         *stream.SDBlob
	seekOffset     int64
}

func NewStream(p *Player, uri string, claim *ljsonrpc.Claim) *Stream {
	stream := claim.Value.GetStream()
	source := stream.GetSource()
	return &Stream{
		URI:         uri,
		ContentType: patchMediaType(source.MediaType),
		Size:        source.GetSize(),

		player:           p,
		claim:            claim,
		source:           source,
		resolvedStream:   stream,
		hash:             hex.EncodeToString(source.SdHash),
		prefetchedChunks: make(map[int]bool, 50),
	}
}

// Filename detects name of the original file, suitable for saving under on the filesystem.
func (s *Stream) Filename() string {
	name := s.source.GetName()
	if name != "" {
		return name
	}
	name = s.claim.NormalizedName
	// exts, err := mime.ExtensionsByType(s.ContentType)
	ext := mime.GetExtensionByType(s.ContentType)
	if ext == "" {
		return name
	}
	return fmt.Sprintf("%v.%v", name, ext)
}

// PrepareForReading downloads stream description from the reflector and tries to determine stream size
// using several methods, including legacy ones for streams that do not have metadata.
func (s *Stream) PrepareForReading() error {
	sdBlob, err := s.player.blobSource.GetSDBlob(s.hash)
	if err != nil {
		return err
	}

	s.sdBlob = sdBlob

	s.setSize()

	return nil
}

func (s *Stream) setSize() {
	if s.Size > 0 {
		return
	}

	if s.source.GetSize() > 0 {
		s.Size = s.source.GetSize()
		return
	}

	size, err := s.getStreamSizeFromLastBlobSize()
	if err == nil {
		s.Size = size
		return
	}

	Logger.Infof("couldn't figure out stream %v size from last chunk: %v", s.URI, err)
	for _, blob := range s.sdBlob.BlobInfos {
		if blob.Length == stream.MaxBlobSize {
			size += MaxChunkSize
		} else {
			size += uint64(blob.Length - 1)
		}
	}
	// last padding is unguessable
	size -= 16

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

	if n == 0 && err == nil {
		err := errors.New("read 0 bytes triggering an endless loop, exiting stream")
		Logger.Errorf("failed to read from stream %v at offset %v: %v", s.URI, s.seekOffset, err)
	}

	return n, err
}

func (s *Stream) readFromChunks(sr streamRange, dest []byte) (int, error) {
	var read int
	for i := 0; i < 2; i++ {
		index, read, err := s.attemptReadFromChunks(sr, dest)
		if err != nil {
			return read, err
		}
		if read > 0 {
			break
		}
		// Dirty data likely - delete from cache and retry
		err = s.RemoveChunk(int(index))
		if err != nil {
			return read, err
		}
		Logger.Warnf("Read 0 bytes for %s at blob index %d/%d at offset %d", s.URI, int(index), len(s.sdBlob.BlobInfos),
			s.seekOffset)
	}

	return read, nil
}

func (s *Stream) attemptReadFromChunks(sr streamRange, dest []byte) (i int64, read int, err error) {
	i = sr.FirstChunkIdx
	for i < sr.LastChunkIdx+1 {
		offset, readLen := sr.ByteRangeForChunk(i)

		b, err := s.GetChunk(int(i))
		if err != nil {
			return i, read, err
		}

		n, err := b.Read(offset, readLen, dest[read:])
		read += n
		if err != nil {
			return i, read, err
		}

		i++
	}
	return i, read, nil
}

func (s *Stream) RemoveChunk(chunkIdx int) error {
	if chunkIdx > len(s.sdBlob.BlobInfos) {
		return errors.New("blob index out of bounds")
	}

	bi := s.sdBlob.BlobInfos[chunkIdx]
	hash := hex.EncodeToString(bi.BlobHash)
	return s.player.blobSource.clearChuckFromCache(hash)
}

// GetChunk returns the nth ReadableChunk of the stream.
func (s *Stream) GetChunk(chunkIdx int) (ReadableChunk, error) {
	if chunkIdx > len(s.sdBlob.BlobInfos) {
		return nil, errors.New("blob index out of bounds")
	}

	bi := s.sdBlob.BlobInfos[chunkIdx]
	hash := hex.EncodeToString(bi.BlobHash)

	chunk, err := s.player.blobSource.GetChunk(hash, s.sdBlob.Key, bi.IV)
	if err != nil || chunk == nil {
		return nil, err
	}

	chunkToPrefetch := chunkIdx + 1
	prefetched := s.prefetchedChunks[chunkToPrefetch]
	if s.player.prefetch && !prefetched {
		s.prefetchedChunks[chunkToPrefetch] = true
		go s.prefetchChunk(chunkToPrefetch)
	}
	return chunk, nil
}

func (s *Stream) prefetchChunk(chunkIdx int) {
	prefetchLen := DefaultPrefetchLen
	chunksLeft := len(s.sdBlob.BlobInfos) - chunkIdx - 1 // Last blob is empty
	if chunksLeft < DefaultPrefetchLen {
		prefetchLen = chunksLeft
	}
	if prefetchLen <= 0 {
		return
	}

	Logger.Debugf("prefetching %v chunks to local cache", prefetchLen)
	for _, bi := range s.sdBlob.BlobInfos[chunkIdx : chunkIdx+prefetchLen] {
		hash := hex.EncodeToString(bi.BlobHash)

		if s.player.blobSource.IsCached(hash) {
			Logger.Debugf("chunk %v found in cache, not prefetching", hash)
			continue
		}

		Logger.Debugf("prefetching chunk %v", hash)
		_, err := s.player.blobSource.GetChunk(hash, s.sdBlob.Key, bi.IV)
		if err != nil {
			Logger.Errorf("failed to prefetch chunk %v: %v", hash, err)
			return
		}
	}
}

// getStreamSizeFromLastChunkSize gets the exact size of a stream from the sd blob and the last chunk
func (s *Stream) getStreamSizeFromLastBlobSize() (uint64, error) {
	if s.claim.Value.GetStream() == nil {
		return 0, errors.New("claim is not a stream")
	}

	numChunks := len(s.sdBlob.BlobInfos) - 1
	if numChunks <= 0 {
		return 0, nil
	}

	lastBlobInfo := s.sdBlob.BlobInfos[numChunks-1]

	lastChunk, err := s.player.blobSource.GetChunk(hex.EncodeToString(lastBlobInfo.BlobHash), s.sdBlob.Key, lastBlobInfo.IV)
	if err != nil {
		return 0, err
	}

	return uint64(MaxChunkSize)*uint64(numChunks-1) + uint64(len(lastChunk)), nil
}
