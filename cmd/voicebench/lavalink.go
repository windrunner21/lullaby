package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	lavalinkAddr = "127.0.0.1:2333"
	// lavalinkPassword is the one Lavalink is started with, through
	// LAVALINK_SERVER_PASSWORD.
	lavalinkPassword = "benchpass"
)

// lavalinkDo calls Lavalink's REST API, with body as JSON, and decodes its
// answer into out.
func lavalinkDo(method, path string, body any, out any) error {
	var r io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+lavalinkAddr+path, r)
	req.Header.Set("Authorization", lavalinkPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e map[string]any
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: %s %v", method, path, resp.Status, e)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// runLavalink plays the warm-up and then each link with Lavalink, the way a
// bot uses it: in voice already, it loads each track, then plays it. pids are
// Lavalink's and its helpers', like the cipher server youtube-source needs,
// which are measured too.
func runLavalink(pids []int, warmUp string, links []string) {
	markAll := func(state string) {
		for _, p := range pids {
			markAs(p, state)
		}
	}
	header := http.Header{"Authorization": {lavalinkPassword}, "User-Id": {fmt.Sprint(botID)}, "Client-Name": {"voicebench/1.0"}}
	ws, _, err := websocket.DefaultDialer.Dial("ws://"+lavalinkAddr+"/v4/websocket", header)
	if err != nil {
		log.Fatal(err)
	}
	var hello struct {
		Op        string `json:"op"`
		SessionID string `json:"sessionId"`
	}
	if err := ws.ReadJSON(&hello); err != nil || hello.Op != "ready" {
		log.Fatalf("lavalink said %+v, %v", hello, err)
	}
	go func() {
		for {
			var m map[string]any
			if err := ws.ReadJSON(&m); err != nil {
				return
			}
			if m["op"] == "event" && m["type"] != "TrackStartEvent" && m["type"] != "TrackEndEvent" {
				log.Printf("lavalink event: %v", m)
			}
		}
	}()
	players := fmt.Sprintf("/v4/sessions/%s/players/%d", hello.SessionID, lavalinkGuild)
	if err := lavalinkDo(http.MethodPatch, players, map[string]any{"voice": map[string]any{
		"token": "bench", "endpoint": gatewayAddr, "sessionId": "bench", "channelId": fmt.Sprint(voiceChannel)}}, nil); err != nil {
		log.Fatal(err)
	}
	if err := control(fmt.Sprintf("/ready?guild=%d", lavalinkGuild), nil); err != nil {
		log.Fatal("lavalink never joined voice: ", err)
	}

	markAll("idle")
	time.Sleep(10 * time.Second)
	markAll("")
	play := func(link string, timed bool) {
		r := result{Engine: "lavalink", Link: link}
		if timed {
			markAll("playing")
		}
		start := time.Now()
		// What a bot does with Lavalink: load the track, then play it.
		var loaded struct {
			LoadType string `json:"loadType"`
			Data     struct {
				Encoded string `json:"encoded"`
				Info    struct {
					Title string `json:"title"`
				} `json:"info"`
				Message string `json:"message"`
			} `json:"data"`
		}
		err := lavalinkDo(http.MethodGet, "/v4/loadtracks?identifier="+url.QueryEscape(link), nil, &loaded)
		r.Lookup = ms(time.Since(start))
		if err == nil && loaded.LoadType != "track" {
			err = fmt.Errorf("loaded %s: %s", loaded.LoadType, loaded.Data.Message)
		}
		if err == nil {
			err = lavalinkDo(http.MethodPatch, players, map[string]any{"track": map[string]any{"encoded": loaded.Data.Encoded}}, nil)
		}
		if err != nil {
			r.Err = err.Error()
			markAll("")
			report(r)
			if strings.Contains(r.Err, "not a bot") {
				log.Fatal("YouTube wants proof Lavalink isn't a robot: stopping")
			}
			return
		}
		r.Title = loaded.Data.Info.Title
		first, err := firstAudio(lavalinkGuild, start)
		if err != nil {
			r.fail(err)
		} else {
			r.First = ms(first.Sub(start))
		}
		time.Sleep(playFor)
		if err == nil {
			pacing(&r, lavalinkGuild, first, first.Add(playFor))
		}
		lavalinkDo(http.MethodPatch, players, map[string]any{"track": map[string]any{"encoded": nil}}, nil)
		if timed {
			markAll("after")
		}
		time.Sleep(2 * time.Second)
		markAll("")
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
	summary("lavalink", pids[0])
	for _, p := range pids[1:] {
		summary("lavalink's cipher server", p)
	}
	lavalinkDo(http.MethodDelete, players, nil, nil)
}
