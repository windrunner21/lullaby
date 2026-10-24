// Package lullaby plays music in Discord voice channels for bots built on
// disgo: songs one after another in each server, without a gap between them.
// It finds songs on YouTube, YouTube Music, SoundCloud and the music
// services, has ffmpeg encode them to Opus, and sends them through disgo's
// voice package, encrypted with dave-go. Package songs has the Song type and
// the errors songs fail with. How the music shows, like a bot's song cards,
// is up to the app, which Events tells what happens.
//
// Times in the comments were measured on a Raspberry Pi 5 with 8 GB, "the
// Pi", on a home connection.
package lullaby

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby/internal/audio"
	"github.com/windrunner21/lullaby/internal/netguard"
	"github.com/windrunner21/lullaby/internal/redact"
	"github.com/windrunner21/lullaby/internal/sources"
	"github.com/windrunner21/lullaby/songs"
)

const (
	// idleFor is how long the player stays in voice once the music stops.
	idleFor = 2 * time.Minute
	// earlyFor is how long the player stays after joining early, while
	// someone typed, if no song comes.
	earlyFor = 30 * time.Second
	// maxPause is how long a song may stay paused before the player leaves.
	maxPause = 10 * time.Minute
	// joinTimeout is how long joining a voice channel may take.
	joinTimeout = 10 * time.Second
	// leaveTimeout is how long leaving may take.
	leaveTimeout = 5 * time.Second
	// MaxSuggestions is how many songs Suggest suggests at most.
	MaxSuggestions = 10
	// suggestedFor is how long suggestions are remembered.
	suggestedFor = 15 * time.Minute
	// maxSuggested is how many suggestions are remembered at most.
	maxSuggested = 500
	// searchedFor is how long a search's results are reused for the same
	// words, unless Config.SearchMemory says otherwise.
	searchedFor = 10 * time.Minute
	// emptyFor is how long a search that found nothing is believed, however
	// long others are kept.
	emptyFor = time.Minute
	// maxSearched is how many searches are remembered at most. Every
	// keystroke's words are one, and one of 20 songs takes about 10 KB, so
	// a full memory is about 10 MB.
	maxSearched = 1000
	// searchTimeout is the longest a search may take, even with nobody
	// waiting for it anymore.
	searchTimeout = 10 * time.Second
	// maxRadios is how many songs' radios are remembered at most. One is
	// about 50 songs, about 24 KB on the Pi, so a full memory is about 5 MB.
	maxRadios = 200
	// settleAfter is how long someone has stopped typing before the first
	// song suggested to them is looked up: long enough that a fast typist's
	// keystrokes don't each cost a lookup, short enough that it's ready
	// before they've picked it.
	settleAfter = 300 * time.Millisecond
	// maxLookAhead, maxLookAheadDay and maxLookingAhead bound the early
	// lookups that ask YouTube: in an hour, in a day, and at once. They're
	// far more than friends typing songs need, so only a runaway, like a
	// script typing, meets them, and then songs are looked up when they're
	// played, as without early lookups. Each asks YouTube once, or twice for
	// a catalog's song: its search and its lookup.
	maxLookAhead    = 60
	maxLookAheadDay = 300
	maxLookingAhead = 2
	// maxTypists is how many typists' last keystrokes are remembered; ones
	// idle for typistIdle are forgotten to make room.
	maxTypists = 64
	typistIdle = 2 * time.Minute
	// touchEvery is how often someone typing warms up YouTube's API.
	touchEvery = time.Minute
	// minNarrowed is how many songs an earlier search, narrowed down to the
	// words typed since, must keep to be suggested at once.
	minNarrowed = 3
	// MaxQueue is how many songs can wait in a server's queue.
	MaxQueue = 100
	// freshFor is how long a song's link must still work when it gets ready
	// to play. YouTube's work for 6 hours, so one that waited long in the
	// queue is looked up again.
	freshFor = 10 * time.Minute
)

var (
	// ErrNotFound is returned by Play when a search finds no song.
	ErrNotFound = errors.New("no song found")
	// ErrJoin is returned by Play when joining the voice channel failed.
	ErrJoin = errors.New("couldn't join voice")
	// ErrFull is returned by Play when the queue has MaxQueue songs.
	ErrFull = errors.New("the queue is full")
	// ErrStopped is returned by Play when the music was stopped before the
	// song could start.
	ErrStopped = errors.New("the music stopped")
	// ErrNoRadio is returned by Radio for a song without a YouTube video,
	// like SoundCloud's, which YouTube Music has no radio for.
	ErrNoRadio = errors.New("no radio for this song")
)

// Config is what an Engine works with. Voice and DAVE are needed; the rest
// have defaults.
type Config struct {
	// Voice is disgo's voice manager, set up with DAVE's sessions:
	// bot.WithVoiceManagerConfigOpts(voice.WithDaveSessionCreateFunc(dave.Create)).
	Voice voice.Manager
	// DAVE keeps the voice connections' encryption sessions, which the
	// engine waits on so the start of a song isn't lost.
	DAVE *DAVE
	// Events hears what happens to the music. Nil hears nothing.
	Events Events
	// HTTP asks the sites for songs and downloads them. Nil is NewClient's.
	// Unless AllowPrivateLinks, the engine uses a copy: an *http.Transport is
	// copied, with connections of its own and the engine's dialer in place of
	// its dial hooks; another kind of transport has each request's host
	// looked up and checked before it's sent. yt-dlp, and ffmpeg reading a
	// playlist it was left, do their own networking.
	HTTP *http.Client
	// Logger is where the engine logs. Nil is slog.Default().
	Logger *slog.Logger

	// FFmpeg is ffmpeg's path, or its name on PATH, which encodes every
	// song. Empty is "ffmpeg". New fails when it can't find it.
	FFmpeg string
	// YTDLP is yt-dlp's path or name: the backup for YouTube and SoundCloud,
	// and the way to every other site. Empty is without it.
	YTDLP string
	// Bun is Bun's path or name, which yt-dlp runs YouTube's JavaScript
	// with. Empty leaves it to yt-dlp.
	Bun string
	// Cache is where yt-dlp keeps what it learned about YouTube's JavaScript.
	// Empty is yt-dlp's own.
	Cache string
	// Cookies is a cookies.txt file from a spare YouTube account, which
	// yt-dlp signs in with during a bot check, so it needs YTDLP too. Empty
	// is never signing in.
	Cookies string

	// AllowPrivateLinks lets songs come from the bot host's own network,
	// like a music server at home. Off, links to private addresses fail with
	// songs.ErrUnplayable, so a stranger's link can't make the bot fetch its
	// owner's router or a cloud's metadata service. Through an
	// *http.Transport, the engine's requests are checked as they connect,
	// redirects included; through another kind of transport, each request's
	// host is checked first, which can't promise where it then connects.
	// Links handed to yt-dlp, or to ffmpeg to read itself, are checked
	// first, but those programs resolve and follow redirects on their own.
	AllowPrivateLinks bool

	// SearchMemory is how long what a search found is reused for the same
	// words, and narrowed down for longer ones. Zero is 10 minutes. A bot
	// whose listeners play the same songs again can keep searches for a day:
	// typing a song again then suggests it at once, without asking YouTube
	// Music, which takes about half a second, though a day-old search may
	// miss a song released since. Only the same words are answered from a
	// longer memory: an earlier search narrowed down for longer words is 10
	// minutes old at most. It doesn't change how long found songs' audio
	// links are reused, which goes by when they expire.
	SearchMemory time.Duration
}

// noEvents hears nothing, for an Engine without Config.Events.
type noEvents struct{}

func (noEvents) Started(snowflake.ID, *Track)    {}
func (noEvents) Ended(snowflake.ID, *Track, End) {}
func (noEvents) Left(snowflake.ID, *Track)       {}
func (noEvents) QueueChanged(snowflake.ID)       {}

// Events hears what happens to the music, so the app can show it. The engine
// never calls them while it holds its own lock, so they may call the engine,
// and the app may call the engine while holding a lock its events take. They
// can be called at the same time from different goroutines, for different
// servers or for one, so they must be safe for that, and in no set order: a
// song that breaks off as soon as it starts can be told about as Ended
// before Started. Playing says what plays now.
type Events interface {
	// Started is called when a song from the queue starts playing by itself,
	// after the one before it. A song Play starts at once isn't told about.
	Started(guildID snowflake.ID, t *Track)
	// Ended is called once a song that played is over by itself: it ran
	// out, broke off or was skipped. A song stopped isn't told about: Stop
	// and Disconnected return the song that was playing, and StopSong's
	// caller has it already. Nor is the end of the engine's lifetime.
	Ended(guildID snowflake.ID, t *Track, end End)
	// Left is called when the engine leaves voice by itself, because nothing
	// played, or a song stayed paused, for a while. playing is the song that
	// was playing, or nil.
	Left(guildID snowflake.ID, playing *Track)
	// QueueChanged is called when the queue changes by itself: a song that
	// won't start is passed over.
	QueueChanged(guildID snowflake.ID)
}

// End is how a song came to be over.
type End struct {
	// SkippedBy is who skipped it, if anyone.
	SkippedBy snowflake.ID
	// Err says why it broke off, if it did.
	Err error
}

// Request is a song asked for.
type Request struct {
	// GuildID is the server to play it in.
	GuildID snowflake.ID
	// ChannelID is the voice channel to play it in. If the engine is in voice
	// in the server already, it stays in its channel: the app decides who may
	// ask for songs there.
	ChannelID snowflake.ID
	// Query is a song's name, or a link.
	Query string
	// Next puts it at the front of the queue.
	Next bool
	// Meta is the Track's.
	Meta any
	// Asked is when it was asked for, which the log counts from. It may be
	// zero.
	Asked time.Time
	// Autoplay marks a song the app chose by itself, like one from Radio
	// when the queue ran out: songs asked for without Next go ahead of it.
	Autoplay bool
}

// Track is a song in a server, playing or waiting its turn.
type Track struct {
	// Meta is the app's own, like who asked for the song and where, from
	// Request.Meta. The engine never looks at it; an app that changes it
	// while others read it keeps it safe itself.
	Meta any

	e        *Engine
	autoplay bool // Request.Autoplay's, which never changes
	// The rest is guarded by e.mu.
	song    songs.Song
	stream  *audio.Stream // started once it plays, or is next in line
	skipped snowflake.ID  // who skipped it, if anyone
	asked   time.Time
	// cover is its cover, found while the song starts, or nil for the
	// song's own.
	cover func() string
	// preparing is closed once its stream has been started, or failed to;
	// nil when that isn't happening. It's started once.
	preparing chan struct{}
}

// Song is the song: its title, artist, length, link and its own cover. It's
// a copy the caller may change.
func (t *Track) Song() songs.Song {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	return owned(t.song)
}

// Autoplay reports whether the app chose the song by itself, with
// Request.Autoplay.
func (t *Track) Autoplay() bool { return t.autoplay }

// owned is a copy of song that doesn't share its header map, for callers
// that may change what they're given.
func owned(song songs.Song) songs.Song {
	song.Audio.Header = song.Audio.Header.Clone()
	return song
}

// Played is how much of the song has played, exact to the frame.
func (t *Track) Played() time.Duration {
	t.e.mu.Lock()
	stream := t.stream
	t.e.mu.Unlock()
	if stream == nil {
		return 0
	}
	return stream.Played()
}

// Cover is the best picture for the song. A YouTube video's own is 4:3 with
// black bars, so a better one is found while the song starts, and waited for
// here if it isn't in yet, for up to a second.
func (t *Track) Cover() string {
	t.e.mu.Lock()
	song, cover := t.song, t.cover
	t.e.mu.Unlock()
	if cover == nil {
		cover = t.e.coverAhead(song)
	}
	if cover == nil {
		return song.Thumbnail
	}
	return cover()
}

// player is the music in one server's voice channel.
type player struct {
	channelID snowflake.ID
	joined    chan struct{} // closed once joining has finished
	joinErr   error         // why joining failed; read once joined is closed
	conn      voice.Conn    // nil until joined
	frames    *frames       // what conn plays
	playing   *Track        // nil while quiet
	queue     []*Track      // queue[0] plays next; its stream starts early, so it plays without a gap
	paused    bool
	idle      *time.Timer // leaves once nothing has played for a while
	// advancing is true while an advance is at work, and again when another
	// was asked for meanwhile.
	advancing, again bool
	// changed is closed when the queue changes, for an advance waiting for
	// the song next in line, which may not be next anymore; nil while none
	// waits.
	changed chan struct{}
}

// playLocked plays t now, in a player with nothing playing. e.mu must be
// held.
func (p *player) playLocked(t *Track, started func()) {
	p.playing, p.paused = t, false
	p.frames.pause(false)
	p.frames.play(t.stream, started)
	p.idle.Stop()
}

// queueChangedLocked wakes an advance waiting for the song next in line.
// e.mu must be held.
func (p *player) queueChangedLocked() {
	if p.changed != nil {
		close(p.changed)
		p.changed = nil
	}
}

// Engine plays music in any number of servers.
type Engine struct {
	ffmpeg string
	http   *http.Client
	yt     *sources.YouTube
	sc     *sources.SoundCloud
	finder *sources.Finder
	conns  voice.Manager
	dave   *DAVE
	events Events
	log    *slog.Logger // nil in tests: see logger
	// guardLinks keeps links handed to ffmpeg off the private network. It's
	// on unless Config.AllowPrivateLinks.
	guardLinks bool
	// searchFor is Config.SearchMemory; zero is searchedFor.
	searchFor time.Duration

	mu        sync.Mutex
	ctx       context.Context // the app's lifetime, from Start: music stops with it
	players   map[snowflake.ID]*player
	turns     map[snowflake.ID]*sync.Mutex // so joining and leaving a server take turns
	suggested map[string]suggestion        // by link
	searched  map[string]search            // by searchKey
	searching map[string]*searching        // searches on their way, by searchKey
	radios    map[string]search            // by video ID
	radioing  map[string]*searching        // radios on their way, by video ID
	typing    map[typist]*typing           // each typist's last keystroke
	// lookedAhead is when the early lookups in the last day asked YouTube,
	// and lookingAhead how many are asking now.
	lookedAhead  []time.Time
	lookingAhead int
	// leaving is closed once a server's old voice connection is gone.
	leaving map[snowflake.ID]chan struct{}
	// spare is an ffmpeg started while someone types, for their song.
	spare   *audio.Stream
	sparing bool      // a spare is starting
	touched time.Time // when YouTube's API was last warmed up for someone typing
	started bool      // Start was called
}

// NewClient returns an HTTP client made for the engine, which the app can
// share. It's a copy of http.DefaultTransport, TLS settings included, that
// keeps connections open between songs and resumes TLS sessions with the
// audio servers. If the app has replaced http.DefaultTransport with a
// transport of its own, say for tracing, the client uses that as it is.
func NewClient() *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Transport: http.DefaultTransport}
	}
	transport := base.Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.ClientSessionCache = tls.NewLRUClientSessionCache(64)
	// Go closes connections after 90 s unused by default, and opening one
	// again cost about 0.25 s on the Pi. SoundCloud's pieces come three at a
	// time.
	transport.IdleConnTimeout = 10 * time.Minute
	transport.MaxIdleConnsPerHost = 8
	return &http.Client{Transport: transport}
}

// New returns an engine. It fails without Config.Voice or Config.DAVE, or
// when it can't find ffmpeg, or yt-dlp or Bun when they're given.
func New(cfg Config) (*Engine, error) {
	switch {
	case cfg.Voice == nil:
		return nil, errors.New("lullaby: Config.Voice is needed")
	case cfg.DAVE == nil:
		return nil, errors.New("lullaby: Config.DAVE is needed")
	}
	ffmpeg, err := exec.LookPath(cmp.Or(cfg.FFmpeg, "ffmpeg"))
	if err != nil {
		return nil, fmt.Errorf("lullaby: ffmpeg, which every song needs: %w", err)
	}
	for _, tool := range []*string{&cfg.YTDLP, &cfg.Bun} {
		if *tool == "" {
			continue
		}
		if *tool, err = exec.LookPath(*tool); err != nil {
			return nil, fmt.Errorf("lullaby: %w", err)
		}
	}
	client := cfg.HTTP
	if client == nil {
		client = NewClient()
	}
	if !cfg.AllowPrivateLinks {
		client = netguard.Client(client)
	}
	logger := cmp.Or(cfg.Logger, slog.Default())
	events := cfg.Events
	if events == nil {
		events = noEvents{}
	}

	yt := sources.NewYouTube(client, sources.BaseURL, sources.MusicBaseURL)
	yt.Logger = logger
	sc := sources.NewSoundCloud(client, sources.SoundCloudSite, sources.SoundCloudAPI)
	var dl, signedIn *sources.YTDLP
	if cfg.YTDLP != "" {
		dl = sources.NewYTDLP(cfg.YTDLP, cfg.Bun, cfg.Cache)
		if cfg.Cookies != "" {
			signedIn = dl.SignedIn(cfg.Cookies)
		}
	}
	// Songs from these play from YouTube Music, found by name.
	catalogs := []sources.Catalog{
		sources.NewSpotify(client, sources.SpotifySite),
		sources.NewAppleMusic(client, sources.AppleAPI),
		sources.NewDeezer(client, sources.DeezerAPI),
		sources.NewYandexMusic(client, sources.YandexAPI),
	}
	finder := sources.NewFinder(yt, sc, dl, signedIn, catalogs...)
	finder.Logger = logger
	finder.GuardLinks = !cfg.AllowPrivateLinks
	e := &Engine{
		ffmpeg:     ffmpeg,
		http:       client,
		yt:         yt,
		sc:         sc,
		finder:     finder,
		conns:      cfg.Voice,
		dave:       cfg.DAVE,
		events:     events,
		log:        logger,
		guardLinks: !cfg.AllowPrivateLinks,
		searchFor:  max(cfg.SearchMemory, 0),
		ctx:        context.Background(),
		players:    make(map[snowflake.ID]*player),
		turns:      make(map[snowflake.ID]*sync.Mutex),
		suggested:  make(map[string]suggestion),
		searched:   make(map[string]search),
		searching:  make(map[string]*searching),
		leaving:    make(map[snowflake.ID]chan struct{}),
	}
	// A Spotify song's search is remembered like anyone's typed one.
	finder.Search = e.search
	return e, nil
}

// logger is where the engine logs: Config.Logger, or slog's default for an
// engine made without New, as in tests.
func (e *Engine) logger() *slog.Logger {
	if e.log == nil {
		return slog.Default()
	}
	return e.log
}

// Start warms up the connections to YouTube and finds SoundCloud's API key,
// so the first song doesn't wait for them. ctx is the engine's lifetime: once
// it's done, the engine stops the music in every server and leaves voice,
// without telling Events, and Play fails with ErrStopped. Start can be called
// once; an engine that's never started lives until the program ends.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("lullaby: Start was called already")
	}
	e.started, e.ctx = true, ctx
	e.mu.Unlock()
	context.AfterFunc(ctx, e.stopAll)
	go func() {
		if err := e.yt.Warm(ctx); err != nil {
			e.logger().Warn("youtube warm-up failed", redact.Attr(err))
		}
	}()
	go func() {
		if err := e.sc.Warm(ctx); err != nil {
			e.logger().Warn("soundcloud warm-up failed", redact.Attr(err))
		}
	}()
	return nil
}

func (e *Engine) life() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lifeLocked()
}

// over reports whether the engine's lifetime is over.
func (e *Engine) over() bool {
	return e.life().Err() != nil
}

// public is err as the app sees it: lullaby's own reasons a song can't play,
// like an answer too big or an address refused, are songs.ErrUnplayable, and
// the links in it are redacted, since they can hold tokens.
func public(err error) error {
	if err == nil {
		return nil
	}
	if (errors.Is(err, audio.ErrTooBig) || errors.Is(err, netguard.ErrPrivate)) && !errors.Is(err, songs.ErrUnplayable) {
		err = fmt.Errorf("%w: %w", songs.ErrUnplayable, err)
	}
	return redact.Error(err)
}

// lifeLocked is life for whoever holds e.mu. An engine made without New, as
// in tests, lives until the program ends.
func (e *Engine) lifeLocked() context.Context {
	if e.ctx == nil {
		return context.Background()
	}
	return e.ctx
}

// Play plays a song in a voice channel, or puts it in the queue when one is
// playing already. It returns the song and its place in the queue, or 0 when
// it plays now. It joins the channel, finds the song and starts ffmpeg all at
// once. ctx only bounds the wait: once Play returns, the song plays on until
// it's over or the engine stops, and a lookup or join given up on goes on
// for whoever asks next.
func (e *Engine) Play(ctx context.Context, r Request) (*Track, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if e.life().Err() != nil {
		return nil, 0, fmt.Errorf("%w: the engine's lifetime is over", ErrStopped)
	}
	t := &Track{Meta: r.Meta, e: e, asked: r.Asked, autoplay: r.Autoplay}
	place, err := e.start(ctx, r.GuildID, r.ChannelID, r.Query, t, r.Next)
	if err != nil {
		return nil, 0, public(err)
	}
	return t, place, nil
}

// Playing returns the song playing in a server, if any.
func (e *Engine) Playing(guildID snowflake.ID) (*Track, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.players[guildID]
	if !ok || p.playing == nil {
		return nil, false
	}
	return p.playing, true
}

// UpNext returns the song next in line in a server and how many come after
// it, if any.
func (e *Engine) UpNext(guildID snowflake.ID) (songs.Song, int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.players[guildID]
	if !ok || len(p.queue) == 0 {
		return songs.Song{}, 0, false
	}
	return owned(p.queue[0].song), len(p.queue) - 1, true
}

// UpNextTrack is UpNext with the song's Track, for its Meta.
func (e *Engine) UpNextTrack(guildID snowflake.ID) (*Track, int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.players[guildID]
	if !ok || len(p.queue) == 0 {
		return nil, 0, false
	}
	return p.queue[0], len(p.queue) - 1, true
}

// Remove takes t out of a server's queue before its turn, like a song the
// app chose by itself that's no longer wanted, and reports whether it was
// waiting there. The song after it gets ready in its place. A song that has
// started isn't removed: Skip it or StopSong it instead.
func (e *Engine) Remove(guildID snowflake.ID, t *Track) bool {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok {
		e.mu.Unlock()
		return false
	}
	i := slices.Index(p.queue, t)
	// frames may have taken over with it a moment before moved takes it out
	// of the queue: then it's playing.
	if i < 0 || t.stream != nil && !p.frames.unqueue(t.stream) {
		e.mu.Unlock()
		return false
	}
	p.queue = slices.Delete(p.queue, i, i+1)
	stream := t.stream
	t.stream = nil
	p.queueChangedLocked()
	e.mu.Unlock()
	if stream != nil {
		stream.Close()
	}
	if i == 0 {
		go e.prepare(guildID) // the song next in line now, unless it's ready or on its way
	}
	return true
}

// Channel returns the voice channel the player is in, or on its way to, in
// a server.
func (e *Engine) Channel(guildID snowflake.ID) (snowflake.ID, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.players[guildID]
	if !ok {
		return 0, false
	}
	return p.channelID, true
}

// Pause pauses t, or lets it go on if it's paused, and reports whether it's
// paused now. ok is false when t isn't playing anymore.
func (e *Engine) Pause(guildID snowflake.ID, t *Track) (paused, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.players[guildID]
	if !ok || p.playing == nil || p.playing != t {
		return false, false
	}
	p.paused = !p.paused
	p.frames.pause(p.paused)
	if p.paused {
		p.idle.Reset(maxPause)
	} else {
		p.idle.Stop()
	}
	return p.paused, true
}

// Skip ends the song playing in a server, and the next one takes over. It
// reports whether a song was playing.
func (e *Engine) Skip(guildID, by snowflake.ID) bool {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok || p.playing == nil {
		e.mu.Unlock()
		return false
	}
	p.playing.skipped = by
	stream := p.playing.stream
	p.paused = false
	p.frames.pause(false)
	e.mu.Unlock()
	// Closed, the song runs out at once, and the next takes over as if it ended.
	stream.Close()
	return true
}
