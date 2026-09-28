/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import "testing"

func TestRequesterFromUserInfo(t *testing.T) {
	person := &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example.test", Subject: "alice", Username: "alice", Groups: []string{"dev"}}
	requester := requesterFromUserInfo(person)
	if requester == nil || requester.Issuer != person.Issuer || requester.Subject != "alice" || requester.Username != "alice" || len(requester.Groups) != 1 {
		t.Fatalf("requester = %+v", requester)
	}
	requester.Groups[0] = "changed"
	if person.Groups[0] != "dev" {
		t.Fatal("the requester must not alias the caller's groups")
	}
	for name, ui := range map[string]*UserInfo{
		"nil":             nil,
		"service account": {AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:ns:sa"},
		"no subject":      {AuthType: AuthTypeOIDC, Issuer: "https://issuer.example.test"},
		"no issuer":       {AuthType: AuthTypeContextToken, Subject: "alice"},
	} {
		if requesterFromUserInfo(ui) != nil {
			t.Fatalf("%s must yield no requester", name)
		}
	}
	if requesterFromUserInfo(&UserInfo{AuthType: AuthTypeContextToken, Issuer: "https://issuer.example.test", Subject: "bob"}) == nil {
		t.Fatal("a context-token caller is a person")
	}
}
