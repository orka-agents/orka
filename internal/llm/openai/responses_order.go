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
}

func (s *orderedResponseSender) chunk(chunk llm.StreamChunk) bool {
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
		if chunk.OutputItemDone {
			return true // Duplicate item-done notifications carry no new output.
		}
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
				if chunk.OutputItemDone {
					continue
				}
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
		if index > s.next && s.open {
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
	if evt.Type == "response.completed" || evt.Type == eventTypeResponseIncomplete {
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
	if evt.Type == "response.output_item.added" || evt.Type == "response.output_item.done" {
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
	if evt.Type == "response.output_text.delta" && evt.Delta != "" {
		if o.indexed || (o.unindexedText && o.unindexedID != id) {
			return nil, fmt.Errorf("response text has no unambiguous output index")
		}
		o.unindexedText, o.unindexedID = true, id
	}
	return nil, nil
}
