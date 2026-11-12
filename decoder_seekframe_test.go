package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

type syntheticWAV struct {
	sampleRate     uint32
	channels       uint16
	bitDepth       uint16
	samples        []int
	headerChunks   [][]byte
	trailingChunks [][]byte
	dataPadding    byte
	hasDataPadding bool
}

func (w syntheticWAV) bytesPerSample() int { return int(w.bitDepth) / 8 }

func (w syntheticWAV) frameSize() int { return int(w.channels) * w.bytesPerSample() }

func (w syntheticWAV) frameCount() int { return len(w.samples) / int(w.channels) }

func (w syntheticWAV) encodeSample(value int) []byte {
	switch w.bitDepth {
	case 8:
		return []byte{uint8(value)}
	case 16:
		b := make([]byte, 2)
		binary.LittleEndian.PutUint16(b, uint16(int16(value)))
		return b
	case 24:
		return audio.Int32toInt24LEBytes(int32(value))
	case 32:
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, uint32(int32(value)))
		return b
	default:
		panic(fmt.Sprintf("unsupported test bit depth %d", w.bitDepth))
	}
}

func chunkBytes(id [4]byte, payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload)+len(payload)%2)
	out = append(out, id[:]...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(payload)))
	out = append(out, size[:]...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func (w syntheticWAV) build() []byte {
	var pcm []byte
	for _, sample := range w.samples {
		pcm = append(pcm, w.encodeSample(sample)...)
	}
	fmtPayload := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtPayload[0:2], 1)
	binary.LittleEndian.PutUint16(fmtPayload[2:4], w.channels)
	binary.LittleEndian.PutUint32(fmtPayload[4:8], w.sampleRate)
	frameSize := w.frameSize()
	binary.LittleEndian.PutUint32(fmtPayload[8:12], w.sampleRate*uint32(frameSize))
	binary.LittleEndian.PutUint16(fmtPayload[12:14], uint16(frameSize))
	binary.LittleEndian.PutUint16(fmtPayload[14:16], w.bitDepth)

	body := chunkBytes([4]byte{'f', 'm', 't', ' '}, fmtPayload)
	for _, chunk := range w.headerChunks {
		body = append(body, chunk...)
	}
	body = append(body, chunkBytes([4]byte{'d', 'a', 't', 'a'}, pcm)...)
	if w.hasDataPadding {
		body = append(body, w.dataPadding)
	}
	for _, chunk := range w.trailingChunks {
		body = append(body, chunk...)
	}

	out := []byte{'R', 'I', 'F', 'F'}
	headerSize := make([]byte, 4)
	binary.LittleEndian.PutUint32(headerSize, uint32(4+len(body)))
	out = append(out, headerSize...)
	out = append(out, 'W', 'A', 'V', 'E')
	out = append(out, body...)
	return out
}

func (w syntheticWAV) samplesAt(frame, frameCount int) []int {
	start := frame * int(w.channels)
	end := start + frameCount*int(w.channels)
	if end > len(w.samples) {
		end = len(w.samples)
	}
	return w.samples[start:end]
}

func patternSamples(channels, frames int) []int {
	samples := make([]int, channels*frames)
	for i := range samples {
		samples[i] = (i*7 + 3) % 1000
	}
	return samples
}

type limitedReadSeeker struct {
	data    []byte
	pos     int64
	maxRead int
	reads   int
}

func (l *limitedReadSeeker) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if l.pos >= int64(len(l.data)) {
		return 0, io.EOF
	}
	l.reads++
	n := len(p)
	if n > l.maxRead {
		n = l.maxRead
	}
	remaining := len(l.data) - int(l.pos)
	if n > remaining {
		n = remaining
	}
	copy(p, l.data[l.pos:l.pos+int64(n)])
	l.pos += int64(n)
	if l.pos == int64(len(l.data)) {
		return n, io.EOF
	}
	return n, nil
}

func (l *limitedReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = l.pos + offset
	case io.SeekEnd:
		target = int64(len(l.data)) + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if target < 0 || target > int64(len(l.data)) {
		return 0, fmt.Errorf("seek out of range: %d", target)
	}
	l.pos = target
	return target, nil
}

type eofWithDataReadSeeker struct {
	data       []byte
	pos        int64
	eofServed  bool
	finalSplit int
}

func (e *eofWithDataReadSeeker) Read(p []byte) (int, error) {
	if e.pos >= int64(len(e.data)) {
		return 0, io.EOF
	}
	remaining := int64(len(e.data)) - e.pos
	n := int64(len(p))
	if n > remaining {
		n = remaining
	}
	if !e.eofServed && e.pos+n >= int64(len(e.data)) && int64(e.finalSplit) < n {
		n = int64(e.finalSplit)
	}
	copy(p, e.data[e.pos:e.pos+n])
	e.pos += n
	if e.pos == int64(len(e.data)) {
		e.eofServed = true
		return int(n), io.EOF
	}
	return int(n), nil
}

func (e *eofWithDataReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = e.pos + offset
	case io.SeekEnd:
		target = int64(len(e.data)) + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if target < 0 || target > int64(len(e.data)) {
		return 0, fmt.Errorf("seek out of range: %d", target)
	}
	e.pos = target
	return target, nil
}

func TestSeekFrame_BitDepthsAndChannels(t *testing.T) {
	for _, bitDepth := range []uint16{8, 16, 24, 32} {
		for _, channels := range []uint16{1, 2, 3} {
			name := fmt.Sprintf("%dbit_%dch", bitDepth, channels)
			t.Run(name, func(t *testing.T) {
				w := syntheticWAV{
					sampleRate: 48000,
					channels:   channels,
					bitDepth:   bitDepth,
					samples:    patternSamples(int(channels), 12),
				}
				d := NewDecoder(bytes.NewReader(w.build()))
				d.ReadInfo()
				if err := d.Err(); err != nil {
					t.Fatalf("ReadInfo failed: %v", err)
				}
				if d.NumChans != channels || d.BitDepth != bitDepth || d.SampleRate != 48000 {
					t.Fatalf("unexpected format: %+v", d.Format())
				}
				if err := d.FwdToPCM(); err != nil {
					t.Fatal(err)
				}
				if got, want := d.PCMLen(), int64(len(w.samples)*w.bytesPerSample()); got != want {
					t.Fatalf("PCMLen = %d, want %d", got, want)
				}

				pos, err := d.SeekFrame(5, io.SeekStart)
				if err != nil || pos != 5 {
					t.Fatalf("SeekFrame start = (%d, %v), want 5, nil", pos, err)
				}
				buf := &audio.IntBuffer{Data: make([]int, 4*int(channels))}
				n, err := d.PCMBuffer(buf)
				if err != nil {
					t.Fatal(err)
				}
				if n != 4*int(channels) {
					t.Fatalf("read %d samples, want %d", n, 4*int(channels))
				}
				wantSamples := w.samplesAt(5, 4)
				if !reflect.DeepEqual(buf.Data[:n], wantSamples) {
					t.Fatalf("samples after seek = %v, want %v", buf.Data[:n], wantSamples)
				}

				pos, err = d.SeekFrame(-2, io.SeekCurrent)
				if err != nil || pos != 7 {
					t.Fatalf("SeekFrame current = (%d, %v), want 7, nil", pos, err)
				}
				n, err = d.PCMBuffer(buf)
				if err != nil {
					t.Fatal(err)
				}
				wantSamples = w.samplesAt(7, 4)
				if !reflect.DeepEqual(buf.Data[:n], wantSamples) {
					t.Fatalf("relative seek samples = %v, want %v", buf.Data[:n], wantSamples)
				}

				pos, err = d.SeekFrame(-3, io.SeekEnd)
				if err != nil || pos != int64(w.frameCount())-3 {
					t.Fatalf("SeekFrame end = (%d, %v), want %d, nil", pos, err, int64(w.frameCount())-3)
				}
				full, err := d.FullPCMBuffer()
				if err != nil {
					t.Fatal(err)
				}
				wantSamples = w.samplesAt(w.frameCount()-3, 3)
				if !reflect.DeepEqual(full.Data, wantSamples) {
					t.Fatalf("FullPCMBuffer after seek = %v, want %v", full.Data, wantSamples)
				}

				if pos, err = d.SeekFrame(0, io.SeekEnd); err != nil || pos != int64(w.frameCount()) {
					t.Fatalf("seek to end = (%d, %v), want %d", pos, err, w.frameCount())
				}
				if n, err = d.PCMBuffer(buf); err != nil || n != 0 {
					t.Fatalf("read at end = (%d, %v), want 0, nil", n, err)
				}
				if pos, err = d.SeekFrame(0, io.SeekStart); err != nil || pos != 0 {
					t.Fatalf("seek back to start = (%d, %v)", pos, err)
				}
				n, err = d.PCMBuffer(buf)
				if err != nil {
					t.Fatal(err)
				}
				wantSamples = w.samplesAt(0, 4)
				if !reflect.DeepEqual(buf.Data[:n], wantSamples) {
					t.Fatalf("recovery samples = %v, want %v", buf.Data[:n], wantSamples)
				}

				if err := d.Rewind(); err != nil {
					t.Fatal(err)
				}
				n, err = d.PCMBuffer(buf)
				if err != nil {
					t.Fatal(err)
				}
				wantSamples = w.samplesAt(0, 4)
				if !reflect.DeepEqual(buf.Data[:n], wantSamples) {
					t.Fatalf("rewind samples = %v, want %v", buf.Data[:n], wantSamples)
				}
			})
		}
	}
}

func TestSeekFrame_MisalignedBufferThenSeek(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 8000,
		channels:   2,
		bitDepth:   16,
		samples:    patternSamples(2, 10),
	}
	d := NewDecoder(bytes.NewReader(w.build()))
	// 3 sample values is not an integer number of stereo frames.
	buf := &audio.IntBuffer{Data: make([]int, 3)}
	n, err := d.PCMBuffer(buf)
	if err != nil || n != 3 {
		t.Fatalf("partial-frame read = (%d, %v), want 3, nil", n, err)
	}
	if _, err := d.SeekFrame(0, io.SeekCurrent); err == nil {
		t.Fatal("SeekFrame(0, SeekCurrent) in the middle of a frame should fail")
	}
	pos, err := d.SeekFrame(2, io.SeekStart)
	if err != nil || pos != 2 {
		t.Fatalf("SeekStart should realign: (%d, %v)", pos, err)
	}
	pos, err = d.SeekFrame(-1, io.SeekEnd)
	if err != nil || pos != 9 {
		t.Fatalf("SeekEnd should realign: (%d, %v)", pos, err)
	}
	n, err = d.PCMBuffer(&audio.IntBuffer{Data: make([]int, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := n, 2; got != want {
		t.Fatalf("read %d samples, want %d", got, want)
	}
}

func TestSeekFrame_InvalidTargetsKeepState(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 8000,
		channels:   1,
		bitDepth:   16,
		samples:    patternSamples(1, 8),
	}
	d := NewDecoder(bytes.NewReader(w.build()))
	pos, err := d.SeekFrame(3, io.SeekStart)
	if err != nil || pos != 3 {
		t.Fatalf("setup seek = (%d, %v)", pos, err)
	}
	buf := &audio.IntBuffer{Data: make([]int, 2)}
	if _, err := d.PCMBuffer(buf); err != nil {
		t.Fatal(err)
	}
	frameNow := int64(5)
	invalid := []struct {
		offset int64
		whence int
	}{
		{-1, io.SeekStart},
		{9, io.SeekStart},
		{1, io.SeekEnd},
		{-9, io.SeekEnd},
		{4, io.SeekCurrent},
		{-6, io.SeekCurrent},
		{0, 99},
		{1 << 62, io.SeekStart},
		{1 << 62, io.SeekEnd},
		{math.MaxInt64, io.SeekStart},
		{math.MinInt64, io.SeekEnd},
	}
	for _, tc := range invalid {
		p, seekErr := d.SeekFrame(tc.offset, tc.whence)
		if seekErr == nil {
			t.Fatalf("SeekFrame(%d, %d) should fail", tc.offset, tc.whence)
		}
		if p != frameNow {
			t.Fatalf("failed SeekFrame(%d, %d) changed reported frame to %d, want %d", tc.offset, tc.whence, p, frameNow)
		}
	}
	n, err := d.PCMBuffer(buf)
	if err != nil {
		t.Fatalf("read after invalid seeks failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("read %d samples after invalid seeks, want 2", n)
	}
	if !reflect.DeepEqual(buf.Data[:n], w.samplesAt(5, 2)) {
		t.Fatalf("state changed by invalid seeks: %v, want %v", buf.Data[:n], w.samplesAt(5, 2))
	}

	// Reading to the end and seeking backwards still returns the same samples.
	for {
		n, err = d.PCMBuffer(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	pos, err = d.SeekFrame(5, io.SeekStart)
	if err != nil || pos != 5 {
		t.Fatalf("backward seek after EOF = (%d, %v)", pos, err)
	}
	n, err = d.PCMBuffer(buf)
	if err != nil || !reflect.DeepEqual(buf.Data[:n], w.samplesAt(5, 2)) {
		t.Fatalf("read after EOF seek = %v, %v, want %v", n, err, w.samplesAt(5, 2))
	}
}

func TestSeekFrame_UnknownOddChunkAndTrailingMetadata(t *testing.T) {
	// An odd-size unknown chunk before data forces the decoder to skip its
	// single alignment padding byte.
	unknownPayload := []byte{0xAA, 0xBB, 0xCC}
	w := syntheticWAV{
		sampleRate:   12000,
		channels:     2,
		bitDepth:     16,
		samples:      patternSamples(2, 6),
		headerChunks: [][]byte{chunkBytes([4]byte{'x', 't', 's', 't'}, unknownPayload)},
		trailingChunks: [][]byte{
			chunkBytes([4]byte{'L', 'I', 'S', 'T'}, []byte("INFOmetadata")),
			chunkBytes([4]byte{'i', 'd', '3', ' '}, []byte{1, 2, 3}),
		},
	}
	raw := w.build()
	d := NewDecoder(bytes.NewReader(raw))
	first, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Data, w.samples) {
		t.Fatalf("decoded %v, want %v", first.Data, w.samples)
	}
	if got, want := d.PCMLen(), int64(len(w.samples)*2); got != want {
		t.Fatalf("PCMLen = %d, want %d (metadata must be excluded)", got, want)
	}
	pos, err := d.SeekFrame(3, io.SeekStart)
	if err != nil || pos != 3 {
		t.Fatalf("seek after odd chunk = (%d, %v)", pos, err)
	}
	buf := &audio.IntBuffer{Data: make([]int, 6)}
	n, err := d.PCMBuffer(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(buf.Data[:n], w.samplesAt(3, 3)) {
		t.Fatalf("seeked samples = %v, want %v", buf.Data[:n], w.samplesAt(3, 3))
	}
}

func TestPCMBuffer_OddPayloadPaddingExcluded(t *testing.T) {
	// 8-bit mono with an odd declared payload; RIFF requires one padding byte,
	// which must not be decoded as a sample.
	w := syntheticWAV{
		sampleRate:     11025,
		channels:       1,
		bitDepth:       8,
		samples:        []int{10, 20, 30, 40, 50},
		hasDataPadding: true,
		dataPadding:    0x7F,
		trailingChunks: [][]byte{chunkBytes([4]byte{'L', 'I', 'S', 'T'}, []byte("x"))},
	}
	raw := w.build()
	d := NewDecoder(bytes.NewReader(raw))
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.PCMLen(), int64(5); got != want {
		t.Fatalf("PCMLen = %d, want %d", got, want)
	}
	full, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Data, w.samples) {
		t.Fatalf("padded payload decoded as %v, want %v", full.Data, w.samples)
	}

	d2 := NewDecoder(bytes.NewReader(raw))
	streamed := []int{}
	buf := &audio.IntBuffer{Data: make([]int, 2)}
	for {
		n, err := d2.PCMBuffer(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		streamed = append(streamed, buf.Data[:n]...)
	}
	if !reflect.DeepEqual(streamed, w.samples) {
		t.Fatalf("streamed padded payload = %v, want %v", streamed, w.samples)
	}
}

func TestPCMBuffer_ShortReadsAndEOFWithData(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 16000,
		channels:   2,
		bitDepth:   24,
		samples:    patternSamples(2, 14),
	}
	raw := w.build()

	t.Run("short reads", func(t *testing.T) {
		rs := &limitedReadSeeker{data: raw, maxRead: 3}
		d := NewDecoder(rs)
		full, err := d.FullPCMBuffer()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(full.Data, w.samples) {
			t.Fatalf("short-read decode lost or duplicated samples: got %d values, want %d", len(full.Data), len(w.samples))
		}
		if rs.reads == 0 {
			t.Fatal("controlled reader was not used")
		}
	})

	t.Run("data and EOF in one read", func(t *testing.T) {
		rs := &eofWithDataReadSeeker{data: raw, finalSplit: 5}
		d := NewDecoder(rs)
		full, err := d.FullPCMBuffer()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(full.Data, w.samples) {
			t.Fatalf("EOF-with-data decode mismatch: got %d values, want %d", len(full.Data), len(w.samples))
		}
	})

	t.Run("streaming small buffers with short reads", func(t *testing.T) {
		rs := &limitedReadSeeker{data: raw, maxRead: 2}
		d := NewDecoder(rs)
		got := []int{}
		buf := &audio.IntBuffer{Data: make([]int, 5)}
		for {
			n, err := d.PCMBuffer(buf)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			got = append(got, buf.Data[:n]...)
		}
		if !reflect.DeepEqual(got, w.samples) {
			t.Fatalf("streaming short reads returned %d values, want %d", len(got), len(w.samples))
		}
	})
}

func TestDecoder_TruncatedPayload(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 16000,
		channels:   2,
		bitDepth:   16,
		samples:    patternSamples(2, 10),
	}
	raw := w.build()
	// Keep every header and the declared data size intact, but end the actual
	// file after 13 bytes of the 40-byte PCM payload.
	dataStart := int64(12 + (8 + 16) + 8)
	if int64(len(raw)) <= dataStart+13 {
		t.Fatalf("test setup issue: raw length %d", len(raw))
	}
	truncated := raw[:dataStart+13]

	t.Run("PCMBuffer returns complete samples and ErrUnexpectedEOF", func(t *testing.T) {
		d := NewDecoder(bytes.NewReader(truncated))
		buf := &audio.IntBuffer{Data: make([]int, 20)}
		n, err := d.PCMBuffer(buf)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
		}
		if n != 6 {
			t.Fatalf("decoded %d complete samples, want 6", n)
		}
		if !reflect.DeepEqual(buf.Data[:n], w.samples[:6]) {
			t.Fatalf("truncated samples = %v, want %v", buf.Data[:n], w.samples[:6])
		}
		for i := n; i < len(buf.Data); i++ {
			if buf.Data[i] != 0 {
				t.Fatalf("buf.Data[%d] was overwritten beyond decoded samples", i)
			}
		}
	})

	t.Run("FullPCMBuffer returns complete samples and ErrUnexpectedEOF", func(t *testing.T) {
		d := NewDecoder(bytes.NewReader(truncated))
		full, err := d.FullPCMBuffer()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
		}
		if !reflect.DeepEqual(full.Data, w.samples[:6]) {
			t.Fatalf("full truncated samples = %v, want %v", full.Data, w.samples[:6])
		}
	})

	t.Run("short reads surface ErrUnexpectedEOF", func(t *testing.T) {
		rs := &limitedReadSeeker{data: truncated, maxRead: 2}
		d := NewDecoder(rs)
		buf := &audio.IntBuffer{Data: make([]int, 20)}
		if _, err := d.PCMBuffer(buf); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})
}

func TestSeekFrame_DoesNotBufferPCM(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 8000,
		channels:   2,
		bitDepth:   16,
		samples:    patternSamples(2, 100),
	}
	raw := w.build()
	rs := &countingReadSeeker{data: raw}
	d := NewDecoder(rs)
	for _, frame := range []int64{90, 10, 70, 0, 99} {
		pos, err := d.SeekFrame(frame, io.SeekStart)
		if err != nil || pos != frame {
			t.Fatalf("SeekFrame(%d) = (%d, %v)", frame, pos, err)
		}
	}
	if rs.bytesRead > int64(len(raw)) {
		t.Fatalf("seeking read %d bytes from a %d-byte file; payload must not be buffered", rs.bytesRead, len(raw))
	}
	readBuf := &audio.IntBuffer{Data: make([]int, 4)}
	n, err := d.PCMBuffer(readBuf)
	if err != nil {
		t.Fatal(err)
	}
	wantSamples := w.samplesAt(99, 2)
	if n != len(wantSamples) || !reflect.DeepEqual(readBuf.Data[:n], wantSamples) {
		t.Fatalf("final read = %v, want %v", readBuf.Data[:n], wantSamples)
	}
}

type countingReadSeeker struct {
	data      []byte
	pos       int64
	bytesRead int64
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	if c.pos >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += int64(n)
	c.bytesRead += int64(n)
	return n, nil
}

func (c *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	target := offset
	if whence == io.SeekCurrent {
		target = c.pos + offset
	} else if whence == io.SeekEnd {
		target = int64(len(c.data)) + offset
	}
	c.pos = target
	return target, nil
}

func mutateWAV(t *testing.T, mutate func(w *syntheticWAV, raw *[]byte)) []byte {
	t.Helper()
	w := syntheticWAV{
		sampleRate: 8000,
		channels:   2,
		bitDepth:   16,
		samples:    patternSamples(2, 8),
	}
	raw := w.build()
	mutate(&w, &raw)
	return raw
}

func TestDecoder_MalformedHeaders(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{
			name: "not RIFF",
			raw:  mutateWAV(t, func(w *syntheticWAV, raw *[]byte) { (*raw)[0] = 'X' }),
		},
		{
			name: "not WAVE",
			raw:  mutateWAV(t, func(w *syntheticWAV, raw *[]byte) { (*raw)[8] = 'X' }),
		},
		{
			name: "missing data",
			raw: func() []byte {
				w := syntheticWAV{sampleRate: 8000, channels: 1, bitDepth: 16, samples: nil}
				raw := w.build()
				return raw[:len(raw)-8]
			}(),
		},
		{
			name: "truncated header",
			raw:  mutateWAV(t, func(w *syntheticWAV, raw *[]byte) { *raw = (*raw)[:20] }),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDecoder(bytes.NewReader(tc.raw))
			if err := d.FwdToPCM(); err == nil {
				t.Fatal("expected a header error, got nil")
			}
			if _, err := d.SeekFrame(0, io.SeekStart); err == nil {
				t.Fatal("SeekFrame should report the header error")
			}
		})
	}
}

func TestDecoder_UnsupportedFormatAndSelfChecks(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 8000,
		channels:   2,
		bitDepth:   16,
		samples:    patternSamples(2, 8),
	}
	raw := w.build()
	// WAV format tag lives at the first fmt payload byte (absolute offset 20).
	raw[20] = 3
	if err := NewDecoder(bytes.NewReader(raw)).FwdToPCM(); err == nil {
		t.Fatal("non-PCM format tag should be rejected")
	}

	mutations := map[string]int{
		"zero channels":                22,
		"zero sample rate":             24,
		"bad block align":              32,
		"bad average bytes per second": 28,
	}
	for name, offset := range mutations {
		t.Run(name, func(t *testing.T) {
			broken := append([]byte(nil), w.build()...)
			binary.LittleEndian.PutUint32(broken[offset:], 0)
			if err := NewDecoder(bytes.NewReader(broken)).FwdToPCM(); err == nil {
				t.Fatal("expected a self-consistency error")
			}
		})
	}

	for _, bits := range []uint16{12, 20, 64} {
		bad := append([]byte(nil), raw...)
		binary.LittleEndian.PutUint16(bad[34:], bits)
		if err := NewDecoder(bytes.NewReader(bad)).FwdToPCM(); err == nil {
			t.Fatalf("bit depth %d should be rejected", bits)
		}
	}
}

func TestDecoder_DeclaredPayloadNotWholeFrames(t *testing.T) {
	f, err := os.Open("fixtures/padded24b.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	if err := d.FwdToPCM(); err == nil {
		t.Fatal("a declared payload that cannot form whole frames should be rejected")
	}
}

func TestDecoder_FixtureMetadataAfterData(t *testing.T) {
	f, err := os.Open("fixtures/misaligned-chunk.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	if err := d.FwdToPCM(); err != nil {
		t.Fatal(err)
	}
	if d.PCMLen() != 3386880 {
		t.Fatalf("PCMLen = %d, want the declared payload only", d.PCMLen())
	}
	pos, err := d.SeekFrame(100, io.SeekStart)
	if err != nil || pos != 100 {
		t.Fatalf("seek = (%d, %v)", pos, err)
	}
	buf := &audio.IntBuffer{Data: make([]int, int(d.NumChans))}
	n, err := d.PCMBuffer(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != int(d.NumChans) {
		t.Fatalf("read %d samples, want one frame of %d", n, d.NumChans)
	}
}

func TestDecoder_SeekFrameAndFullBufferAlternate(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 22050,
		channels:   2,
		bitDepth:   16,
		samples:    patternSamples(2, 16),
	}
	d := NewDecoder(bytes.NewReader(w.build()))
	streamBuf := &audio.IntBuffer{Data: make([]int, 6)}
	n, err := d.PCMBuffer(streamBuf)
	if err != nil || n != 6 {
		t.Fatalf("initial stream read = (%d, %v)", n, err)
	}
	if !reflect.DeepEqual(streamBuf.Data[:n], w.samplesAt(0, 3)) {
		t.Fatalf("initial samples = %v", streamBuf.Data[:n])
	}
	if _, err := d.SeekFrame(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	full, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Data, w.samplesAt(10, 6)) {
		t.Fatalf("full buffer from frame 10 = %v", full.Data)
	}
	if _, err := d.SeekFrame(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	n, err = d.PCMBuffer(streamBuf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(streamBuf.Data[:n], w.samplesAt(2, 3)) {
		t.Fatalf("stream after full buffer = %v", streamBuf.Data[:n])
	}
}

func TestSeekFrame_LISTChunkBeforeData(t *testing.T) {
	w := syntheticWAV{
		sampleRate:   22050,
		channels:     2,
		bitDepth:     24,
		samples:      patternSamples(2, 9),
		headerChunks: [][]byte{chunkBytes([4]byte{'L', 'I', 'S', 'T'}, []byte("INFO"))},
	}
	d := NewDecoder(bytes.NewReader(w.build()))
	full, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Data, w.samples) {
		t.Fatalf("LIST-before-data decode = %v, want %v", full.Data, w.samples)
	}
	pos, err := d.SeekFrame(4, io.SeekStart)
	if err != nil || pos != 4 {
		t.Fatalf("seek past LIST chunk = (%d, %v)", pos, err)
	}
	buf := &audio.IntBuffer{Data: make([]int, 4)}
	n, err := d.PCMBuffer(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(buf.Data[:n], w.samplesAt(4, 2)) {
		t.Fatalf("samples = %v, want %v", buf.Data[:n], w.samplesAt(4, 2))
	}
}

func TestSeekFrame_RecoveryAfterTruncation(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 16000,
		channels:   2,
		bitDepth:   16,
		samples:    patternSamples(2, 10),
	}
	raw := w.build()
	dataStart := int64(12 + (8 + 16) + 8)
	truncated := raw[:dataStart+13]
	d := NewDecoder(bytes.NewReader(truncated))
	buf := &audio.IntBuffer{Data: make([]int, 20)}
	n, err := d.PCMBuffer(buf)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if n != 6 {
		t.Fatalf("decoded %d samples before truncation, want 6", n)
	}
	pos, err := d.SeekFrame(0, io.SeekStart)
	if err != nil || pos != 0 {
		t.Fatalf("seek after truncation = (%d, %v)", pos, err)
	}
	n, err = d.PCMBuffer(buf)
	if !errors.Is(err, io.ErrUnexpectedEOF) || n != 6 {
		t.Fatalf("re-read = (%d, %v), want 6 samples and io.ErrUnexpectedEOF", n, err)
	}
	if !reflect.DeepEqual(buf.Data[:n], w.samples[:6]) {
		t.Fatalf("re-read samples = %v, want %v", buf.Data[:n], w.samples[:6])
	}
}

func TestPCMBuffer_DoesNotOverwriteTail(t *testing.T) {
	w := syntheticWAV{
		sampleRate: 8000,
		channels:   1,
		bitDepth:   16,
		samples:    []int{11, 22, 33},
	}
	d := NewDecoder(bytes.NewReader(w.build()))
	buf := &audio.IntBuffer{Data: []int{-1, -1, -1, -1, -1}}
	n, err := d.PCMBuffer(buf)
	if err != nil || n != 3 {
		t.Fatalf("read = (%d, %v)", n, err)
	}
	want := []int{11, 22, 33, -1, -1}
	if !reflect.DeepEqual(buf.Data, want) {
		t.Fatalf("buffer tail changed: %v, want %v", buf.Data, want)
	}
}
