package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"slices"

	"terva.sh/terva/packages/core"
	"terva.sh/terva/packages/provider"
)

// imageless is a turn's session store with every image left out: each
// message and compaction is written with a note in place of each image,
// so a resumed session sends the model no image and the store does not
// grow by an image's bytes each turn. The engine's live transcript keeps
// the image for the rest of the turn it arrived in. An exclusion the
// engine records is written as it is, keyed by the image's hash.
//
// The policy is the agent's, not the store's: sessions.Session stays a
// faithful core.TranscriptStore, as terva's transcripttest suite holds
// it to.
type imageless struct{ core.TranscriptStore }

func (s imageless) AppendMessage(m provider.Message) error {
	return s.TranscriptStore.AppendMessage(withoutImages(m))
}

func (s imageless) AppendCompaction(ms []provider.Message, res core.CompactResult) error {
	return s.TranscriptStore.AppendCompaction(allWithoutImages(ms), res)
}

// allWithoutImages is ms with every image replaced by its note. It leaves
// ms as it was, and returns it unchanged when it holds no image.
func allWithoutImages(ms []provider.Message) []provider.Message {
	var out []provider.Message
	for i, m := range ms {
		n := withoutImages(m)
		if out == nil && !sameContent(n.Content, m.Content) {
			out = slices.Clone(ms)
		}
		if out != nil {
			out[i] = n
		}
	}
	if out == nil {
		return ms
	}
	return out
}

// withoutImages is m with every image, in its content or in a tool
// result, replaced by its note. m's own content is left as it was, as
// the engine's live transcript shares it.
func withoutImages(m provider.Message) provider.Message {
	m.Content = contentWithoutImages(m.Content)
	return m
}

func contentWithoutImages(in []provider.Content) []provider.Content {
	var out []provider.Content
	set := func(i int, c provider.Content) {
		if out == nil {
			out = slices.Clone(in)
		}
		out[i] = c
	}
	for i, c := range in {
		switch b := c.(type) {
		case provider.ImageBlock:
			set(i, provider.TextBlock{Text: imageNote(b)})
		case provider.ToolResultBlock:
			if inner := contentWithoutImages(b.Content); !sameContent(inner, b.Content) {
				b.Content = inner
				set(i, b)
			}
		}
	}
	if out == nil {
		return in
	}
	return out
}

// sameContent reports whether a and b are the same slice, as
// contentWithoutImages returns its input when it changed nothing.
func sameContent(a, b []provider.Content) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// imageNote is what a session keeps of an image: its type, its size when
// it can be read, and a prefix of the sha256 of its bytes, so a later
// turn knows a picture was there and can ask a tool for it again.
func imageNote(b provider.ImageBlock) string {
	sum := sha256.Sum256(b.Data)
	desc := b.MimeType
	if desc == "" {
		desc = "unknown type"
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(b.Data)); err == nil {
		desc += fmt.Sprintf(", %dx%d", cfg.Width, cfg.Height)
	}
	return fmt.Sprintf("[image not kept: %s, sha256 %s]", desc, hex.EncodeToString(sum[:6]))
}
