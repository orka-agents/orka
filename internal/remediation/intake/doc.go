// Package intake normalizes caller-supplied JSON problem reports without
// accessing repositories, models, or the network. It selects title, problem,
// description, summary, repository, versions, and restricted. Other fields are
// omitted. Unspecified restriction defaults to true.
//
// SourceDigest covers exact input bytes, including omitted fields. Sections
// retain JSON pointers into the original report. Repository/version claims
// remain unverified. Limits are enforced before and after JSON encoding; input
// is never silently truncated.
//
// HTML is converted to text without executing or fetching anything. Obvious
// credentials and administrative text are scrubbed as best-effort minimization,
// not a complete disclosure boundary. Successful parsing does not authorize
// model submission or publication, and content remains untrusted.
package intake
