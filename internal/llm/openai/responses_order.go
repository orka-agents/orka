/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package openai

import (
	"fmt"
	"slices"

	"github.com/openai/openai-go/v3/responses"

	"github.com/orka-agents/orka/internal/llm"
)

// orderedResponseSender streams the current output item immediately, retaining
// later items until their predecessors finish. Function arguments can finish
// out of order; their readiness must not change the assistant's output order.
// This is used only for requests entering through the Responses endpoint.
type orderedResponseSender struct {
	send    streamSender
	pending map[int64][]llm.StreamChunk
	next    int64
	open    bool
	// Freeze statuses at queue entry, including items awaiting predecessors.
	statuses map[int64]string
}

func (s *orderedResponseSender) chunk(chunk llm.StreamChunk) bool {
	// A failed/refused outcome must not recover queued output or be replaced
	// by a secondary ordering error while flushing it.
	if chunk.Error != nil || (chunk.Done && chunk.StopReason == stopReasonRefusal) {
		return s.send(chunk)
	}
	if chunk.OutputItemDone {
		key := responseTextKey(chunk.OutputIndex)
		if known, ok := s.statuses[key]; ok {
			if chunk.OutputItemStatus != "" && chunk.OutputItemStatus != known {
				return failResponsesStream(s.send, fmt.Errorf("response item changed completion status"))
			}
			return true
		}
		status := chunk.OutputItemStatus
		if status == "" {
			status = stopReasonCompleted
		}
		if status != stopReasonCompleted && status != stopReasonIncomplete {
			return failResponsesStream(s.send, fmt.Errorf("response item has an invalid completion status"))
		}
		if s.statuses == nil {
			s.statuses = map[int64]string{}
		}
		s.statuses[key] = status
		chunk.OutputItemStatus = status
	}
	if chunk.OutputIndex == nil {
		if chunk.Done || chunk.Error != nil {
			if !s.flush() {
				return false
			}
		}
		return s.send(chunk)
	}
	index := *chunk.OutputIndex
	if index < s.next {
		s.send(llm.StreamChunk{Error: fmt.Errorf("response output changed after its item completed"), Done: true})
		return false
	}
	s.pending[index] = append(s.pending[index], chunk)
	return s.drain()
}

func (s *orderedResponseSender) drain() bool {
	for {
		chunks, ok := s.pending[s.next]
		if !ok {
			return true
		}
		delete(s.pending, s.next)
		for _, chunk := range chunks {
			if *chunk.OutputIndex < s.next {
				s.send(llm.StreamChunk{Error: fmt.Errorf("response output changed after its item completed"), Done: true})
				return false
			}
			if !s.send(chunk) {
				return false
			}
			if chunk.OutputItemDone {
				s.open = false
				s.next++
			} else {
				s.open = true
			}
		}
	}
}

// Some compatible upstreams supply the complete item only in the terminal
// response. Flush those items in index order, leaving the last text item open
// so the terminal outcome can mark a token-budget-truncated item incomplete.
func (s *orderedResponseSender) flush() bool {
	indices := make([]int64, 0, len(s.pending))
	for index := range s.pending {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	for _, index := range indices {
		if _, ok := s.pending[index]; !ok {
			continue
		}
		if index > s.next {
			if !s.open || index != s.next+1 {
				s.send(llm.StreamChunk{Error: fmt.Errorf("response output has a missing predecessor"), Done: true})
				return false
			}
			previous := s.next
			if !s.send(llm.StreamChunk{OutputIndex: &previous, OutputItemDone: true}) {
				return false
			}
			s.open = false
		}
		s.next = index
		if !s.drain() {
			return false
		}
	}
	return true
}

// responseOutputOrder validates the index/identity relationship before output
// reaches the queue. Compatible text-only streams may omit indices entirely,
// but mixing that mode with indexed items must never yield reordered success.
type responseOutputOrder struct {
	byID          map[string]int64
	byIndex       map[int64]string
	indexed       bool
	unindexedText bool
	unindexedID   string
	text          map[int64]*responseTextItem
}

func (o *responseOutputOrder) bind(id string, index int64) error {
	if index < 0 || o.unindexedText {
		return fmt.Errorf("ambiguous response output index")
	}
	if old, ok := o.byID[id]; id != "" && ok && old != index {
		return fmt.Errorf("response item changed output index")
	}
	if old := o.byIndex[index]; id != "" && old != "" && old != id {
		return fmt.Errorf("response output index changed item identity")
	}
	o.indexed = true
	if id != "" {
		if o.byID == nil {
			o.byID = map[string]int64{}
			o.byIndex = map[int64]string{}
		}
		o.byID[id], o.byIndex[index] = index, id
	}
	return nil
}

func (o *responseOutputOrder) eventIndex(evt responses.ResponseStreamEventUnion) (*int64, error) {
	if evt.Type == eventTypeResponseCompleted || evt.Type == eventTypeResponseIncomplete {
		if o.unindexedText {
			output := evt.Response.Output
			if len(output) > 1 || (len(output) == 1 && (output[0].Type != responseOutputTypeMessage || (o.unindexedID != "" && output[0].ID != o.unindexedID))) {
				return nil, fmt.Errorf("ambiguous unindexed response output")
			}
			return nil, nil
		}
		for i, item := range evt.Response.Output {
			if err := o.bind(item.ID, int64(i)); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	id := evt.ItemID
	if evt.Type == eventTypeResponseOutputItemAdded || evt.Type == eventTypeResponseOutputItemDone {
		id = evt.Item.ID
	}
	if evt.JSON.OutputIndex.Valid() {
		index := evt.OutputIndex
		if err := o.bind(id, index); err != nil {
			return nil, err
		}
		return &index, nil
	}
	if index, ok := o.byID[id]; id != "" && ok {
		return &index, nil
	}
	if isResponseTextEvent(evt) || (evt.Type == eventTypeResponseOutputItemDone && evt.Item.Type == responseOutputTypeMessage) {
		if o.indexed || (o.unindexedText && o.unindexedID != "" && id != "" && o.unindexedID != id) {
			return nil, fmt.Errorf("response text has no unambiguous output index")
		}
		o.unindexedText = true
		if id != "" {
			o.unindexedID = id
		}
	}
	return nil, nil
}

func (o *responseOutputOrder) completeMessages(evt responses.ResponseStreamEventUnion, send streamSender) bool {
	for i, item := range evt.Response.Output {
		if item.Type != responseOutputTypeMessage {
			continue
		}
		index := int64(i)
		outputIndex := &index
		if o.unindexedText {
			outputIndex = nil
		}
		status := item.Status
		if status == "" && !o.textItem(outputIndex).closed && evt.Type == eventTypeResponseIncomplete && i == len(evt.Response.Output)-1 {
			status = stopReasonIncomplete
		}
		if !o.completeText(item, outputIndex, send) {
			return false
		}
		if !send(llm.StreamChunk{OutputIndex: outputIndex, OutputItemDone: true, OutputItemStatus: status}) {
			return false
		}
	}
	return o.validateTextCompletion(send)
}
