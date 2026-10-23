# Contributing

Thanks for helping. This page covers how to set up, how the code is laid out, how the engine works, and what to know before you send a change.

## Setting up

You need Go 1.26 or newer, and ffmpeg. yt-dlp and Bun (up to 1.3.14) are only for the backup and for other sites' links.

| Command | What it does |
|---|---|
| `make check` | `make fmt`, `make vet` and `make test` in one go. Run it before every change you send |
| `make fmt` · `make vet` · `make test` | Format the code (this rewrites files), vet it, or test it |
| `make build-bench` | Build the benchmark for a Raspberry Pi 5 into `bin/`; [BENCHMARKS.md](BENCHMARKS.md) says how to run it |
| `make ffmpeg` | Build the lean ffmpeg for a Raspberry Pi 5 into `bin/`, cross-compiled with zig in `build/`; see `scripts/build-ffmpeg.sh` |
| `make clean` | Remove `bin/` and `build/` |

To try a change in Discord, run [`examples/bot`](examples/bot/main.go) with a test bot's token: `DISCORD_TOKEN=... go run ./examples/bot`.

## Layout

```
*.go                the engine, package lullaby: Engine, Config, Track, Events, the queue, joining and leaving
songs/              the Song type, the errors songs fail with, and helpers for YouTube and SoundCloud links
internal/sources/   finds songs and their audio: YouTube, YouTube Music, SoundCloud, the music services, yt-dlp
internal/audio/     turns audio into 20 ms Opus frames with ffmpeg, downloading it in pieces
internal/netguard/  keeps the engine's requests off the bot host's own network
internal/redact/    hides tokens in links before they're logged or handed back
cmd/voicebench/     the benchmark against Lavalink, through a stand-in Discord voice server
examples/bot/       a small music bot built on lullaby
scripts/            build-ffmpeg.sh, the lean ffmpeg's build
```

In the engine, `lullaby.go` is the API and its types, `example_test.go` the examples in its docs, `queue.go` starting songs, the queue and the changeover, `voice.go` joining and leaving, `search.go` finding songs and suggestions, `ready.go` what happens while someone types, `frames.go` what disgo's audio sender plays, `dave.go` the encryption sessions, and `cover.go` the covers.

What's public is a promise to its users. Everything that follows the sites, which change often, is internal: `sources`, `audio`, and the guards. `songs` is only what an app needs to show songs and tell errors apart. Keep new code internal or unexported unless an app needs it. Settings go in `Config`, so adding one doesn't break anyone.

## How the engine works

- **One player per server.** It holds the voice connection, the song playing, the queue and whether it's paused, all guarded by the engine's lock.
- **One frame provider per connection.** `frames` is set once, when the player joins, and plays whichever song it's given. The song next in line waits ready, with its ffmpeg running, and takes over in the same 20 ms as the last frame of the one before.
- **The changeover keeps the books first.** When a song runs out, `frames` switches to the next one and calls `moved` right away, inside disgo's audio sender, so the queue, pause and skips always match what plays. `moved` only keeps the books, under the lock. Everything slow waits for `afterMove`: getting the song after ready, waiting for the old ffmpeg, and telling the app.
- **The app is told outside the lock.** The engine never calls `Events` while holding its lock, so an app may call the engine from its events, and may call the engine while holding a lock its events take. Keep it that way.
- **Joining and leaving a server take turns.** disgo's voice manager knows connections by server, so a join waits until the server's old connection is gone, and a stopped join hangs up before letting go of its turn.
- **Contexts.** `Start`'s context is the engine's lifetime: songs, their ffmpeg and downloads, joins and lookups live as long as it. The context given to `Play`, `Suggest` or `SuggestFor` only bounds how long the caller waits, since lookups and searches are shared, and a song plays on after `Play` returns.
- **Requests are shared.** Searches and lookups for the same thing at the same time share one request. Searches are remembered for `Config.SearchMemory`, 10 minutes unless it says otherwise, for the same words; for longer words they're narrowed down only while 10 minutes old at most, since narrowed choices stand in for a search and an old one could show fewer. At 1000, the one used longest ago goes first, narrowing counting as use, and one that found nothing is believed for a minute. A catalog's song is remembered by its ID (`Catalog.Key`), so two shares of one Spotify song, which differ in `si=`, are one song; a short link is named by the catalog's own link for the song once it's followed, but a link with a name of its own keeps it, since that can name another country's Apple store, and its YouTube Music search goes through the engine's, remembered like a typed one. A pasted link is looked up while someone is still choosing, through the fast ways only (`sources.Finder.FindAhead`).
- **The first song suggested is looked up** 300 ms (`settleAfter`) after the keystroke it was found for, or as soon as its search is back if that took longer, unless the same person (`SuggestFor`'s guild and user) typed again: one lookup per pause, not per keystroke, and a slow search for an old keystroke can't replace a newer one's. A pasted link takes the same path without the pause. A song already remembered asks nobody; the other early lookups are capped at 2 at once, 60 an hour and 300 a day (`maxLookingAhead`, `maxLookAhead`, `maxLookAheadDay`), and none happen during a bot check, including a catalog song's search. The caps count lookups, not requests: one may look a song up on YouTube and, for a catalog's song, search YouTube Music for it too, while a SoundCloud song asks YouTube nothing. Renewing the visitor ID and warming up connections are separate. Past the caps, or for a 65th person typing at once (`maxTypists`), songs are looked up when they're played. Play itself is never limited. YouTube Music's search box (`music/get_search_suggestions`) answers in 0.12–0.14 s where a search takes 0.49–0.59 s, but it suggests a few songs at most, often slowed or sped-up versions, and none for half-typed words. Suggestions from it were tried on 2026-10-23 and taken out the same day: they were worse, and felt no faster in Discord, where the trip there and back takes longer than the search.
- **A `Track`'s own data rides along as `Meta`**, which the engine never looks at. Its song, stream and cover are guarded by the engine's lock, so read them with `Song`, `Played` and `Cover`.

## Tests

Tests never contact anything outside the machine; some open sockets on localhost. Sites are `httptest` servers or fake transports that answer with real responses saved from them; voice connections are fakes kept by disgo's own manager, so its habits are the real ones; ffmpeg is a shell script that writes Opus silence, except in `internal/audio`'s stream tests, which use ffmpeg when it's installed and skip otherwise. Tests should pass offline and finish in seconds.

A test that looped forever with `-race` once filled 31 GB of RAM. `make test` gives up after a minute. On Linux with systemd, cap new tests' memory too:

```
systemd-run --user --scope -q -p MemoryMax=4G -p MemorySwapMax=0 go test -race -timeout 120s ./...
```

## Writing

Comments are short, plain sentences that say why, not what. Match the code around you: its naming, its comments and its idioms. When a comment gives a time, say what it was measured on; "the Pi" means a Raspberry Pi 5 with 8 GB on a home connection.

## Things to know

- **YouTube** is asked for songs as its iPhone app does, with a visitor ID, which is renewed in the background. When YouTube stops accepting `iosVersion` in `internal/sources/youtube.go`, songs still play through yt-dlp, and a newer version from youtube-source's `Ios.java` usually fixes it. The iOS client only gives AAC, so every song is encoded to Opus. One long download from YouTube is throttled to about twice playback speed, which is why `audio.Fetch` asks for 1 MB pieces, up to 16 MB ahead, kept in reused 64 KB slabs. YouTube turns yt-dlp's links down for a moment after handing them out, so `audio.Fetch` retries a 403 until the link's `Ready` time.
- **Bot checks.** YouTube judges the internet address asking: a few hundred automated requests in a day got one address asked to "Sign in to confirm you're not a bot" for nearly every video, for hours. Test against YouTube gently. With `Config.Cookies`, `sources.Finder` signs in through yt-dlp for 15 minutes at a time, one lookup at a time. We chose a spare account's cookies over OAuth, which youtube-source uses: YouTube only accepts OAuth from its TV client, whose links have to be unscrambled with YouTube's own JavaScript, and yt-dlp dropped OAuth in 2024 for that reason.
- **SoundCloud** is asked with the key its website's scripts hold, found again whenever SoundCloud refuses it. It sends HLS playlists of short pieces, which `audio.FetchHLS` downloads three at a time into the ffmpeg started during the lookup, about 0.6 s sooner than ffmpeg reading the playlist itself. Live streams and playlists it doesn't handle go to `audio.Open`, which has ffmpeg read them, following web addresses only.
- **Spotify, Apple Music, Deezer and Yandex Music** never give bots whole songs: Deezer's are encrypted, and Yandex only gives them to a signed-in account. Each is a `sources.Catalog` that reads a song's name, artists, length and cover from its public pages or API, and `Finder` plays the same song from YouTube Music. A match needs two of the three: the title, one of the artists, and a length within 3 s. Anything over 10 s off, like an acoustic version, never counts.
- **yt-dlp** runs with Bun as its JavaScript runtime, in its own process group with a 30-second limit, so stopping it stops Bun too. YouTube playlist and channel links never reach it, since it would look up every video on them, and it's asked with `--flat-playlist`, so another site's playlist comes back as one record.
- **Every song is encoded to 20 ms Opus**, even one that's Opus already: other sites' Opus can come in other frame lengths. ffmpeg takes about 95 ms to start on the Pi, so it starts while the song is looked up, or earlier, as the spare from `GetReady`. Its flags don't change how soon the first frame comes.
- **DAVE.** Discord has required end-to-end encrypted voice since 2026-03-01. dave-go is pure Go, so bots stay one static binary. It's young and hasn't had an outside audit, so it's pinned to a commit with its October 2026 fixes. A connection's first frames wait while its encryption is set up (`ShouldHoldFrames`), or the start of a song would be dropped.
- **Strangers' links.** In a public server, anyone can paste a link, so the engine's own requests connect only to public addresses (`netguard.Client`, which checks each address as it's dialed), links handed to yt-dlp or to ffmpeg are checked before, and `Config.AllowPrivateLinks` turns it off. Answers have sizes far above any song's: yt-dlp's (8 MiB), playlists (2 MiB, 10,000 pieces) and pieces (8 MiB); errors past those aren't retried or left to ffmpeg. yt-dlp runs with `--ignore-config`. Logs and the errors handed back have links' query values and passwords hidden (`redact`), since a private SoundCloud link's `secret_token` or a music server's API key can be in them; YouTube's `v=` stays.
- **The lean ffmpeg** (`scripts/build-ffmpeg.sh`, `make ffmpeg`) is built the way it is for these reasons:
  - **Why at all.** Every song goes through ffmpeg, and up to three run at once. A distribution's loads hundreds of libraries and fills in their addresses every time it starts (Debian's: 213 and 44,950), which is most of its 95 ms start and 23 MB waiting. BENCHMARKS.md has the numbers.
  - **zig** cross-compiles from any machine to any other with one download, and brings musl for the target, so there's no cross toolchain, Docker, or building on the Pi.
  - **musl, static, at a fixed address** (not PIE): there's nothing to load and nothing to fill in, so it starts in 0.2 ms, and ffmpegs running at once share the file's pages. A static build at a picked address still fills in every table's addresses at start: the ready-made 117 MB build waits in 12 MB.
  - **Only what's used**, with `--disable-everything`: the audio decoders and containers songs come in, libopus and the Ogg muxer for lullaby's output, the protocols lullaby passes (`pipe` for songs it downloads, `http,https,tls,tcp,crypto` for playlists ffmpeg reads itself), and the filters ffmpeg's command line needs to change sample rates and channels. Leaving out the rest is also less code reading strangers' files.
  - **`--enable-pthreads`**: `--disable-autodetect`, which keeps the build machine's libraries out, also turns threads off, and ffmpeg's command line isn't built without them.
  - **OpenSSL for https**, because ffmpeg with it trusts the system's certificates by itself, from `/etc/ssl` (`--openssldir=/etc/ssl`) or `SSL_CERT_FILE` and `SSL_CERT_DIR`. ffmpeg 9 checks certificates by default, and with mbedtls, the first choice for its size, it trusts none until it's given `-ca_file`, which it doesn't pass on to an HLS playlist's pieces, so they'd fail. OpenSSL 3 is Apache-licensed, so the build has `--enable-version3`, making it LGPL 3.
  - **Pinned sources**: each version has its checksum in the script, from a tarball whose signature checked out, ffmpeg's with FFmpeg's release key and OpenSSL's with OpenSSL's. To update, change the version and checksum, check the signature, build, and run the formats and https tests in BENCHMARKS.md before using it. Nothing updates it by itself.
- **disgo surprises**, all checked in its v0.19.6 source:
  - The voice manager knows connections by server: `CreateConn` hands back the server's connection if it still has one, and closing an old connection after a new one opened in the same server drops the new one.
  - The audio sender sets up its cancel function in its own goroutine, so replacing a connection's frame provider right after setting one can crash. That's why there's one provider per connection.
  - `SetOpusFrameProvider` doesn't wait for the old sender, so the sender can't deadlock against a join.
  - For a frame too big for its 512-byte encrypt buffer, the UDP connection grows the buffer's capacity but passes it at its old length. dave-go reads the length and drops the frame, which made songs stutter. `roomy` in `dave.go` hands dave-go the whole capacity.

## Benchmarks

Measure on real hardware, not a laptop: [BENCHMARKS.md](BENCHMARKS.md) has the method and how to run `cmd/voicebench` against Lavalink. Keep YouTube gentle there too.

## Sending a change

- `make check` passes.
- Say what you tried in Discord: timing and voice only show up live.
- Keep each change to one thing, and match the code around it.
