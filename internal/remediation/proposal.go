package remediation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

const maxProposalBytes = 256 << 10

type ProposedFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Executable bool   `json:"executable"`
}

type CheckProposal struct {
	Scope        []string                    `json:"scope"`
	Gaps         []string                    `json:"gaps"`
	Requirements []pv.EnvironmentRequirement `json:"requirements"`
	Files        []ProposedFile              `json:"files"`
	Checks       []pv.Check                  `json:"checks"`
	Services     []pv.Service                `json:"services"`
	Application  *pv.ApplicationPlan         `json:"application,omitempty"`
}

type PatchProposal struct {
	Summary         string              `json:"summary"`
	Patch           string              `json:"patch"`
	Edits           []SourceEdit        `json:"edits,omitempty"`
	DeclaredChanges []pv.DeclaredChange `json:"declaredChanges"`
	Limitations     []string            `json:"limitations"`
}

type SourceEdit struct {
	Path        string `json:"path"`
	Old         string `json:"old"`
	New         string `json:"new"`
	Occurrences *int   `json:"occurrences,omitempty"`
}

const MaxEditOccurrences = 16

func decodeProposal(raw string, destination any) error {
	content := []byte(raw)
	if len(content) == 0 || len(content) > maxProposalBytes || !utf8.Valid(content) {
		return errors.New("agent proposal is empty, oversized, or not UTF-8")
	}
	if err := uniqueProposalJSON(json.NewDecoder(bytes.NewReader(content)), 0); err != nil {
		return errors.New("agent proposal must be one JSON object without duplicate fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("agent proposal does not match the required schema")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("agent proposal contains trailing data")
	}
	return nil
}

func uniqueProposalJSON(decoder *json.Decoder, depth int) error {
	if depth > 20 {
		return errors.New("proposal nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		if depth == 0 {
			return errors.New("proposal must be an object")
		}
		return nil
	}
	if depth == 0 && delim != '{' {
		return errors.New("proposal must be an object")
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[strings.ToLower(name)] {
				return errors.New("duplicate proposal key")
			}
			keys[strings.ToLower(name)] = true
		}
		if err := uniqueProposalJSON(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func DecodeCheckProposal(raw string) (CheckProposal, error) {
	return decodeCheckProposal(raw, nil)
}

func DecodeApplicationCheckProposal(raw string, policy *pv.ApplicationPolicy) (CheckProposal, error) {
	proposal, err := decodeCheckProposal(raw, policy)
	if err == nil && proposal.Application == nil {
		err = errors.New("application-runtime proposals require a frozen application plan")
	}
	return proposal, err
}

func DecodeApplicationPolicy(raw []byte) (*pv.ApplicationPolicy, error) {
	var policy pv.ApplicationPolicy
	if err := decodeProposal(string(raw), &policy); err != nil {
		return nil, errors.New("application policy must be a strict bounded JSON object")
	}
	var wire struct {
		Resources []map[string]json.RawMessage `json:"resources"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return nil, errors.New("application policy resources are invalid")
	}
	for _, resource := range wire.Resources {
		var scope json.RawMessage
		for key, value := range resource {
			if strings.EqualFold(key, "namespaced") {
				scope = bytes.TrimSpace(value)
			}
		}
		if !bytes.Equal(scope, []byte("true")) && !bytes.Equal(scope, []byte("false")) {
			return nil, errors.New("application resource scope requires an explicit namespaced boolean")
		}
	}
	if err := pv.ValidateApplicationPolicy(policy); err != nil {
		return nil, errors.New("application policy is invalid")
	}
	return &policy, nil
}

func DecodeApplicationRuntimeQualification(raw []byte) (*pv.ApplicationRuntimeQualification, error) {
	var qualification pv.ApplicationRuntimeQualification
	if err := decodeProposal(string(raw), &qualification); err != nil {
		return nil, errors.New("application runtime qualification must be a strict bounded JSON object")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, errors.New("application runtime qualification is invalid")
	}
	var swap json.RawMessage
	for name, value := range fields {
		if strings.EqualFold(name, "nodeStorageBytes") || strings.EqualFold(name, "runtimeConfigDigest") {
			return nil, errors.New("application runtime qualification contains ambiguous resource or configuration evidence")
		}
		if strings.EqualFold(name, "nativeSwapMax") {
			swap = bytes.TrimSpace(value)
		}
	}
	if !bytes.Equal(swap, []byte("0")) {
		return nil, errors.New("application runtime qualification requires an explicit native no-swap bound")
	}
	if err := pv.ValidateApplicationRuntimeQualification(qualification); err != nil {
		return nil, errors.New("application runtime qualification is invalid")
	}
	return &qualification, nil
}

func decodeCheckProposal(raw string, policy *pv.ApplicationPolicy) (CheckProposal, error) {
	var proposal CheckProposal
	if err := decodeProposal(raw, &proposal); err != nil {
		return proposal, err
	}
	if len(proposal.Scope) == 0 || len(proposal.Scope) > 100 || len(proposal.Gaps) > 100 ||
		len(proposal.Requirements) > 32 || len(proposal.Files) > pv.MaxFrozenFiles ||
		len(proposal.Checks) == 0 || len(proposal.Checks) > 100 || len(proposal.Services) > pv.MaxServices {
		return proposal, errors.New("check proposal is incomplete or exceeds its bounds")
	}
	if proposal.Application != nil {
		err := validateApplicationCheckProposal(&proposal, policy)
		return proposal, err
	}
	return proposal, validateSourceCheckProposal(proposal)
}

func validateApplicationCheckProposal(proposal *CheckProposal, policy *pv.ApplicationPolicy) error {
	if len(proposal.Files) != 0 || len(proposal.Services) != 0 || len(proposal.Requirements) != 0 {
		return errors.New("application plans cannot add executable check files, services, or ambient requirements")
	}
	if err := canonicalApplicationResources(proposal); err != nil {
		return err
	}
	if err := pv.ValidateApplicationPlanWithPolicy(*proposal.Application, policy); err != nil {
		return errors.New("application plan is invalid")
	}
	for index := range proposal.Checks {
		normalizeApplicationExpectations(&proposal.Checks[index])
		if err := pv.ValidateApplicationCheckWithPolicy(proposal.Checks[index], policy); err != nil {
			return errors.New("application check is invalid")
		}
	}
	return nil
}

func validateSourceCheckProposal(proposal CheckProposal) error {
	if len(proposal.Files) == 0 {
		return errors.New("source-harness checks require frozen executable files")
	}
	files := make(map[string]bool)
	for _, file := range proposal.Files {
		if file.Path == "" || path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path ||
			file.Path == "." || file.Path == ".." || strings.HasPrefix(file.Path, "../") ||
			strings.ContainsAny(file.Path, "\\\x00\r\n") || len(file.Path) > 255 {
			return errors.New("check proposal contains an unsafe file path")
		}
		if _, found := files[file.Path]; found {
			return errors.New("check proposal contains duplicate files")
		}
		files[file.Path] = file.Executable
	}
	commands := make([][]string, 0, len(proposal.Checks)+len(proposal.Services))
	for _, check := range proposal.Checks {
		if check.Application != nil {
			return errors.New("application checks require an application plan")
		}
		if check.HTTP != nil {
			commands = append(commands, check.HTTP.ServerCommand)
		} else {
			commands = append(commands, check.Command)
		}
	}
	for _, service := range proposal.Services {
		commands = append(commands, service.Command)
	}
	for _, command := range commands {
		if len(command) == 0 || !strings.HasPrefix(command[0], "/checks/") ||
			path.Clean(command[0]) != command[0] || !files[strings.TrimPrefix(command[0], "/checks/")] {
			return errors.New("check commands must use executable frozen files under /checks")
		}
	}
	return nil
}

func canonicalApplicationResources(proposal *CheckProposal) error {
	canonical := func(content *json.RawMessage) error {
		decoder := json.NewDecoder(bytes.NewReader(*content))
		decoder.UseNumber()
		var value map[string]any
		if decoder.Decode(&value) != nil || value == nil {
			return errors.New("application resources must be JSON objects")
		}
		var trailing any
		if !errors.Is(decoder.Decode(&trailing), io.EOF) {
			return errors.New("application resource contains trailing data")
		}
		data, err := json.Marshal(value)
		if err != nil {
			return errors.New("application resource is invalid")
		}
		*content = data
		return nil
	}
	for index := range proposal.Application.Resources {
		if err := canonical(&proposal.Application.Resources[index]); err != nil {
			return err
		}
	}
	for index := range proposal.Checks {
		check := proposal.Checks[index].Application
		if check == nil {
			continue
		}
		for index := range check.Resources {
			if err := canonical(&check.Resources[index]); err != nil {
				return err
			}
		}
		for index := range check.Actions {
			if err := canonical(&check.Actions[index].Resource); err != nil {
				return err
			}
		}
	}
	return nil
}

// Models propose exact API values, not cryptographic hashes. Freeze those
// values through the trusted canonicalizer before manifest validation; an
// already-frozen digest expectation is left unchanged for review revisions.
func normalizeApplicationExpectations(check *pv.Check) {
	if check.Application == nil || check.Application.Kubernetes == nil {
		return
	}
	for _, expectation := range []*pv.Expectation{&check.Healthy, &check.Failure} {
		if expectation.ExitCode != 0 || len(expectation.Services) != 0 ||
			uniqueProposalJSON(json.NewDecoder(strings.NewReader(expectation.Stdout)), 0) != nil {
			continue
		}
		var value pv.ApplicationAPIValue
		decoder := json.NewDecoder(strings.NewReader(expectation.Stdout))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&value) != nil {
			continue
		}
		var extra any
		if !errors.Is(decoder.Decode(&extra), io.EOF) {
			continue
		}
		if frozen, err := pv.ApplicationAPIExpectation(value); err == nil {
			*expectation = frozen
		}
	}
}

func requireProtectedChecks(proposal CheckProposal) error {
	for _, check := range proposal.Checks {
		if check.HTTP == nil {
			return errors.New("automatic remediation requires protected HTTP observations; in-process or command-only assertions are unsupported")
		}
	}
	return nil
}

func DecodePatchProposal(raw string) (PatchProposal, error) {
	var proposal PatchProposal
	if err := decodeProposal(raw, &proposal); err != nil {
		return proposal, err
	}
	if strings.TrimSpace(proposal.Summary) == "" || len(proposal.Summary) > 16384 ||
		(proposal.Patch == "") == (len(proposal.Edits) == 0) || strings.ContainsRune(proposal.Patch, '\x00') ||
		len(proposal.DeclaredChanges) == 0 || len(proposal.DeclaredChanges) > 100 || len(proposal.Limitations) > 100 {
		return proposal, errors.New("patch proposal must contain a bounded summary, Git diff, and declared changes")
	}
	if proposal.Patch != "" && !strings.HasPrefix(proposal.Patch, "diff --git ") {
		return proposal, errors.New("candidate patch must be a Git unified diff")
	}
	if len(proposal.Edits) > 64 {
		return proposal, errors.New("candidate edit count exceeds its limit")
	}
	for index, edit := range proposal.Edits {
		if edit.Old == "" || edit.Old == edit.New || edit.Path == "" || path.IsAbs(edit.Path) ||
			path.Clean(edit.Path) != edit.Path || edit.Path == ".." || strings.HasPrefix(edit.Path, "../") ||
			strings.ContainsAny(edit.Path, "\\\x00\r\n") {
			return proposal, fmt.Errorf("candidate edit %d must change exact source text in a safe relative file; empty and unchanged edits are forbidden", index)
		}
		if edit.Occurrences != nil && (*edit.Occurrences < 1 || *edit.Occurrences > MaxEditOccurrences) {
			return proposal, fmt.Errorf("candidate edit %d occurrences must be between 1 and %d", index, MaxEditOccurrences)
		}
	}
	return proposal, nil
}
