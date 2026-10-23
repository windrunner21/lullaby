// Command voicebench compares lullaby with Lavalink the way
// Discord would hear them: both connect to a stand-in Discord voice server on
// the same machine, which notes when each one's audio arrives, how evenly it
// comes, and how much memory and CPU each engine takes. BENCHMARKS.md has
// the results and how to run it.
//
//	voicebench server <cert.pem>                    the stand-in voice server, which writes its certificate to cert.pem
//	voicebench lullaby <cert.pem> <warm-up> <links>… lullaby plays the warm-up untimed, then each link
//	voicebench lavalink <pids> <warm-up> <links>…   Lavalink, running already; pids are its own and its helpers', like 1234,5678
//
// It asks YouTube for every song, so keep the links few: a few hundred
// requests in a day got the home address bot-checked for hours.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 3 || os.Args[1] != "server" && len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: voicebench server <cert.pem> | lullaby <cert.pem> <warm-up> <links>… | lavalink <pids> <warm-up> <links>…")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		serve(os.Args[2])
	case "lullaby":
		runLullaby(os.Args[2], os.Args[3], os.Args[4:])
	case "lavalink":
		var pids []int
		for _, f := range strings.Split(os.Args[2], ",") {
			pid, _ := strconv.Atoi(f)
			pids = append(pids, pid)
		}
		runLavalink(pids, os.Args[3], os.Args[4:])
	default:
		log.Fatalf("no %q: it's server, lullaby or lavalink", os.Args[1])
	}
}

const (
	// playFor is how long each song plays while memory, CPU and pacing are
	// measured.
	playFor = 15 * time.Second
	// botID, voiceChannel and the guilds are made up: nothing reaches
	// Discord. Each engine plays in a guild of its own.
	botID         = 42
	voiceChannel  = 100
	lullabyGuild  = 1
	lavalinkGuild = 2
)

// result is one song played by one engine.
type result struct {
	Engine  string  `json:"engine"`
	Link    string  `json:"link"`
	Title   string  `json:"title"`
	Lookup  float64 `json:"lookupMs"`     // until the song was found
	First   float64 `json:"firstAudioMs"` // until its first packet reached the voice server
	Packets int     `json:"packets"`
	Late    int     `json:"late"` // packets more than 30 ms after the one before
	MaxGap  float64 `json:"maxGapMs"`
	Err     string  `json:"err,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// control asks the voice server's control API for path, and decodes its
// answer into out.
func control(path string, out any) error {
	resp, err := http.Get("http://" + controlAddr + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// firstAudio waits for a guild's first packet after t0.
func firstAudio(guild int, t0 time.Time) (time.Time, error) {
	var got struct {
		At int64 `json:"at"`
	}
	if err := control(fmt.Sprintf("/first?guild=%d&after=%d", guild, t0.UnixNano()), &got); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, got.At), nil
}

// pacing fills in how evenly a guild's packets came between two times, or
// why that couldn't be measured.
func pacing(r *result, guild int, from, to time.Time) {
	var got struct {
		Packets int     `json:"packets"`
		Late    int     `json:"late"`
		MaxGap  float64 `json:"maxGapMs"`
	}
	if err := control(fmt.Sprintf("/stats?guild=%d&from=%d&to=%d", guild, from.UnixNano(), to.UnixNano()), &got); err != nil {
		r.fail(fmt.Errorf("pacing: %w", err))
		return
	}
	r.Packets, r.Late, r.MaxGap = got.Packets, got.Late, got.MaxGap
}

// fail adds why something about the song couldn't be measured.
func (r *result) fail(err error) {
	if r.Err != "" {
		r.Err += "; "
	}
	r.Err += err.Error()
}

// report prints a song's result, as JSON for keeping and as a line to read.
func report(r result) {
	b, _ := json.Marshal(r)
	fmt.Println("RESULT", string(b))
	log.Printf("%-8s %-46q found %7.1f ms, first audio %7.1f ms, %d packets, %d late, max gap %.1f ms %s",
		r.Engine, r.Title, r.Lookup, r.First, r.Packets, r.Late, r.MaxGap, r.Err)
}

// markAs tells the voice server, which measures the engine, what it's doing.
func markAs(pid int, state string) {
	control(fmt.Sprintf("/mark?pid=%d&state=%s", pid, state), nil)
}

// summary prints what the voice server measured of an engine.
func summary(engine string, pid int) {
	var got map[string]float64
	if err := control(fmt.Sprintf("/usage?pid=%d", pid), &got); err != nil {
		log.Printf("usage: %v", err)
		return
	}
	got["pid"] = float64(pid)
	b, _ := json.Marshal(map[string]any{"engine": engine, "usage": got})
	fmt.Println("USAGE", string(b))
	log.Printf("%s: idle %.1f MB; playing %.1f MB on average, %.1f MB at most; CPU %.1f%% of a core idle, %.1f%% playing",
		engine, got["idleMB"], got["playingMeanMB"], got["playingPeakMB"], got["idleCPUPercent"], got["playingCPUPercent"])
}

// pss reads a process's proportional set size in MB.
func pss(pid int) float64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Pss:") {
			kb, _ := strconv.Atoi(strings.Fields(line)[1])
			return float64(kb) / 1024
		}
	}
	return 0
}

// ticks reads a process's CPU time: its own, and its waited-for children's.
// It counts in Linux's 100 ticks a second, as on the Pi.
func ticks(pid int) (own, children time.Duration) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0
	}
	// The command name can hold spaces, so count from the closing parenthesis.
	f := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+2:])
	n := func(i int) time.Duration {
		v, _ := strconv.ParseInt(f[i], 10, 64)
		return time.Duration(v) * 10 * time.Millisecond
	}
	// utime, stime, cutime and cstime are fields 14 to 17, 12 to 15 here.
	return n(11) + n(12), n(13) + n(14)
}

// childrenOf lists the processes whose parent is pid.
func childrenOf(pid int) []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		f := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+2:])
		if ppid, _ := strconv.Atoi(f[1]); ppid == pid {
			out = append(out, child)
		}
	}
	return out
}
