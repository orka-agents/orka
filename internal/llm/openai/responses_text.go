/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package openai

import (
	"errors"
	"strings"

	"github.com/openai/openai-go/v3/responses"
	"github.com/orka-agents/orka/internal/llm"
)

const responseTextContradiction = "response text contradicts streamed output"

// Part snapshots are independent: equal concatenated text does not establish
// that each content_index agrees. Buffer later parts until predecessors finish.
type responseTextPart struct {
	text string
	sent int
	done bool
}

type responseTextItem struct {
	parts    map[int64]*responseTextPart
	next     int64
	closed   bool
	implicit bool
}

func responseTextKey(index *int64) int64 {
	if index == nil {
		return -1
	}
	return *index
}

func (o *responseOutputOrder) textItem(index *int64) *responseTextItem {
	if o.text == nil {
		o.text = map[int64]*responseTextItem{}
	}
	key := responseTextKey(index)
	if o.text[key] == nil {
		o.text[key] = &responseTextItem{parts: map[int64]*responseTextPart{}}
	}
	return o.text[key]
}

func isResponseTextEvent(evt responses.ResponseStreamEventUnion) bool {
	switch evt.Type {
	case eventTypeResponseOutputTextDelta, "response.output_text.done":
		return true
	case eventTypeResponseContentPartAdded, eventTypeResponseContentPartDone:
		return evt.Part.Type == responseContentTypeOutputText
	}
	return false
}

func (o *responseOutputOrder) textEvent(evt responses.ResponseStreamEventUnion, index *int64, send streamSender) bool {
	item := o.textItem(index)
	partIndex := evt.ContentIndex
	if !evt.JSON.ContentIndex.Valid() {
		if len(item.parts) > 1 || (len(item.parts) == 1 && item.parts[0] == nil) {
			return failResponsesStream(send, errors.New("ambiguous response content index"))
		}
		partIndex, item.implicit = 0, true
	}
	if partIndex < 0 || (item.implicit && partIndex != 0) {
		return failResponsesStream(send, errors.New("ambiguous response content index"))
	}
	part := item.parts[partIndex]
	if part == nil {
		if item.closed {
			return failResponsesStream(send, errors.New(responseTextContradiction))
		}
		part = &responseTextPart{}
		item.parts[partIndex] = part
	}
	switch evt.Type {
	case eventTypeResponseContentPartAdded:
		if !part.prefix(evt.Part.Text) {
			return failResponsesStream(send, errors.New(responseTextContradiction))
		}
	case eventTypeResponseOutputTextDelta:
		if evt.Delta != "" && (part.done || item.closed) {
			return failResponsesStream(send, errors.New(responseTextContradiction))
		}
		part.text += evt.Delta
	case "response.output_text.done", eventTypeResponseContentPartDone:
		text := evt.Text
		if evt.Type == eventTypeResponseContentPartDone {
			text = evt.Part.Text
		}
		if !part.snapshot(text) {
			return failResponsesStream(send, errors.New(responseTextContradiction))
		}
	}
	return item.drain(index, send)
}

// Added snapshots may repeat an earlier prefix or extend the current text,
// but they do not complete the part or change text that already completed.
func (p *responseTextPart) prefix(text string) bool {
	if strings.HasPrefix(p.text, text) {
		return true
	}
	if p.done || !strings.HasPrefix(text, p.text) {
		return false
	}
	p.text = text
	return true
}

func (p *responseTextPart) snapshot(text string) bool {
	if !strings.HasPrefix(text, p.text) || (p.done && text != p.text) {
		return false
	}
	p.text, p.done = text, true
	return true
}

func (item *responseTextItem) drain(index *int64, send streamSender) bool {
	for {
		part := item.parts[item.next]
		if part == nil {
			return true
		}
		if part.sent < len(part.text) {
			if !send(llm.StreamChunk{Content: part.text[part.sent:], OutputIndex: index}) {
				return false
			}
			part.sent = len(part.text)
		}
		if !part.done {
			return true
		}
		item.next++
	}
}

func (o *responseOutputOrder) messageSnapshot(snapshot responses.ResponseOutputItemUnion, index *int64, done bool, send streamSender) bool {
	item := o.textItem(index)
	if index == nil && !o.unindexedText {
		return failResponsesStream(send, errors.New(responseTextContradiction))
	}
	// Final snapshots cannot remove parts. Added snapshots may still show
	// an earlier prefix of the message, including fewer content parts.
	if done {
		for i := range item.parts {
			if i >= int64(len(snapshot.Content)) {
				return failResponsesStream(send, errors.New(responseTextContradiction))
			}
		}
	}
	if item.implicit && len(snapshot.Content) > 1 {
		return failResponsesStream(send, errors.New("ambiguous response content index"))
	}
	for i, content := range snapshot.Content {
		if content.Type != responseContentTypeOutputText {
			continue
		}
		part := item.parts[int64(i)]
		if part == nil {
			if item.closed {
				return failResponsesStream(send, errors.New(responseTextContradiction))
			}
			part = &responseTextPart{}
			item.parts[int64(i)] = part
		}
		if (done && !part.snapshot(content.Text)) || (!done && !part.prefix(content.Text)) {
			return failResponsesStream(send, errors.New(responseTextContradiction))
		}
	}
	item.closed = item.closed || done
	return item.drain(index, send)
}

// A terminal response may omit snapshots, but cannot silently drop a part
// whose predecessor never arrived or completed.
func (o *responseOutputOrder) validateTextCompletion(send streamSender) bool {
	for _, item := range o.text {
		for i, part := range item.parts {
			if i > item.next || part.sent != len(part.text) {
				return failResponsesStream(send, errors.New("response content has a missing predecessor"))
			}
		}
	}
	return true
}
