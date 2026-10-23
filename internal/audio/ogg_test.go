package audio

import (
	"bytes"
	"io"
	"testing"
)

// oggPage builds an Ogg page out of segments of at most 255 bytes.
func oggPage(segments ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString("OggS")
	b.Write(make([]byte, 22)) // version, type, position, serial, sequence and checksum, which aren't read
	b.WriteByte(byte(len(segments)))
	for _, s := range segments {
		b.WriteByte(byte(len(s)))
	}
	for _, s := range segments {
		b.Write(s)
	}
	return b.Bytes()
}

// segments cuts a packet into Ogg segments: 255 bytes each, then a shorter
// one, which is empty when the packet's size is a multiple of 255.
func segments(packet []byte) [][]byte {
	var out [][]byte
	for len(packet) >= 255 {
		out = append(out, packet[:255])
		packet = packet[255:]
	}
	return append(out, packet)
}

func TestOgg(t *testing.T) {
	small := []byte{0xfc, 0xff, 0xfe}
	exact := bytes.Repeat([]byte{1}, 255)
	long := bytes.Repeat([]byte{2}, 600) // goes on into the next page
	longParts := segments(long)

	var stream bytes.Buffer
	stream.Write(oggPage(segments([]byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00"))...))
	stream.Write(oggPage(segments([]byte("OpusTags\x08\x00\x00\x00Lavf61.7"))...))
	stream.Write(oggPage(append(segments(small), segments(exact)...)...))
	stream.Write(oggPage(longParts[:2]...))
	stream.Write(oggPage(longParts[2:]...))

	o := newOggReader(&stream)
	for i, want := range [][]byte{small, exact, long} {
		got, err := o.next()
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("packet %d is %d bytes, want %d", i, len(got), len(want))
		}
	}
	if _, err := o.next(); err != io.EOF {
		t.Errorf("after the last packet: %v, want io.EOF", err)
	}
}

func TestOggBroken(t *testing.T) {
	page := oggPage(segments([]byte("OpusHead"))...)
	tests := map[string][]byte{
		"not Ogg":   []byte("ID3\x04\x00\x00\x00\x00\x00\x00 this is an mp3 file, not an Ogg one"),
		"cut short": page[:len(page)-3],
	}
	for name, data := range tests {
		if _, err := newOggReader(bytes.NewReader(data)).next(); err == nil || err == io.EOF {
			t.Errorf("%s: error = %v, want a real error", name, err)
		}
	}
}
