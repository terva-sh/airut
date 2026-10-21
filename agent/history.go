package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"terva.sh/terva/packages/core"
	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
)

func checkHistory(h airut.History) error {
	switch {
	case h.MaxMessages < 0, h.MaxTokens < 0, h.MaxAge < 0, h.FollowWithin < 0:
		return errors.New("a bound is negative")
	case h.RoomMessages < airut.NoRoomMessages || h.RoomMessages > airut.MaxRoomMessages:
		return fmt.Errorf("RoomMessages is %d, outside %d to %d", h.RoomMessages, airut.NoRoomMessages, airut.MaxRoomMessages)
	case h.Compaction != "" && h.Compaction != airut.CompactSummarize && h.Compaction != airut.CompactDrop:
		return fmt.Errorf("unknown compaction %q", h.Compaction)
	}
	return nil
}

// history is the bound a turn runs with: the turn's own fields, with the
// agent's default in each one the turn leaves zero.
func history(turn, def airut.History) airut.History {
	if turn.MaxMessages == 0 {
		turn.MaxMessages = def.MaxMessages
	}
	if turn.MaxTokens == 0 {
		turn.MaxTokens = def.MaxTokens
	}
	if turn.MaxAge == 0 {
		turn.MaxAge = def.MaxAge
	}
	if turn.Compaction == "" {
		turn.Compaction = def.Compaction
	}
	if turn.RoomMessages == 0 {
		turn.RoomMessages = def.RoomMessages
	}
	if turn.FollowWithin == 0 {
		turn.FollowWithin = def.FollowWithin
	}
	if turn.Compaction == "" {
		turn.Compaction = airut.CompactSummarize
	}
	return turn
}

// window returns the part of a resumed transcript a turn bounded by h
// sees. Messages older than MaxAge go whatever the compaction. Under
// CompactDrop so does the oldest part beyond MaxMessages and MaxTokens;
// under CompactSummarize that part stays for the summarizer, which
// summarizer reports through due. What remains starts at a person's
// message, so it never opens on a tool result whose call was cut off.
// A message with no time is one terva recorded none for, such as a
// compaction's summary, and is never too old.
func window(tr core.Transcript, h airut.History, now time.Time) (out core.Transcript, due bool) {
	ms := tr.Messages
	cut := 0
	if h.MaxAge > 0 {
		oldest := now.Add(-h.MaxAge)
		for i, m := range ms {
			if !m.Time.IsZero() && m.Time.Before(oldest) {
				cut = i + 1
			}
		}
	}
	over := overLimit(ms[cut:], h)
	if over > 0 && h.Compaction == airut.CompactDrop {
		cut += over
		over = 0
	}
	if cut > 0 {
		for cut < len(ms) && ms[cut].Role != provider.RoleUser {
			cut++
		}
		tr.Messages = ms[cut:]
		// The gauge measured the whole transcript; the next request
		// reports the size of what is left.
		tr.ResumeContext = provider.Usage{}
	}
	return tr, over > 0
}

// overLimit is how many of the oldest messages must go for the rest to
// fit h's message and token caps.
func overLimit(ms []provider.Message, h airut.History) int {
	n := 0
	if h.MaxMessages > 0 && len(ms) > h.MaxMessages {
		n = len(ms) - h.MaxMessages
	}
	if h.MaxTokens > 0 {
		total := 0
		for _, m := range ms[n:] {
			total += estimate(m)
		}
		for ; n < len(ms) && total > h.MaxTokens; n++ {
			total -= estimate(ms[n])
		}
	}
	return n
}

// estimate is a message's size in tokens, as terva estimates a
// transcript: a token for every four bytes of the serialized message.
func estimate(m provider.Message) int {
	b, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// keepTail is how many recent messages a summarizing compaction keeps
// verbatim: what fits in half of each cap, and no more than terva keeps
// by default, so a summary always has something to replace.
func keepTail(ms []provider.Message, h airut.History) int {
	half := h
	half.MaxMessages = (h.MaxMessages + 1) / 2
	half.MaxTokens = (h.MaxTokens + 1) / 2
	if h.MaxMessages == 0 {
		half.MaxMessages = 0
	}
	if h.MaxTokens == 0 {
		half.MaxTokens = 0
	}
	return min(len(ms)-overLimit(ms, half), core.AutoCompactKeepTail)
}

// policy is terva's default compaction, near the end of the model's
// window, with one more reason to compact before a turn: a resumed
// transcript past the turn's History.
type policy struct {
	core.DefaultCompactionPolicy
	due  bool
	keep int
}

func (p policy) Decide(s core.CompactionState) core.CompactionDecision {
	d := p.DefaultCompactionPolicy.Decide(s)
	if s.Point == core.CompactBeforeTurn && p.due {
		d.Compact, d.KeepTail = true, p.keep
	}
	return d
}
