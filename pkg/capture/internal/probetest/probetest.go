// Package probetest runs ffprobe for the capture packages' tests. Without
// ffprobe on PATH a check skips, unless MAESTRO_D_REQUIRE_FFPROBE=1 (set in
// CI), when it fails.
package probetest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Stream is one stream as ffprobe reports it.
type Stream struct {
	CodecName  string `json:"codec_name"`
	CodecTag   string `json:"codec_tag_string"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SampleRate string `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Frames     string `json:"nb_read_frames"`
}

// FrameCount is nb_read_frames as a number.
func (s Stream) FrameCount() int {
	n, _ := strconv.Atoi(s.Frames)
	return n
}

// Result is ffprobe's report on a file.
type Result struct {
	Streams []Stream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// Duration is the container duration in seconds.
func (r Result) Duration() float64 {
	d, _ := strconv.ParseFloat(r.Format.Duration, 64)
	return d
}

// Find returns the first stream with codec name, or false.
func (r Result) Find(codec string) (Stream, bool) {
	for _, s := range r.Streams {
		if s.CodecName == codec {
			return s, true
		}
	}
	return Stream{}, false
}

// Binary returns ffprobe's path, or skips (fails when ffprobe is
// required).
func Binary(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("ffprobe")
	if err != nil {
		if os.Getenv("MAESTRO_D_REQUIRE_FFPROBE") == "1" {
			t.Fatalf("ffprobe is required (MAESTRO_D_REQUIRE_FFPROBE=1) but not on PATH: %v", err)
		}
		t.Skip("ffprobe not on PATH")
	}
	return p
}

// Probe runs `ffprobe -v error -show_entries
// stream=codec_name,codec_tag_string,width,height,sample_rate,channels,nb_read_frames:format=duration
// -count_frames -of json` on path, failing the test when ffprobe exits
// non-zero or reports any error (decoding every frame included).
func Probe(t testing.TB, path string) Result {
	t.Helper()
	bin := Binary(t)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "-v", "error",
		"-show_entries", "stream=codec_name,codec_tag_string,width,height,sample_rate,channels,nb_read_frames:format=duration",
		"-count_frames", "-of", "json", path)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var r Result
	if err == nil {
		err = json.Unmarshal(stdout.Bytes(), &r)
	}
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("ffprobe %s: %v\n%s%s", path, err, stderr.String(), stdout.String())
	}
	return r
}
