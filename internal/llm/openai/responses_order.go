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
	order   *responseOutputOrder
	pending map[int64][]llm.StreamChunk
	next    int64
	open    bool
	// Freeze statuses at queue entry, including items awaiting predecessors.
	statuses      map[int64]string
	hasIncomplete bool
	hasToolCall   bool
}

// A terminal snapshot can close an earlier message and drain queued calls.
// Hold that output until every snapshot and ordering check has succeeded.
func (s *orderedResponseSender) terminal(handle func(streamSender)) {
	downstream := s.send
	var chunks []llm.StreamChunk
	s.send = func(chunk llm.StreamChunk) bool {
		chunks = append(chunks, chunk)
		return true
	}
	handle(s.chunk)
	s.send = downstream
	for _, chunk := range chunks {
		if chunk.Error != nil || (chunk.Done && !responsesTerminalAllowsOutput(chunk.StopReason)) {
			downstream(chunk)
			return
		}
	}
	for _, chunk := range chunks {
		if !downstream(chunk) {
			return
		}
	}
}

func (s *orderedResponseSender) chunk(chunk llm.StreamChunk) bool {
	// A failed/refused outcome must not recover queued output or be replaced
	// by a secondary ordering error while flushing it.
	if chunk.Error != nil || (chunk.Done && !responsesTerminalAllowsOutput(chunk.StopReason)) {
		return s.send(chunk)
	}
	incomplete := s.hasIncomplete || (s.order != nil && s.order.hasIncomplete)
	if chunk.Done && incomplete && chunk.StopReason != stopReasonLength {
		return failResponsesStream(s.send, fmt.Errorf("response completed after an incomplete output item"))
	}
	if incomplete && (s.hasToolCall || chunk.ToolCall != nil) {
		return failResponsesStream(s.send, fmt.Errorf("response returned tool calls with incomplete output"))
	}
	if chunk.ToolCall != nil {
		s.hasToolCall = true
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
			status = s.order.messageStatus(chunk.OutputIndex)
		}
		if status == "" {
			status = stopReasonCompleted
		}
		if status != stopReasonCompleted && status != stopReasonIncomplete {
			return failResponsesStream(s.send, fmt.Errorf("response item has an invalid completion status"))
		}
		if status == stopReasonIncomplete {
			if s.hasToolCall {
				return failResponsesStream(s.send, fmt.Errorf("response returned tool calls with incomplete output"))
			}
			s.hasIncomplete = true
		}
		if s.statuses == nil {
			s.statuses = map[int64]string{}
		}
		s.statuses[key] = status
		chunk.OutputItemStatus = status
	}
	if chunk.OutputIndex == nil {
		return s.unindexedChunk(chunk)
	}
	index := *chunk.OutputIndex
	if index < s.next {
		s.send(llm.StreamChunk{Error: fmt.Errorf("response output changed after its item completed"), Done: true})
		return false
	}
	s.pending[index] = append(s.pending[index], chunk)
	return s.drain()
}

func (s *orderedResponseSender) unindexedChunk(chunk llm.StreamChunk) bool {
	if chunk.OutputItemDone {
		s.open = false
	} else if chunk.Content != "" {
		s.open = true
	}
	if chunk.Done || chunk.Error != nil {
		if !s.flush() {
			return false
		}
	}
	return s.send(chunk)
}

// Only successful completions and supported token-budget truncation may
// recover buffered output. Missing, failed and refused terminals abort it.
func responsesTerminalAllowsOutput(stopReason string) bool {
	if stopReason == stopReasonToolCalls || stopReason == stopReasonLength {
		return true
	}
	return llm.NormalizeCompletionOutcome(&llm.CompletionResponse{StopReason: stopReason}) == llm.CompletionOutcomeCompleted
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
	// Empty messages still establish boundaries, including after the last
	// text chunk. Keep their observed slots in terminal recovery order.
	if s.order != nil {
		for index, itemType := range s.order.typesByIndex {
			if index < s.next || itemType != responseOutputTypeMessage {
				continue
			}
			if _, pending := s.pending[index]; !pending {
				indices = append(indices, index)
			}
		}
	}
	slices.Sort(indices)
	for _, index := range indices {
		if index < s.next {
			continue
		}
		for index > s.next {
			// Empty added messages have an observed slot but emit no chunk.
			// Complete each such predecessor before releasing later output.
			observedMessage := s.order != nil && s.order.typesByIndex[s.next] == responseOutputTypeMessage
			if !s.open && !observedMessage {
				s.send(llm.StreamChunk{Error: fmt.Errorf("response output has a missing predecessor"), Done: true})
				return false
			}
			previous := s.next
			status := s.order.messageStatus(&previous)
			if status == "" {
				status = stopReasonCompleted
			}
			if !s.send(llm.StreamChunk{OutputIndex: &previous, OutputItemDone: true, OutputItemStatus: status}) {
				return false
			}
			s.open = false
			s.next++
		}
		s.next = index
		if !s.drain() {
			return false
		}
	}
	// Preserve an observed final status when a terminal omits its snapshots.
	// Unknown last-message statuses remain open for outcome-based inference.
	if s.open {
		index := s.next
		outputIndex := &index
		if s.order != nil && s.order.unindexedText {
			outputIndex = nil
		}
		if status := s.order.messageStatus(outputIndex); status != "" {
			if !s.send(llm.StreamChunk{OutputIndex: outputIndex, OutputItemDone: true, OutputItemStatus: status}) {
				return false
			}
			s.open = false
			if outputIndex != nil {
				s.next++
			}
		}
	}
	return true
}

// responseOutputOrder validates the index/identity/type relationship before output
// reaches the queue. Compatible text-only streams may omit indices entirely,
// but mixing that mode with indexed items must never yield reordered success.
type responseOutputOrder struct {
	byID            map[string]int64
	byIndex         map[int64]string
	typesByID       map[string]string
	typesByIndex    map[int64]string
	statusByID      map[string]string
	statusByIndex   map[int64]string
	unindexedStatus string
	hasIncomplete   bool
	indexed         bool
	unindexedText   bool
	unindexedID     string
	text            map[int64]*responseTextItem
}

func (o *responseOutputOrder) bind(id string, index int64, itemType, status string) error {
	if index < 0 || o.unindexedText {
		return fmt.Errorf("ambiguous response output index")
	}
	if old, ok := o.byID[id]; id != "" && ok && old != index {
		return fmt.Errorf("response item changed output index")
	}
	if old := o.byIndex[index]; id != "" && old != "" && old != id {
		return fmt.Errorf("response output index changed item identity")
	}
	if err := o.bindType(id, &index, itemType); err != nil {
		return err
	}
	if err := o.observeStatus(id, &index, status); err != nil {
		return err
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

// Record inferred types even before an event supplies an index. Later metadata
// may connect an ID-only event to an index-only event, and both must agree.
func (o *responseOutputOrder) bindType(id string, index *int64, itemType string) error {
	knownTypes := []string{o.typesByID[id]}
	if index != nil {
		knownTypes = append(knownTypes, o.typesByIndex[*index])
	}
	for _, known := range knownTypes {
		if known == "" {
			continue
		}
		if itemType != "" && itemType != known {
			return fmt.Errorf("response output item changed type")
		}
		itemType = known
	}
	if itemType != "" {
		if o.typesByID == nil {
			o.typesByID = map[string]string{}
			o.typesByIndex = map[int64]string{}
		}
		if id != "" {
			o.typesByID[id] = itemType
		}
		if index != nil {
			o.typesByIndex[*index] = itemType
		}
	}
	return nil
}

// Final statuses can arrive with an added snapshot, before parts finish or an
// index is known. Keep those observations tied to the same identity bindings.
func (o *responseOutputOrder) observeStatus(id string, index *int64, status string) error {
	if status == "in_progress" {
		status = ""
	}
	if status != "" && status != stopReasonCompleted && status != stopReasonIncomplete {
		return fmt.Errorf("response item has an invalid completion status")
	}
	knownStatuses := []string{o.statusByID[id]}
	if index != nil {
		knownStatuses = append(knownStatuses, o.statusByIndex[*index])
	} else if o.unindexedText {
		knownStatuses = append(knownStatuses, o.unindexedStatus)
	}
	for _, known := range knownStatuses {
		if known == "" {
			continue
		}
		if status != "" && status != known {
			return fmt.Errorf("response item changed completion status")
		}
		status = known
	}
	if status == "" {
		return nil
	}
	if o.statusByID == nil {
		o.statusByID = map[string]string{}
		o.statusByIndex = map[int64]string{}
	}
	if id != "" {
		o.statusByID[id] = status
	}
	if index != nil {
		o.statusByIndex[*index] = status
	} else if o.unindexedText {
		o.unindexedStatus = status
	}
	o.hasIncomplete = o.hasIncomplete || status == stopReasonIncomplete
	return nil
}

func (o *responseOutputOrder) messageStatus(index *int64) string {
	if o == nil {
		return ""
	}
	if index == nil {
		return o.unindexedStatus
	}
	if o.typesByIndex[*index] != responseOutputTypeMessage {
		return ""
	}
	return o.statusByIndex[*index]
}

func (o *responseOutputOrder) validateTerminalMembership(output []responses.ResponseOutputItemUnion) error {
	// Compatible terminals may omit snapshots entirely. A nonempty snapshot
	// is authoritative and must account for every observed output item.
	if len(output) == 0 {
		return nil
	}
	for index := range o.typesByIndex {
		if index >= int64(len(output)) {
			return fmt.Errorf("response terminal omitted an observed output index")
		}
	}
	ids := make(map[string]bool, len(output))
	for _, item := range output {
		ids[item.ID] = true
	}
	for id, itemType := range o.typesByID {
		// The function tracker reconciles ID-only calls through call_id.
		// ID-only messages have no alternate identity for that association.
		if itemType != responseOutputTypeMessage {
			continue
		}
		if _, indexed := o.byID[id]; !indexed && !ids[id] {
			return fmt.Errorf("response terminal omitted an observed output identity")
		}
	}
	return nil
}

func (o *responseOutputOrder) eventIndex(evt responses.ResponseStreamEventUnion) (*int64, error) {
	if evt.Type == eventTypeResponseCompleted || evt.Type == eventTypeResponseIncomplete {
		if err := o.validateTerminalMembership(evt.Response.Output); err != nil {
			return nil, err
		}
		if o.unindexedText {
			output := evt.Response.Output
			if len(output) > 1 || (len(output) == 1 && (output[0].Type != responseOutputTypeMessage || (o.unindexedID != "" && output[0].ID != o.unindexedID))) {
				return nil, fmt.Errorf("ambiguous unindexed response output")
			}
			if len(output) == 1 {
				return nil, o.observeStatus(output[0].ID, nil, output[0].Status)
			}
			return nil, nil
		}
		for i, item := range evt.Response.Output {
			if err := o.bind(item.ID, int64(i), item.Type, item.Status); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	id := evt.ItemID
	itemType := ""
	status := ""
	switch {
	case evt.Type == eventTypeResponseOutputItemAdded || evt.Type == eventTypeResponseOutputItemDone:
		id = evt.Item.ID
		itemType = evt.Item.Type
		status = evt.Item.Status
	case isResponseTextEvent(evt):
		itemType = responseOutputTypeMessage
	case evt.Type == eventTypeResponseFunctionCallArgumentsDelta || evt.Type == eventTypeResponseFunctionCallArgumentsDone:
		itemType = eventTypeFunctionCall
	}
	if evt.JSON.OutputIndex.Valid() {
		if !o.unindexedText {
			index := evt.OutputIndex
			if err := o.bind(id, index, itemType, status); err != nil {
				return nil, err
			}
			return &index, nil
		}
		// Index zero can identify the existing single unindexed message.
		// Keep its text/status state and apply the identity checks below.
		if evt.OutputIndex != 0 || itemType != responseOutputTypeMessage {
			return nil, fmt.Errorf("ambiguous response output index")
		}
	}
	if index, ok := o.byID[id]; id != "" && ok {
		if err := o.bind(id, index, itemType, status); err != nil {
			return nil, err
		}
		return &index, nil
	}
	return nil, o.observeUnindexedEvent(evt, id, itemType, status)
}

func (o *responseOutputOrder) observeUnindexedEvent(evt responses.ResponseStreamEventUnion, id, itemType, status string) error {
	if err := o.bindType(id, nil, itemType); err != nil {
		return err
	}
	messageText := evt.Item.Type == responseOutputTypeMessage && (evt.Type == eventTypeResponseOutputItemDone ||
		(evt.Type == eventTypeResponseOutputItemAdded && len(evt.Item.Content) > 0))
	if isResponseTextEvent(evt) || messageText || (o.unindexedText && itemType == responseOutputTypeMessage) {
		if o.indexed || (o.unindexedText && o.unindexedID != "" && id != "" && o.unindexedID != id) {
			return fmt.Errorf("response text has no unambiguous output index")
		}
		// Anonymous text can follow an ID-only empty added snapshot. Use
		// that message's metadata only when the association is unique.
		for knownID, knownType := range o.typesByID {
			if knownType != responseOutputTypeMessage {
				continue
			}
			if id != "" && id != knownID {
				return fmt.Errorf("response text has no unambiguous output identity")
			}
			id = knownID
		}
		o.unindexedText = true
		if id != "" {
			o.unindexedID = id
		}
	}
	return o.observeStatus(id, nil, status)
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
		if status == "" {
			status = o.messageStatus(outputIndex)
		}
		if status == "" && evt.Type == eventTypeResponseIncomplete && i == len(evt.Response.Output)-1 {
			status = stopReasonIncomplete
		}
		if !o.messageSnapshot(item, outputIndex, true, send) {
			return false
		}
		if !send(llm.StreamChunk{OutputIndex: outputIndex, OutputItemDone: true, OutputItemStatus: status}) {
			return false
		}
	}
	return o.validateTextCompletion(send)
}
