package patchverification

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

const (
	credentialEnvironmentName = `[A-Za-z_][A-Za-z0-9_]*`
	credentialPlaceholder     = `(?:\$` + credentialEnvironmentName + `|\$\{` + credentialEnvironmentName + `\}|%` + credentialEnvironmentName + `%)`
	credentialQuotedName      = `(?:"` + credentialEnvironmentName + `"|'` + credentialEnvironmentName + `')`
	credentialNames           = `api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|password|authorization`
)

var (
	credentialFormats         = regexp.MustCompile(`(?i)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-(?:proj-)?[A-Za-z0-9_-]{20,}))`)
	credentialField           = regexp.MustCompile(`(?i)(` + credentialNames + `)["']?\s*(:=|[:=])\s*`)
	credentialJSONField       = regexp.MustCompile(`(?i)(` + credentialNames + `)$`)
	credentialSchemaMetadata  = regexp.MustCompile(`^(?:type|format|title|description|\$ref|\$schema|minLength|maxLength|pattern|readOnly|writeOnly|nullable|deprecated)$`)
	credentialScheme          = regexp.MustCompile(`(?i)^["']?(?:bearer|basic)(?:[\t ]+|$)`)
	credentialEmpty           = regexp.MustCompile("^(?:\"\"|''|``)")
	credentialNull            = regexp.MustCompile(`^null\b`)
	credentialHeaderRef       = regexp.MustCompile(`^` + credentialPlaceholder)
	credentialPlaceholderOnly = regexp.MustCompile(`^` + credentialPlaceholder + `$`)
	credentialRef             = regexp.MustCompile(`^(?:` + credentialPlaceholder +
		`|"` + credentialPlaceholder + `"|'` + credentialPlaceholder + `'` +
		`|(?:os\.(?:Getenv|LookupEnv|getenv)|os\.environ\.get|System\.getenv|getenv|std::env::var)\([\t ]*` + credentialQuotedName + `[\t ]*\)` +
		`|(?:os\.environ|process\.env)\[[\t ]*` + credentialQuotedName + `[\t ]*\]` +
		`|process\.env\.` + credentialEnvironmentName + `)`)
	credentialIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)
)

// CredentialMatcher screens source and evidence without returning matched bytes.
// Its zero value rejects known private-key/token formats and nonempty scalar
// credential assignments, including short literals. JSON schema metadata, empty
// values and complete environment references are not scalar credentials. Schema
// defaults, examples, enums and other non-metadata members remain screened as
// credential values, as do array elements.
//
// This is not an exhaustive secret detector or a language/data-flow parser:
// unknown formats, arbitrary encoding and values hidden behind other field
// names/metadata are not resolved. Placeholder-looking real values cannot be
// distinguished from references. Ambiguous bare assignments remain rejected;
// only Go's := makes a bare identifier a reference, not a configuration literal.
type CredentialMatcher struct{}

func (CredentialMatcher) Match(content []byte) bool {
	if matched, isTar := matchTarArchive(content); isTar {
		return matched
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) != 0 && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"') && json.Valid(trimmed) {
		// Decode actual JSON, not arbitrary backslash sequences in source.
		// Streaming avoids retaining another complete manifest/object tree.
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		return credentialJSONValue(decoder, "")
	}
	return credentialText(maskGoCredentialReferences(content))
}

func matchTarArchive(content []byte) (matched bool, isTar bool) {
	if len(content) < 512 || len(content)%512 != 0 || !bytes.Equal(content[257:262], []byte("ustar")) {
		return false, false
	}
	reader := tar.NewReader(bytes.NewReader(content))
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return false, entries > 0
		}
		if err != nil {
			return true, true
		}
		entries++
		if (CredentialMatcher{}).MatchString(header.Name) || (CredentialMatcher{}).MatchString(header.Linkname) {
			return true, true
		}
		if header.Size > 0 {
			entryBytes, err := io.ReadAll(io.LimitReader(reader, MaxPublicSourceArchiveBytes+1))
			if err != nil || int64(len(entryBytes)) != header.Size || (CredentialMatcher{}).Match(entryBytes) {
				return true, true
			}
		}
	}
}

func credentialJSONValue(decoder *json.Decoder, field string) bool {
	token, err := decoder.Token()
	if err != nil {
		return true
	}
	switch token := token.(type) {
	case json.Delim:
		for decoder.More() {
			name := ""
			if token == '{' {
				key, err := decoder.Token()
				if err != nil {
					return true
				}
				var ok bool
				name, ok = key.(string)
				if !ok || credentialText([]byte(name)) {
					return true
				}
			}
			if credentialJSONField.MatchString(field) && (token == '[' || !credentialSchemaMetadata.MatchString(name)) {
				name = field
			}
			if credentialJSONValue(decoder, name) {
				return true
			}
		}
		_, err := decoder.Token()
		return err != nil
	case string:
		if match := credentialJSONField.FindStringSubmatch(field); match != nil {
			if bytes.EqualFold([]byte(match[1]), []byte("authorization")) {
				return credentialText([]byte("authorization: " + token))
			}
			if token != "" && !credentialPlaceholderOnly.MatchString(token) {
				return true
			}
		}
		return credentialText(maskGoCredentialReferences([]byte(token)))
	case nil:
		return false
	default:
		return credentialJSONField.MatchString(field)
	}
}

func credentialText(content []byte) bool {
	if credentialFormats.Match(content) {
		return true
	}
	for {
		match := credentialField.FindSubmatchIndex(content)
		if match == nil {
			return false
		}
		withPrefix := match[2] >= 4 && bytes.EqualFold(content[match[2]-4:match[2]], []byte("with"))
		field := content[match[2]:match[3]]
		operator := content[match[4]:match[5]]
		content = content[match[1]:]
		if withPrefix {
			continue
		}
		value := content
		if bytes.EqualFold(field, []byte("authorization")) {
			scheme := credentialScheme.FindIndex(value)
			if scheme == nil {
				continue
			}
			value = value[scheme[1]:]
			if len(value) == 0 || credentialNonValue(credentialHeaderRef, value) {
				continue
			}
			return true
		}
		if len(value) == 0 || value[0] == 0 || (bytes.Equal(operator, []byte("=")) && value[0] == '=') {
			continue
		}
		if bytes.Equal(operator, []byte(":")) && (value[0] == '{' || value[0] == '[') {
			decoder := json.NewDecoder(bytes.NewReader(value))
			decoder.UseNumber()
			if credentialJSONValue(decoder, string(field)) || !credentialValueEnded(value[decoder.InputOffset():]) {
				return true
			}
			content = content[decoder.InputOffset():]
			continue
		}
		if credentialNonValue(credentialEmpty, value) || credentialNonValue(credentialRef, value) ||
			(bytes.Equal(operator, []byte(":")) && credentialNonValue(credentialNull, value)) ||
			(bytes.Equal(operator, []byte(":=")) && credentialNonValue(credentialIdentifier, value)) {
			continue
		}
		return true
	}
}

func (matcher CredentialMatcher) MatchString(content string) bool {
	return matcher.Match([]byte(content))
}

func credentialNonValue(pattern *regexp.Regexp, value []byte) bool {
	match := pattern.FindIndex(value)
	if match == nil {
		return false
	}
	return credentialValueEnded(value[match[1]:])
}

func credentialValueEnded(remainder []byte) bool {
	remainder = bytes.TrimLeft(remainder, " \t")
	if len(remainder) == 0 {
		return true
	}
	switch remainder[0] {
	case '\n', '\r', 0, ',', ';', '}', ']', ')':
		return true
	case '"', '\'':
		// A containing JSON string may close here, but another nonempty
		// literal must not be mistaken for that closing delimiter.
		remainder = bytes.TrimLeft(remainder[1:], " \t\r\n")
		return len(remainder) == 0 || remainder[0] == ',' || remainder[0] == '}' || remainder[0] == ']'
	}
	return false
}
