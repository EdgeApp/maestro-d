package cli

// maestro-d: `get screenshot`'s --image, --quality and --region.

import (
	"image"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type shotFlags struct {
	image   string
	quality int
	region  string
}

// options builds the request: --image, else the image type --out's
// extension names, else the device's default.
func (f shotFlags) options(out string) (daemon.ScreenshotOptions, error) {
	o := daemon.ScreenshotOptions{Image: strings.ToLower(f.image), Quality: f.quality}
	if o.Image == "" {
		switch strings.ToLower(filepath.Ext(out)) {
		case ".png":
			o.Image = "png"
		case ".jpg", ".jpeg":
			o.Image = "jpeg"
		}
	}
	if f.region != "" {
		parts := strings.Split(f.region, ",")
		if len(parts) != 4 {
			return o, daemon.Errorf(daemon.CodeUsage, "--region %q: use x,y,width,height", f.region)
		}
		var v [4]int
		for i, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return o, daemon.Errorf(daemon.CodeUsage, "--region %q: %q is not a whole number", f.region, p)
			}
			v[i] = n
		}
		if v[2] < 1 || v[3] < 1 {
			return o, daemon.Errorf(daemon.CodeUsage, "--region %q: width and height must be at least 1", f.region)
		}
		r := image.Rect(v[0], v[1], v[0]+v[2], v[1]+v[3])
		o.Region = &r
	}
	return o, nil
}

// extensionFor is the file extension for a screenshot's content type.
func extensionFor(contentType string) string {
	if strings.Contains(contentType, "jpeg") {
		return "jpg"
	}
	return "png"
}
