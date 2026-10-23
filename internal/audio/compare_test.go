package audio

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCompareFFmpeg measures ffmpegs side by side, as lullaby runs them:
// how long one takes to start, to its first sound, how much memory it takes
// waiting and playing, and its CPU for a whole song. It runs only when told
// which ffmpegs and which song; BENCHMARKS.md says how.
func TestCompareFFmpeg(t *testing.T) {
	builds, song := os.Getenv("LULLABY_COMPARE_FFMPEG"), os.Getenv("LULLABY_COMPARE_SONG")
	if builds == "" || song == "" {
		t.Skip("set LULLABY_COMPARE_FFMPEG to ffmpegs, separated by commas, and LULLABY_COMPARE_SONG to a song")
	}
	input, err := os.ReadFile(song)
	if err != nil {
		t.Fatal(err)
	}
	for _, ffmpeg := range strings.Split(builds, ",") {
		t.Logf("%s", ffmpeg)

		var starts []time.Duration
		for range 15 {
			began := time.Now()
			if err := exec.Command(ffmpeg, "-hide_banner", "-version").Run(); err != nil {
				t.Fatal(err)
			}
			starts = append(starts, time.Since(began))
		}
		t.Logf("  starting (-version), median of 15: %v", median(starts))

		var cold, warm []time.Duration
		for range 10 {
			c := startCompared(t, ffmpeg)
			began := time.Now()
			go c.feed(input)
			cold = append(cold, c.firstSound().Sub(began))
			c.stop()

			c = startCompared(t, ffmpeg)
			time.Sleep(500 * time.Millisecond) // a spare, started while someone types
			began = time.Now()
			go c.feed(input)
			warm = append(warm, c.firstSound().Sub(began))
			c.stop()
		}
		t.Logf("  first sound, started with its song: median %v; already running: median %v", median(cold), median(warm))

		// Waiting for a song, as the spare: one, then three at once.
		var spares []*compared
		for range 3 {
			spares = append(spares, startCompared(t, ffmpeg))
		}
		time.Sleep(time.Second)
		all := 0
		for i, c := range spares {
			pss, private := memoryOf(c.cmd.Process.Pid)
			if i == 0 {
				t.Logf("  idle: PSS %d KB, private %d KB", pss, private)
			}
			all += pss
		}
		t.Logf("  three idle: PSS %d KB in all", all)
		for _, c := range spares {
			c.stop()
		}

		// Playing: given the whole song and read only so far, as lullaby
		// reads 10 seconds ahead, so it waits mid-song.
		c := startCompared(t, ffmpeg)
		go c.feed(input)
		io.CopyN(io.Discard, c.out, 200<<10)
		time.Sleep(time.Second)
		pss, private := memoryOf(c.cmd.Process.Pid)
		t.Logf("  playing: PSS %d KB, private %d KB", pss, private)
		c.stop()

		var cpu []time.Duration
		for range 3 {
			cmd := exec.Command(ffmpeg, comparedArgs...)
			cmd.Stdin, cmd.Stdout = bytes.NewReader(input), io.Discard
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			cpu = append(cpu, cmd.ProcessState.UserTime()+cmd.ProcessState.SystemTime())
		}
		t.Logf("  CPU for the whole song, as fast as it goes: median %v", median(cpu))
	}
}

// comparedArgs are the arguments lullaby starts ffmpeg with for a song
// given on stdin, as start in stream.go puts them together.
var comparedArgs = []string{"-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-map", "0:a:0",
	"-c:a", "libopus", "-b:a", "128k", "-ar", "48000", "-ac", "2", "-frame_duration", "20",
	"-page_duration", "20000", "-f", "ogg", "pipe:1"}

// compared is one ffmpeg being measured.
type compared struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   io.ReadCloser
}

func startCompared(t *testing.T, ffmpeg string) *compared {
	t.Helper()
	cmd := exec.Command(ffmpeg, comparedArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &compared{cmd: cmd, stdin: stdin, out: out}
}

// feed gives it the song, and closes its input after, as a download does
// when it ends.
func (c *compared) feed(song []byte) {
	c.stdin.Write(song)
	c.stdin.Close()
}

func (c *compared) stop() {
	c.stdin.Close()
	c.cmd.Process.Kill()
	c.cmd.Wait()
}

// firstSound reads until the first Ogg page after the two header pages,
// the first one with sound, and returns when it came.
func (c *compared) firstSound() time.Time {
	var seen []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.out.Read(buf)
		seen = append(seen, buf[:n]...)
		if bytes.Count(seen, []byte("OggS")) >= 3 || err != nil {
			return time.Now()
		}
	}
}

// memoryOf reads a process's PSS, which shares the pages it shares with
// others fairly between them, and what's its alone, in KB.
func memoryOf(pid int) (pss, private int) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		n, _ := strconv.Atoi(fields[1])
		switch fields[0] {
		case "Pss:":
			pss = n
		case "Private_Clean:", "Private_Dirty:":
			private += n
		}
	}
	return pss, private
}

func median(d []time.Duration) time.Duration {
	slices.Sort(d)
	return d[len(d)/2]
}
