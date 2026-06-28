/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package genai

import "strings"

type ContentCaptureMode int

const (
	ContentCaptureModeNone ContentCaptureMode = iota
	ContentCaptureModeSpanOnly
	ContentCaptureModeEventOnly
	ContentCaptureModeSpanAndEvent
)

func ParseContentCaptureMode(value string) ContentCaptureMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ContentCaptureNone, "no_content", "off", "false", "0":
		return ContentCaptureModeNone
	case ContentCaptureSpan, "span_only":
		return ContentCaptureModeSpanOnly
	case ContentCaptureEvent, "event_only":
		return ContentCaptureModeEventOnly
	case ContentCaptureAll, "all", "both", "true", "1":
		return ContentCaptureModeSpanAndEvent
	default:
		return ContentCaptureModeNone
	}
}

func (m ContentCaptureMode) String() string {
	switch m {
	case ContentCaptureModeSpanOnly:
		return ContentCaptureSpan
	case ContentCaptureModeEventOnly:
		return ContentCaptureEvent
	case ContentCaptureModeSpanAndEvent:
		return ContentCaptureAll
	default:
		return ContentCaptureNone
	}
}
