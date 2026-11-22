package cli

import (
	"image"
	"testing"
)

func TestShotFlags(t *testing.T) {
	for out, want := range map[string]string{"a.png": "png", "a.JPG": "jpeg", "a.jpeg": "jpeg", "a": "", "-": ""} {
		o, err := shotFlags{}.options(out)
		if err != nil || o.Image != want {
			t.Errorf("%s: %q %v", out, o.Image, err)
		}
	}
	o, err := shotFlags{image: "PNG", quality: 70, region: "1, 2,3,4"}.options("x.jpg")
	if err != nil || o.Image != "png" || o.Quality != 70 || *o.Region != image.Rect(1, 2, 4, 6) {
		t.Errorf("%+v %v", o, err)
	}
	for _, bad := range []string{"1,2,3", "a,b,c,d", "1,2,3,x", "100,100,-50,-50", "1,1,0,5", "1,1,5,-1"} {
		if _, err := (shotFlags{region: bad}).options(""); err == nil {
			t.Errorf("--region %q accepted", bad)
		}
	}
	if extensionFor("image/jpeg") != "jpg" || extensionFor("image/png") != "png" || extensionFor("") != "png" {
		t.Error("extensionFor")
	}
}
