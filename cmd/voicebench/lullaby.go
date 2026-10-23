package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"

	"github.com/windrunner21/lullaby"
	"github.com/windrunner21/lullaby/internal/netguard"
	"github.com/windrunner21/lullaby/songs"
)

// quiet is an app that shows nothing.
type quiet struct{}

func (quiet) Started(snowflake.ID, *lullaby.Track)            {}
func (quiet) Ended(snowflake.ID, *lullaby.Track, lullaby.End) {}
func (quiet) Left(snowflake.ID, *lullaby.Track)               {}
func (quiet) QueueChanged(snowflake.ID)                       {}

// audioWatch notes when the first request for a song's audio goes out after
// a mark, which is when the song was found.
type audioWatch struct {
	base http.RoundTripper
	mu   sync.Mutex
	mark time.Time
	at   time.Time
}

func (w *audioWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Hostname(), "googlevideo.com") {
		w.mu.Lock()
		if w.at.IsZero() && time.Now().After(w.mark) {
			w.at = time.Now()
		}
		w.mu.Unlock()
	}
	return w.base.RoundTrip(r)
}

func (w *audioWatch) reset(mark time.Time) {
	w.mu.Lock()
	w.mark, w.at = mark, time.Time{}
	w.mu.Unlock()
}

func (w *audioWatch) found() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

// runLullaby plays the warm-up and then each link with lullaby, as a bot
// using it does: warmed up at start, and in voice already, as it is by the
// time someone has typed a song. Then it plays each link again, found already.
func runLullaby(certFile, warmUp string, links []string) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	pem, err := os.ReadFile(certFile)
	if err != nil {
		log.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	dialer := &websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots}, HandshakeTimeout: 5 * time.Second}

	dave := lullaby.NewDAVE()
	var manager voice.Manager
	endpoint := gatewayAddr
	manager = voice.NewManager(func(_ context.Context, guildID snowflake.ID, channelID *snowflake.ID, _, _ bool) error {
		if channelID == nil {
			return nil
		}
		// What Discord's gateway would answer with.
		go func() {
			manager.HandleVoiceStateUpdate(gateway.EventVoiceStateUpdate{VoiceState: discord.VoiceState{
				GuildID: guildID, ChannelID: channelID, UserID: botID, SessionID: "bench"}})
			manager.HandleVoiceServerUpdate(gateway.EventVoiceServerUpdate{Token: "bench", GuildID: guildID, Endpoint: &endpoint})
		}()
		return nil
	}, botID, voice.WithDaveSessionCreateFunc(dave.Create),
		voice.WithConnConfigOpts(voice.WithConnGatewayConfigOpts(voice.WithGatewayDialer(dialer))))

	// The client New makes by default, guarded the same way, with a watch
	// around it; New would otherwise check each request's host on its own.
	client := netguard.Client(lullaby.NewClient())
	watch := &audioWatch{base: client.Transport}
	client.Transport = watch
	engine, err := lullaby.New(lullaby.Config{Voice: manager, DAVE: dave, Events: quiet{}, HTTP: client, AllowPrivateLinks: true})
	if err != nil {
		log.Fatal(err)
	}

	self := os.Getpid()

	ctx := context.Background()
	engine.Start(ctx) // as a bot does when it starts
	time.Sleep(3 * time.Second)
	engine.JoinEarly(lullabyGuild, voiceChannel) // as a bot does while someone types
	if err := control(fmt.Sprintf("/ready?guild=%d", lullabyGuild), nil); err != nil {
		log.Fatal("never joined voice: ", err)
	}

	markAs(self, "idle")
	time.Sleep(10 * time.Second)
	markAs(self, "")
	play := func(link string, timed bool) {
		r := result{Engine: "lullaby", Link: link}
		if timed {
			markAs(self, "playing")
		}
		start := time.Now()
		watch.reset(start)
		t, _, err := engine.Play(context.Background(), lullaby.Request{GuildID: lullabyGuild, ChannelID: voiceChannel, Query: link})
		if err != nil {
			r.Err = err.Error()
			markAs(self, "")
			report(r)
			if errors.Is(err, songs.ErrBotCheck) {
				log.Fatal("YouTube wants proof lullaby isn't a robot: stopping")
			}
			return
		}
		r.Title = t.Song().Title
		first, err := firstAudio(lullabyGuild, start)
		if err != nil {
			r.fail(err)
		} else {
			r.First = ms(first.Sub(start))
			if found := watch.found(); !found.IsZero() {
				r.Lookup = ms(found.Sub(start))
			}
		}
		time.Sleep(playFor)
		if err == nil {
			pacing(&r, lullabyGuild, first, first.Add(playFor))
		}
		engine.Skip(lullabyGuild, 0)
		if timed {
			markAs(self, "after")
		}
		time.Sleep(2 * time.Second) // ffmpeg is gone, and its CPU time counted
		markAs(self, "")
		if !timed {
			r.Engine += "-warmup" // the first song after starting, not counted in the usage
		}
		report(r)
	}

	log.Printf("warm-up: %s", warmUp)
	play(warmUp, false)
	for _, link := range links {
		play(link, true)
	}
	// The same songs again: found already, like a link pasted while the
	// person was still choosing, which a bot can look up meanwhile with
	// Suggest.
	for _, link := range links {
		r := result{Engine: "lullaby-known", Link: link}
		start := time.Now()
		t, _, err := engine.Play(context.Background(), lullaby.Request{GuildID: lullabyGuild, ChannelID: voiceChannel, Query: link})
		if err == nil {
			r.Title = t.Song().Title
			if first, err := firstAudio(lullabyGuild, start); err != nil {
				r.fail(err)
			} else {
				r.First = ms(first.Sub(start))
			}
			time.Sleep(3 * time.Second)
			engine.Skip(lullabyGuild, 0)
			time.Sleep(time.Second)
		} else {
			r.Err = err.Error()
		}
		report(r)
	}
	summary("lullaby", self)
	engine.Stop(lullabyGuild)
	time.Sleep(time.Second)
}
