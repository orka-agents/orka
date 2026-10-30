package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const validMeeting = `{"subject":"Planning","agenda":["Next steps"],"attendees":{"mode":"everyone"}}`

func TestMeetingDraftTypedByteBoundsAndMaxProjection(t *testing.T) {
	names := make([]string, 10)
	for i := range names {
		names[i] = strings.Repeat("é", 64)
	}
	base := MeetingDraft{Subject: strings.Repeat("é", 100), Agenda: []string{strings.Repeat("é", 128), "b", "c", "d", "e"}, DurationMinutes: 480, Attendees: MeetingAttendees{Mode: "named", Names: names}, PreferredWindow: strings.Repeat("é", 128), SuggestedStart: "2026-10-12T09:00:00+12:00"}
	metadata, err := base.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 21 || metadata["meeting.agenda.4"] != "e" || metadata["meeting.name.9"] != strings.Repeat("é", 64) {
		t.Fatalf("invalid maximum projection %#v", metadata)
	}
	delivery := DeliveryRequest{ProtocolVersion: Version, DeliveryID: "delivery", IdempotencyID: "delivery", OriginatingEvent: "event", AccountID: "account", ContextID: "room", ReplyTarget: "room", Kind: DeliveryKindMessage, Text: "Draft above", Metadata: metadata}
	if err := ValidateDeliveryRequest(&delivery); err != nil {
		t.Fatalf("maximum projection exceeds delivery bounds: %v", err)
	}
	for _, change := range []func(*MeetingDraft){
		func(d *MeetingDraft) { d.Subject += "x" },
		func(d *MeetingDraft) { d.Agenda = []string{strings.Repeat("é", 129)} },
		func(d *MeetingDraft) { d.Agenda = []string{"bad\x7fitem"} },
		func(d *MeetingDraft) { d.Attendees.Names = []string{strings.Repeat("é", 65)} },
		func(d *MeetingDraft) { d.Attendees.Names = append(d.Attendees.Names, "Extra") },
		func(d *MeetingDraft) { d.Attendees.Names = []string{"bad\x00name"} },
		func(d *MeetingDraft) { d.PreferredWindow += "x" },
		func(d *MeetingDraft) { d.DurationMinutes = -1 },
		func(d *MeetingDraft) { d.Subject = strings.Repeat("a", 189) + " token is x" },
		func(d *MeetingDraft) { d.Agenda = []string{strings.Repeat("a", 245) + " token is x"} },
		func(d *MeetingDraft) { d.Attendees.Names = []string{strings.Repeat("a", 117) + " token is x"} },
		func(d *MeetingDraft) { d.PreferredWindow = strings.Repeat("a", 245) + " token is x" },
	} {
		draft := base
		change(&draft)
		if _, err := draft.Metadata(); err == nil {
			t.Fatal("invalid typed bounds accepted")
		}
	}
	var draft MeetingDraft
	for _, raw := range []string{strings.Replace(validMeeting, `"subject":`, `"subj\u0065ct":"Other","subject":`, 1), strings.Replace(validMeeting, "{", "{"+strings.Repeat(" ", 32769), 1)} {
		if json.Unmarshal([]byte(raw), &draft) == nil {
			t.Fatal("duplicate/oversize JSON accepted")
		}
	}
}

func TestMeetingDraftClosedValidationAndProjection(t *testing.T) {
	var draft MeetingDraft
	if err := json.Unmarshal([]byte(validMeeting), &draft); err != nil {
		t.Fatal(err)
	}
	metadata, err := draft.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"meeting.version": "1", "meeting.subject": "Planning", "meeting.durationMinutes": "30", "meeting.attendees": "everyone", "meeting.agenda.0": "Next steps"}
	if !reflect.DeepEqual(want, metadata) {
		t.Fatalf("projection = %#v", metadata)
	}
	var explicit MeetingDraft
	if err := json.Unmarshal([]byte(strings.Replace(validMeeting, `"subject":`, `"durationMinutes":30,"subject":`, 1)), &explicit); err != nil {
		t.Fatal(err)
	}
	other, err := explicit.Metadata()
	if err != nil || !reflect.DeepEqual(metadata, other) {
		t.Fatalf("default not canonical: %#v %v", other, err)
	}
	for _, raw := range []string{
		`null`, `{}`, strings.Replace(validMeeting, `"subject":"Planning"`, `"Subject":"Planning"`, 1),
		strings.Replace(validMeeting, `"subject":"Planning"`, `"subject":"Planning","subject":"Other"`, 1),
		strings.Replace(validMeeting, `"subject":"Planning"`, `"subject":"Planning","url":"https://example.com"`, 1),
		strings.Replace(validMeeting, `"Planning"`, `""`, 1), strings.Replace(validMeeting, `"Planning"`, `"\ud800"`, 1), strings.Replace(validMeeting, `"Planning"`, `"bad\ntext"`, 1),
		strings.Replace(validMeeting, `"Planning"`, `"`+strings.Repeat("é", 101)+`"`, 1),
		strings.Replace(validMeeting, `["Next steps"]`, `[]`, 1), strings.Replace(validMeeting, `["Next steps"]`, `["a","b","c","d","e","f"]`, 1),
		strings.Replace(validMeeting, `"mode":"everyone"`, `"mode":"everyone","names":["Alice"]`, 1),
		strings.Replace(validMeeting, `"mode":"everyone"`, `"mode":"named","names":["Alice@example.com"]`, 1),
		strings.Replace(validMeeting, `"mode":"everyone"`, `"mode":"named","names":[]`, 1),
		strings.Replace(validMeeting, `"mode":"everyone"`, `"mode":"everyone","mode":"everyone"`, 1),
		strings.Replace(validMeeting, `"subject":`, `"durationMinutes":0,"subject":`, 1), strings.Replace(validMeeting, `"subject":`, `"durationMinutes":481,"subject":`, 1),
		strings.Replace(validMeeting, `"subject":`, `"durationMinutes":1.5,"subject":`, 1), strings.Replace(validMeeting, `"subject":`, `"durationMinutes":null,"subject":`, 1),
		strings.Replace(validMeeting, `"subject":`, `"preferredWindow":"","subject":`, 1),
		strings.Replace(validMeeting, `"subject":`, `"suggestedStart":"2026-02-30T10:00:00Z","subject":`, 1),
		strings.Replace(validMeeting, `"subject":`, `"suggestedStart":"2026-02-01T10:00:00","subject":`, 1),
		strings.Replace(validMeeting, `"subject":`, `"suggestedStart":"2026-02-01T10:00:00+24:00","subject":`, 1),
		strings.Replace(validMeeting, `"subject":`, `"suggestedStart":"9999-12-31T23:59:00Z","subject":`, 1),
		validMeeting + `{}`,
	} {
		t.Run(raw[:min(len(raw), 75)], func(t *testing.T) {
			var d MeetingDraft
			if json.Unmarshal([]byte(raw), &d) == nil {
				t.Fatalf("accepted invalid draft %s", raw)
			}
		})
	}
	for _, raw := range []string{strings.Replace(validMeeting, `"mode":"everyone"`, `"mode":"named","names":["Alice Smith","Bob"]`, 1), strings.Replace(validMeeting, `"subject":`, `"suggestedStart":"2026-02-01T10:00:00+12:00","preferredWindow":"Tomorrow","durationMinutes":480,"subject":`, 1)} {
		var d MeetingDraft
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Metadata(); err != nil {
			t.Fatal(err)
		}
	}
}
