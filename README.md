# lullaby

[![Go Reference](https://pkg.go.dev/badge/github.com/windrunner21/lullaby.svg)](https://pkg.go.dev/github.com/windrunner21/lullaby)
[![CI](https://github.com/windrunner21/lullaby/actions/workflows/ci.yml/badge.svg)](https://github.com/windrunner21/lullaby/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Music for Discord bots written in Go with [disgo](https://github.com/disgoorg/disgo). lullaby plays songs in voice channels from inside your bot's own process, with no Lavalink server, JVM or separate node to run. A YouTube link starts playing in about 0.2 s, a queue plays without gaps, and a playing server takes about 60 MB, ffmpeg included.

```go
music, err := lullaby.New(lullaby.Config{Voice: client.VoiceManager, DAVE: dave})
track, place, err := music.Play(ctx, lullaby.Request{GuildID: guildID, ChannelID: voiceChannelID, Query: "snooze sza"})
```

lullaby runs in Rem's Little Helper, a Discord bot, on a Raspberry Pi 5, and was split out of it to be used on its own. It's young: until v1, the API may still change between minor versions.

## Features

- **Songs from a name or a link.** A name is searched on YouTube Music. Links can be from YouTube, SoundCloud, Spotify, Apple Music, Deezer or Yandex Music. The last four don't give bots their songs, so lullaby reads the song's name, artists and length there, and plays the same song from YouTube Music, with the original's cover.
- **Suggestions while someone types:** from memory, from an earlier search narrowed down to the words typed since, or from YouTube Music. Searches can be remembered for as long as you like, so a song typed again is suggested at once. Once they stop typing, the engine tries to look the first song up, within a budget, so picking it or pressing Enter can play it at once. Picking one plays it with YouTube Music's title, artist and cover.
- **A queue in every server, without gaps.** The next song is ready, with its ffmpeg running, before the one playing ends, and takes over in the same 20 ms. A song can go to the front of the queue. Pause, skip and stop act at once.
- **Songs like this one.** `Radio` gives YouTube Music's radio for a song, about 50 songs its app plays after it, for a bot that keeps the music going when the queue runs out. A pick queued with `Request.Autoplay` waits behind the songs people ask for, and plays without a gap like any other.
- **Ready before Enter.** While someone is still typing, lullaby can join their voice channel, warm up the connection to YouTube, start an ffmpeg and look up a pasted link.
- **A backup.** With [yt-dlp](https://github.com/yt-dlp/yt-dlp), links from other sites play too, and yt-dlp takes over when YouTube or SoundCloud change something. During a YouTube bot check, it can sign in with a spare account.
- **Encrypted voice.** Discord requires DAVE, which lullaby gets from [dave-go](https://github.com/thomas-vilte/dave-go), in pure Go, so your bot stays one static binary.
- **Yours to show.** The engine tells your bot what happens: a song started, ended, broke off or was skipped. Your bot shows that however it likes. Rem's Little Helper posts a card for each song, with buttons.

## How it compares

On a Raspberry Pi 5, against Lavalink 4.2.2 with youtube-source's newest build, both sending to the same stand-in Discord voice server. [BENCHMARKS.md](BENCHMARKS.md) has the songs, the method and every number.

| | lullaby | Lavalink |
|---|---|---|
| A YouTube link to its first audio, median | **0.22 s** | 0.45 s |
| The first song after starting | about 0.2 s | 1.8 s |
| Memory while playing | **63 MB**, with ffmpeg | 474 MB, with the cipher server it needs for YouTube |
| Memory idle | **18 MB** | 400 MB |
| CPU while playing, of one core | 6.6% | 7.0% |
| Late packets | none | none |

## Install

```sh
go get github.com/windrunner21/lullaby
```

You need Go 1.26 or newer, disgo v0.19.6, and [ffmpeg](https://ffmpeg.org), which encodes every song to Opus: `sudo apt install ffmpeg`, or `brew install ffmpeg`.

The backup is optional: [yt-dlp](https://github.com/yt-dlp/yt-dlp#installation), with [Bun](https://bun.sh) up to 1.3.14 to run YouTube's JavaScript, since yt-dlp doesn't support newer Bun yet (`curl -fsSL https://bun.sh/install | bash -s "bun-v1.3.14"`). Without them, everything plays but other sites' links, with no backup.

lullaby is tested on Linux. On systems other than Unix, stopping yt-dlp may leave the programs it started running.

### A lean ffmpeg

Every song goes through ffmpeg, and a distribution's ffmpeg is built for everything ffmpeg can do: Debian's loads 213 libraries, for video, hardware and formats no song comes in, before it starts encoding. That costs about 95 ms each time one starts and 23 MB for each one waiting, and lullaby keeps up to three running at once.

`make ffmpeg` builds one with only what lullaby uses: the audio formats songs come in, the Opus encoder, and the protocols lullaby passes ffmpeg, in one static 8 MB file with nothing to load. It's cross-compiled with [zig](https://ziglang.org) from pinned, checksummed sources, so any Linux machine can build it for any other: `bin/ffmpeg-linux-arm64` for a Raspberry Pi, or `TARGET=x86_64-linux-musl make ffmpeg` for amd64. Point `Config.FFmpeg` at it. On a Raspberry Pi 5:

| | Debian's | lean |
|---|---|---|
| Starting | 95 ms | 0.2 ms |
| Memory waiting, as the spare | 23 MB | 1.1 MB |
| Memory while playing | 45 MB | 7.3 MB |
| CPU for a song | 5.9 s | 4.5 s |

It plays AAC, MP3, Opus, Vorbis, FLAC, ALAC, AC-3 and PCM, in MP4, WebM, Matroska, Ogg, MPEG-TS and HLS playlists, and trusts the system's certificates for https, as a distribution's does. A song in anything else won't play; add its decoder and container to the script. It gets no security updates by itself: build it again when ffmpeg or OpenSSL releases a fix. [BENCHMARKS.md](BENCHMARKS.md#a-lean-ffmpeg) has the measurements and how to repeat them, and [CONTRIBUTING.md](CONTRIBUTING.md#things-to-know) why it's built as it is.

## Quick start

```go
dave := lullaby.NewDAVE()
client, err := disgo.New(token,
	bot.WithGatewayConfigOpts(gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildVoiceStates)),
	bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagVoiceStates)), // to know who's in which channel
	bot.WithVoiceManagerConfigOpts(voice.WithDaveSessionCreateFunc(dave.Create)),
	// Song titles come from strangers: one saying "@everyone" pings nobody.
	bot.WithRestConfigOpts(rest.WithDefaultAllowedMentions(discord.AllowedMentions{
		Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{},
	})),
)
if err != nil {
	return err
}
music, err := lullaby.New(lullaby.Config{Voice: client.VoiceManager, DAVE: dave, Events: myEvents})
if err != nil {
	return err // no ffmpeg, say
}
music.Start(ctx) // warms up the connections, so the first song doesn't wait for them

// In /play, with the asker's voice channel:
track, place, err := music.Play(ctx, lullaby.Request{GuildID: guildID, ChannelID: voiceChannelID, Query: query})
```

[`examples/bot`](examples/bot/main.go) is a whole music bot in about 250 lines: `/play` with suggestions, `/skip` and `/stop`. Run it with `DISCORD_TOKEN=... go run ./examples/bot`. The [package docs](https://pkg.go.dev/github.com/windrunner21/lullaby) have more examples.

## Using the engine

| | What it does |
|---|---|
| `New(Config)`, `Start(ctx)` | Make the engine, then start it, once, with your bot's lifetime. When `ctx` is done, the engine stops the music in every server and leaves voice |
| `Play(ctx, Request)` | Joins the voice channel, finds the song and starts ffmpeg all at once, or puts the song in the queue. Returns the `*Track` and its place in line, 0 when it plays now. `ctx` only bounds the wait: the song plays on after `Play` returns. Once the engine is in voice in a server, it stays in its channel, so check who may ask before you call `Play` |
| `SuggestFor(ctx, SuggestRequest{GuildID, UserID, Query})` | Songs for autocomplete, at most `MaxSuggestions`. Once that person stops typing, the first is looked up if the budget allows. A link gets none, and is looked up ahead instead |
| `Suggest(ctx, words)` | `SuggestFor` without saying who's typing, so everyone's keystrokes replace each other's early lookups |
| `JoinEarly`, `GetReady` | Get ready for someone typing: join their channel, warm up YouTube, start an ffmpeg |
| `Radio(ctx, song)` | YouTube Music's radio for a song: about 50 songs to pick from when your bot chooses what plays next. Remembered like searches. A song without a YouTube video, like SoundCloud's, has none: `ErrNoRadio` |
| `Pause`, `Skip`, `Stop`, `StopSong` | Control the music. `StopSong` stops only while a given song plays, so an old song's buttons can't stop a newer one |
| `Remove(guildID, track)` | Takes a song out of the queue before its turn, like a pick that's no longer wanted. A song that has started isn't removed |
| `Playing`, `UpNext`, `UpNextTrack`, `Channel` | What's playing, what's next and how many after it, and where. `UpNextTrack` gives the next song's `Track`, for its `Meta` |
| `Moved`, `Disconnected` | Pass on the bot's own voice state changes, when someone moves it or disconnects it |

A `Track` is one song: `Song()` gives its title, artist, length and link, `Played()` how far it has played, `Cover()` its best picture, `Autoplay()` whether your bot chose it by itself, and `Meta` whatever your bot gave `Request.Meta`, like who asked and where. Songs asked for without `Next` go ahead of the ones queued with `Request.Autoplay`.

Your `Events` hear what happens: `Started` when a song from the queue starts by itself, `Ended` when one is over by itself or skipped (with who skipped it, or why it broke off), `Left` when the engine leaves voice by itself, after nothing played or a song stayed paused for a while, and `QueueChanged` when a song that won't start is passed over. A song you stop isn't told about: `Stop` and `Disconnected` return it. The engine never calls events while holding its own lock, so they may call it back, and your bot may call the engine while holding a lock its events take. They can run at the same time, for different servers or for one, and in no set order: a song that breaks off as soon as it starts can be `Ended` before it's `Started`. `Playing` says what plays now.

What the engine hands you is yours: `Song()`, `UpNext`, `Suggest`, `SuggestFor` and `Radio` return copies. Errors and logs have the links in them redacted, since a link's query can hold a token.

The engine leaves a channel 2 minutes after the music stops, 30 seconds after `JoinEarly` if no song comes, and after a song has been paused for 10 minutes. A queue holds up to `MaxQueue` (100) songs.

### Config

| Field | | Default |
|---|---|---|
| `Voice` | disgo's voice manager, set up with `DAVE.Create` | needed |
| `DAVE` | the voice connections' encryption sessions, from `NewDAVE` | needed |
| `Events` | hears what happens to the music | nobody hears |
| `HTTP` | asks the sites for songs and downloads them; yt-dlp and ffmpeg do their own networking | `NewClient()`, which keeps connections open between songs |
| `Logger` | where the engine logs | `slog.Default()` |
| `FFmpeg` | ffmpeg's path, or its name on `PATH`; `New` fails without it | `"ffmpeg"` |
| `YTDLP`, `Bun` | yt-dlp's and Bun's paths or names, for the backup | no backup |
| `Cache` | where yt-dlp keeps what it learned about YouTube's JavaScript | yt-dlp's own |
| `Cookies` | a spare YouTube account's `cookies.txt`, for bot checks; needs `YTDLP` | never signing in |
| `AllowPrivateLinks` | let songs come from the bot host's own network, like a music server at home; see below | off |
| `SearchMemory` | how long a search is reused for the same words, up to 1000 searches; one that found nothing is believed for a minute at most. A day suits friends who play the same songs again, though a day-old search may miss a new release. Radios are kept as long, up to 200 | 10 minutes |

### Errors

`Play` can fail with these, to check with `errors.Is` and explain to whoever asked:

| Error | Means |
|---|---|
| `lullaby.ErrNotFound` | a search found nothing |
| `lullaby.ErrJoin` | joining the voice channel failed |
| `lullaby.ErrFull` | the queue has `MaxQueue` songs |
| `lullaby.ErrStopped` | the music was stopped before the song could start |
| `songs.ErrUnplayable` | private, gone, blocked here, only a preview, far too big to be a song, or a link to the bot host's own network |
| `songs.ErrPlaylist` | a playlist or channel link; lullaby plays one song at a time |
| `songs.ErrNoMatch` | a Spotify, Apple Music, Deezer or Yandex Music song that isn't on YouTube Music |
| `songs.ErrNoYTDLP` | a link from another site, without yt-dlp |
| `songs.ErrNoAudio` | YouTube gave no plain audio link for the song, and there's no yt-dlp to try instead |
| `songs.ErrBotCheck` | YouTube wants a sign-in to prove the bot isn't a bot; see below |

`Radio` fails with `lullaby.ErrNoRadio` for a song without a YouTube video, like SoundCloud's.

### Packages

| Package | |
|---|---|
| [`lullaby`](https://pkg.go.dev/github.com/windrunner21/lullaby) | The engine |
| [`lullaby/songs`](https://pkg.go.dev/github.com/windrunner21/lullaby/songs) | The `Song` type, the errors above, and helpers for YouTube and SoundCloud links |
| `internal/...` | How songs are found and played: the sites' clients, ffmpeg and the downloads. Internal, so they can change as the sites do without breaking your bot |
| `cmd/voicebench` | The benchmark against Lavalink |
| `examples/bot` | A small music bot |

## Strangers' links

In a public server, anyone can paste a link. By default, lullaby refuses links to the bot host's own network, like `http://192.168.0.1/` or `http://localhost:2333/`, with `songs.ErrUnplayable`, so a stranger can't make the bot fetch its owner's router or a cloud's metadata service. Its own requests connect only to public addresses: through an `*http.Transport`, like `NewClient`'s, they're checked as they connect, redirects included; through another kind of transport, each request's host is looked up and checked first, which can't promise where it then connects; behind a proxy, only the proxy's address is checked. A refused address isn't asked again, and isn't handed to yt-dlp or ffmpeg instead. Links handed to yt-dlp, or to ffmpeg for a playlist lullaby doesn't read itself, are checked first, but those programs resolve names, follow redirects and download on their own, without lullaby's limits, so for them it's a best effort: run the bot where it can't reach what it shouldn't if that matters. Set `Config.AllowPrivateLinks` to play from a server of your own at home.

lullaby also keeps strangers' links from costing much: yt-dlp's answer, playlists and their pieces have sizes far above any song's, and yt-dlp runs with `--ignore-config`, so what it does is up to lullaby, not config files on the machine. yt-dlp gets the link after `--`, so a link can't pass it options.

## Tips for speed

- **Call `JoinEarly` and `GetReady` from autocomplete**, with the typist's voice channel, and use `SuggestFor`, with who's typing, for the choices. By the time they press Enter, the bot is in voice, ffmpeg is running, and a pasted link, or the first song suggested, is usually found.
- **Tell Discord to wait while `Play` works, not before**: start `DeferCreateMessage` in a goroutine, call `Play`, then wait for the defer before editing the answer. Each answer to Discord can take 0.2–0.4 s, longer than finding a YouTube song. [`examples/bot`](examples/bot/main.go) does this.
- **Use a lean ffmpeg** (above): it starts in 0.2 ms rather than about 95 ms, which shows wherever a song gets an ffmpeg that wasn't started ahead, and each ffmpeg running takes a sixth of the memory or less.
- **Keep one engine** for every server. It keeps its connections to the sites open between songs, remembers searches (10 minutes, or `SearchMemory`), and shares lookups between people asking for the same song. If you pass your own `Config.HTTP`, keep its connections open for minutes, as `NewClient` does, and base it on an `*http.Transport`: with another kind of transport, lullaby has to look up each request's host itself before sending it, one more DNS lookup each time.

## Keeping YouTube working

lullaby asks YouTube for songs as its iPhone app does, the way [youtube-source](https://github.com/lavalink-devs/youtube-source) does for Lavalink, which gets plain audio links without running YouTube's JavaScript. Now and then YouTube stops accepting the app version lullaby says it is (`iosVersion` in `internal/sources/youtube.go`). Songs then play through yt-dlp, and the version in youtube-source's `Ios.java` usually fixes it: update lullaby.

YouTube judges the internet address asking. After a few hundred automated requests in a day, it can ask every request from that address to "Sign in to confirm you're not a bot", for hours, and lullaby returns `songs.ErrBotCheck`. Test against YouTube gently.

### Signing in during a bot check

With a spare account's cookies in `Config.Cookies`, lullaby signs in through yt-dlp while YouTube asks, and goes back to not signing in 15 minutes later. Songs take a few seconds longer while it's signed in. Use a spare account, never your own: Google can block accounts used like this.

1. Make a new Google account. In Chrome or Chromium, install [Get cookies.txt LOCALLY](https://chrome.google.com/webstore/detail/get-cookiestxt-locally/cclelndahbckbenkjhflpdbgdldlbecc) and turn on **Allow in Incognito** in its details. In Firefox, use [cookies.txt](https://addons.mozilla.org/en-US/firefox/addon/cookies-txt/).
2. Open one private window, and sign in to YouTube there with the spare account.
3. In the same tab, go to `https://www.youtube.com/robots.txt`, and export the cookies with the extension as `youtube-cookies.txt`.
4. Close the private window, and don't open YouTube with that account again. YouTube renews the cookies of every session left open, and the exported ones stop working.
5. Run `chmod 600 youtube-cookies.txt`, and give its path as `Config.Cookies`, with yt-dlp in `Config.YTDLP`. The bot needs to write to it too: yt-dlp saves the cookies YouTube renews.

The file signs in to the account, like a password, so keep it to yourself and out of git. If the log says the cookies stopped working, export them again the same way.

Playing music from YouTube in Discord may go against YouTube's terms. Use lullaby for your own servers and friends.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) has how the engine works, what we learned the hard way about YouTube, SoundCloud, DAVE and disgo, and how to test. `make check` before you send a change.

## Credits

The iOS client approach follows [youtube-source](https://github.com/lavalink-devs/youtube-source) (MIT). [yt-dlp](https://github.com/yt-dlp/yt-dlp) is the backup. [dave-go](https://github.com/thomas-vilte/dave-go) and [godave](https://github.com/disgoorg/godave) provide DAVE, and [disgo](https://github.com/disgoorg/disgo) the rest of Discord.

## License

[MIT](LICENSE)
