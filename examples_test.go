package wav

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/go-audio/audio"
)

// buildStereoWAV builds a tiny in-memory stereo 16-bit PCM WAV containing
// frameCount frames, followed by a trailing metadata chunk.
func buildStereoWAV(frameCount int) []byte {
	const channels, bits, rate = 2, 16, 8000
	frameSize := channels * bits / 8
	pcm := make([]byte, frameCount*frameSize)
	for frame := 0; frame < frameCount; frame++ {
		for channel := 0; channel < channels; channel++ {
			value := int16(frame*10 + channel)
			offset := (frame*channels + channel) * (bits / 8)
			binary.LittleEndian.PutUint16(pcm[offset:], uint16(value))
		}
	}
	fmtChunk := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtChunk[0:2], 1) // PCM
	binary.LittleEndian.PutUint16(fmtChunk[2:4], channels)
	binary.LittleEndian.PutUint32(fmtChunk[4:8], rate)
	binary.LittleEndian.PutUint32(fmtChunk[8:12], rate*uint32(frameSize))
	binary.LittleEndian.PutUint16(fmtChunk[12:14], uint16(frameSize))
	binary.LittleEndian.PutUint16(fmtChunk[14:16], bits)
	body := append([]byte("fmt \x10\x00\x00\x00"), fmtChunk...)
	body = append(body, 'd', 'a', 't', 'a')
	var dataSize [4]byte
	binary.LittleEndian.PutUint32(dataSize[:], uint32(len(pcm)))
	body = append(body, dataSize[:]...)
	body = append(body, pcm...)
	// A trailing chunk must never be read as PCM; its size is even so no
	// alignment padding is required.
	body = append(body, 'L', 'I', 'S', 'T', 0x04, 0, 0, 0, 'I', 'N', 'F', 'O')
	out := []byte("RIFF")
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(4+len(body)))
	out = append(out, header...)
	out = append(out, 'W', 'A', 'V', 'E')
	return append(out, body...)
}

func ExampleDecoder_SeekFrame() {
	// 6 stereo frames = 12 sample values = 24 PCM bytes. Frames are shared
	// across channels, so SeekFrame counts frames, not sample values.
	d := NewDecoder(bytes.NewReader(buildStereoWAV(6)))
	pos, err := d.SeekFrame(3, io.SeekStart)
	if err != nil {
		log.Fatal(err)
	}
	frameBytes := int64(d.NumChans) * (int64(d.BitDepth) / 8)
	fmt.Printf("seeked to frame %d of %d\n", pos, d.PCMLen()/frameBytes)

	// One stereo frame per read: n counts sample values, not frames.
	buf := &audio.IntBuffer{Format: d.Format(), Data: make([]int, d.NumChans)}
	n, err := d.PCMBuffer(buf)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("read %d sample values: %v\n", n, buf.Data[:n])

	// io.SeekEnd is measured from the total frame count; -1 lands on the last
	// frame and the trailing LIST chunk remains invisible to PCM reads.
	if _, err := d.SeekFrame(-1, io.SeekEnd); err != nil {
		log.Fatal(err)
	}
	n, err = d.PCMBuffer(buf)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("last frame: %v\n", buf.Data[:n])

	// Output:
	// seeked to frame 3 of 6
	// read 2 sample values: [30 31]
	// last frame: [50 51]
}

func ExampleDecoder_Duration() {
	f, err := os.Open("fixtures/kick.wav")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	dur, err := NewDecoder(f).Duration()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s duration: %s\n", f.Name(), dur)
	// Output: fixtures/kick.wav duration: 204.172335ms
}

func ExampleDecoder_IsValidFile() {
	f, err := os.Open("fixtures/kick.wav")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fmt.Printf("is this file valid: %t", NewDecoder(f).IsValidFile())
	// Output: is this file valid: true
}

func ExampleEncoder_Write() {
	f, err := os.Open("fixtures/kick.wav")
	if err != nil {
		panic(fmt.Sprintf("couldn't open audio file - %v", err))
	}

	// Decode the original audio file
	// and collect audio content and information.
	d := NewDecoder(f)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		panic(err)
	}
	f.Close()
	fmt.Println("Old file ->", d)

	// Destination file
	out, err := os.Create("testOutput/kick.wav")
	if err != nil {
		panic(fmt.Sprintf("couldn't create output file - %v", err))
	}

	// setup the encoder and write all the frames
	e := NewEncoder(out,
		buf.Format.SampleRate,
		int(d.BitDepth),
		buf.Format.NumChannels,
		int(d.WavAudioFormat))
	if err = e.Write(buf); err != nil {
		panic(err)
	}
	// close the encoder to make sure the headers are properly
	// set and the data is flushed.
	if err = e.Close(); err != nil {
		panic(err)
	}
	out.Close()

	// reopen to confirm things worked well
	out, err = os.Open("testOutput/kick.wav")
	if err != nil {
		panic(err)
	}
	d2 := NewDecoder(out)
	d2.ReadInfo()
	fmt.Println("New file ->", d2)
	out.Close()
	os.Remove(out.Name())

	// Output:
	// Old file -> Format: WAVE - 1 channels @ 22050 / 16 bits - Duration: 0.204172 seconds
	// New file -> Format: WAVE - 1 channels @ 22050 / 16 bits - Duration: 0.204172 seconds
}

func ExampleDecoder_ReadMetadata() {
	f, err := os.Open("fixtures/listinfo.wav")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	d := NewDecoder(f)
	d.ReadMetadata()
	if d.Err() != nil {
		log.Fatal(err)
	}
	fmt.Printf("%#v\n", d.Metadata)
	// Output:
	// &wav.Metadata{SamplerInfo:(*wav.SamplerInfo)(nil), Artist:"artist", Comments:"my comment", Copyright:"", CreationDate:"2017", Engineer:"", Technician:"", Genre:"genre", Keywords:"", Medium:"", Title:"track title", Product:"album title", Subject:"", Software:"", Source:"", Location:"", TrackNbr:"42", CuePoints:[]*wav.CuePoint(nil)}
}
