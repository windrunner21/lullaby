package lullaby_test

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby"
)

// A bot sets disgo up with DAVE's sessions and voice states in its cache,
// makes the engine, and plays songs in the voice channel of whoever asks.
// examples/bot in the repository is a whole bot.
func Example() {
	ctx := context.Background()
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
		log.Fatal(err)
	}
	music, err := lullaby.New(lullaby.Config{Voice: client.VoiceManager, DAVE: dave})
	if err != nil {
		log.Fatal(err)
	}
	music.Start(ctx) // warms up the connections, so the first song doesn't wait for them

	// In a /play handler, with the guild and the asker's voice channel:
	var guildID, voiceChannelID snowflake.ID
	track, place, err := music.Play(ctx, lullaby.Request{GuildID: guildID, ChannelID: voiceChannelID, Query: "snooze sza"})
	switch {
	case err != nil:
		fmt.Println("couldn't play it:", err)
	case place > 0:
		fmt.Printf("%s is number %d in line\n", track.Song().Title, place)
	default:
		fmt.Println("playing", track.Song().Title)
	}
}

// Autocomplete suggests songs while someone types, and gets ready for the one
// they'll pick: it joins their voice channel, warms up YouTube and starts an
// ffmpeg, so the song starts as soon as they press Enter.
func ExampleEngine_Suggest() {
	var music *lullaby.Engine
	autocomplete := func(e *handler.AutocompleteEvent, voiceChannelID snowflake.ID) error {
		music.JoinEarly(*e.GuildID(), voiceChannelID)
		music.GetReady()
		choices := []discord.AutocompleteChoice{}
		for _, s := range music.Suggest(e.Ctx, e.Data.String("song")) {
			// The value is the song's link, which Play plays at once, with
			// YouTube Music's title and artist.
			choices = append(choices, discord.AutocompleteChoiceString{Name: s.Title + " · " + s.Artist, Value: s.Link})
		}
		return e.AutocompleteResult(choices)
	}
	_ = autocomplete
}

// cards posts a message when a song from the queue starts, in the text
// channel it was asked for in, which it keeps as the Track's Meta. The
// client is set up to ping nobody, as in Example.
type cards struct {
	client *bot.Client
}

func (c cards) Started(_ snowflake.ID, t *lullaby.Track) {
	channelID := t.Meta.(snowflake.ID)
	c.client.Rest.CreateMessage(channelID, discord.MessageCreate{Content: "Now playing " + t.Song().Title})
}

func (cards) Ended(snowflake.ID, *lullaby.Track, lullaby.End) {}
func (cards) Left(snowflake.ID, *lullaby.Track)               {}
func (cards) QueueChanged(snowflake.ID)                       {}

// Events hears what happens to the music. A song's own data, like where it was
// asked for, rides along as Request.Meta and comes back as Track.Meta.
func ExampleEvents() {
	// With a client and DAVE set up as in Example:
	newMusic := func(client *bot.Client, dave *lullaby.DAVE) (*lullaby.Engine, error) {
		return lullaby.New(lullaby.Config{Voice: client.VoiceManager, DAVE: dave, Events: cards{client: client}})
	}
	// In /play, the text channel it was asked in rides along as Meta:
	play := func(ctx context.Context, music *lullaby.Engine, guildID, voiceChannelID, textChannelID snowflake.ID) error {
		_, _, err := music.Play(ctx, lullaby.Request{GuildID: guildID, ChannelID: voiceChannelID, Query: "snooze sza", Meta: textChannelID})
		return err
	}
	_, _ = newMusic, play
}
