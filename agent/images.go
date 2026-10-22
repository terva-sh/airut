package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif" // registered for image.Decode: a GIF's first frame
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registered for image.Decode
	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
)

// maxImagePixels caps the pixels an image may claim before it is decoded,
// so a small file that declares a vast canvas is refused before anything
// is allocated for it.
const maxImagePixels = 64 << 20

// maxSentImageBytes is the most a prepared image may hold, the tightest
// per-image limit among the major vision APIs. A PNG larger than this,
// as a photograph saved losslessly can be, is sent as a JPEG, and a JPEG
// larger than this at a lower quality or smaller. A variable for tests.
var maxSentImageBytes = 5 << 20

// jpegQuality is what a re-encoded JPEG is written at.
const jpegQuality = 90

// imageInfo describes a prepared image for the log: never its bytes.
type imageInfo struct {
	mime          string
	width, height int
	size          int
	sha           string // a prefix of the hex sha256 of what is sent
	scaled        bool
}

// imageSkipped is why an image was not sent, as a short kind for the log
// and a sentence for the model.
type imageSkipped struct {
	kind, reason string
}

// prepareImage readies img for a model: its format read from its bytes,
// scaled down so its longer side is at most maxSide, and re-encoded when
// it was scaled or is not a PNG or JPEG. A PNG or JPEG that already fits
// is sent as it is.
func prepareImage(img airut.Image, maxSide int) (provider.ImageBlock, imageInfo, *imageSkipped) {
	if len(img.Data) > airut.MaxImageBytes {
		return provider.ImageBlock{}, imageInfo{}, &imageSkipped{"too_large",
			fmt.Sprintf("it is %s, over the %s limit", sizeOf(len(img.Data)), sizeOf(airut.MaxImageBytes))}
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(img.Data))
	if err != nil {
		return provider.ImageBlock{}, imageInfo{}, unreadable()
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return provider.ImageBlock{}, imageInfo{}, &imageSkipped{"too_many_pixels",
			fmt.Sprintf("it is %dx%d pixels, more than can be read", cfg.Width, cfg.Height)}
	}
	// Decoded even when it is sent as it came, so that a truncated or
	// corrupt file is a note here rather than a request the provider
	// refuses.
	src, _, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		return provider.ImageBlock{}, imageInfo{}, unreadable()
	}
	fits := max(cfg.Width, cfg.Height) <= maxSide
	if fits && (format == "png" || format == "jpeg") && len(img.Data) <= maxSentImageBytes {
		return send(img.Data, "image/"+format, cfg.Width, cfg.Height, false)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if !fits {
		w, h = scaledSize(w, h, maxSide)
		dst := image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
		src = dst
	}
	if format != "jpeg" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			return provider.ImageBlock{}, imageInfo{}, unreadable()
		}
		if buf.Len() <= maxSentImageBytes {
			return send(buf.Bytes(), "image/png", w, h, !fits)
		}
		// Lossless is too large, so it goes as a JPEG. Transparency is
		// lost, which a model looking at the picture can spare.
	}
	data, jw, jh, ok := jpegWithin(src, maxSentImageBytes)
	if !ok {
		return provider.ImageBlock{}, imageInfo{}, &imageSkipped{"too_large_encoded",
			fmt.Sprintf("it stayed over the %s limit however far it was scaled down", sizeOf(maxSentImageBytes))}
	}
	return send(data, "image/jpeg", jw, jh, !fits || jw != w || jh != h)
}

// jpegWithin encodes src as a JPEG of at most limit bytes: at
// jpegQuality, then at lower qualities, then smaller by a quarter each
// time, so that whatever ImageMaxSide a turn sets, what is sent stays
// under the limit. It reports false when even a small image is over it.
func jpegWithin(src image.Image, limit int) (data []byte, w, h int, ok bool) {
	b := src.Bounds()
	w, h = b.Dx(), b.Dy()
	for _, q := range []int{jpegQuality, 75, 60} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: q}); err != nil {
			return nil, 0, 0, false
		}
		if buf.Len() <= limit {
			return buf.Bytes(), w, h, true
		}
	}
	for max(w, h) > minShrunkSide {
		w, h = max(1, w*3/4), max(1, h*3/4)
		dst := image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 60}); err != nil {
			return nil, 0, 0, false
		}
		if buf.Len() <= limit {
			return buf.Bytes(), w, h, true
		}
	}
	return nil, 0, 0, false
}

// minShrunkSide is the smallest longer side jpegWithin scales an image
// down to before it gives up.
const minShrunkSide = 64

func send(data []byte, mime string, w, h int, scaled bool) (provider.ImageBlock, imageInfo, *imageSkipped) {
	sum := sha256.Sum256(data)
	return provider.ImageBlock{MimeType: mime, Data: data},
		imageInfo{mime: mime, width: w, height: h, size: len(data), sha: hex.EncodeToString(sum[:6]), scaled: scaled}, nil
}

func unreadable() *imageSkipped {
	return &imageSkipped{"unreadable", "it could not be read as a PNG, JPEG, GIF, or WebP image"}
}

// scaledSize is w by h scaled so the longer side is maxSide, keeping the
// aspect ratio, and neither side less than one pixel.
func scaledSize(w, h, maxSide int) (int, int) {
	if w >= h {
		return maxSide, max(1, int((int64(h)*int64(maxSide)+int64(w)/2)/int64(w)))
	}
	return max(1, int((int64(w)*int64(maxSide)+int64(h)/2)/int64(h))), maxSide
}

// sizeOf is n bytes for a person to read.
func sizeOf(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)>>10)
}

// images prepares a turn's or a tool's images, in order, as content for
// the model: an ImageBlock for each that can be sent, and in place of
// each that cannot, a note naming it by its place from one, so the model
// knows a picture was meant and why it does not see it. from names where
// they came from in the log, and tool which tool gave them.
func (a *Agent) images(c *airut.Ctx, from, tool string, imgs []airut.Image, maxSide int) []provider.Content {
	var out []provider.Content
	for i, img := range imgs {
		attrs := []any{"invocation", c.ID(), "from", from, "index", i + 1}
		if tool != "" {
			attrs = append(attrs, "tool", tool)
		}
		b, info, skip := prepareImage(img, maxSide)
		if skip != nil {
			a.log.Info("agent image not sent", append(attrs, "reason", skip.kind, "bytes", len(img.Data))...)
			out = append(out, provider.TextBlock{Text: fmt.Sprintf("[image %d not sent: %s]", i+1, skip.reason)})
			continue
		}
		a.log.Info("agent image", append(attrs, "mime", info.mime, "width", info.width, "height", info.height,
			"bytes", info.size, "sha256", info.sha, "scaled", info.scaled)...)
		out = append(out, b)
	}
	return out
}
