package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
)

// MeetingDraftExtension identifies the structured-message schema for meeting.version "1".
const MeetingDraftExtension = "meetingDraft.v1"

// MaxMeetingDraftJSONBytes allows fully escaped bounded strings, not arbitrary
// worker metadata. The projection has at most 21 entries in the V1 envelope.
const MaxMeetingDraftJSONBytes = 32 << 10

// MeetingDraft is an unscheduled proposal, never a destination or calendar API.
// Attendee names are ordinary display names; the adapter resolves them locally.
type MeetingDraft struct {
	Subject         string           `json:"subject"`
	Agenda          []string         `json:"agenda"`
	DurationMinutes int              `json:"durationMinutes"`
	Attendees       MeetingAttendees `json:"attendees"`
	PreferredWindow string           `json:"preferredWindow,omitempty"`
	SuggestedStart  string           `json:"suggestedStart,omitempty"`
}

type MeetingAttendees struct {
	Mode  string   `json:"mode"`
	Names []string `json:"names,omitempty"`
}

// DecodeClosedObject preserves exact spelling and rejects duplicate properties
// (including escaped aliases). Struct decoding alone is case-insensitive and
// last-wins. Callers impose their own byte and required-property bounds.
func DecodeClosedObject(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	invalid := errors.New("invalid closed JSON object")
	if !ValidJSONUnicode(data) {
		return nil, invalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalid
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, invalid
		}
		key, ok := token.(string)
		if !ok {
			return nil, invalid
		}
		known := slices.Contains(allowed, key)
		if _, exists := fields[key]; !known || exists {
			return nil, invalid
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, invalid
		}
		fields[key] = raw
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return nil, invalid
	}
	return fields, nil
}

func (d *MeetingDraft) UnmarshalJSON(data []byte) error {
	invalid := errors.New("invalid meeting draft")
	if len(data) > MaxMeetingDraftJSONBytes {
		return invalid
	}
	fields, err := DecodeClosedObject(data, "subject", "agenda", "durationMinutes", "attendees", "preferredWindow", "suggestedStart")
	if err != nil {
		return invalid
	}
	for _, key := range []string{"subject", "agenda", "attendees"} {
		if _, ok := fields[key]; !ok {
			return invalid
		}
	}
	type plain MeetingDraft
	var value plain
	if json.Unmarshal(data, &value) != nil {
		return invalid
	}
	if _, ok := fields["durationMinutes"]; !ok {
		value.DurationMinutes = 30
	} else if value.DurationMinutes == 0 {
		return invalid
	}
	for _, key := range []string{"preferredWindow", "suggestedStart"} {
		if raw, ok := fields[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
				return invalid
			}
		}
	}
	attendeeFields, err := DecodeClosedObject(fields["attendees"], "mode", "names")
	if err != nil {
		return invalid
	}
	if _, ok := attendeeFields["mode"]; !ok {
		return invalid
	}
	if value.Attendees.Mode == "everyone" {
		if _, ok := attendeeFields["names"]; ok {
			return invalid
		}
	}
	result := MeetingDraft(value)
	if _, err := result.Metadata(); err != nil {
		return invalid
	}
	*d = result
	return nil
}

var meetingStartPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-](0\d|1\d|2[0-3]):[0-5]\d)$`)

func validDraftString(s string, maxBytes int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= maxBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// Canonicalize before immutable request comparison using the store's stable
// trim/redaction policy. Validate raw and transformed bounds, never truncate.
func canonicalDraftString(s string, maxBytes int) (string, bool) {
	if !validDraftString(s, maxBytes) {
		return "", false
	}
	s = redact.SensitiveText(strings.TrimSpace(s))
	return s, validDraftString(s, maxBytes)
}

// Metadata validates typed callers as well as JSON callers and normalizes the
// omitted duration. No arbitrary metadata, attendee address or destination can
// be injected here; user-typed subject/agenda text is not an address boundary.
func (d MeetingDraft) Metadata() (map[string]string, error) {
	invalid := errors.New("invalid meeting draft")
	duration := d.DurationMinutes
	if duration == 0 {
		duration = 30
	}
	subject, valid := canonicalDraftString(d.Subject, 200)
	if !valid || len(d.Agenda) < 1 || len(d.Agenda) > 5 || duration < 1 || duration > 480 {
		return nil, invalid
	}
	m := map[string]string{"meeting.version": "1", "meeting.subject": subject, "meeting.durationMinutes": strconv.Itoa(duration), "meeting.attendees": d.Attendees.Mode}
	for i, item := range d.Agenda {
		item, valid := canonicalDraftString(item, 256)
		if !valid {
			return nil, invalid
		}
		m["meeting.agenda."+strconv.Itoa(i)] = item
	}
	switch d.Attendees.Mode {
	case "everyone":
		if len(d.Attendees.Names) != 0 {
			return nil, invalid
		}
	case "named":
		if len(d.Attendees.Names) < 1 || len(d.Attendees.Names) > 10 {
			return nil, invalid
		}
		for i, name := range d.Attendees.Names {
			if strings.Contains(name, "@") {
				return nil, invalid
			}
			name, valid := canonicalDraftString(name, 128)
			if !valid {
				return nil, invalid
			}
			m["meeting.name."+strconv.Itoa(i)] = name
		}
	default:
		return nil, invalid
	}
	if d.PreferredWindow != "" {
		window, valid := canonicalDraftString(d.PreferredWindow, 256)
		if !valid {
			return nil, invalid
		}
		m["meeting.preferredWindow"] = window
	}
	if d.SuggestedStart != "" {
		if !meetingStartPattern.MatchString(d.SuggestedStart) {
			return nil, invalid
		}
		start, err := time.Parse(time.RFC3339Nano, d.SuggestedStart)
		if err != nil || start.Year() < 1 || start.Add(time.Duration(duration)*time.Minute).Year() > 9999 {
			return nil, invalid
		}
		m["meeting.suggestedStart"] = d.SuggestedStart
	}
	return m, nil
}
