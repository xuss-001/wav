package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

var (
	// CIDList is the chunk ID for a LIST chunk
	CIDList = [4]byte{'L', 'I', 'S', 'T'}
	// CIDSmpl is the chunk ID for a smpl chunk
	CIDSmpl = [4]byte{'s', 'm', 'p', 'l'}
	// CIDINFO is the chunk ID for an INFO chunk
	CIDInfo = []byte{'I', 'N', 'F', 'O'}
	// CIDCue is the chunk ID for the cue chunk
	CIDCue = [4]byte{'c', 'u', 'e', 0x20}
)

// Decoder handles the decoding of wav files.
type Decoder struct {
	r      io.ReadSeeker
	parser *riff.Parser

	NumChans   uint16
	BitDepth   uint16
	SampleRate uint32

	AvgBytesPerSec uint32
	WavAudioFormat uint16

	err error
	// PCMSize is the declared size of the first "data" chunk, without its
	// RIFF alignment padding byte.
	PCMSize         int
	pcmDataAccessed bool
	// pcmChunk is available so we can use the LimitReader
	PCMChunk *riff.Chunk
	// pcmDataOffset is the absolute byte offset of the first "data" chunk
	// payload in the underlying reader.
	pcmDataOffset int64
	// pcmPos is the current read offset within the declared PCM payload.
	pcmPos int64
	// pcmReader only exposes the declared PCM payload bytes.
	pcmReader io.Reader
	// bytesPerFrame is NumChans * bytesPerSample and is always > 0 once
	// the PCM data was located.
	bytesPerFrame int
	// bytesPerSample is the size of one sample value once the PCM data was
	// located.
	bytesPerSample int
	// totalFrames is the number of complete frames declared in the payload.
	totalFrames int64
	// Metadata for the current file
	Metadata *Metadata
}

// NewDecoder creates a decoder for the passed wav reader.
// Note that the reader doesn't get rewinded as the container is processed.
func NewDecoder(r io.ReadSeeker) *Decoder {
	return &Decoder{
		r:      r,
		parser: riff.New(r),
	}
}

// Seek provides access to the cursor position in the PCM data
func (d *Decoder) Seek(offset int64, whence int) (int64, error) {
	return d.r.Seek(offset, whence)
}

// Rewind allows the decoder to be rewound to the beginning of the PCM data.
// This is useful if you want to keep on decoding the same file in a loop.
func (d *Decoder) Rewind() error {
	if d == nil {
		return fmt.Errorf("can't rewind a nil decoder")
	}
	d.err = nil
	if err := d.ensurePCM(); err != nil {
		return fmt.Errorf("failed to locate the PCM data: %w", err)
	}
	if _, err := d.SeekFrame(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek back to the first frame: %w", err)
	}
	return nil
}

// SampleBitDepth returns the bit depth encoding of each sample.
func (d *Decoder) SampleBitDepth() int32 {
	if d == nil {
		return 0
	}
	return int32(d.BitDepth)
}

// PCMLen returns the total number of bytes in the PCM data chunk
func (d *Decoder) PCMLen() int64 {
	if d == nil {
		return 0
	}
	if d.pcmDataAccessed {
		return d.pcmPayloadSize()
	}
	return int64(d.PCMSize)
}

// Err returns the first non-EOF error that was encountered by the Decoder.
func (d *Decoder) Err() error {
	if errors.Is(d.err, io.EOF) {
		return nil
	}
	return d.err
}

// EOF returns positively if the underlying reader reached the end of file.
func (d *Decoder) EOF() bool {
	if d == nil || errors.Is(d.err, io.EOF) {
		return true
	}
	return false
}

// IsValidFile verifies that the file is valid/readable.
func (d *Decoder) IsValidFile() bool {
	d.err = d.readHeaders()
	if d.err != nil {
		return false
	}
	if d.NumChans < 1 {
		return false
	}
	if d.BitDepth < 8 {
		return false
	}
	if d, err := d.Duration(); err != nil || d <= 0 {
		return false
	}

	return true
}

// ReadInfo reads the underlying reader until the comm header is parsed.
// This method is safe to call multiple times.
func (d *Decoder) ReadInfo() {
	d.err = d.readHeaders()
}

// ReadMetadata parses the file for extra metadata such as the INFO list chunk.
// The entire file will be read and should be rewinded if more data must be
// accessed.
func (d *Decoder) ReadMetadata() {
	if d.Metadata != nil {
		return
	}
	d.ReadInfo()
	if d.Err() != nil || d.Metadata != nil {
		return
	}
	var (
		chunk *riff.Chunk
		err   error
	)
	for err == nil {
		chunk, err = d.parser.NextChunk()
		if err != nil {
			break
		}

		switch chunk.ID {
		case CIDList:
			if err = DecodeListChunk(d, chunk); err != nil {
				if !errors.Is(err, io.EOF) {
					d.err = err
				}
			}
			if d.Metadata != nil && d.Metadata.SamplerInfo != nil {
				// we got everything we were looking for
				break
			}
		case CIDSmpl:
			if err = DecodeSamplerChunk(d, chunk); err != nil {
				if !errors.Is(err, io.EOF) {
					d.err = err
				}
			}
		case CIDCue:
			if err = DecodeCueChunk(d, chunk); err != nil {
				if !errors.Is(err, io.EOF) {
					d.err = err
				}
			}
		default:
			// fmt.Println(string(chunk.ID[:]))
			chunk.Drain()
		}
	}

}

// FwdToPCM forwards the underlying reader until the start of the PCM chunk.
// If the PCM chunk was already read, no data will be found (you need to rewind).
func (d *Decoder) FwdToPCM() error {
	if d == nil {
		return fmt.Errorf("PCM data not found")
	}
	return d.ensurePCM()
}

// SeekFrame moves the read cursor to the requested audio frame. offset and the
// returned position are expressed in audio frames; one frame contains one
// sample value per channel. Position 0 is the first frame of the first "data"
// chunk and the valid range is [0, total frame count]. io.SeekEnd computes the
// target from the total frame count.
//
// The first call parses the container header and locates the "data" chunk,
// including skipping any LIST or unknown chunk (and odd-size alignment padding)
// found between "fmt " and "data". Seeking does not decode or buffer the PCM
// payload: the cursor is moved directly with the underlying io.ReadSeeker.
//
// io.SeekCurrent fails if the current read position is in the middle of a
// frame; io.SeekStart and io.SeekEnd always realign to a frame boundary. An
// invalid whence or target leaves the current position untouched.
func (d *Decoder) SeekFrame(offset int64, whence int) (int64, error) {
	if d == nil {
		return 0, fmt.Errorf("can't seek on a nil decoder")
	}
	if err := d.ensurePCM(); err != nil {
		return 0, err
	}

	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		if d.pcmPos%int64(d.bytesPerFrame) != 0 {
			return d.currentFrame(), fmt.Errorf("SeekFrame with io.SeekCurrent is not allowed in the middle of a frame")
		}
		if addOverflows(d.pcmPos/int64(d.bytesPerFrame), offset) {
			return d.currentFrame(), fmt.Errorf("frame position overflow: %d + %d", d.pcmPos/int64(d.bytesPerFrame), offset)
		}
		target = d.pcmPos/int64(d.bytesPerFrame) + offset
	case io.SeekEnd:
		if addOverflows(d.totalFrames, offset) {
			return d.currentFrame(), fmt.Errorf("frame position overflow: %d + %d", d.totalFrames, offset)
		}
		target = d.totalFrames + offset
	default:
		return d.currentFrame(), fmt.Errorf("invalid whence value: %d", whence)
	}
	if target < 0 {
		return d.currentFrame(), fmt.Errorf("negative frame position: %d", target)
	}
	if target > d.totalFrames {
		return d.currentFrame(), fmt.Errorf("frame position %d is beyond the last frame %d", target, d.totalFrames)
	}

	if target > math.MaxInt64/int64(d.bytesPerFrame) {
		return d.currentFrame(), fmt.Errorf("byte position overflow at frame %d", target)
	}
	byteOffset := target * int64(d.bytesPerFrame)
	if d.pcmDataOffset > math.MaxInt64-byteOffset {
		return d.currentFrame(), fmt.Errorf("absolute byte position overflow at frame %d", target)
	}
	if _, err := d.r.Seek(d.pcmDataOffset+byteOffset, io.SeekStart); err != nil {
		return d.currentFrame(), fmt.Errorf("failed to seek to frame %d: %w", target, err)
	}
	d.pcmPos = byteOffset
	d.pcmReader = io.LimitReader(d.r, d.pcmPayloadSize()-byteOffset)
	d.PCMChunk = d.newPCMChunk()
	return target, nil
}

func (d *Decoder) currentFrame() int64 {
	return d.pcmPos / int64(d.bytesPerFrame)
}

func (d *Decoder) pcmPayloadSize() int64 {
	return int64(d.PCMSize)
}

func (d *Decoder) newPCMChunk() *riff.Chunk {
	return &riff.Chunk{
		ID:   riff.DataFormatID,
		Size: int(d.pcmPayloadSize() - d.pcmPos),
		Pos:  0,
		R:    d.pcmReader,
	}
}

func addOverflows(a, b int64) bool {
	return (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b)
}

// ensurePCM locates the "fmt " and first "data" chunks. It is idempotent and
// does not read any PCM sample.
func (d *Decoder) ensurePCM() error {
	if d.pcmDataAccessed {
		return nil
	}

	// Start from the absolute beginning so this works regardless of where the
	// header parsing helpers left the underlying reader.
	if _, err := d.r.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to rewind the reader while locating the PCM data: %w", err)
	}

	var riffID [4]byte
	if _, err := io.ReadFull(d.r, riffID[:]); err != nil {
		d.err = fmt.Errorf("malformed WAV header, RIFF id: %w", err)
		return d.err
	}
	if riffID != riff.RiffID {
		d.err = fmt.Errorf("%s - %s", riffID, riff.ErrFmtNotSupported)
		return d.err
	}
	var riffSize uint32
	if err := binary.Read(d.r, binary.LittleEndian, &riffSize); err != nil {
		d.err = fmt.Errorf("malformed WAV header, RIFF size: %w", err)
		return d.err
	}
	var waveID [4]byte
	if err := binary.Read(d.r, binary.BigEndian, &waveID); err != nil {
		d.err = fmt.Errorf("malformed WAV header, WAVE id: %w", err)
		return d.err
	}
	if waveID != riff.WavFormatID {
		d.err = fmt.Errorf("%s - %s", waveID, riff.ErrFmtNotSupported)
		return d.err
	}

	var (
		haveFmt        bool
		wavFormat      uint16
		numChans       uint16
		sampleRate     uint32
		avgBytesPerSec uint32
		blockAlign     uint16
		bitsPerSample  uint16
		dataOffset     int64 = -1
		dataSize       int64
		absolutePos    int64 = 12
	)
	for {
		var chunkID [4]byte
		if _, err := io.ReadFull(d.r, chunkID[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			d.err = fmt.Errorf("malformed WAV header, chunk id: %w", err)
			return d.err
		}
		var declaredSize uint32
		if err := binary.Read(d.r, binary.LittleEndian, &declaredSize); err != nil {
			d.err = fmt.Errorf("malformed WAV header, chunk size: %w", err)
			return d.err
		}
		absolutePos += 8
		chunkStart := absolutePos
		chunkSize := int64(declaredSize)
		paddedSize := chunkSize + chunkSize%2
		if paddedSize < 0 || chunkStart > math.MaxInt64-paddedSize {
			d.err = fmt.Errorf("malformed WAV header: chunk %s size %d overflows the container", chunkID, declaredSize)
			return d.err
		}

		switch chunkID {
		case riff.FmtID:
			if declaredSize < 16 {
				d.err = fmt.Errorf("malformed WAV header: fmt chunk is too short (%d bytes)", declaredSize)
				return d.err
			}
			fmtData := make([]byte, chunkSize)
			if _, err := io.ReadFull(d.r, fmtData); err != nil {
				d.err = fmt.Errorf("malformed WAV header, truncated fmt chunk: %w", err)
				return d.err
			}
			wavFormat = binary.LittleEndian.Uint16(fmtData[0:2])
			numChans = binary.LittleEndian.Uint16(fmtData[2:4])
			sampleRate = binary.LittleEndian.Uint32(fmtData[4:8])
			avgBytesPerSec = binary.LittleEndian.Uint32(fmtData[8:12])
			blockAlign = binary.LittleEndian.Uint16(fmtData[12:14])
			bitsPerSample = binary.LittleEndian.Uint16(fmtData[14:16])
			haveFmt = true
		case riff.DataFormatID:
			if !haveFmt {
				d.err = fmt.Errorf("malformed WAV header: data chunk found before fmt chunk")
				return d.err
			}
			dataOffset = chunkStart
			dataSize = chunkSize
		default:
			// LIST, smpl, cue, JUNK and any other chunk: skip the payload
			// and its word-alignment padding without interpreting it.
		}

		nextPos := chunkStart + paddedSize
		if dataOffset >= 0 {
			// The first data chunk ends the header scan. Padding and metadata
			// following it must never be exposed as PCM samples.
			absolutePos = nextPos
			break
		}
		if _, err := d.r.Seek(nextPos, io.SeekStart); err != nil {
			d.err = fmt.Errorf("malformed WAV header, failed to skip chunk %s: %w", chunkID, err)
			return d.err
		}
		absolutePos = nextPos
	}

	if !haveFmt {
		d.err = fmt.Errorf("malformed WAV header: fmt chunk not found")
		return d.err
	}
	if wavFormat != 1 {
		d.err = fmt.Errorf("unsupported WAV audio format: %d, only integer PCM (format 1) is supported", wavFormat)
		return d.err
	}
	if numChans == 0 {
		d.err = fmt.Errorf("malformed WAV header: number of channels must be greater than 0")
		return d.err
	}
	if sampleRate == 0 {
		d.err = fmt.Errorf("malformed WAV header: sample rate must be greater than 0")
		return d.err
	}
	bytesPerSample, ok := pcmBytesPerSample(int(bitsPerSample))
	if !ok {
		d.err = fmt.Errorf("unsupported PCM bit depth: %d, only 8, 16, 24 and 32 integer bits are supported", bitsPerSample)
		return d.err
	}
	frameBytes := int64(numChans) * int64(bytesPerSample)
	if frameBytes > math.MaxInt64 || int(frameBytes) != int(numChans)*bytesPerSample {
		d.err = fmt.Errorf("malformed WAV header: frame size overflows")
		return d.err
	}
	if int(blockAlign) != int(frameBytes) {
		d.err = fmt.Errorf("malformed WAV header: block align %d does not match channels %d x %d bytes/sample", blockAlign, numChans, bytesPerSample)
		return d.err
	}
	expectedAvg := sampleRate * uint32(frameBytes)
	if avgBytesPerSec != expectedAvg {
		d.err = fmt.Errorf("malformed WAV header: average bytes per second %d does not match sample rate %d x frame size %d", avgBytesPerSec, sampleRate, frameBytes)
		return d.err
	}
	if dataOffset < 0 {
		d.err = ErrPCMChunkNotFound
		return d.err
	}
	if dataSize < 0 {
		d.err = fmt.Errorf("malformed WAV header: negative PCM payload size %d", dataSize)
		return d.err
	}
	if dataSize%frameBytes != 0 {
		d.err = fmt.Errorf("malformed WAV header: PCM payload size %d does not contain an integer number of %d-byte frames", dataSize, frameBytes)
		return d.err
	}

	d.WavAudioFormat = wavFormat
	d.NumChans = numChans
	d.BitDepth = bitsPerSample
	d.SampleRate = sampleRate
	d.AvgBytesPerSec = avgBytesPerSec
	// Keep the declared payload size (it excludes the RIFF padding byte), but
	// only whole samples/frames are exposed as audio data.
	d.PCMSize = int(dataSize)
	d.bytesPerFrame = int(frameBytes)
	d.bytesPerSample = bytesPerSample
	d.totalFrames = dataSize / frameBytes
	d.pcmDataOffset = dataOffset
	d.pcmPos = 0
	if _, err := d.r.Seek(dataOffset, io.SeekStart); err != nil {
		d.err = fmt.Errorf("failed to seek to the PCM payload: %w", err)
		return d.err
	}
	// Keep the legacy parser populated for Duration()/String() consumers.
	d.parser.ID = riff.RiffID
	d.parser.Size = riffSize
	d.parser.Format = riff.WavFormatID
	d.parser.WavAudioFormat = wavFormat
	d.parser.NumChannels = numChans
	d.parser.SampleRate = sampleRate
	d.parser.AvgBytesPerSec = avgBytesPerSec
	d.parser.BlockAlign = blockAlign
	d.parser.BitsPerSample = bitsPerSample
	d.pcmReader = io.LimitReader(d.r, d.usablePayloadSize())
	d.PCMChunk = d.newPCMChunk()
	d.pcmDataAccessed = true
	d.err = nil
	return nil
}

// usablePayloadSize is the largest number of payload bytes that can be decoded
// into complete sample values. A declared payload whose last bytes do not form
// a complete frame (or sample) still exposes every complete sample; frame
// addressing itself is restricted to whole frames.
func (d *Decoder) usablePayloadSize() int64 {
	return d.pcmPayloadSize() - d.pcmPayloadSize()%int64(d.bytesPerSample)
}

func pcmBytesPerSample(bitDepth int) (int, bool) {
	switch bitDepth {
	case 8, 16, 24, 32:
		return bitDepth / 8, true
	default:
		return 0, false
	}
}

// readPCM reads up to len(p) bytes from the declared PCM payload only.
// A read that reaches the declared payload boundary in the same call as the
// underlying EOF is reported as io.ErrUnexpectedEOF so that truncated files are
// not mistaken for a clean end of stream.
func (d *Decoder) readPCM(p []byte) (int, error) {
	remaining := d.usablePayloadSize() - d.pcmPos
	if remaining <= 0 || len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	var total int
	for total < len(p) {
		n, err := d.pcmReader.Read(p[total:])
		total += n
		d.pcmPos += int64(n)
		if err != nil {
			if err == io.EOF {
				if total == len(p) {
					// Clean declared-payload boundary; the next read returns (0, nil).
					err = nil
				} else {
					err = io.ErrUnexpectedEOF
				}
			}
			d.PCMChunk = d.newPCMChunk()
			return total, err
		}
	}
	d.PCMChunk = d.newPCMChunk()
	return total, nil
}

// WasPCMAccessed returns positively if the PCM data was previously accessed.
func (d *Decoder) WasPCMAccessed() bool {
	if d == nil {
		return false
	}
	return d.pcmDataAccessed
}

// FullPCMBuffer is an inefficient way to access all the PCM data contained in the
// audio container. The entire PCM data is held in memory.
// Consider using PCMBuffer() instead.
func (d *Decoder) FullPCMBuffer() (*audio.IntBuffer, error) {
	if err := d.ensurePCM(); err != nil {
		return nil, err
	}
	if d.PCMChunk == nil {
		return nil, errors.New("PCM chunk not found")
	}
	format := &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}

	buf := &audio.IntBuffer{Data: make([]int, 4096), Format: format, SourceBitDepth: int(d.BitDepth)}
	sampleSize := int(bytesPerSample(int(d.BitDepth)))
	sampleBufData := make([]byte, sampleSize)
	decodeF, err := sampleDecodeFunc(int(d.BitDepth))
	if err != nil {
		return nil, fmt.Errorf("could not get sample decode func %w", err)
	}

	i := 0
	for {
		remainingSamples := (d.usablePayloadSize() - d.pcmPos) / int64(sampleSize)
		if remainingSamples == 0 {
			break
		}
		readSampleCount := int(remainingSamples)
		maxSamples := 4096 / sampleSize
		if readSampleCount > maxSamples {
			readSampleCount = maxSamples
		}
		readBytes := readSampleCount * sampleSize
		tmp := make([]byte, readBytes)
		m, readErr := d.readPCM(tmp)
		chunkReader := bytes.NewReader(tmp[:m])
		for m >= sampleSize {
			value, decodeErr := decodeF(chunkReader, sampleBufData)
			if decodeErr != nil {
				if errors.Is(decodeErr, io.EOF) {
					break
				}
				buf.Data = buf.Data[:i]
				return buf, decodeErr
			}
			buf.Data[i] = value
			i++
			m -= sampleSize
			if i == len(buf.Data) {
				buf.Data = append(buf.Data, make([]int, 4096)...)
			}
		}
		if readErr != nil {
			buf.Data = buf.Data[:i]
			return buf, readErr
		}
	}
	buf.Data = buf.Data[:i]
	return buf, nil
}

// PCMBuffer populates the passed PCM buffer
func (d *Decoder) PCMBuffer(buf *audio.IntBuffer) (n int, err error) {
	if buf == nil {
		return 0, nil
	}

	if err := d.ensurePCM(); err != nil {
		return 0, err
	}
	if d.PCMChunk == nil {
		return 0, ErrPCMChunkNotFound
	}

	format := &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}

	buf.SourceBitDepth = int(d.BitDepth)
	decodeF, err := sampleDecodeFunc(int(d.BitDepth))
	if err != nil {
		return 0, fmt.Errorf("could not get sample decode func %w", err)
	}

	bPerSample := bytesPerSample(int(d.BitDepth))
	size := len(buf.Data) * bPerSample
	if size == 0 {
		buf.Format = format
		return 0, nil
	}
	tmpBuf := make([]byte, size)
	m, err := d.readPCM(tmpBuf)
	if m == 0 {
		return 0, err
	}
	readErr := err
	bufR := bytes.NewReader(tmpBuf[:m])
	sampleBuf := make([]byte, bPerSample, bPerSample)

	// Note that we populate the buffer even if the
	// size of the buffer doesn't fit an even number of frames.
	for n = 0; n < len(buf.Data) && m >= bPerSample; n++ {
		var decodeErr error
		buf.Data[n], decodeErr = decodeF(bufR, sampleBuf)
		if decodeErr != nil {
			break
		}
		m -= bPerSample
	}
	buf.Format = format
	if readErr != nil {
		return n, readErr
	}
	return n, nil
}

// Format returns the audio format of the decoded content.
func (d *Decoder) Format() *audio.Format {
	if d == nil {
		return nil
	}
	return &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}
}

// NextChunk returns the next available chunk
func (d *Decoder) NextChunk() (*riff.Chunk, error) {
	if d.err = d.readHeaders(); d.err != nil {
		d.err = fmt.Errorf("failed to read header - %v", d.err)
		return nil, d.err
	}

	var (
		id   [4]byte
		size uint32
	)

	id, size, d.err = d.parser.IDnSize()
	if d.err != nil {
		d.err = fmt.Errorf("error reading chunk header - %v", d.err)
		return nil, d.err
	}

	// TODO: any reason we don't use d.parser.NextChunk (riff.NextChunk) here?
	// It correctly handles the misaligned chunk.

	// TODO: copied over from riff.parser.NextChunk
	// all RIFF chunks (including WAVE "data" chunks) must be word aligned.
	// If the data uses an odd number of bytes, a padding byte with a value of zero must be placed at the end of the sample data.
	// The "data" chunk header's size should not include this byte.
	if size%2 == 1 {
		size++
	}

	c := &riff.Chunk{
		ID:   id,
		Size: int(size),
		R:    io.LimitReader(d.r, int64(size)),
	}
	return c, d.err
}

// Duration returns the time duration for the current audio container
func (d *Decoder) Duration() (time.Duration, error) {
	if d == nil || d.parser == nil {
		return 0, errors.New("can't calculate the duration of a nil pointer")
	}
	return d.parser.Duration()
}

// String implements the Stringer interface.
func (d *Decoder) String() string {
	return d.parser.String()
}

// readHeaders is safe to call multiple times
func (d *Decoder) readHeaders() error {
	if d == nil || d.NumChans > 0 {
		return nil
	}

	id, size, err := d.parser.IDnSize()
	if err != nil {
		return err
	}
	d.parser.ID = id
	if d.parser.ID != riff.RiffID {
		return fmt.Errorf("%s - %s", d.parser.ID, riff.ErrFmtNotSupported)
	}
	d.parser.Size = size
	if err := binary.Read(d.r, binary.BigEndian, &d.parser.Format); err != nil {
		return err
	}

	var chunk *riff.Chunk
	var rewindBytes int64

	for err == nil {
		chunk, err = d.parser.NextChunk()
		if err != nil {
			break
		}

		if chunk.ID == riff.FmtID {
			chunk.DecodeWavHeader(d.parser)
			d.NumChans = d.parser.NumChannels
			d.BitDepth = d.parser.BitsPerSample
			d.SampleRate = d.parser.SampleRate
			d.WavAudioFormat = d.parser.WavAudioFormat
			d.AvgBytesPerSec = d.parser.AvgBytesPerSec

			if rewindBytes > 0 {
				d.r.Seek(-(rewindBytes + int64(chunk.Size) + 8), 1)
			}
			break
		} else if chunk.ID == CIDList {
			// The list chunk can be in the header or footer
			// because so many players don't support that chunk properly
			// it is recommended to have it at the end of the file.
			DecodeListChunk(d, chunk)
			// unexpected chunk order, might be a bext chunk
			rewindBytes += int64(chunk.Size) + 8
		} else if chunk.ID == CIDSmpl {
			DecodeSamplerChunk(d, chunk)
			rewindBytes += int64(chunk.Size) + 8
		} else {
			// unexpected chunk order, might be a bext chunk
			rewindBytes += int64(chunk.Size) + 8
			// drain the chunk
			io.CopyN(io.Discard, d.r, int64(chunk.Size))
		}
	}

	return d.err
}

func bytesPerSample(bitDepth int) int {
	return bitDepth / 8
}

// sampleDecodeFunc returns a function that can be used to convert
// a byte range into an int value based on the amount of bits used per sample.
// Note that 8bit samples are unsigned, all other values are signed.
func sampleDecodeFunc(bitsPerSample int) (func(io.Reader, []byte) (int, error), error) {
	// NOTE: WAV PCM data is stored using little-endian
	switch bitsPerSample {
	case 8:
		// 8bit values are unsigned
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:1])
			return int(buf[0]), err
		}, nil
	case 16:
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:2])
			return int(int16(binary.LittleEndian.Uint16(buf[:2]))), err
		}, nil
	case 24:
		// -34,359,738,367 (0x7FFFFF) to 34,359,738,368	(0x800000)
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:3])
			if err != nil {
				return 0, err
			}
			return int(audio.Int24LETo32(buf[:3])), nil
		}, nil
	case 32:
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:4])
			return int(int32(binary.LittleEndian.Uint32(buf[:4]))), err
		}, nil
	default:
		return nil, fmt.Errorf("unhandled byte depth:%d", bitsPerSample)
	}
}

// sampleDecodeFloat64Func returns a function that can be used to convert
// a byte range into a float64 value based on the amount of bits used per sample.
func sampleFloat64DecodeFunc(bitsPerSample int) (func([]byte) float64, error) {
	bytesPerSample := bitsPerSample / 8
	switch bytesPerSample {
	case 1:
		// 8bit values are unsigned
		return func(s []byte) float64 {
			return float64(uint8(s[0]))
		}, nil
	case 2:
		return func(s []byte) float64 {
			return float64(int(s[0]) + int(s[1])<<8)
		}, nil
	case 3:
		return func(s []byte) float64 {
			var output int32
			output |= int32(s[2]) << 0
			output |= int32(s[1]) << 8
			output |= int32(s[0]) << 16
			return float64(output)
		}, nil
	case 4:
		// TODO: fix the float64 conversion (current int implementation)
		return func(s []byte) float64 {
			return float64(int(s[0]) + int(s[1])<<8 + int(s[2])<<16 + int(s[3])<<24)
		}, nil
	default:
		return nil, fmt.Errorf("unhandled byte depth:%d", bitsPerSample)
	}
}
