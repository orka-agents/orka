/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package oversight

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// ErrQuery marks a search query the grammar refuses.
var ErrQuery = errors.New("invalid oversight query")

// Query limits.
const (
	MaxQueryBytes  = 1 << 10
	MaxQueryTerms  = 16
	MaxQueryValues = 16
)

// queryFacets maps a search key to the facet kind it matches.
var queryFacets = map[string]string{
	"concern":           oversightv1.FeatureConcern,
	"taint":             oversightv1.FeatureTaint,
	"bin":               oversightv1.FeatureBinary,
	"path":              oversightv1.FeaturePath,
	"dest":              oversightv1.FeatureDest,
	"domain":            oversightv1.FeatureDestDomain,
	"dest_domain":       oversightv1.FeatureDestDomain,
	"effect":            oversightv1.FeatureEffect,
	"bound":             FacetBound,
	"charter":           FacetCharter,
	"signal":            FacetSignal,
	"producer":          FacetProducer,
	"identity":          FacetWorkloadIdentity,
	"workload_identity": FacetWorkloadIdentity,
}

// queryFields are the keys matched against decision fields.
var queryFields = []string{"tool", GroupByTask, GroupByAgent, "run", "disposition", "feedback", "minScore", "wouldPause", "since", "sig"}

// QueryKeys lists every key the grammar accepts, for clients and errors.
func QueryKeys() []string {
	keys := slices.Clone(queryFields)
	for key := range queryFacets {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// ParseQuery reads a search query into a filter over the namespace's
// decisions. Terms are "key:value", where a value may list alternatives with
// "|" or "," and use "*" as a wildcard; bare words and "quoted phrases" are
// free text matched against each decision's redacted search text. Every term
// must match. A word whose prefix is not a known key is free text, so URLs and
// "host:port" values can be searched as typed. now anchors "since:".
//
//nolint:gocyclo // One switch over the grammar's keys.
func ParseQuery(q string, now time.Time) (storagev1.OversightDecisionFilter, []oversightv1.QueryTerm, error) {
	var filter storagev1.OversightDecisionFilter
	if len(q) > MaxQueryBytes {
		return filter, nil, fmt.Errorf("%w: longer than %d bytes", ErrQuery, MaxQueryBytes)
	}
	tokens, err := tokenize(q)
	if err != nil {
		return filter, nil, err
	}
	if len(tokens) > MaxQueryTerms {
		return filter, nil, fmt.Errorf("%w: more than %d terms", ErrQuery, MaxQueryTerms)
	}
	var terms []oversightv1.QueryTerm
	for _, token := range tokens {
		key, value, keyed := strings.Cut(token.text, ":")
		if token.quoted || !keyed || !knownKey(key) {
			text := strings.TrimSpace(token.text)
			if text == "" {
				continue
			}
			filter.Text = append(filter.Text, text)
			terms = append(terms, oversightv1.QueryTerm{Key: "text", Value: text})
			continue
		}
		if value == "" {
			return filter, nil, fmt.Errorf("%w: %s: needs a value", ErrQuery, key)
		}
		alternatives := splitAlternatives(value)
		if len(alternatives) > MaxQueryValues {
			return filter, nil, fmt.Errorf("%w: %s: more than %d values", ErrQuery, key, MaxQueryValues)
		}
		terms = append(terms, oversightv1.QueryTerm{Key: key, Value: value})
		if kind, ok := queryFacets[key]; ok {
			matches := make([]storagev1.OversightFacetMatch, 0, len(alternatives))
			for _, alternative := range alternatives {
				matches = append(matches, storagev1.OversightFacetMatch{
					Kind: kind, Value: alternative, Glob: strings.Contains(alternative, "*"),
				})
			}
			if len(matches) == 1 {
				filter.Facets = append(filter.Facets, matches[0])
				continue
			}
			if len(filter.AnyFacets) > 0 {
				return filter, nil, fmt.Errorf("%w: only one facet key may list alternatives", ErrQuery)
			}
			filter.AnyFacets = matches
			continue
		}
		switch key {
		case "tool":
			filter.ToolNames = append(filter.ToolNames, alternatives...)
		case GroupByAgent:
			filter.AgentNames = append(filter.AgentNames, alternatives...)
		case "run":
			if len(alternatives) != 1 {
				return filter, nil, fmt.Errorf("%w: run: takes one value", ErrQuery)
			}
			filter.RunID = alternatives[0]
		case GroupByTask:
			for _, alternative := range alternatives {
				if prefix, ok := strings.CutSuffix(alternative, "*"); ok && !strings.Contains(prefix, "*") && len(alternatives) == 1 {
					filter.TaskPrefix = prefix
				} else if strings.Contains(alternative, "*") {
					return filter, nil, fmt.Errorf("%w: task: takes a name, names, or one name prefix ending in *", ErrQuery)
				} else {
					filter.TaskNames = append(filter.TaskNames, alternative)
				}
			}
		case "disposition":
			for _, alternative := range alternatives {
				if !slices.Contains(oversightv1.Dispositions, oversightv1.Disposition(alternative)) {
					return filter, nil, fmt.Errorf("%w: disposition: %q is not a disposition", ErrQuery, alternative)
				}
			}
			filter.Dispositions = append(filter.Dispositions, alternatives...)
		case "feedback":
			if len(alternatives) != 1 {
				return filter, nil, fmt.Errorf("%w: feedback: takes one value", ErrQuery)
			}
			switch alternatives[0] {
			case storagev1.OversightFeedbackUnreviewed, storagev1.OversightFeedbackReviewed,
				string(oversightv1.FeedbackRealProblem), string(oversightv1.FeedbackFalseAlarm), string(oversightv1.FeedbackUnclear):
				filter.Feedback = alternatives[0]
			default:
				return filter, nil, fmt.Errorf("%w: feedback: is unreviewed, reviewed, real_problem, false_alarm or unclear", ErrQuery)
			}
		case "minScore":
			score, err := strconv.ParseFloat(value, 64)
			if err != nil || score < 0 || score > 1 {
				return filter, nil, fmt.Errorf("%w: minScore: is a number from 0 to 1", ErrQuery)
			}
			filter.MinScore = &score
		case "wouldPause":
			paused, err := strconv.ParseBool(value)
			if err != nil {
				return filter, nil, fmt.Errorf("%w: wouldPause: is true or false", ErrQuery)
			}
			filter.WouldHavePaused = &paused
		case "since":
			since, err := ParseWindow(value)
			if err != nil {
				return filter, nil, err
			}
			filter.ObservedSince = now.Add(-since)
		case "sig":
			filter.SignatureHash = value
		}
	}
	return filter, terms, nil
}

func knownKey(key string) bool {
	_, facet := queryFacets[key]
	return facet || slices.Contains(queryFields, key)
}

func splitAlternatives(value string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '|' || r == ',' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

type queryToken struct {
	text   string
	quoted bool
}

// tokenize splits on whitespace; a double-quoted phrase is one free-text
// token, and key:"quoted value" keeps the key.
func tokenize(q string) ([]queryToken, error) {
	var tokens []queryToken
	var current strings.Builder
	inQuote, quotedWhole, started := false, false, false
	flush := func() {
		if started {
			tokens = append(tokens, queryToken{text: current.String(), quoted: quotedWhole})
		}
		current.Reset()
		inQuote, quotedWhole, started = false, false, false
	}
	for _, r := range q {
		switch {
		case r == '"':
			if !inQuote && !started {
				quotedWhole = true
			}
			inQuote = !inQuote
			started = true
		case !inQuote && (r == ' ' || r == '\t' || r == '\n'):
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("%w: unterminated quote", ErrQuery)
	}
	flush()
	return tokens, nil
}

// ParseWindow reads a duration such as "90m", "24h" or "7d", up to 90 days.
func ParseWindow(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	var window time.Duration
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%w: window %q", ErrQuery, value)
		}
		window = time.Duration(n) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("%w: window %q", ErrQuery, value)
		}
		window = parsed
	}
	if window <= 0 || window > 90*24*time.Hour {
		return 0, fmt.Errorf("%w: window must be positive and at most 90d", ErrQuery)
	}
	return window, nil
}
