# Benchmarks

How fast lullaby plays, and how much it takes, measured on the Raspberry Pi 5 where it runs Rem's Little Helper, a Discord bot. The numbers are from 2026-10-23: two runs while lullaby was still the music engine inside that bot, and a third on lullaby's own code after its review, with the network guard, the limits and the shutdown changes in. The main comparison is with [Lavalink](https://github.com/lavalink-devs/Lavalink), the engine most Discord music bots use. Then three ffmpeg builds, since every song goes through one: [A lean ffmpeg](#a-lean-ffmpeg).

## lullaby against Lavalink

Measured on 2026-10-23. Five YouTube songs, played one after another by each engine, both sending their audio to the same stand-in Discord voice server on the Pi (see [How it was measured](#how-it-was-measured)).

| | lullaby | Lavalink |
|---|---|---|
| A YouTube link to its first audio (median) | **0.22 s** (0.17–0.30 s, three runs) | 0.45 s (0.45–0.51 s) |
| Finding the song | 141–189 ms | 178–225 ms, and it looks each song up again to play it |
| A song found already | 114–139 ms | not measured |
| The first song after starting | about 0.2 s | 1.84 s |
| Late packets (over 30 ms after the one before) | none | none |
| Largest gap between packets | 24.1 ms | 23.1 ms |
| Memory, idle | **18 MB** | 400 MB (311 MB Lavalink, 89 MB its cipher server) |
| Memory while playing | **62–63 MB** with ffmpeg | 474 MB on average, 483 MB at most |
| CPU idle, of one core | **0.1%** | 3.1% (2.3% Lavalink, 0.8% its cipher server) |
| CPU while playing | 6.4–6.6% | 7.0% |
| Starting up | ready at once | 10 s until it takes requests |
| A SoundCloud song (`soundcloud.com/david-kim-29213317/313-eminem-8-mile-ending-battles`) | played | failed: "Did not detect any supported formats" |

Both play smoothly, and use about the same CPU while playing, since both turn YouTube's AAC into Opus. lullaby gets the first sound out about 0.23 s sooner and uses about a seventh of the memory.

### The songs

The warm-up, played first by each engine and not timed, was [SZA, Snooze](https://www.youtube.com/watch?v=LDY_XyxBu8A). lullaby played the five three times: twice inside Rem's Little Helper, and once on its own code after its review. Lavalink played them once.

| Song | Link | lullaby, run 1 | lullaby, run 2 | lullaby, after the review | Lavalink |
|---|---|---|---|---|---|
| Rick Astley, Never Gonna Give You Up | [dQw4w9WgXcQ](https://www.youtube.com/watch?v=dQw4w9WgXcQ) | 174 ms | 197 ms | 218 ms | 481 ms |
| Queen, Bohemian Rhapsody | [fJ9rUzIMcZQ](https://www.youtube.com/watch?v=fJ9rUzIMcZQ) | 273 ms | 296 ms | 216 ms | 510 ms |
| The Weeknd, Blinding Lights | [4NRXx6U8ABQ](https://www.youtube.com/watch?v=4NRXx6U8ABQ) | 171 ms | 196 ms | 216 ms | 450 ms |
| Ed Sheeran, Shape of You | [JGwWNGJdvx8](https://www.youtube.com/watch?v=JGwWNGJdvx8) | 212 ms | 218 ms | 215 ms | 451 ms |
| Luis Fonsi, Despacito | [kJQP7kiw5Fk](https://www.youtube.com/watch?v=kJQP7kiw5Fk) | 193 ms | 256 ms | 237 ms | 451 ms |
| **Median** | | **193 ms** | **218 ms** | **216 ms** | **451 ms** |

Played again, with the songs found already (as for a link pasted while someone is still choosing, which `Suggest` looks up meanwhile), lullaby's first audio came after 114–139 ms. That still includes starting ffmpeg, about 95 ms, which `GetReady` does ahead while someone types.

### What was used

| | |
|---|---|
| Machine | Raspberry Pi 5 Model B, 8 GB, 4 cores; Debian 13 (trixie), kernel 6.18.50+rpt-rpi-2712; booting from a USB SSD |
| Network | a home connection in Baku |
| lullaby | runs 1 and 2: the engine as it was in Rem's Little Helper on 2026-10-23; the third: lullaby's own code after its review, with Rem's Little Helper running on the Pi meanwhile. Go 1.27.3, disgo v0.19.6, dave-go d05c150; ffmpeg 7.1.5 (Raspberry Pi OS's) |
| Lavalink | 4.2.2, the latest release, on OpenJDK 21.0.12.1; lavaplayer 2.2.6, Koe 3.0.0-pre6 |
| Lavalink's YouTube | youtube-source at commit 2be8e542 (2026-09-17, its newest, newer than release 1.18.2), clients IOS, WEB_REMIX, ANDROID_VR and WEB in that order |
| Lavalink's cipher server | yt-cipher at 1e1fd8e (2026-08-05) on Deno 2.9.8. youtube-source asks it for YouTube's player script even for the iOS client, so without it no song played |

Both engines found songs through YouTube's iOS client, and both used their defaults otherwise. Lavalink had its default heap; a smaller `-Xmx` would lower its memory.

## How it was measured

Lavalink only sends audio once it's connected to a Discord voice server, so both engines connected to a stand-in one, written for this, on the same Pi. It speaks enough of Discord's voice gateway (version 8, which both used) for a client to start sending: Hello, Identify, Ready, IP discovery over UDP, Select Protocol and Session Description, with `aead_aes256_gcm_rtpsize`, which both chose. It runs over `wss://` with a certificate made for the test. lullaby's side trusted it through disgo's `voice.WithGatewayDialer`, and Lavalink's through a copy of Java's trust store with the certificate added, given to its JVM with `-Djavax.net.ssl.trustStore`.

The voice server timestamps every audio packet it receives. Timings are from the request to the first packet that reached it, so they include everything but Discord's own network:

- **lullaby:** from calling `Engine.Play` with the link, in voice already, as it is by the time someone has typed a song (`JoinEarly`). "Finding the song" is when the first request for the song's audio went out.
- **Lavalink:** as a bot uses it: `GET /v4/loadtracks` with the link, then `PATCH /v4/sessions/{id}/players/{guild}` with the encoded track, with the player in voice already. "Finding the song" is how long `loadtracks` took.

Each song played for 15 s and was then skipped (lullaby) or stopped (Lavalink), with 2 s before the next. Pacing is from the packets of those 15 s: about 750 per song, one every 20 ms.

Memory is PSS, which divides shared libraries fairly between the processes using them, summed over the engine and its children: for lullaby, its ffmpeg. CPU is from `/proc`, including children once they've finished. Both were sampled four times a second by the voice server's own process, so measuring cost neither engine anything. Idle is 10 s in voice with nothing playing, before the first song. Memory while playing is from samples taken from each request until the song stopped. CPU while playing is from each request to 2 s after the song stopped, so the time ffmpeg takes to stop counts too.

Each engine played an untimed warm-up song first, and ran alone, with the other stopped. lullaby started as a bot starts it: `Start` warms the connections to YouTube and SoundCloud in the background. Lavalink had just started. The first two runs and Lavalink's cost about 35 requests to YouTube, and the third about 7; none were refused.

Notes on the method:

- A song's first packets are often silence, which Opus sends as a few bytes: Queen's first 14 frames were. Those count as its first audio, the same for both engines.
- The stand-in server doesn't offer DAVE, Discord's end-to-end encryption, so neither engine encrypted audio with it. On Discord both do: lullaby with dave-go, in Go, and Lavalink with libdave through JNI.
- Discord itself isn't in the numbers. Every answer to Discord took 0.19–0.36 s from Baku, the same for any engine. A bot can save one of them by telling Discord to wait while it looks the song up, rather than before, as Rem's Little Helper does.
- FlaviBot runs on its own servers, so it couldn't be measured here. If it's built on Lavalink, as most music bots are, the Lavalink column is its engine.
- A first run measured lullaby's memory and CPU from inside its own process, which put the sampling's cost on lullaby: 2.1% CPU idle instead of 0.1%. Only its timings were kept, as run 1.

## Running it again

The benchmark is [`cmd/voicebench`](cmd/voicebench), a Go program in three parts: `voicebench server`, the stand-in voice server, with its control API on `127.0.0.1:9444`; `voicebench lullaby`, the engine; and `voicebench lavalink`, which drives Lavalink through its REST and websocket API. Each engine runs alone, so stop the other first.

`make build-bench` builds it for the Pi, as `bin/voicebench-linux-arm64`. Copy it there as `voicebench`, into a folder with Java 21 in `java/`, Lavalink in `lavalink/`, yt-cipher in `yt-cipher/` and Deno in `bin/deno`. There, with `Y=https://www.youtube.com/watch?v=`:

```sh
# The voice server, which writes the certificate the engines trust. Wait for it.
mkdir -p vb && setsid ./voicebench server vb/cert.pem > vb/server.log 2>&1 &
until curl -so /dev/null http://127.0.0.1:9444/stats; do sleep 0.1; done

# lullaby.
./voicebench lullaby vb/cert.pem ${Y}LDY_XyxBu8A ${Y}dQw4w9WgXcQ ${Y}fJ9rUzIMcZQ ${Y}4NRXx6U8ABQ ${Y}JGwWNGJdvx8 ${Y}kJQP7kiw5Fk > vb/lullaby.out

# Lavalink trusts the certificate through a copy of Java's trust store.
cp java/lib/security/cacerts vb/trust.jks
java/bin/keytool -importcert -noprompt -alias voicebench -file vb/cert.pem -keystore vb/trust.jks -storepass changeit

# youtube-source's cipher server, then Lavalink.
(cd yt-cipher && DENO_DIR=../cache/deno HOST=127.0.0.1 PORT=8001 setsid ../bin/deno run --allow-net --allow-read --allow-write --allow-env server.ts > ../vb/cipher.log 2>&1 &)
(cd lavalink && LAVALINK_SERVER_PASSWORD=benchpass setsid ../java/bin/java -Djavax.net.ssl.trustStore=../vb/trust.jks \
  -Djavax.net.ssl.trustStorePassword=changeit -jar Lavalink.jar > ../vb/lavalink.log 2>&1 &)
# Once it answers on 127.0.0.1:2333, about 10 s later:
./voicebench lavalink $(pgrep -x java),$(pgrep -x deno) ${Y}LDY_XyxBu8A ${Y}dQw4w9WgXcQ ${Y}fJ9rUzIMcZQ ${Y}4NRXx6U8ABQ ${Y}JGwWNGJdvx8 ${Y}kJQP7kiw5Fk > vb/lavalink.out
```

Lavalink's `application.yml` turns its own YouTube off and loads youtube-source, with its password set by `LAVALINK_SERVER_PASSWORD` above:

```yaml
server:
  port: 2333
  address: 127.0.0.1
lavalink:
  plugins:
    - dependency: "dev.lavalink.youtube:youtube-plugin:2be8e542d3f6f178e048dca565892684c2e40177"
      snapshot: true
  server:
    sources:
      youtube: false
      soundcloud: true
      http: true
plugins:
  youtube:
    enabled: true
    allowSearch: true
    clients: [IOS, MUSIC, ANDROID_VR, WEB]
    remoteCipher:
      url: "http://127.0.0.1:8001"
      userAgent: "voicebench"
```

Each run prints a `RESULT` line per song, the warm-up's marked `-warmup`, and a `USAGE` line at the end, as JSON, and readable lines on stderr. Stop the processes with `pkill -x` and their names: `pkill -f` would also match the shell an SSH command runs in.

Keep YouTube gentle: the run above asks it about 35 times. A few hundred requests in a day got the home address bot-checked for hours, and either engine stops if YouTube asks it to prove it isn't a robot.

## A lean ffmpeg

Every song goes through ffmpeg, which turns its audio into Opus for Discord, and lullaby keeps up to two running for each server with music, the song playing and the next one getting ready, and one spare, started while someone types. So ffmpeg is most of lullaby's memory, and starting one is on the way to the first sound whenever none was started ahead: when nobody typed first, when a song is skipped to before it's ready, and for the playlists ffmpeg reads itself, like live streams.

A distribution's ffmpeg is built for everything ffmpeg can do. Debian's loads 213 shared libraries, for video, hardware and codecs no song comes in, and the system's loader fills in 44,950 addresses in them before ffmpeg runs a line of its own. `make ffmpeg` ([`scripts/build-ffmpeg.sh`](scripts/build-ffmpeg.sh)) builds one with only what lullaby uses, in one static file at a fixed address, so there's nothing to load and nothing to fill in. [CONTRIBUTING.md](CONTRIBUTING.md#things-to-know) says why it's built as it is.

Measured on 2026-10-23 on the Pi:

| | Debian's | Ready-made static | Lean |
|---|---|---|---|
| Version | 7.1.5, Debian's package | 8.1.3, [BtbN's](https://github.com/BtbN/FFmpeg-Builds) linuxarm64 LGPL build | 9.0.2, `make ffmpeg` |
| File | 395 KB, and 213 libraries | 117 MB, and 8 of the system's libraries | 8.0 MB, nothing else |
| Starting (`-version`) | 95 ms | 9.9 ms | **0.2 ms** |
| First sound, started with its song | 97 ms | 14 ms | **4.0 ms** |
| First sound, already running | 4.4 ms | 4.1 ms | **3.8 ms** |
| Memory waiting for a song, as the spare | 23 MB | 12 MB | **1.1 MB** |
| Three waiting at once | 70 MB | 36 MB | **3.3 MB** |
| Memory while playing | 45 MB | 32 MB | **7.3 MB** |
| CPU for the whole song | 5.9 s | 5.2 s | **4.5 s** |

- **Starting and waiting.** Linking everything into one file, as the ready-made build does, halves the start and the memory. It still has every codec ffmpeg has, 117 MB of them, at an address picked when it starts, so the addresses in all their tables are filled in then too, and the pages they're on become its own. The lean build has neither: its start is the kernel starting a program, and three of them waiting share almost everything.
- **Already running**, the three are within 0.7 ms of each other: encoding is the same work in each. That's why lullaby starts a spare while someone types, and with a spare waiting, the build changes the first sound by little. The memory and the CPU are every song's.
- **CPU.** The lean build is a newer ffmpeg, linked differently; which of those saves the CPU wasn't pinned down.

### What the lean build plays

It has the decoders for AAC, MP3, Opus, Vorbis, FLAC, ALAC, AC-3, E-AC-3 and PCM; the containers MP4, WebM and Matroska, Ogg, MP3, raw AAC, FLAC, WAV, AC-3 and MPEG-TS; HLS playlists; Opus out, in Ogg; and the protocols pipe, file, http, https, tls, tcp and crypto, with OpenSSL for https. That's what YouTube, SoundCloud and other sites' songs come in. A song in anything else won't play: add its decoder and container to the script.

Tested with a 5-second tone in each of 16 forms, made with a full ffmpeg and turned into Opus by the lean build, under emulation on an x86 machine: AAC in MP4 and raw; MP3; Opus in WebM, Ogg and Matroska; Vorbis; FLAC; ALAC; WAV; AC-3; mono AAC at 22 kHz; and HLS playlists of MPEG-TS and of fragmented MP4 pieces. Each came out as Opus of the same length. On the Pi, YouTube's AAC and Opus copies of SZA's Snooze came out whole, 280.8 and 280.7 s. An MP4 with its index at the end can't be read from a pipe, by any ffmpeg; YouTube's have it at the start.

For https, which ffmpeg needs for the playlists it reads itself: a public Ogg song over https played with no certificate given, on the x86 machine and on the Pi, from each system's own certificates; an HLS playlist served over https by a test server, with its test authority trusted through `SSL_CERT_FILE`, played whole, its 6 pieces fetched over https too; and with that authority not trusted, it was refused ("certificate verify failed").

### How it was measured

`TestCompareFFmpeg` in [`internal/audio/compare_test.go`](internal/audio/compare_test.go) runs each ffmpeg with lullaby's own arguments, one build after another, with Rem's Little Helper running on the Pi meanwhile. The lean column is its run with the build `make ffmpeg` makes now; the ready-made build was measured earlier the same day, by the same steps. The song was YouTube's own AAC audio of [SZA, Snooze](https://www.youtube.com/watch?v=LDY_XyxBu8A) (itag 140, 4.5 MB, 4:41), saved on the Pi, so nothing was downloaded.

- **Starting:** the median of 15 runs of `ffmpeg -version`.
- **First sound:** from giving ffmpeg the song to its first Ogg page with sound in it, after the two header pages: started with its song, or 0.5 s before, as the spare is. Medians of 10.
- **Memory** is PSS, from `/proc/<pid>/smaps_rollup`, which divides the pages processes share fairly between them: one ffmpeg waiting for its song, three at once, and one playing, given the whole song with only 200 KB of its Opus read, as lullaby reads 10 seconds ahead, so it waits mid-song.
- **CPU:** user and system time for the whole song, encoded as fast as it goes. The median of 3.

### Running it again

On any Linux machine, in lullaby's folder:

```sh
make ffmpeg   # bin/ffmpeg-linux-arm64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o bin/audio-linux-arm64.test ./internal/audio
```

Copy both to the Pi, with a song a few minutes long, then:

```sh
LULLABY_COMPARE_FFMPEG=/usr/bin/ffmpeg,./ffmpeg-linux-arm64 LULLABY_COMPARE_SONG=song.m4a \
  ./audio-linux-arm64.test -test.run TestCompareFFmpeg -test.v
```

It takes about a minute per build and asks no site for anything.

## Decoding YouTube's answers

Every answer from YouTube's API is read whole into a buffer kept between requests, then decoded (`decode` in `internal/sources/youtube.go`). Until v0.3.0, a `json.Decoder` read it, growing a buffer of its own for every answer. On the Pi on 2026-10-24, with YouTube Music's whole 566 KB answer for a song's radio, read 32 KB at a time as it comes, to its 50 songs:

| | `json.Decoder`, before | A kept buffer, now |
|---|---|---|
| Time | 5.9–7.2 ms | **3.8 ms** |
| Garbage | 2.2 MB | **0.15–0.16 MB** |
| Allocations | about 1,500 | 1,386 |

Searches and lookups are read the same way. A buffer is kept only while it's used again: Go's garbage collector lets idle ones go after two collections, and one grown past 2 MB isn't kept at all.

### Running it again

`go test -run '^$' -bench Decode -benchmem ./internal/sources` runs it on any machine. For the Pi:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o bin/sources-linux-arm64.test ./internal/sources
```

Copy it to the Pi, with `internal/sources/testdata/` as `testdata/` next to it, then:

```sh
./sources-linux-arm64.test -test.run '^$' -test.bench Decode -test.benchmem
```

The answer, `testdata/radio-snooze.json.gz`, was saved from YouTube Music, with the asker's visitor ID taken out and its tracking tokens blanked at the same length. It asks no site for anything.

## Other measurements

Also on the Pi, while the engine was being built, first inside Rem's Little Helper:

| What | Measured | When |
|---|---|---|
| ffmpeg starting | about 95 ms, so it's started while the song is looked up | 2026-10-22 |
| ffmpeg's first frame, once the audio arrives | 4.1–4.2 ms with any of `-probesize`, `-flush_packets`, `-fflags +nobuffer` or `-threads 1`, so its flags stay the default | 2026-10-23 |
| ffmpeg's memory | 55 MB RSS each: about 40 MB of program, shared by every ffmpeg running, and 15 MB of its own. Its flags don't change it | 2026-10-23 |
| The engine's own memory for a 3.2 MB song | 13.4 MB, from 18.8 MB once downloads went into reused 64 KB slabs; 16.8 MB with the next song ready, from 24.3 MB | 2026-10-23 |
| Playing, per 20 ms frame | about 2 small allocations, so pooling frames wouldn't help | 2026-10-23 |
| A YouTube Music search | 0.68 s cold, 0.36–0.37 s warm, and at once from memory or narrowed from an earlier search | 2026-10-23 |
| A YouTube Music search, again | 0.49–0.59 s warm, almost all of it YouTube's: its 12 KB answer, 191 KB unpacked, downloads in 5–10 ms and decodes in 2–3 ms | 2026-10-23 |
| YouTube Music's search box | 0.12–0.14 s warm, but a few songs at most, and only words for "snooze s". Tried for suggestions and taken out: worse choices, and no faster in Discord | 2026-10-23 |
| A song's lookup, then its first 64 KB | 0.19 s for the lookup, almost all YouTube's; 77 ms for the audio over a new connection | 2026-10-23 |
| A SoundCloud link to its first frame | 0.94 s cold, 0.50 s warm, with lullaby reading the playlist itself; 1.14–1.34 s when ffmpeg read it | 2026-10-23 |
| SoundCloud's key, found at startup | 0.71 s | 2026-10-23 |
| A Spotify link to its first frame | about 1 s: its page, a YouTube Music search, then the song | 2026-10-23 |
| Spotify, Apple Music, Deezer and Yandex Music | 0.24–0.48 s to read a song; six of six test songs matched on YouTube Music, one in Cyrillic | 2026-10-23 |
| yt-dlp, the backup | 5–6 s per lookup: 0.9 s to start, about 1.4 s for YouTube's JavaScript with Bun | 2026-10-22 |
| Go's 20 ms timer | within 1.1 ms, even with 13,651 garbage collections in 30 s | 2026-10-22 |
| YouTube Music's radio for a song | 49 songs in 1.0 s cold, 0.81 s warm, almost all YouTube's; at once from memory | 2026-10-24 |
| A radio kept in memory | under 24 KB, so 200, the most kept, under 5 MB | 2026-10-24 |
