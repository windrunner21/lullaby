package audio

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// oggReader takes the Opus packets out of an Ogg stream, as ffmpeg writes it,
// and skips the two header packets at the start. Each packet is one frame.
type oggReader struct {
	r       *bufio.Reader
	header  [27]byte
	lacing  [255]byte
	packets [][]byte // finished and waiting to be returned
	partial []byte   // a packet that goes on in the next page
	seen    int      // packets so far, the headers included
}

func newOggReader(r io.Reader) *oggReader {
	return &oggReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next Opus packet, or io.EOF after the last.
func (o *oggReader) next() ([]byte, error) {
	for len(o.packets) == 0 {
		if err := o.page(); err != nil {
			return nil, err
		}
	}
	p := o.packets[0]
	o.packets = o.packets[1:]
	return p, nil
}

// page reads one Ogg page and keeps the packets that end in it.
func (o *oggReader) page() error {
	if _, err := io.ReadFull(o.r, o.header[:]); err != nil {
		if err == io.EOF {
			return io.EOF // between pages: the clean end
		}
		return fmt.Errorf("ogg: %w", err)
	}
	if string(o.header[:4]) != "OggS" {
		return errors.New("ogg: not an Ogg page")
	}
	// The lacing values are the sizes of the page's segments. A packet ends
	// with the first segment shorter than 255 bytes.
	lacing := o.lacing[:o.header[26]]
	if _, err := io.ReadFull(o.r, lacing); err != nil {
		return fmt.Errorf("ogg: %w", unexpected(err))
	}
	size := 0
	for _, l := range lacing {
		size += int(l)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(o.r, body); err != nil {
		return fmt.Errorf("ogg: %w", unexpected(err))
	}

	start, end := 0, 0
	for _, l := range lacing {
		end += int(l)
		if l == 255 {
			continue
		}
		packet := body[start:end]
		if o.partial != nil {
			packet = append(o.partial, packet...)
			o.partial = nil
		}
		start = end
		if o.seen++; o.seen > 2 {
			o.packets = append(o.packets, packet)
		}
	}
	if start < end {
		o.partial = append(o.partial, body[start:end]...)
	}
	return nil
}

// unexpected turns an EOF in the middle of a page into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
