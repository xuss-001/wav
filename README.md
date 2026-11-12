# wav codec

[![GoDoc](https://godoc.org/github.com/go-audio/wav?status.svg)](https://godoc.org/github.com/go-audio/wav)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-audio/wav)](https://goreportcard.com/report/github.com/go-audio/wav)
[![Coverage Status](https://codecov.io/gh/go-audio/wav/graph/badge.svg)](https://codecov.io/gh/go-audio/wav)
[![Build Status](https://travis-ci.org/go-audio/wav.svg)](https://travis-ci.org/go-audio/wav)

Wav audio file encoder and decoder. See [GoDoc](https://godoc.org/github.com/go-audio/wav) for more details.

## Seeking to audio frames

`Decoder.SeekFrame(offset, whence)` moves the read cursor in **audio frames**
without decoding or buffering the PCM payload. It supports `io.SeekStart`,
`io.SeekCurrent` and `io.SeekEnd`; position `0` is the first frame of the first
`data` chunk and the valid range is `[0, total frame count]`, inclusive. Header
parsing and `data` chunk lookup happen automatically on the first call, so
chunks such as `LIST`, `JUNK` or vendor chunks (including odd-size alignment
padding) between `fmt ` and `data` are skipped correctly.

Terminology:

- A **sample value** is one integer from one channel (1, 2, 3 or 4 bytes for
  8, 16, 24 or 32-bit integer PCM).
- A **frame** contains one sample value for every channel. With `N` channels
  and `B` bytes per sample, one frame is `N*B` bytes.
- A **byte** is a position in the underlying file. `Seek` works in bytes and is
  a direct proxy to the underlying reader; `SeekFrame` works in frames and keeps
  the decoder's remaining-payload state consistent. `PCMLen()` reports the
  declared `data` payload size in bytes, excluding RIFF padding and any chunk
  following `data`.

Extract a multi-channel preview without loading the whole recording:

```go
f, err := os.Open("recording.wav")
if err != nil {
    log.Fatal(err)
}
defer f.Close()

d := wav.NewDecoder(f)
// Frames are shared across channels: 10000 frames are 20000 stereo samples.
if _, err := d.SeekFrame(int64(d.SampleRate)*5, io.SeekStart); err != nil {
    log.Fatal(err) // unsupported format, malformed header, missing data, etc.
}

frameCount := int64(d.SampleRate) * 2 // a two-second stereo preview
buf := &audio.IntBuffer{
    Format: d.Format(),
    Data:   make([]int, int(d.NumChans)*4096),
}
var framesRead int64
for framesRead < frameCount {
    n, err := d.PCMBuffer(buf)
    if err != nil {
        // io.ErrUnexpectedEOF means the physical file ended before the
        // declared data payload; n still contains every complete sample
        // decoded before the truncation.
        log.Fatal(err)
    }
    if n == 0 {
        break // clean end of the declared payload: (0, nil)
    }
    process(buf.Data[:n])
    framesRead += int64(n) / int64(d.NumChans)
}
```

Boundary and error behavior:

- Only RIFF/WAVE integer PCM (format tag `1`) at 8, 16, 24 or 32 bits is
  supported. Channel count, sample rate, block alignment and average byte rate
  are validated for self-consistency; malformed headers, missing `data` chunks
  and payloads that cannot form whole frames return errors instead of panicking.
- A read landing in the middle of a frame makes `io.SeekCurrent` fail; use
  `io.SeekStart` or `io.SeekEnd` to realign. Invalid `whence`, negative or
  out-of-range targets and integer overflow leave the current position and
  remaining payload untouched.
- Short reads and a read returning data together with `io.EOF` never lose or
  duplicate samples. A file truncated before the declared payload ends returns
  the complete samples decoded so far plus an error matching
  `errors.Is(err, io.ErrUnexpectedEOF)`; reaching the declared payload boundary
  ends normally with `(0, nil)`. RIFF padding and metadata after `data` are not
  decoded as audio.
