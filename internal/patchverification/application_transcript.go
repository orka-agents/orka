package patchverification

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
)

const MaxApplicationTranscriptBytes = 256 << 10

type ApplicationStreamFrame struct {
	Version     int                           `json:"version"`
	Kind        string                        `json:"kind"`
	TaskUID     string                        `json:"taskUID"`
	PodUID      string                        `json:"podUID"`
	InputDigest string                        `json:"inputDigest"`
	Action      *ApplicationActionObservation `json:"action,omitempty"`
	Report      *ApplicationReport            `json:"report,omitempty"`
	Code        string                        `json:"code,omitempty"`
}

func ApplicationWorkerInputDigest(input ApplicationWorkerInput) (string, error) {
	if !validManifestUTF8(reflect.ValueOf(input)) {
		return "", ErrEvidence
	}
	content, err := json.Marshal(input)
	if err != nil || len(content) > MaxApplicationInputBytes {
		return "", ErrLimit
	}
	return Digest(content), nil
}

func ValidateApplicationStreamFrame(input ApplicationWorkerInput, frame ApplicationStreamFrame) error {
	digest, err := ApplicationWorkerInputDigest(input)
	if err != nil || frame.Version != ApplicationProtocolVersion || frame.InputDigest != digest ||
		frame.TaskUID != input.TaskUID || !identifierPattern.MatchString(frame.PodUID) ||
		input.Worker.PodUID != "" && frame.PodUID != input.Worker.PodUID {
		return ErrBinding
	}
	switch frame.Kind {
	case ApplicationFrameAction:
		return validateApplicationActionFrame(input, frame)
	case ApplicationFrameFinal:
		if frame.Action != nil || frame.Report == nil || frame.Code != "" ||
			frame.Report.Version != ApplicationProtocolVersion || frame.Report.TaskUID != input.TaskUID ||
			frame.Report.PodUID != frame.PodUID || frame.Report.Evidence.Observation.PodUID != frame.PodUID {
			return ErrEvidence
		}
		if err := validateApplicationFinalAPIBaseline(input, frame.Report.Evidence.Observation); err != nil {
			return err
		}
		return ValidateApplicationWorkerObservation(input.Manifest, input.Binding, frame.Report.Evidence.Observation)
	case ApplicationFrameFailure:
		if frame.Action != nil || frame.Report != nil || frame.Code != "observer-incomplete" {
			return ErrEvidence
		}
		return nil
	default:
		return ErrEvidence
	}
}

func validateApplicationFinalAPIBaseline(input ApplicationWorkerInput, observation Observation) error {
	for _, check := range input.Manifest.Checks {
		if check.ID != input.CheckID {
			continue
		}
		if observation.Application == nil || observation.CheckID != check.ID {
			return ErrBinding
		}
		baseline, err := applicationCheckAPIBaseline(input, check, observation.Application.Actions)
		if err != nil || !reflect.DeepEqual(baseline, observation.Application.APIBaseline) {
			return ErrBinding
		}
		return nil
	}
	return ErrBinding
}

func validateApplicationActionFrame(input ApplicationWorkerInput, frame ApplicationStreamFrame) error {
	if frame.Action == nil || frame.Report != nil || frame.Code != "" {
		return ErrEvidence
	}
	var check *Check
	for i := range input.Manifest.Checks {
		if input.Manifest.Checks[i].ID == input.CheckID {
			check = &input.Manifest.Checks[i]
		}
	}
	index := frame.Action.Index
	if check == nil || index < 0 || index >= len(check.Application.Actions) || index >= len(input.ActionTargets) {
		return ErrEvidence
	}
	for _, actor := range input.Actors {
		if actor.ID == frame.Action.Actor.ID {
			return ValidateApplicationActionObservation(check.Application.Actions[index], input.ActionTargets[index],
				actor, *frame.Action, input.Manifest.ApplicationPolicy)
		}
	}
	return ErrBinding
}

// Complete preceding frames remain usable cleanup receipts after a truncated
// tail. They do not establish final evidence or permission to replay an action.
func DecodeApplicationTranscript(reader io.Reader) ([]ApplicationStreamFrame, error) {
	bounded := io.LimitReader(reader, MaxApplicationTranscriptBytes+1)
	content, err := io.ReadAll(bounded)
	if err != nil || len(content) > MaxApplicationTranscriptBytes {
		return nil, ErrLimit
	}
	lines := bufio.NewReader(bytes.NewReader(content))
	frames := []ApplicationStreamFrame{}
	terminal := false
	for {
		line, readErr := lines.ReadBytes('\n')
		if len(line) == 0 && readErr == io.EOF {
			if !terminal {
				return frames, io.ErrUnexpectedEOF
			}
			return frames, nil
		}
		if readErr != nil {
			return frames, io.ErrUnexpectedEOF
		}
		if terminal || len(frames) >= 5 || len(line) > MaxApplicationReportBytes {
			return frames, ErrEvidence
		}
		var frame ApplicationStreamFrame
		if err := decodePreparationJSON(bytes.NewReader(line), &frame, MaxApplicationReportBytes); err != nil {
			return frames, err
		}
		if frame.Kind != ApplicationFrameAction && frame.Kind != ApplicationFrameFinal && frame.Kind != ApplicationFrameFailure {
			return frames, errors.New("unknown application transcript frame")
		}
		terminal = frame.Kind == ApplicationFrameFinal || frame.Kind == ApplicationFrameFailure
		frames = append(frames, frame)
	}
}
