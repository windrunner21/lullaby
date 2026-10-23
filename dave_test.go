package lullaby

import (
	"bytes"
	"log/slog"
	"slices"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

// fakeConn stands in for a disgo voice connection, which DAVE gets as the
// session's callbacks.
type fakeConn struct{ guild snowflake.ID }

func (c *fakeConn) GuildID() snowflake.ID                 { return c.guild }
func (c *fakeConn) SendMLSKeyPackage([]byte) error        { return nil }
func (c *fakeConn) SendMLSCommitWelcome([]byte) error     { return nil }
func (c *fakeConn) SendReadyForTransition(uint16) error   { return nil }
func (c *fakeConn) SendInvalidCommitWelcome(uint16) error { return nil }

func TestBigFramesFit(t *testing.T) {
	d := NewDAVE()
	conn := &fakeConn{guild: 1}
	s := d.Create(slog.Default(), "1561439000512831680", conn)
	defer d.forget(1, conn)

	// At 128 kbit/s, a 20 ms frame can be over 600 bytes. disgo v0.19.6 makes
	// room for it like this: still 512 long, with the capacity grown.
	frame := bytes.Repeat([]byte{0xfc}, 650)
	buf := slices.Grow(make([]byte, 512), s.MaxEncryptedFrameSize(len(frame)))
	n, err := s.Encrypt(1, frame, buf)
	if err != nil {
		t.Fatalf("a 650-byte frame: %v", err)
	}
	// Before encryption starts, frames go through as they are.
	if !bytes.Equal(buf[:n], frame) {
		t.Errorf("got %d bytes back, want the 650-byte frame", n)
	}
}

func TestDAVE(t *testing.T) {
	d := NewDAVE()
	old, current := &fakeConn{guild: 1}, &fakeConn{guild: 1}
	d.Create(nil, "1561439000512831680", old)
	d.Create(nil, "1561439000512831680", current)

	if d.hold(1) {
		t.Error("a session that hasn't started encrypting yet holds frames")
	}
	// The player left the old connection after joining again: the new session stays.
	d.forget(1, old)
	if _, ok := d.sessions[1]; !ok {
		t.Fatal("forgetting an old connection closed the new one's session")
	}
	d.forget(1, current)
	if _, ok := d.sessions[1]; ok {
		t.Error("the session is still kept after its connection was forgotten")
	}
}
