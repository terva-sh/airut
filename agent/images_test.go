package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// gifOf is a two-frame GIF whose first frame is red and second blue.
func gifOf(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	frame := func(i uint8) *image.Paletted {
		p := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for k := range p.Pix {
			p.Pix[k] = i
		}
		return p
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame(0), frame(1)}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func dims(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the prepared image does not decode: %v", err)
	}
	return cfg.Width, cfg.Height, format
}

func TestPrepareImage(t *testing.T) {
	small := pngOf(t, 40, 20)
	webp, err := os.ReadFile("testdata/blue-purple-pink.lossy.webp") // from golang.org/x/image's test data
	if err != nil {
		t.Fatal(err)
	}
	ww, wh, _ := dims(t, webp)

	for _, c := range []struct {
		name          string
		data          []byte
		maxSide       int
		mime          string
		width, height int
	}{
		{"a PNG that fits", small, 1568, "image/png", 40, 20},
		{"a wide PNG", pngOf(t, 3000, 1000), 1568, "image/png", 1568, 523},
		{"a tall JPEG", jpegOf(t, 1000, 2000), 500, "image/jpeg", 250, 500},
		{"a GIF", gifOf(t, 30, 10), 1568, "image/png", 30, 10},
		{"a WebP", webp, 1568, "image/png", ww, wh},
		{"a line", pngOf(t, 4000, 1), 100, "image/png", 100, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, info, skip := prepareImage(airut.Image{Data: c.data}, c.maxSide)
			if skip != nil {
				t.Fatalf("skipped: %s", skip.reason)
			}
			w, h, format := dims(t, b.Data)
			if b.MimeType != c.mime || "image/"+format != c.mime || w != c.width || h != c.height {
				t.Fatalf("got %s (%s) %dx%d, want %s %dx%d", b.MimeType, format, w, h, c.mime, c.width, c.height)
			}
			if info.width != w || info.height != h || info.size != len(b.Data) || len(info.sha) != 12 {
				t.Errorf("info = %+v", info)
			}
		})
	}

	// A PNG or JPEG that fits goes as it came.
	if b, _, _ := prepareImage(airut.Image{Data: small, Type: "image/jpeg"}, 1568); !bytes.Equal(b.Data, small) || b.MimeType != "image/png" {
		t.Errorf("a PNG that fits was re-encoded, or took its claimed type: %s", b.MimeType)
	}
	// A GIF is its first frame.
	b, _, _ := prepareImage(airut.Image{Data: gifOf(t, 4, 4)}, 1568)
	if img, err := png.Decode(bytes.NewReader(b.Data)); err != nil {
		t.Fatal(err)
	} else if r, g, bl, _ := img.At(1, 1).RGBA(); r>>8 != 255 || g != 0 || bl != 0 {
		t.Errorf("the GIF's pixel is %d,%d,%d, want its first frame's red", r>>8, g>>8, bl>>8)
	}

	// A GIF header claiming a 65535x65535 canvas, in a few bytes.
	bomb := []byte("GIF89a\xff\xff\xff\xff\x00\x00\x00;")
	for _, c := range []struct {
		name, kind string
		data       []byte
	}{
		{"not an image", "unreadable", []byte("<svg/>")},
		{"empty", "unreadable", nil},
		{"a truncated PNG", "unreadable", small[:len(small)/2]},
		{"too many bytes", "too_large", make([]byte, airut.MaxImageBytes+1)},
		{"too many pixels", "too_many_pixels", bomb},
	} {
		if _, _, skip := prepareImage(airut.Image{Data: c.data}, 1568); skip == nil || skip.kind != c.kind {
			t.Errorf("%s: skip = %+v, want %s", c.name, skip, c.kind)
		}
	}
}

// noiseOf is a w by h image of noise, which compresses badly.
func noiseOf(w, h int) *image.NRGBA {
	r := rand.New(rand.NewPCG(1, 2))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(r.Uint32())
	}
	return img
}

// TestPreparedImagesStayUnderTheByteLimit checks that what is sent is
// never over maxSentImageBytes, whatever ImageMaxSide allows: a PNG too
// large goes as a JPEG, a JPEG too large goes at a lower quality or
// smaller, and one that cannot get under it becomes a note.
func TestPreparedImagesStayUnderTheByteLimit(t *testing.T) {
	defer func(n int) { maxSentImageBytes = n }(maxSentImageBytes)
	maxSentImageBytes = 40 << 10
	noise := noiseOf(400, 300)
	var p, j bytes.Buffer
	if err := png.Encode(&p, noise); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, noise, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"png": p.Bytes(), "jpeg": j.Bytes()} {
		if len(data) <= maxSentImageBytes {
			t.Fatalf("the %s fixture is only %d bytes", name, len(data))
		}
		b, info, skip := prepareImage(airut.Image{Data: data}, 4000)
		if skip != nil {
			t.Fatalf("%s: skipped: %s", name, skip.reason)
		}
		w, h, format := dims(t, b.Data)
		if len(b.Data) > maxSentImageBytes || format != "jpeg" || b.MimeType != "image/jpeg" {
			t.Errorf("%s: sent %d bytes as %s (%s), want a JPEG within %d", name, len(b.Data), b.MimeType, format, maxSentImageBytes)
		}
		if w*3 != h*4 || info.width != w || info.height != h || info.scaled != (w != 400) {
			t.Errorf("%s: sent %dx%d, info %+v; the aspect ratio must hold", name, w, h, info)
		}
	}

	maxSentImageBytes = 100
	if _, _, skip := prepareImage(airut.Image{Data: p.Bytes()}, 4000); skip == nil || skip.kind != "too_large_encoded" {
		t.Errorf("an image that cannot get under the limit: skip = %+v", skip)
	}
}

// lockedBuffer is a log the engine's goroutines and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// images counts the images in ms, in content and in tool results, and
// joins their text.
func imagesIn(ms []provider.Message) (n int, text string) {
	var b strings.Builder
	var walk func([]provider.Content)
	walk = func(cs []provider.Content) {
		for _, c := range cs {
			switch c := c.(type) {
			case provider.ImageBlock:
				n++
			case provider.TextBlock:
				b.WriteString(c.Text + "\n")
			case provider.ToolResultBlock:
				walk(c.Content)
			}
		}
	}
	for _, m := range ms {
		walk(m.Content)
	}
	return n, b.String()
}

func TestTurnSendsImagesWithThePrompt(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	logs := &lockedBuffer{}
	e.agent.log = slog.New(slog.NewTextHandler(logs, nil))
	small, large := pngOf(t, 40, 20), pngOf(t, 300, 150)
	e.ask(airut.Turn{Prompt: "what is this", ImageMaxSide: 100,
		Images: []airut.Image{{Data: small}, {Data: []byte("not a picture")}, {Data: large}}})
	e.model.play(say("a gradient"))
	evs := airuttest.Start(t, e.bot).Text("!ask")

	if got := final(evs); got != "a gradient" {
		t.Fatalf("answer = %q", got)
	}
	req := e.model.requests()[0]
	last := req.Messages[len(req.Messages)-1]
	var sent []provider.ImageBlock
	for _, c := range last.Content {
		if b, ok := c.(provider.ImageBlock); ok {
			sent = append(sent, b)
		}
	}
	if len(sent) != 2 || !bytes.Equal(sent[0].Data, small) {
		t.Fatalf("the model was sent %d images, want the two readable ones, the first as it came", len(sent))
	}
	if w, h, _ := dims(t, sent[1].Data); w != 100 || h != 50 {
		t.Errorf("the large image was sent at %dx%d, want 100x50", w, h)
	}
	if _, text := imagesIn([]provider.Message{last}); !strings.Contains(text, "what is this") ||
		!strings.Contains(text, "[image 2 not sent: it could not be read") {
		t.Errorf("the prompt's text = %q, want the prompt and a note for image 2", text)
	}

	l := logs.String()
	if strings.Count(l, "msg=\"agent image\"") != 2 || !strings.Contains(l, "reason=unreadable") || !strings.Contains(l, "width=100") {
		t.Errorf("the images were not logged by size and kind:\n%s", l)
	}
	for _, img := range [][]byte{small, large, sent[1].Data} {
		if strings.Contains(l, base64.StdEncoding.EncodeToString(img)[:24]) || strings.Contains(l, string(img[8:24])) {
			t.Fatalf("a log line holds image bytes:\n%s", l)
		}
	}
	if strings.Contains(l, "not a picture") || strings.Contains(l, "what is this") {
		t.Errorf("a log line holds chat text:\n%s", l)
	}
}

// imageTool is an airut.ImageTool that returns a caption and pictures. It
// restricts itself, so the core wraps it, and the wrapper must keep its
// images.
type imageTool struct{ images []airut.Image }

func (imageTool) Name() string            { return "fetch" }
func (imageTool) Description() string     { return "Fetch a picture" }
func (imageTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (imageTool) Allows(*airut.Ctx) (bool, error) {
	return true, nil
}
func (imageTool) Call(*airut.Ctx, json.RawMessage) (string, error) {
	return "", nil
}
func (x imageTool) CallImages(*airut.Ctx, json.RawMessage) (airut.ToolOutput, error) {
	return airut.ToolOutput{Text: "the picture you edited", Images: x.images}, nil
}

func TestImageToolResult(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	pic := pngOf(t, 40, 20)
	e.ask(airut.Turn{Prompt: "look again", Tools: []airut.Tool{imageTool{[]airut.Image{{Data: pic}, {Data: []byte("x")}}}}})
	e.model.play(call("c1", "fetch", `{}`), say("I see it"))
	if got := final(airuttest.Start(t, e.bot).Text("!ask")); got != "I see it" {
		t.Fatalf("answer = %q", got)
	}
	var res provider.ToolResultBlock
	for _, m := range e.model.requests()[1].Messages {
		for _, c := range m.Content {
			if r, ok := c.(provider.ToolResultBlock); ok {
				res = r
			}
		}
	}
	if len(res.Content) != 3 || res.IsError {
		t.Fatalf("the tool result = %+v, want its text, its image, and a note", res)
	}
	if tb, ok := res.Content[0].(provider.TextBlock); !ok || tb.Text != "the picture you edited" {
		t.Errorf("the result opens with %+v", res.Content[0])
	}
	if ib, ok := res.Content[1].(provider.ImageBlock); !ok || !bytes.Equal(ib.Data, pic) || ib.MimeType != "image/png" {
		t.Errorf("the result's image is %T", res.Content[1])
	}
	if tb, ok := res.Content[2].(provider.TextBlock); !ok || !strings.HasPrefix(tb.Text, "[image 2 not sent:") {
		t.Errorf("the unreadable image became %+v", res.Content[2])
	}
}

func TestImagesAreNotKeptInTheSession(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	pic := pngOf(t, 40, 20)
	var key string
	e.bot.Command("look", "Look", func(c *airut.Ctx) error {
		key = c.SessionKey(airut.PerChannel)
		return c.Agent(airut.Turn{Prompt: "look", Session: airut.PerChannel, Images: []airut.Image{{Data: pic}},
			Tools: []airut.Tool{imageTool{[]airut.Image{{Data: pic}}}}}).StreamTo(c)
	})
	e.ask(airut.Turn{Prompt: "and now?", Session: airut.PerChannel})
	e.model.play(call("c1", "fetch", `{}`), say("seen"), say("from memory"))
	a := airuttest.Start(t, e.bot)
	a.Text("!look")

	// Within the turn the image stays: the request after the tool call
	// still carries both.
	if n, _ := imagesIn(e.model.requests()[1].Messages); n != 2 {
		t.Fatalf("the turn's second request carried %d images, want the prompt's and the tool's", n)
	}

	s, err := e.agent.db.Session(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := s.Reload()
	if err != nil {
		t.Fatal(err)
	}
	n, text := imagesIn(tr.Messages)
	if n != 0 || strings.Count(text, "[image not kept: image/png, 40x20, sha256 ") != 2 {
		t.Fatalf("the session holds %d images and %q, want two notes and no image", n, text)
	}

	// A log written before images were left out resumes without them.
	if err := s.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{
		provider.ImageBlock{MimeType: "image/png", Data: pic}}}); err != nil {
		t.Fatal(err)
	}
	a.Text("!ask")
	if n, text := imagesIn(e.model.requests()[2].Messages); n != 0 || strings.Count(text, "[image not kept:") != 3 {
		t.Fatalf("a resumed turn sent %d images and %q, want none and three notes", n, text)
	}
}

func TestRefusedImage(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	pic, other := pngOf(t, 40, 20), pngOf(t, 20, 40)
	var key string
	e.bot.Command("look", "Look", func(c *airut.Ctx) error {
		key = c.SessionKey(airut.PerChannel)
		return c.Agent(airut.Turn{Prompt: "look", Session: airut.PerChannel, Images: []airut.Image{{Data: pic}}}).StreamTo(c)
	})
	// A gateway that takes no image parts answers with a 400 that names
	// the image, which is what terva's engine reads as a refusal.
	e.model.play(reply{err: &provider.ProviderError{Provider: "fake", Status: 400,
		Msg: "Invalid image: the image data you provided does not represent a valid image"}}, say("I cannot see it, but I can help"))
	evs := airuttest.Start(t, e.bot).Text("!look")

	if got := final(evs); got != "I cannot see it, but I can help" {
		t.Fatalf("answer = %q; the turn should go on to an answer", got)
	}
	reqs := e.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want the refused one and its retry", len(reqs))
	}
	if n, _ := imagesIn(reqs[0].Messages); n != 1 {
		t.Fatalf("the first request carried %d images", n)
	}
	n, text := imagesIn(reqs[1].Messages)
	if n != 0 || !strings.Contains(text, "[image omitted") {
		t.Fatalf("the retry carried %d images and %q, want terva's note in place of the image", n, text)
	}

	// The exclusion is recorded by the image's hash: the same image
	// written to the session again is left out when it resumes, and
	// another is not.
	s, err := e.agent.db.Session(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{
		provider.ImageBlock{MimeType: "image/png", Data: pic}, provider.ImageBlock{MimeType: "image/png", Data: other}}}); err != nil {
		t.Fatal(err)
	}
	tr, err := s.Reload()
	if err != nil {
		t.Fatal(err)
	}
	var kept [][]byte
	for _, m := range tr.Messages {
		for _, c := range m.Content {
			if b, ok := c.(provider.ImageBlock); ok {
				kept = append(kept, b.Data)
			}
		}
	}
	if len(kept) != 1 || !bytes.Equal(kept[0], other) {
		t.Fatalf("the session resumes %d images; the refused one must be excluded and only the other kept", len(kept))
	}
}

func TestNegativeImageMaxSide(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "x", ImageMaxSide: -1})
	airuttest.Start(t, e.bot).Text("!ask")
	if n := len(e.model.requests()); n != 0 {
		t.Fatalf("a turn with a negative ImageMaxSide made %d requests", n)
	}
}
