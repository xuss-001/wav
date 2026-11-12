package wav

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-audio/riff"
)

var (
	// ErrInvalidWhence is returned when SeekFrame is called with an unsupported whence.
	ErrInvalidWhence = errors.New("wav: invalid whence for SeekFrame")
	// ErrFrameOutOfRange is returned when SeekFrame targets a frame outside [0, totalFrames].
	ErrFrameOutOfRange = errors.New("wav: frame position out of range")
	// ErrMisalignedFrame is returned by SeekFrame(io.SeekCurrent) when the current
	// read position is not aligned on an audio frame boundary.
	ErrMisalignedFrame = errors.New("wav: current read position is not frame aligned")
	// ErrUnsupportedSeekFormat is returned when the content is not integer PCM
	// supported by SeekFrame.
	ErrUnsupportedSeekFormat = errors.New("wav: SeekFrame only supports 8/16/24/32 bit integer PCM (format 1)")
)

// pcmReader is a limited reader over the declared PCM payload of the data
// chunk. RIFF alignment padding and any chunk following data are never exposed
// through it, so they cannot be decoded as sample values.
//
// Unlike io.LimitReader, its remaining window can be reconfigured after the
// underlying ReadSeeker is moved by SeekFrame.
type pcmReader struct {
	d         *Decoder
	remaining int64
}

// Read implements io.Reader.
func (r *pcmReader) Read(p []byte) (int, error) {
	if r == nil || r.d == nil || r.d.r == nil {
		return 0, errors.New("wav: invalid PCM reader")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	max := int64(len(p))
	if max > r.remaining {
		max = r.remaining
	}
	n, err := r.d.r.Read(p[:max])
	r.remaining -= int64(n)
	r.d.pcmPos += int64(n)
	return n, err
}

// SeekFrame moves the PCM read cursor to the given audio frame position. An
// audio frame contains one sample per channel, so a frame is
// NumChannels * (BitDepth/8) bytes long while a sample is a single channel
// value.
//
// offset and the returned position are both expressed in audio frames and are
// relative to the first valid PCM byte of the data chunk. The supported whence
// values are io.SeekStart, io.SeekCurrent and io.SeekEnd (the SeekEnd origin is
// the total number of frames). Valid positions are in [0, totalFrames], so it
// is possible to seek exactly to the end.
//
// The first call parses the RIFF/WAVE header and locates the data chunk
// without assuming a fixed header size: LIST, JUNK and unknown chunks between
// fmt and data are skipped, including their odd-size alignment padding. The
// underlying reader is repositioned directly; the PCM prefix is never decoded
// and discarded.
//
// Only uncompressed integer PCM (format tag 1) at 8, 16, 24 or 32 bits per
// sample is supported. Invalid files, missing data chunks, payloads that do
// not contain an integer number of frames and invalid seek requests return an
// error without panicking. After a successful seek, PCMBuffer and
// FullPCMBuffer read from the target frame; Rewind always returns to the first
// frame.
//
// With io.SeekCurrent, the seek fails if the current byte position is in the
// middle of a frame (for example after filling a non frame aligned buffer).
// io.SeekStart and io.SeekEnd always re-align the cursor. Invalid requests do
// not mutate the read position or the remaining payload range.
func (d *Decoder) SeekFrame(offset int64, whence int) (int64, error) {
	if d == nil {
		return 0, errors.New("wav: SeekFrame called on a nil decoder")
	}
	if whence != io.SeekStart && whence != io.SeekCurrent && whence != io.SeekEnd {
		return 0, ErrInvalidWhence
	}
	if !d.pcmDataAccessed {
		if err := d.FwdToPCM(); err != nil {
			return 0, err
		}
	}
	if d.PCMChunk == nil || d.pcmData == nil {
		return 0, ErrPCMChunkNotFound
	}

	frameBytes, totalFrames, err := d.frameLayout()
	if err != nil {
		return 0, err
	}

	// Compute the target before touching the underlying reader so an invalid
	// request leaves the decoder state untouched.
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		pos := d.pcmPos - d.dataOffset
		if pos%frameBytes != 0 {
			return 0, ErrMisalignedFrame
		}
		if offset > 0 && pos > (1<<63)-1-offset {
			return 0, ErrFrameOutOfRange
		}
		if offset < 0 && pos < -offset {
			return 0, ErrFrameOutOfRange
		}
		target = pos/frameBytes + offset
	case io.SeekEnd:
		target = totalFrames + offset
	}
	if target < 0 || target > totalFrames {
		return 0, ErrFrameOutOfRange
	}
	if target > (1<<63-1)/frameBytes {
		return 0, ErrFrameOutOfRange
	}
	byteOffset := d.dataOffset + target*frameBytes
	if _, err := d.r.Seek(byteOffset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("wav: failed to seek to frame %d: %w", target, err)
	}
	d.pcmPos = byteOffset
	d.pcmData.remaining = d.dataSize - (byteOffset - d.dataOffset)
	d.PCMChunk.Pos = int(byteOffset - d.dataOffset)
	d.err = nil
	return target, nil
}

// frameLayout validates the container and PCM layout and returns the number of
// bytes per frame and the total number of complete audio frames.
func (d *Decoder) frameLayout() (frameBytes, totalFrames int64, err error) {
	if d.parser == nil || d.parser.Format != riff.WavFormatID {
		err = ErrUnsupportedSeekFormat
		return
	}
	if d.WavAudioFormat != 1 {
		err = fmt.Errorf("%w: format tag %d", ErrUnsupportedSeekFormat, d.WavAudioFormat)
		return
	}
	switch d.BitDepth {
	case 8, 16, 24, 32:
	default:
		err = fmt.Errorf("%w: %d bits per sample", ErrUnsupportedSeekFormat, d.BitDepth)
		return
	}
	if d.NumChans == 0 {
		err = errors.New("wav: invalid number of channels")
		return
	}
	if d.SampleRate == 0 {
		err = errors.New("wav: invalid sample rate")
		return
	}
	bytesPerSample := int64(d.BitDepth) / 8
	frameBytes = int64(d.NumChans) * bytesPerSample
	if frameBytes <= 0 {
		err = fmt.Errorf("wav: invalid frame layout: %d channels x %d bytes/sample, %d PCM bytes", d.NumChans, bytesPerSample, d.dataSize)
		return
	}
	if d.dataSize < 0 || d.dataSize%frameBytes != 0 {
		err = fmt.Errorf("wav: PCM payload of %d bytes does not contain an integer number of %d-byte frames", d.dataSize, frameBytes)
		return
	}
	// The fmt chunk values must be self consistent.
	expectedBlockAlign := uint16(frameBytes)
	if d.parser.BlockAlign != 0 && d.parser.BlockAlign != expectedBlockAlign {
		err = fmt.Errorf("wav: inconsistent block align: fmt says %d, format says %d", d.parser.BlockAlign, expectedBlockAlign)
		return
	}
	if d.AvgBytesPerSec != 0 && d.AvgBytesPerSec != uint32(int64(d.SampleRate)*frameBytes) {
		err = fmt.Errorf("wav: inconsistent average bytes per second: fmt says %d, format says %d", d.AvgBytesPerSec, uint32(int64(d.SampleRate)*frameBytes))
		return
	}
	totalFrames = d.dataSize / frameBytes
	return frameBytes, totalFrames, nil
}
