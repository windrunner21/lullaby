package lullaby

import (
	"log/slog"
	"sync"

	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/thomas-vilte/dave-go/session"
)

// DAVE keeps the end-to-end encryption session of each server's voice
// connection, which Discord has required since 2026-03-01. Songs wait while a
// session is being set up, so their start isn't sent unencrypted and dropped.
type DAVE struct {
	mu       sync.Mutex
	sessions map[snowflake.ID]daveSession
}

type daveSession struct {
	conn any // the voice connection it belongs to
	*session.Session
}

// NewDAVE returns a DAVE with no sessions yet.
func NewDAVE() *DAVE {
	return &DAVE{sessions: make(map[snowflake.ID]daveSession)}
}

// Create makes the session for a new voice connection. It's the
// godave.SessionCreateFunc for disgo's voice manager, which passes the
// connection in as callbacks.
func (d *DAVE) Create(logger *slog.Logger, userID godave.UserID, callbacks godave.Callbacks) godave.Session {
	s := session.New(userID, callbacks, session.WithLogger(logger))
	conn, ok := callbacks.(interface{ GuildID() snowflake.ID })
	if !ok {
		return roomy{s}
	}
	d.mu.Lock()
	old, had := d.sessions[conn.GuildID()]
	d.sessions[conn.GuildID()] = daveSession{conn: callbacks, Session: s}
	d.mu.Unlock()
	if had {
		old.Close()
	}
	return roomy{s}
}

// roomy hands dave-go the whole buffer disgo made for an encrypted frame.
// disgo v0.19.6 grows the buffer's capacity for a big frame but passes it at
// its old length of 512, which dave-go reads as too small. Every frame over
// about 490 bytes was dropped, and songs stuttered.
type roomy struct{ *session.Session }

func (r roomy) Encrypt(ssrc uint32, frame, encrypted []byte) (int, error) {
	return r.Session.Encrypt(ssrc, frame, encrypted[:cap(encrypted)])
}

// hold reports whether a server's frames should wait, because its encryption
// is still being set up.
func (d *DAVE) hold(guildID snowflake.ID) bool {
	d.mu.Lock()
	s, ok := d.sessions[guildID]
	d.mu.Unlock()
	return ok && s.ShouldHoldFrames()
}

// forget closes the session of a voice connection the player has left. A
// newer connection's session in the same server is left alone.
func (d *DAVE) forget(guildID snowflake.ID, conn any) {
	d.mu.Lock()
	s, ok := d.sessions[guildID]
	if ok && s.conn == conn {
		delete(d.sessions, guildID)
	}
	d.mu.Unlock()
	if ok && s.conn == conn {
		s.Close()
	}
}
