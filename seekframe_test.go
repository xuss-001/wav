package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/go-audio/audio"
)

// wavBuilder assembles small RIFF/WAVE containers byte by byte so tests can
// exercise arbitrary chunk ordering, odd sizes and alignment padding.
type wavBuilder struct {
	chunks    []byte
	formatTag uint16
}

func newWavBuilder(numChans uint16, rate uint32, bits uint16) *wavBuilder {
	b := &wavBuilder{formatTag: 1}
	b.writeFmt(numChans, rate, bits)
	return b
}

func le(v interface{}) []byte {
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func (b *wavBuilder) writeFmt(numChans uint16, rate uint32, bits uint16) {
	blockAlign := numChans * bits / 8
	avg := rate * uint32(blockAlign)
	fmt := new(bytes.Buffer)
	fmt.Write(le(b.formatTag))
	fmt.Write(le(numChans))
	fmt.Write(le(rate))
	fmt.Write(le(avg))
	fmt.Write(le(blockAlign))
	fmt.Write(le(bits))
	b.chunk("fmt ", fmt.Bytes())
}

func (b *wavBuilder) chunk(id string, payload []byte) {
	b.chunks = append(b.chunks, []byte(id)...)
	b.chunks = append(b.chunks, le(uint32(len(payload)))...)
	b.chunks = append(b.chunks, payload...)
	if len(payload)%2 == 1 {
		b.chunks = append(b.chunks, 0)
	}
}

func (b *wavBuilder) data(pcm []byte) {
	b.chunk("data", pcm)
}

func (b *wavBuilder) bytes() []byte {
	body := append([]byte("WAVE"), b.chunks...)
	out := bytes.NewBuffer(nil)
	out.WriteString("RIFF")
	out.Write(le(uint32(len(body))))
	out.Write(body)
	return out.Bytes()
}

// pcmSamples encodes sequential sample values for the given bit depth. 8 bit
// PCM is unsigned, the other depths are signed little-endian integers.
func pcmSamples(bits uint16, samples ...int) []byte {
	out := make([]byte, 0, len(samples)*int(bits/8))
	for _, s := range samples {
		switch bits {
		case 8:
			out = append(out, byte(s))
		case 16:
			v := int16(s)
			out = append(out, byte(v), byte(v>>8))
		case 24:
			v := int32(s)
			out = append(out, byte(v), byte(v>>8), byte(v>>16))
		case 32:
			v := int32(s)
			out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		}
	}
	return out
}

func decodeAll(t *testing.T, d *Decoder, bufSize int) []int {
	t.Helper()
	out := make([]int, 0)
	buf := &audio.IntBuffer{Data: make([]int, bufSize)}
	for {
		n, err := d.PCMBuffer(buf)
		if n > 0 {
			out = append(out, buf.Data[:n]...)
		}
		if err != nil {
			t.Fatalf("unexpected PCMBuffer error: %v", err)
		}
		if n == 0 {
			return out
		}
	}
}

func TestSeekFrame_BitDepthsAndChannels(t *testing.T) {
	depths := []uint16{8, 16, 24, 32}
	channelSets := []uint16{1, 2, 3}
	for _, bits := range depths {
		for _, chans := range channelSets {
			t.Run(fmt.Sprintf("%dbit_%dch", bits, chans), func(t *testing.T) {
				frameCount := 6
				samples := make([]int, 0, int(chans)*frameCount)
				for i := 0; i < frameCount*int(chans); i++ {
					if bits == 8 {
						// 8 bit PCM is unsigned.
						samples = append(samples, (i*7+3)%256)
					} else {
						samples = append(samples, (i+1)-frameCount*int(chans)/2)
					}
				}
				b := newWavBuilder(chans, 8000, bits)
				b.data(pcmSamples(bits, samples...))
				d := NewDecoder(bytes.NewReader(b.bytes()))

				// First call performs header parsing and data chunk location.
				pos, err := d.SeekFrame(2, io.SeekStart)
				if err != nil {
					t.Fatalf("SeekFrame: %v", err)
				}
				if pos != 2 {
					t.Fatalf("position = %d, want 2", pos)
				}
				wantPCMLen := int64(len(samples)) * int64(bits/8)
				if d.PCMLen() != wantPCMLen {
					t.Fatalf("PCMLen = %d, want %d", d.PCMLen(), wantPCMLen)
				}
				got := decodeAll(t, d, 7)
				want := samples[2*int(chans):]
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("samples after seek = %v, want %v", got, want)
				}

				// Seeking to the end then backwards must keep producing data.
				end, err := d.SeekFrame(0, io.SeekEnd)
				if err != nil || end != int64(frameCount) {
					t.Fatalf("SeekEnd = %d, %v, want %d", end, err, frameCount)
				}
				if n, err := d.PCMBuffer(&audio.IntBuffer{Data: make([]int, 3)}); err != nil || n != 0 {
					t.Fatalf("read at end = (%d, %v), want (0, nil)", n, err)
				}
				if pos, err := d.SeekFrame(-4, io.SeekEnd); err != nil || pos != 2 {
					t.Fatalf("SeekEnd -4 = %d, %v, want 2", pos, err)
				}
				got = decodeAll(t, d, 5)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("samples after re-seek = %v, want %v", got, want)
				}

				// Rewind returns to the first frame.
				if err := d.Rewind(); err != nil {
					t.Fatal(err)
				}
				got = decodeAll(t, d, 1)
				if !reflect.DeepEqual(got, samples) {
					t.Fatalf("after rewind = %v, want %v", got, samples)
				}
			})
		}
	}
}

func TestSeekFrame_RoundTripAllWhence(t *testing.T) {
	bits := uint16(16)
	chans := uint16(2)
	samples := []int{10, 11, 20, 21, 30, 31, 40, 41, 50, 51}
	b := newWavBuilder(chans, 44100, bits)
	b.data(pcmSamples(bits, samples...))
	d := NewDecoder(bytes.NewReader(b.bytes()))

	cases := []struct {
		offset int64
		whence int
		want   int64
	}{
		{0, io.SeekStart, 0},
		{3, io.SeekStart, 3},
		{2, io.SeekCurrent, 5},
		{-2, io.SeekCurrent, 1},
		{0, io.SeekCurrent, 3},
		{-1, io.SeekEnd, 4},
		{-5, io.SeekEnd, 0},
		{5, io.SeekStart, 5},
	}
	for _, tc := range cases {
		// Every case starts from frame 3 so SeekCurrent math is deterministic.
		if _, err := d.SeekFrame(3, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		pos, err := d.SeekFrame(tc.offset, tc.whence)
		if err != nil {
			t.Fatalf("SeekFrame(%d, %d): %v", tc.offset, tc.whence, err)
		}
		if pos != tc.want {
			t.Fatalf("SeekFrame(%d, %d) = %d, want %d", tc.offset, tc.whence, pos, tc.want)
		}
		got := decodeAll(t, d, 3)
		want := samples[int(pos)*int(chans):]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("data at frame %d = %v, want %v", pos, got, want)
		}
	}
}

func TestSeekFrame_FullAndStreamMixed(t *testing.T) {
	bits := uint16(16)
	chans := uint16(2)
	samples := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	b := newWavBuilder(chans, 48000, bits)
	b.data(pcmSamples(bits, samples...))
	d := NewDecoder(bytes.NewReader(b.bytes()))

	full, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Data, samples) {
		t.Fatalf("full = %v, want %v", full.Data, samples)
	}
	if pos, err := d.SeekFrame(2, io.SeekStart); err != nil || pos != 2 {
		t.Fatalf("seek after full = (%d, %v)", pos, err)
	}
	stream := decodeAll(t, d, 4)
	if !reflect.DeepEqual(stream, samples[4:]) {
		t.Fatalf("stream = %v, want %v", stream, samples[4:])
	}
	if pos, err := d.SeekFrame(1, io.SeekStart); err != nil || pos != 1 {
		t.Fatalf("seek back = (%d, %v)", pos, err)
	}
	full2, err := d.FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full2.Data, samples[2:]) {
		t.Fatalf("full from frame 1 = %v, want %v", full2.Data, samples[2:])
	}
}

func TestSeekFrame_NonFrameAlignedBuffer(t *testing.T) {
	bits := uint16(16)
	chans := uint16(2)
	samples := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	b := newWavBuilder(chans, 48000, bits)
	b.data(pcmSamples(bits, samples...))
	d := NewDecoder(bytes.NewReader(b.bytes()))
	if _, err := d.SeekFrame(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// 3 samples = 1.5 frames: the byte cursor ends in the middle of a frame.
	buf := &audio.IntBuffer{Data: make([]int, 3)}
	n, err := d.PCMBuffer(buf)
	if err != nil || n != 3 {
		t.Fatalf("PCMBuffer = (%d, %v)", n, err)
	}
	if !reflect.DeepEqual(buf.Data, samples[:3]) {
		t.Fatalf("buf = %v, want %v", buf.Data, samples[:3])
	}
	if _, err := d.SeekFrame(0, io.SeekCurrent); !errors.Is(err, ErrMisalignedFrame) {
		t.Fatalf("SeekCurrent misaligned err = %v, want ErrMisalignedFrame", err)
	}
	// Start and End based seeks re-align the cursor.
	pos, err := d.SeekFrame(1, io.SeekStart)
	if err != nil || pos != 1 {
		t.Fatalf("SeekStart after misalignment = (%d, %v)", pos, err)
	}
	// After re-alignment and reading exactly one frame, SeekCurrent reports frame 2.
	oneFrame := &audio.IntBuffer{Data: make([]int, int(chans))}
	if n, err := d.PCMBuffer(oneFrame); err != nil || n != int(chans) {
		t.Fatalf("one frame read = (%d, %v)", n, err)
	}
	cur, err := d.SeekFrame(0, io.SeekCurrent)
	if err != nil || cur != 2 {
		t.Fatalf("SeekCurrent = (%d, %v), want frame 2", cur, err)
	}
	got := decodeAll(t, d, 2)
	if !reflect.DeepEqual(got, samples[4:]) {
		t.Fatalf("re-aligned data = %v, want %v", got, samples[4:])
	}
}

func TestSeekFrame_OddUnknownChunkBeforeData(t *testing.T) {
	bits := uint16(8)
	samples := []int{10, 20, 30, 40, 50, 60}
	b := &wavBuilder{formatTag: 1}
	b.writeFmt(1, 16000, bits)
	b.chunk("xyz!", []byte{1, 2, 3})
	b.data(pcmSamples(bits, samples...))

	d := NewDecoder(bytes.NewReader(b.bytes()))
	pos, err := d.SeekFrame(3, io.SeekStart)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 3 {
		t.Fatalf("pos = %d", pos)
	}
	got := decodeAll(t, d, 2)
	if !reflect.DeepEqual(got, samples[3:]) {
		t.Fatalf("got %v, want %v", got, samples[3:])
	}
}

func TestSeekFrame_OddPayloadPaddingAndTrailingMetadata(t *testing.T) {
	bits := uint16(8)
	// Odd payload length: one RIFF padding byte follows the data, then more chunks.
	samples := []int{11, 22, 33, 44, 55}
	b := &wavBuilder{formatTag: 1}
	b.writeFmt(1, 22050, bits)
	b.data(pcmSamples(bits, samples...))
	b.chunk("LIST", []byte{9, 8, 7})
	b.chunk("id3 ", []byte{0, 1, 2, 3})

	d := NewDecoder(bytes.NewReader(b.bytes()))
	if _, err := d.SeekFrame(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if d.PCMLen() != int64(len(samples)) {
		t.Fatalf("PCMLen = %d, want %d (padding excluded)", d.PCMLen(), len(samples))
	}
	got := decodeAll(t, d, 2)
	if !reflect.DeepEqual(got, samples) {
		t.Fatalf("decoded %v, want %v (padding/metadata must not be samples)", got, samples)
	}
	full, err := NewDecoder(bytes.NewReader(b.bytes())).FullPCMBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Data, samples) {
		t.Fatalf("full decoded %v, want %v", full.Data, samples)
	}
}

// shortReadSeeker returns at most maxRead bytes per Read call, simulating a
// stream that delivers data in small chunks without ever returning io.EOF
// together with data.
type shortReadSeeker struct {
	data    []byte
	pos     int64
	maxRead int
}

func (s *shortReadSeeker) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := len(p)
	if n > s.maxRead {
		n = s.maxRead
	}
	if remaining := len(s.data) - int(s.pos); n > remaining {
		n = remaining
	}
	start := int(s.pos)
	copy(p, s.data[start:start+n])
	s.pos += int64(n)
	return n, nil
}

func (s *shortReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = s.pos + offset
	case io.SeekEnd:
		target = int64(len(s.data)) + offset
	default:
		return 0, errors.New("bad whence")
	}
	if target < 0 || target > int64(len(s.data)) {
		return 0, errors.New("seek out of range")
	}
	s.pos = target
	return target, nil
}

// eofWithDataSeeker makes the final non-empty Read return data together with
// io.EOF, a behavior allowed by io.Reader.
type eofWithDataSeeker struct {
	data []byte
	pos  int64
}

func (s *eofWithDataSeeker) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.pos:])
	s.pos += int64(n)
	return n, io.EOF
}

func (s *eofWithDataSeeker) Seek(offset int64, whence int) (int64, error) {
	target := offset
	if whence == io.SeekCurrent {
		target += s.pos
	}
	if whence == io.SeekEnd {
		target += int64(len(s.data))
	}
	if target < 0 || target > int64(len(s.data)) {
		return 0, errors.New("seek out of range")
	}
	s.pos = target
	return target, nil
}

func TestPCMBuffer_ShortReadsAndEOFWithData(t *testing.T) {
	bits := uint16(16)
	chans := uint16(2)
	samples := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	raw := pcmSamples(bits, samples...)
	b := newWavBuilder(chans, 32000, bits)
	b.data(raw)
	file := b.bytes()

	for _, maxRead := range []int{1, 2, 3, 5, 7} {
		t.Run(fmt.Sprintf("short-%d", maxRead), func(t *testing.T) {
			d := NewDecoder(&shortReadSeeker{data: file, maxRead: maxRead})
			got := decodeAll(t, d, 3)
			if !reflect.DeepEqual(got, samples) {
				t.Fatalf("short read samples = %v, want %v", got, samples)
			}
		})
	}

	t.Run("eof-with-data", func(t *testing.T) {
		d := NewDecoder(&eofWithDataSeeker{data: file})
		got := decodeAll(t, d, 4)
		if !reflect.DeepEqual(got, samples) {
			t.Fatalf("eof-with-data samples = %v, want %v", got, samples)
		}
	})
}

func TestPCMBuffer_Truncation(t *testing.T) {
	bits := uint16(16)
	chans := uint16(2)
	samples := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	raw := pcmSamples(bits, samples...)
	b := newWavBuilder(chans, 32000, bits)
	b.data(raw)
	file := b.bytes()

	for _, cut := range []int{1, 3, 4, 7} {
		t.Run(fmt.Sprintf("cut-%d", cut), func(t *testing.T) {
			truncated := make([]byte, len(file)-cut)
			copy(truncated, file)
			d := NewDecoder(bytes.NewReader(truncated))
			buf := &audio.IntBuffer{Data: make([]int, len(samples)+2)}
			n, err := d.PCMBuffer(buf)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
			}
			// Only complete samples decoded before the cut are returned.
			expectSamples := (len(raw) - cut) / int(bits/8)
			if n != expectSamples {
				t.Fatalf("n = %d, want %d", n, expectSamples)
			}
			if !reflect.DeepEqual(buf.Data[:n], samples[:n]) {
				t.Fatalf("samples = %v, want %v", buf.Data[:n], samples[:n])
			}
			// Unused slots must be left untouched.
			for i := n; i < len(buf.Data); i++ {
				if buf.Data[i] != 0 {
					t.Fatalf("buf.Data[%d] overwritten: %d", i, buf.Data[i])
				}
			}

			d2 := NewDecoder(bytes.NewReader(truncated))
			full, ferr := d2.FullPCMBuffer()
			if !errors.Is(ferr, io.ErrUnexpectedEOF) {
				t.Fatalf("FullPCMBuffer err = %v, want io.ErrUnexpectedEOF", ferr)
			}
			if !reflect.DeepEqual(full.Data, samples[:expectSamples]) {
				t.Fatalf("full samples = %v, want %v", full.Data, samples[:expectSamples])
			}
		})
	}
}

func TestSeekFrame_InvalidRequestsPreserveState(t *testing.T) {
	bits := uint16(16)
	chans := uint16(2)
	samples := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	b := newWavBuilder(chans, 48000, bits)
	b.data(pcmSamples(bits, samples...))
	d := NewDecoder(bytes.NewReader(b.bytes()))
	if _, err := d.SeekFrame(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	remaining := d.pcmData.remaining
	pos := d.pcmPos

	bad := []struct {
		offset int64
		whence int
		want   error
	}{
		{-1, io.SeekStart, ErrFrameOutOfRange},
		{7, io.SeekStart, ErrFrameOutOfRange},
		{-3, io.SeekCurrent, ErrFrameOutOfRange},
		{100, io.SeekEnd, ErrFrameOutOfRange},
		{1 << 62, io.SeekEnd, ErrFrameOutOfRange},
		{1, 99, ErrInvalidWhence},
		{1 << 62, io.SeekStart, ErrFrameOutOfRange},
	}
	for _, tc := range bad {
		p, err := d.SeekFrame(tc.offset, tc.whence)
		if !errors.Is(err, tc.want) {
			t.Fatalf("SeekFrame(%d, %d) err = %v, want %v", tc.offset, tc.whence, err, tc.want)
		}
		if p != 0 {
			t.Fatalf("invalid seek returned position %d", p)
		}
		if d.pcmPos != pos || d.pcmData.remaining != remaining {
			t.Fatalf("state changed after invalid seek: pos %d->%d remaining %d->%d", pos, d.pcmPos, remaining, d.pcmData.remaining)
		}
	}
	// The decoder still reads correct data from frame 2.
	got := decodeAll(t, d, 4)
	if !reflect.DeepEqual(got, samples[4:]) {
		t.Fatalf("data after invalid seeks = %v, want %v", got, samples[4:])
	}
}

func TestPCMBuffer_DoesNotOverwriteUnusedData(t *testing.T) {
	bits := uint16(8)
	samples := []int{5, 6, 7}
	b := newWavBuilder(1, 8000, bits)
	b.data(pcmSamples(bits, samples...))
	d := NewDecoder(bytes.NewReader(b.bytes()))
	buf := &audio.IntBuffer{Data: []int{100, 101, 102, 103, 104}}
	n, err := d.PCMBuffer(buf)
	if err != nil || n != 3 {
		t.Fatalf("PCMBuffer = (%d, %v)", n, err)
	}
	want := []int{5, 6, 7, 103, 104}
	if !reflect.DeepEqual(buf.Data, want) {
		t.Fatalf("buf = %v, want %v", buf.Data, want)
	}
}

func TestSeekFrame_MalformedFiles(t *testing.T) {
	valid := func() *wavBuilder {
		b := newWavBuilder(2, 48000, 16)
		b.data(pcmSamples(16, 1, 2, 3, 4))
		return b
	}

	t.Run("not RIFF/WAVE", func(t *testing.T) {
		d := NewDecoder(bytes.NewReader([]byte("RIFX....WAVE............")))
		if _, err := d.SeekFrame(0, io.SeekStart); err == nil {
			t.Fatal("expected error for non RIFF container")
		}
	})

	t.Run("missing data", func(t *testing.T) {
		b := &wavBuilder{formatTag: 1}
		b.writeFmt(1, 8000, 16)
		d := NewDecoder(bytes.NewReader(b.bytes()))
		if _, err := d.SeekFrame(0, io.SeekStart); err == nil {
			t.Fatal("expected a clear missing PCM data error, got nil")
		}
	})

	t.Run("non PCM format tag", func(t *testing.T) {
		b := &wavBuilder{formatTag: 3}
		b.writeFmt(2, 48000, 16)
		b.data(pcmSamples(16, 1, 2, 3, 4))
		d := NewDecoder(bytes.NewReader(b.bytes()))
		if _, err := d.SeekFrame(0, io.SeekStart); !errors.Is(err, ErrUnsupportedSeekFormat) {
			t.Fatalf("err = %v, want ErrUnsupportedSeekFormat", err)
		}
	})

	t.Run("unsupported bit depth", func(t *testing.T) {
		b := &wavBuilder{formatTag: 1}
		b.writeFmt(1, 8000, 12)
		b.data([]byte{1, 2, 3})
		d := NewDecoder(bytes.NewReader(b.bytes()))
		if _, err := d.SeekFrame(0, io.SeekStart); !errors.Is(err, ErrUnsupportedSeekFormat) {
			t.Fatalf("err = %v, want ErrUnsupportedSeekFormat", err)
		}
	})

	t.Run("zero channels", func(t *testing.T) {
		b := &wavBuilder{formatTag: 1}
		b.writeFmt(0, 8000, 16)
		b.data(pcmSamples(16, 1, 2))
		d := NewDecoder(bytes.NewReader(b.bytes()))
		if _, err := d.SeekFrame(0, io.SeekStart); err == nil {
			t.Fatal("expected error for zero channels")
		}
	})

	t.Run("payload not frame aligned", func(t *testing.T) {
		b := &wavBuilder{formatTag: 1}
		b.writeFmt(2, 8000, 16)
		// 3 bytes with 4-byte frames cannot form a complete frame.
		b.data([]byte{1, 2, 3})
		d := NewDecoder(bytes.NewReader(b.bytes()))
		if _, err := d.SeekFrame(0, io.SeekStart); err == nil {
			t.Fatal("expected error for non frame aligned payload")
		}
	})

	t.Run("inconsistent block align", func(t *testing.T) {
		raw := valid().bytes()
		// BlockAlign sits at RIFF offset 32 (12 + fmt header 8 + 12 payload bytes).
		raw[32] = 99
		d := NewDecoder(bytes.NewReader(raw))
		if _, err := d.SeekFrame(0, io.SeekStart); err == nil {
			t.Fatal("expected error for inconsistent block align")
		}
	})
}

// countingSeeker wraps a ReadSeeker and counts bytes consumed through Read,
// proving SeekFrame jumps directly instead of decoding/discarding the prefix.
type countingSeeker struct {
	r         io.ReadSeeker
	readBytes int
}

func (c *countingSeeker) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.readBytes += n
	return n, err
}

func (c *countingSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}

func TestSeekFrame_DoesNotDecodePrefix(t *testing.T) {
	bits := uint16(16)
	samples := make([]int, 64)
	for i := range samples {
		samples[i] = i
	}
	b := newWavBuilder(1, 8000, bits)
	b.data(pcmSamples(bits, samples...))
	cs := &countingSeeker{r: bytes.NewReader(b.bytes())}
	d := NewDecoder(cs)
	if pos, err := d.SeekFrame(30, io.SeekStart); err != nil || pos != 30 {
		t.Fatalf("SeekFrame = (%d, %v)", pos, err)
	}
	// Header discovery reads only the structural chunks, not the PCM prefix.
	if cs.readBytes > 64 {
		t.Fatalf("SeekFrame consumed %d bytes through Read; prefix must be skipped via Seek", cs.readBytes)
	}
	got := decodeAll(t, d, 8)
	if !reflect.DeepEqual(got, samples[30:]) {
		t.Fatalf("data = %v, want %v", got, samples[30:])
	}
}

func TestSeekFrame_FixturesRoundTrip(t *testing.T) {
	fixtures := []struct {
		path       string
		frameAt    int64
		bufSamples int
	}{
		{"fixtures/bass.wav", 1000, 257},
		{"fixtures/kick.wav", 100, 255},
		{"fixtures/8bit.wav", 50, 127},
		{"fixtures/32bit.wav", 33, 200},
	}
	for _, fx := range fixtures {
		t.Run(fx.path, func(t *testing.T) {
			f, err := os.Open(fx.path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d := NewDecoder(f)
			full, err := d.FullPCMBuffer()
			if err != nil {
				t.Fatal(err)
			}
			chans := int(d.NumChans)
			totalFrames := len(full.Data) / chans
			if fx.frameAt >= int64(totalFrames) {
				t.Fatalf("bad test offset %d for %d frames", fx.frameAt, totalFrames)
			}
			if pos, err := d.SeekFrame(fx.frameAt, io.SeekStart); err != nil || pos != fx.frameAt {
				t.Fatalf("SeekFrame = (%d, %v)", pos, err)
			}
			got := decodeAll(t, d, fx.bufSamples)
			want := full.Data[int(fx.frameAt)*chans:]
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("seeked stream length/content mismatch: got %d samples, want %d", len(got), len(want))
			}
			// SeekCurrent at an aligned position works and Rewind resets.
			if pos, err := d.SeekFrame(0, io.SeekCurrent); err != nil || pos != int64(totalFrames) {
				t.Fatalf("SeekCurrent at end = (%d, %v), want %d", pos, err, totalFrames)
			}
			if err := d.Rewind(); err != nil {
				t.Fatal(err)
			}
			first := decodeAll(t, d, fx.bufSamples)
			if !reflect.DeepEqual(first, full.Data) {
				t.Fatalf("data after Rewind does not match full buffer")
			}
		})
	}
}
