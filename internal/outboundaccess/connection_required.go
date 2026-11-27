package outboundaccess

import (
	"cmp"
	"fmt"
	"strings"
)

type ConnectionRequiredKind string

const (
	ConnectionNotLinked     ConnectionRequiredKind = "NotLinked"
	ConnectionExpired       ConnectionRequiredKind = "Expired"
	ConnectionNotReady      ConnectionRequiredKind = "NotReady"
	ConnectionNoCredential  ConnectionRequiredKind = "NoCredential"
	ConnectionAmbiguous     ConnectionRequiredKind = "Ambiguous"
	ConnectionNotBound      ConnectionRequiredKind = "NotBound"
	ConnectionRevoked       ConnectionRequiredKind = "Revoked"
	ConnectionClientChanged ConnectionRequiredKind = "ClientChanged"
	ConnectionDeleting      ConnectionRequiredKind = "Deleting"
	ConnectionChanged       ConnectionRequiredKind = "Changed"
	ConnectionRelinked      ConnectionRequiredKind = "Relinked"
)

type ConnectionRequiredError struct {
	Kind        ConnectionRequiredKind
	Provider    string
	DisplayName string
	Namespace   string
	State       string
	Links       int
}

func (e *ConnectionRequiredError) Error() string {
	name := strings.TrimSpace(e.DisplayName)
	if name == "" {
		name = strings.TrimSpace(e.Provider)
	}
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
