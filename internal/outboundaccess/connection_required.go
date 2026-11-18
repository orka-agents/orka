package outboundaccess

import (
	"cmp"
	"fmt"
	"strings"
)

// ConnectionRequiredKind names why a requester's linked account cannot serve
// a call.
type ConnectionRequiredKind string

const (
	// ConnectionNotLinked: the requester has no link to the provider, or it
	// is being removed.
	ConnectionNotLinked ConnectionRequiredKind = "NotLinked"
	// ConnectionExpired: the link's grant has expired.
	ConnectionExpired ConnectionRequiredKind = "Expired"
	// ConnectionNotReady: the link exists but is not Ready, for example
	// Revoked or still Pending consent.
	ConnectionNotReady ConnectionRequiredKind = "NotReady"
	// ConnectionNoCredential: the link is Ready but Orka holds no credential
	// for it.
	ConnectionNoCredential ConnectionRequiredKind = "NoCredential"
	// ConnectionAmbiguous: the requester has several links to the provider.
	ConnectionAmbiguous ConnectionRequiredKind = "Ambiguous"
	// ConnectionNotBound: the link is Ready but was not bound to the Task,
	// because its consent or granted scopes did not match the provider.
	ConnectionNotBound ConnectionRequiredKind = "NotBound"
	// ConnectionRevoked: the provider revoked the link's grant.
	ConnectionRevoked ConnectionRequiredKind = "Revoked"
	// ConnectionClientChanged: the provider's OAuth client changed since the
	// link's token was issued.
	ConnectionClientChanged ConnectionRequiredKind = "ClientChanged"
	// ConnectionDeleting: the link is being deleted.
	ConnectionDeleting ConnectionRequiredKind = "Deleting"
	// ConnectionChanged: the link changed after the Task started; a new run
	// binds the current one.
	ConnectionChanged ConnectionRequiredKind = "Changed"
	// ConnectionRelinked: the person linked again after the Task started; a
	// new run binds the new grant.
	ConnectionRelinked ConnectionRequiredKind = "Relinked"
)

// ConnectionRequiredError refuses a call that the person who asked fixes by
// linking or re-linking an account, or that a new run of the Task fixes.
// Nothing was sent. Its message is built only from the provider's name,
// display name, and namespace and the link's state, never from a credential
// or an upstream response, so the agent can relay it and the controller can
// log it.
type ConnectionRequiredError struct {
	Kind        ConnectionRequiredKind
	Provider    string
	DisplayName string
	Namespace   string
	// State is the link's state for ConnectionNotReady.
	State string
	// Links counts the requester's links for ConnectionAmbiguous.
	Links int
}

// Error tells the agent what the person must do, for the agent to pass on.
func (e *ConnectionRequiredError) Error() string {
	name := strings.TrimSpace(e.DisplayName)
	if name == "" {
		name = strings.TrimSpace(e.Provider)
	}
	// "Azure is not linked" and "The Azure link", or generic phrases when the
	// provider is unknown.
	notLinked, theLink := "No account is linked for the person who asked.", "The linked account of the person who asked"
	if name != "" {
		notLinked, theLink = name+" is not linked for the person who asked.", "The "+name+" link for the person who asked"
	}
	link := "They need to link it again, then run this Task again."
	if provider := strings.TrimSpace(e.Provider); provider != "" {
		command := "orka connect " + provider
		if namespace := strings.TrimSpace(e.Namespace); namespace != "" {
			command += " -n " + namespace
		}
		link = fmt.Sprintf("They need to run `%s`, then run this Task again.", command)
	}
	switch e.Kind {
	case ConnectionNotLinked:
		return notLinked + " " + link
	case ConnectionExpired:
		return theLink + " has expired. " + link
	case ConnectionNotReady:
		state := ""
		if strings.TrimSpace(e.State) != "" {
			state = " (" + strings.TrimSpace(e.State) + ")"
		}
		return theLink + " is not ready" + state + ". " + link
	case ConnectionNoCredential:
		return theLink + " holds no credential. " + link
	case ConnectionAmbiguous:
		// The commands name the Task's namespace when it is known, so the
		// person lists and deletes links there, not in their CLI default.
		scope := ""
		if namespace := strings.TrimSpace(e.Namespace); namespace != "" {
			scope = " -n " + namespace
		}
		return fmt.Sprintf("The person who asked has %d links to %s, so Orka uses none. "+
			"They need to keep one (`orka connection list%s`, then `orka connection delete <name>%s`), then run this Task again.",
			e.Links, cmp.Or(name, "this provider"), scope, scope)
	case ConnectionNotBound:
		return theLink + " does not match what the provider requires now (its consent or granted scopes differ). " + link
	case ConnectionRevoked:
		return theLink + " was revoked by the provider. " + link
	case ConnectionClientChanged:
		return theLink + " was issued before the provider's OAuth client changed. " + link
	case ConnectionDeleting:
		return theLink + " is being deleted. " + link
	case ConnectionChanged:
		return theLink + " changed after this Task started. Run this Task again."
	case ConnectionRelinked:
		return theLink + " was re-linked after this Task started. Run this Task again."
	default:
		return theLink + " cannot be used. " + link
	}
}
