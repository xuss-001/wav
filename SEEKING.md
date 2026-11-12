# Frame-accurate seeking with `Decoder.SeekFrame`

`SeekFrame` supports offline clip extraction from large WAV files without
decoding (or buffering) the whole recording. The first call parses the
RIFF/WAVE header and locates the first `data` chunk by walking the real chunk
list, so headers of any size, `LIST`/`JUNK`/unknown chunks and odd-size chunk
padding are handled correctly. Subsequent seeks move the underlying
`io.ReadSeeker` directly; the PCM prefix is never decoded and discarded.

## Frames, samples and bytes

- A **sample** is one numeric value for one channel at one instant.
- An audio **frame** contains one sample for every channel, so one frame is
  `NumChannels` samples.
- A **byte** is the on-disk unit. One sample uses `BitDepth/8` bytes, so one
  frame is `NumChannels * BitDepth/8` bytes.
- `SeekFrame` offsets and its return value are always **frame numbers**
  relative to the first valid PCM byte of the `data` chunk. `PCMLen()` still
  reports the declared payload size in **bytes** and excludes RIFF padding.

For a stereo 16-bit file, one frame is 2 samples = 4 bytes. Seeking to frame
10 skips 40 bytes of PCM, and the next `PCMBuffer` call starts at the sample
pair of frame 10.

## Supported formats and errors

Only RIFF/WAVE files with format tag `1` (uncompressed integer PCM) and bit
depths of 8, 16, 24 or 32 bits are supported. The channel count and sample
rate must be valid, block alignment and byte rate must be self consistent,
and the declared payload must contain an integer number of frames.

Errors are returned instead of panicking for malformed headers, a missing
`data` chunk, an unsupported format or a payload that cannot form complete
frames. The sentinel errors can be inspected with `errors.Is`:

- `ErrInvalidWhence`: the whence argument is not one of `io.SeekStart`,
  `io.SeekCurrent` or `io.SeekEnd`.
- `ErrFrameOutOfRange`: the target frame is negative or beyond the total frame
  count, including integer overflow.
- `ErrMisalignedFrame`: `io.SeekCurrent` was requested while the byte cursor
  was in the middle of a frame (for example after a non frame aligned
  `PCMBuffer`). `io.SeekStart` and `io.SeekEnd` can always re-align it.
- `ErrUnsupportedSeekFormat`: the container or PCM format is not supported.

Invalid seek requests never mutate the read position or remaining payload
range, so the following read still sees the same data. Positions are bounded
by `[0, totalFrames]`; seeking to `totalFrames` is legal and the following read
terminates with `(0, nil)`.

## Reading and truncation behavior

- `PCMBuffer` counts only fully decoded samples in `n` and never overwrites
  `buf.Data[n:]`; buffers that do not hold an integer number of frames remain
  supported.
- Reaching the declared end of the payload keeps the historical `(0, nil)`
  termination; the RIFF padding byte and chunks following `data` are never
  decoded as samples.
- If the physical stream ends before the declared payload, the complete
  samples decoded so far are returned together with an `io.ErrUnexpectedEOF`
  recognizable via `errors.Is`. `FullPCMBuffer` follows the same boundary and
  truncation rules.
- `Rewind` always returns to frame zero. The byte-level `Seek` keeps its
  original raw byte semantics; do not mix arbitrary byte `Seek` calls with
  `SeekFrame`/`PCMBuffer` on the same decoder.

## Example: extract a stereo preview clip

```go
package main

import (
	"errors"
	"io"
	"os"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

func extractPreview(path string, startFrame, maxFrames int64) (*audio.IntBuffer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	d := wav.NewDecoder(f)
	if _, err := d.SeekFrame(startFrame, io.SeekStart); err != nil {
		return nil, err
	}

	// Each frame contributes NumChannels samples. Small fixed buffers keep the
	// memory footprint constant regardless of the file size.
	channels := int(d.NumChans)
	chunkFrames := 1024
	buf := &audio.IntBuffer{Data: make([]int, chunkFrames*channels)}
	out := &audio.IntBuffer{Format: d.Format(), SourceBitDepth: int(d.BitDepth)}

	var readFrames int64
	for readFrames < maxFrames {
		n, err := d.PCMBuffer(buf)
		if n > 0 {
			out.Data = append(out.Data, buf.Data[:n]...)
			readFrames += int64(n) / int64(channels)
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return out, err // physical file truncated before declared end
		}
		if err != nil {
			return out, err
		}
		if n == 0 {
			break // declared payload reached with (0, nil)
		}
	}

	if maxFrames < readFrames {
		keep := maxFrames * int64(channels)
		out.Data = out.Data[:keep]
	}
	return out, nil
}
```
