// Command bot is a small music bot built on lullaby: /play a song's name or
// link, with suggestions while you type, /skip and /stop.
//
//	DISCORD_TOKEN=... go run ./examples/bot
//
// It needs ffmpeg. With yt-dlp and Bun installed too, it also plays links
// from other sites, and YouTube when the fast way stops working.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby"
)

var commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "play",
		Description: "Play a song in your voice channel",
		Contexts:    []discord.InteractionContextType{discord.InteractionContextTypeGuild},
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{Name: "song", Description: "A song's name, or a link", Required: true, Autocomplete: true},
			discord.ApplicationCommandOptionBool{Name: "next", Description: "Play it before the rest of the queue"},
		},
	},
	discord.SlashCommandCreate{Name: "skip", Description: "Skip to the next song",
		Contexts: []discord.InteractionContextType{discord.InteractionContextTypeGuild}},
	discord.SlashCommandCreate{Name: "stop", Description: "Stop the music and leave",
		Contexts: []discord.InteractionContextType{discord.InteractionContextTypeGuild}},
}

func main() {
	if err := run(); err != nil {
		slog.Error("bot stopped", slog.Any("err", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Voice is encrypted end to end with DAVE, which Discord requires.
	dave := lullaby.NewDAVE()
	client, err := disgo.New(os.Getenv("DISCORD_TOKEN"),
		bot.WithGatewayConfigOpts(gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildVoiceStates)),
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagVoiceStates)),
		bot.WithVoiceManagerConfigOpts(voice.WithDaveSessionCreateFunc(dave.Create)),
		// Song titles come from strangers: one saying "@everyone" pings nobody.
		bot.WithRestConfigOpts(rest.WithDefaultAllowedMentions(discord.AllowedMentions{
			Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{},
		})),
	)
	if err != nil {
		return err
	}
	b := &musicBot{client: client}
	b.music, err = lullaby.New(lullaby.Config{
		Voice:  client.VoiceManager,
		DAVE:   dave,
		Events: b,
		YTDLP:  lookPath("yt-dlp"),
		Bun:    lookPath("bun"),
	})
	if err != nil {
		return err
	}

	r := handler.New()
	r.SlashCommand("/play", b.play)
	r.Autocomplete("/play", b.suggest)
	r.SlashCommand("/skip", b.skip)
	r.SlashCommand("/stop", b.stop)
	client.AddEventListeners(r, bot.NewListenerFunc(b.onVoiceUpdate))

	if _, err := client.Rest.SetGlobalCommands(client.ApplicationID, commands); err != nil {
		return fmt.Errorf("set commands: %w", err)
	}
	if err := b.music.Start(ctx); err != nil {
		return err
	}
	if err := client.OpenGateway(ctx); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	slog.Info("playing music; press Ctrl+C to stop")
	<-ctx.Done()

	closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client.Close(closing)
	return nil
}

// musicBot answers the commands, and is the lullaby.Events that hears what
// happens to the music.
type musicBot struct {
	client *bot.Client
	music  *lullaby.Engine
}

func (b *musicBot) play(data discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	guildID := *e.GuildID()
	channelID, ok := b.voiceChannel(guildID, e.User().ID)
	if !ok {
		return e.CreateMessage(discord.MessageCreate{Content: "Join a voice channel first.", Flags: discord.MessageFlagEphemeral})
	}
	// Telling Discord to wait takes a moment of its own, up to 0.3 s, so the
	// song is found meanwhile. The answer waits for it.
	deferred := make(chan error, 1)
	go func() { deferred <- e.DeferCreateMessage(false) }()
	t, place, err := b.music.Play(e.Ctx, lullaby.Request{
		GuildID:   guildID,
		ChannelID: channelID,
		Query:     data.String("song"),
		Next:      data.Bool("next"),
		Meta:      e.Channel().ID(), // where to say when it starts, if it waits in the queue
		Asked:     e.ID().Time(),
	})
	if derr := <-deferred; derr != nil {
		// The song may be playing, but there's no answer to edit.
		return fmt.Errorf("answer /play: %w", derr)
	}
	var text string
	switch {
	case err != nil:
		text = "I couldn't play that: " + err.Error()
	case place > 0:
		text = fmt.Sprintf("Added %s. It's number %d in line.", t.Song().Title, place)
	default:
		text = "Playing " + t.Song().Title
	}
	_, err = e.UpdateInteractionResponse(discord.MessageUpdate{Content: &text})
	return err
}

// suggest suggests songs while someone types in /play, and gets ready for the
// one they'll pick meanwhile: it joins their voice channel, warms up the
// connection to YouTube and starts an ffmpeg. A bot should check it may speak
// in the channel before joining it early.
func (b *musicBot) suggest(e *handler.AutocompleteEvent) error {
	guildID := *e.GuildID()
	if channelID, ok := b.voiceChannel(guildID, e.User().ID); ok {
		b.music.JoinEarly(guildID, channelID)
	}
	b.music.GetReady()
	// Discord gives autocomplete 3 seconds.
	ctx, cancel := context.WithTimeout(e.Ctx, 2500*time.Millisecond)
	defer cancel()
	choices := []discord.AutocompleteChoice{}
	for _, s := range b.music.SuggestFor(ctx, lullaby.SuggestRequest{GuildID: guildID, UserID: e.User().ID, Query: e.Data.String("song")}) {
		name := s.Title
		if s.Artist != "" {
			name += " · " + s.Artist
		}
		choices = append(choices, discord.AutocompleteChoiceString{Name: shorten(name, 100), Value: s.Link})
	}
	return e.AutocompleteResult(choices)
}

func (b *musicBot) skip(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	if !b.music.Skip(*e.GuildID(), e.User().ID) {
		return e.CreateMessage(discord.MessageCreate{Content: "Nothing's playing.", Flags: discord.MessageFlagEphemeral})
	}
	return e.CreateMessage(discord.MessageCreate{Content: "Skipped."})
}

func (b *musicBot) stop(_ discord.SlashCommandInteractionData, e *handler.CommandEvent) error {
	if _, ok := b.music.Stop(*e.GuildID()); !ok {
		return e.CreateMessage(discord.MessageCreate{Content: "Nothing's playing.", Flags: discord.MessageFlagEphemeral})
	}
	return e.CreateMessage(discord.MessageCreate{Content: "Stopped."})
}

// onVoiceUpdate follows the bot when someone moves it to another channel,
// and stops the music when someone disconnects it.
func (b *musicBot) onVoiceUpdate(e *events.GuildVoiceStateUpdate) {
	if e.VoiceState.UserID != e.Client().ID() {
		return
	}
	if e.VoiceState.ChannelID == nil {
		b.music.Disconnected(e.VoiceState.GuildID)
		return
	}
	b.music.Moved(e.VoiceState.GuildID, *e.VoiceState.ChannelID)
}

func (b *musicBot) voiceChannel(guildID, userID snowflake.ID) (snowflake.ID, bool) {
	state, ok := b.client.Caches.VoiceState(guildID, userID)
	if !ok || state.ChannelID == nil {
		return 0, false
	}
	return *state.ChannelID, true
}

// Started says in the song's text channel when a song from the queue starts.
func (b *musicBot) Started(_ snowflake.ID, t *lullaby.Track) {
	channelID := t.Meta.(snowflake.ID)
	if _, err := b.client.Rest.CreateMessage(channelID, discord.MessageCreate{Content: "Now playing " + t.Song().Title}); err != nil {
		slog.Warn("couldn't say what's playing", slog.Any("err", err))
	}
}

func (b *musicBot) Ended(_ snowflake.ID, t *lullaby.Track, end lullaby.End) {
	if end.Err != nil {
		slog.Warn("song broke off", slog.String("song", t.Song().Title), slog.Any("err", end.Err))
	}
}

func (b *musicBot) Left(snowflake.ID, *lullaby.Track) {}
func (b *musicBot) QueueChanged(snowflake.ID)         {}

// lookPath finds a program, or returns "" when it isn't installed.
func lookPath(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

// shorten cuts s to at most n characters, as Discord's choices allow.
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
