package capture

import (
	"errors"
	"strings"
	"testing"
)

// U1: fps defaults to 15; 3 and 60 are accepted; 2, 61, -1 and
// non-integers are refused with a message naming 3–60.
func TestOptions_FPS(t *testing.T) {
	if o, err := (Options{}).Normalize(); err != nil || o.FPS != 15 {
		t.Fatalf("default = %+v, %v", o, err)
	}
	for _, v := range []int{3, 60} {
		if o, err := (Options{FPS: v, Audio: true}).Normalize(); err != nil || o.FPS != v || !o.Audio {
			t.Errorf("Normalize(%d) = %+v, %v", v, o, err)
		}
	}
	for _, v := range []int{2, 61, -1} {
		if _, err := (Options{FPS: v}).Normalize(); err == nil || !strings.Contains(err.Error(), "from 3 to 60") {
			t.Errorf("Normalize(%d) err = %v", v, err)
		}
	}
	for in, want := range map[string]int{"": 15, "3": 3, " 60 ": 60, "15": 15} {
		if got, err := ParseFPS(in); err != nil || got != want {
			t.Errorf("ParseFPS(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"2", "61", "-1", "15.5", "abc", "1e1"} {
		if _, err := ParseFPS(in); err == nil || !strings.Contains(err.Error(), "from 3 to 60") || !strings.Contains(err.Error(), in) {
			t.Errorf("ParseFPS(%q) err = %v", in, err)
		}
	}
}

func TestUnavailable(t *testing.T) {
	u := &Unavailable{Reason: ReasonCameraPermission, Message: "denied in Settings"}
	if u.Error() != "screen capture unavailable (camera-permission): denied in Settings" {
		t.Errorf("Error = %q", u.Error())
	}
	if (&Unavailable{Reason: ReasonNoFrames}).Error() != "screen capture unavailable: no-frames" {
		t.Error("Error without message")
	}
	wrapped := errors.Join(errors.New("attach"), u)
	if ReasonOf(wrapped, "x") != ReasonCameraPermission || ReasonOf(errors.New("plain"), "x") != "x" || ReasonOf(nil, "y") != "y" {
		t.Error("ReasonOf")
	}
	if err := asUnavailable(u); err != u {
		t.Errorf("asUnavailable kept %v", err)
	}
	var got *Unavailable
	if !errors.As(asUnavailable(nil), &got) || got.Reason != ReasonCaptureLost || got.Message != "" {
		t.Errorf("asUnavailable(nil) = %v", got)
	}
	for _, r := range []string{ReasonSimulator, ReasonUnsupportedPlatform, ReasonNotMacOS, ReasonNoCaptureDevice,
		ReasonAmbiguousDeviceName, ReasonHelperFailed, ReasonAndroidTooOld, ReasonScrcpyFailed} {
		if r == "" || strings.ContainsAny(r, " _") {
			t.Errorf("reason %q", r)
		}
	}
}

func TestCodecStrings(t *testing.T) {
	v := &VideoFormat{SPS: [][]byte{{0x67, 0x64, 0x00, 0x28, 0xac}}}
	if s, err := VideoCodec(v); err != nil || s != "avc3.640028" {
		t.Errorf("VideoCodec = %q, %v", s, err)
	}
	if _, err := VideoCodec(&VideoFormat{}); err == nil {
		t.Error("VideoCodec without an SPS")
	}
	if _, err := VideoCodec(nil); err == nil {
		t.Error("VideoCodec(nil)")
	}
	for asc, want := range map[string]string{"\x11\x90": "mp4a.40.2", "\x2b\x92\x08\x00": "mp4a.40.5", "\xf8\x80": "mp4a.40.36"} {
		if s, err := AudioCodec(&AudioFormat{Codec: CodecAAC, ASC: []byte(asc)}); err != nil || s != want {
			t.Errorf("AudioCodec(%x) = %q, %v; want %s", asc, s, err, want)
		}
	}
	for _, f := range []*AudioFormat{nil, {Codec: CodecAAC}, {Codec: CodecAAC, ASC: []byte{0xf8}}, {Codec: "opus"}} {
		if _, err := AudioCodec(f); err == nil {
			t.Errorf("AudioCodec(%+v) succeeded", f)
		}
	}
	if s, _ := AudioCodec(&AudioFormat{Codec: CodecLPCM}); s != "ipcm" {
		t.Errorf("lpcm codec = %q", s)
	}
	if s, _ := AudioCodec(&AudioFormat{Codec: CodecLPCM, Float: true}); s != "fpcm" {
		t.Errorf("float lpcm codec = %q", s)
	}
	if s, err := Codecs(v, &AudioFormat{Codec: CodecLPCM}); err != nil || s != "avc3.640028,ipcm" {
		t.Errorf("Codecs = %q, %v", s, err)
	}
	if s, err := Codecs(v, nil); err != nil || s != "avc3.640028" {
		t.Errorf("Codecs video only = %q, %v", s, err)
	}
	if _, err := Codecs(nil, nil); err == nil {
		t.Error("Codecs without video")
	}
	if _, err := Codecs(v, &AudioFormat{Codec: "opus"}); err == nil {
		t.Error("Codecs with a bad audio codec")
	}
}
