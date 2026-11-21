//go:build darwin

package computer

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The helper is built from the repository's source into a temporary cache
// and run on images; what it reads on real login screens is proven end to
// end (e2e/computer.e2e.ts), and Classify's rules by TestClassify.
func TestVisionRecognizer(t *testing.T) {
	if _, err := os.Stat("/usr/bin/xcrun"); err != nil {
		t.Skip("no xcrun")
	}
	src, _ := filepath.Abs("../../../drivers/macos/MaestroDText")
	cache := t.TempDir()
	oldSrc, oldCache := helperSourceDir, helperCacheDir
	helperSourceDir = func() string { return src }
	helperCacheDir = func() string { return cache }
	textBuildMu.Lock()
	textBuilt = ""
	textBuildMu.Unlock()
	t.Cleanup(func() {
		helperSourceDir, helperCacheDir = oldSrc, oldCache
		textBuildMu.Lock()
		textBuilt = ""
		textBuildMu.Unlock()
	})

	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	img.Set(1, 1, color.Black)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	boxes, err := (VisionRecognizer{}).Recognize(context.Background(), buf.Bytes())
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(boxes) != 0 {
		t.Errorf("text in a blank image: %+v", boxes)
	}
	var rec VisionRecognizer
	if _, err := rec.Recognize(context.Background(), []byte("not an image")); err == nil || !strings.Contains(err.Error(), "not an image") {
		t.Errorf("garbage: %v", err)
	}
	// A second build of the same source reuses the cached binary.
	bin, err := buildTextHelper(context.Background(), src, cache)
	if err != nil || !strings.HasPrefix(bin, cache) {
		t.Errorf("cached build: %s %v", bin, err)
	}
	if _, err := buildTextHelper(context.Background(), t.TempDir(), cache); err == nil {
		t.Error("built without a source")
	}
}

func TestParseBoxes(t *testing.T) {
	b, err := parseBoxes([]byte(`[{"text":"Enter Password","x":0.4,"y":0.9,"w":0.1,"h":0.02}]`))
	if err != nil || len(b) != 1 || b[0].Text != "Enter Password" || b[0].CenterX() != 0.45 {
		t.Errorf("%+v %v", b, err)
	}
	if _, err := parseBoxes([]byte("nope")); err == nil {
		t.Error("bad output accepted")
	}
	f := RecognizerFunc(func(context.Context, []byte) ([]TextBox, error) { return b, nil })
	if got, _ := f.Recognize(context.Background(), nil); len(got) != 1 {
		t.Error("RecognizerFunc")
	}
}
