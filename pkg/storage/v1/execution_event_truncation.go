package v1

// ExecutionEventTruncation records whether public event payload fields were truncated.
// Lengths are measured after redaction so metadata cannot expose raw secret sizes.
type ExecutionEventTruncation struct {
	SummaryTruncated         bool `json:"summaryTruncated,omitempty"`
	SummaryOriginalChars     int  `json:"summaryOriginalChars,omitempty"`
	ContentTextTruncated     bool `json:"contentTextTruncated,omitempty"`
	ContentTextOriginalChars int  `json:"contentTextOriginalChars,omitempty"`
	ContentJSONTruncated     bool `json:"contentJsonTruncated,omitempty"`
	ContentJSONOriginalBytes int  `json:"contentJsonOriginalBytes,omitempty"`
}

// Empty reports whether no truncation metadata was set.
func (m ExecutionEventTruncation) Empty() bool {
	return !m.SummaryTruncated && m.SummaryOriginalChars == 0 &&
		!m.ContentTextTruncated && m.ContentTextOriginalChars == 0 &&
		!m.ContentJSONTruncated && m.ContentJSONOriginalBytes == 0
}
