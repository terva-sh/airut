package sessions

import (
	"encoding/json"
	"fmt"
	"time"

	"terva.sh/terva/packages/provider"
)

// terva's provider.Message holds its content as an interface with no JSON
// encoding, and the codec terva itself uses is marked unstable. So the
// log carries its own: each block tagged with its type, holding exactly
// the stable fields. codec_test.go fails when terva adds a field the
// codec does not carry, rather than letting it be dropped.

type wireMessage struct {
	Role    provider.Role     `json:"role"`
	Content []wireBlock       `json:"content"`
	Time    time.Time         `json:"time"`
	Meta    map[string]string `json:"meta"` // no omitempty: nil and empty differ
}

// wireBlock is one content block. Bytes are []byte so that they survive
// whatever they hold: tool-call arguments need not be valid JSON. They
// are pointers so that nil and empty stay distinct, as a resume compares
// them exactly.
type wireBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"` // text

	MimeType string  `json:"mime_type,omitempty"` // image
	Data     *[]byte `json:"data,omitempty"`

	ID           string  `json:"id,omitempty"` // image, tool call, reasoning, compaction
	Name         string  `json:"name,omitempty"`
	Arguments    *[]byte `json:"arguments,omitempty"`
	RawArguments string  `json:"raw_arguments,omitempty"`
	Signature    string  `json:"signature,omitempty"`

	CallID  string      `json:"call_id,omitempty"` // tool result
	Content []wireBlock `json:"content,omitempty"`
	// Nil marks content that was nil rather than empty, which a resume
	// compares exactly.
	Nil     bool `json:"nil,omitempty"`
	IsError bool `json:"is_error,omitempty"`

	Summary   string `json:"summary,omitempty"` // reasoning
	Encrypted string `json:"encrypted,omitempty"`
	Shape     string `json:"shape,omitempty"`

	Provider string `json:"provider,omitempty"` // compaction
}

// The block type tags.
const (
	tagText       = "text"
	tagImage      = "image"
	tagToolCall   = "tool_call"
	tagToolResult = "tool_result"
	tagReasoning  = "reasoning"
	tagCompaction = "compaction"
)

func encodeMessages(ms []provider.Message) ([]wireMessage, error) {
	if ms == nil {
		return nil, nil
	}
	out := make([]wireMessage, len(ms))
	for i, m := range ms {
		w, err := encodeMessage(m)
		if err != nil {
			return nil, err
		}
		out[i] = w
	}
	return out, nil
}

func encodeMessage(m provider.Message) (wireMessage, error) {
	content, err := encodeBlocks(m.Content)
	if err != nil {
		return wireMessage{}, err
	}
	return wireMessage{Role: m.Role, Content: content, Time: m.Time, Meta: m.Meta}, nil
}

func encodeBlocks(in []provider.Content) ([]wireBlock, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]wireBlock, len(in))
	for i, c := range in {
		switch b := c.(type) {
		case provider.TextBlock:
			out[i] = wireBlock{Type: tagText, Text: b.Text}
		case provider.ImageBlock:
			out[i] = wireBlock{Type: tagImage, MimeType: b.MimeType, Data: bytesOf(b.Data), ID: b.ID}
		case provider.ToolCallBlock:
			out[i] = wireBlock{Type: tagToolCall, ID: b.ID, Name: b.Name, Arguments: bytesOf(b.Arguments),
				RawArguments: b.RawArguments, Signature: b.Signature}
		case provider.ToolResultBlock:
			inner, err := encodeBlocks(b.Content)
			if err != nil {
				return nil, err
			}
			out[i] = wireBlock{Type: tagToolResult, CallID: b.CallID, Content: inner, Nil: b.Content == nil, IsError: b.IsError}
		case provider.ReasoningBlock:
			out[i] = wireBlock{Type: tagReasoning, ID: b.ID, Summary: b.Summary, Encrypted: b.Encrypted, Shape: b.Shape}
		case provider.CompactionBlock:
			out[i] = wireBlock{Type: tagCompaction, ID: b.ID, Encrypted: b.Encrypted, Provider: b.Provider}
		default:
			// A block this codec does not know would be lost; failing
			// latches a persistence error in the engine instead.
			return nil, fmt.Errorf("sessions: cannot store a %T content block", c)
		}
	}
	return out, nil
}

func decodeMessages(ws []wireMessage) ([]provider.Message, error) {
	if ws == nil {
		return nil, nil
	}
	out := make([]provider.Message, len(ws))
	for i, w := range ws {
		m, err := decodeMessage(w)
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

func decodeMessage(w wireMessage) (provider.Message, error) {
	content, err := decodeBlocks(w.Content, w.Content == nil)
	if err != nil {
		return provider.Message{}, err
	}
	return provider.Message{Role: w.Role, Content: content, Time: w.Time, Meta: w.Meta}, nil
}

func decodeBlocks(in []wireBlock, isNil bool) ([]provider.Content, error) {
	if isNil {
		return nil, nil
	}
	out := make([]provider.Content, len(in))
	for i, b := range in {
		switch b.Type {
		case tagText:
			out[i] = provider.TextBlock{Text: b.Text}
		case tagImage:
			out[i] = provider.ImageBlock{MimeType: b.MimeType, Data: sliceOf(b.Data), ID: b.ID}
		case tagToolCall:
			out[i] = provider.ToolCallBlock{ID: b.ID, Name: b.Name, Arguments: json.RawMessage(sliceOf(b.Arguments)),
				RawArguments: b.RawArguments, Signature: b.Signature}
		case tagToolResult:
			inner, err := decodeBlocks(b.Content, b.Nil)
			if err != nil {
				return nil, err
			}
			out[i] = provider.ToolResultBlock{CallID: b.CallID, Content: inner, IsError: b.IsError}
		case tagReasoning:
			out[i] = provider.ReasoningBlock{ID: b.ID, Summary: b.Summary, Encrypted: b.Encrypted, Shape: b.Shape}
		case tagCompaction:
			out[i] = provider.CompactionBlock{ID: b.ID, Encrypted: b.Encrypted, Provider: b.Provider}
		default:
			return nil, fmt.Errorf("sessions: unknown content block type %q in the log", b.Type)
		}
	}
	return out, nil
}

// bytesOf and sliceOf carry a byte slice through the log with nil and
// empty kept apart.
func bytesOf(b []byte) *[]byte {
	if b == nil {
		return nil
	}
	return &b
}

func sliceOf(p *[]byte) []byte {
	if p == nil {
		return nil
	}
	if *p == nil {
		return []byte{}
	}
	return *p
}
