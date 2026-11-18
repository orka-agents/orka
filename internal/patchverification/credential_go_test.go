package patchverification

import "testing"

func TestCredentialMatcherGoReferencesAreNotValues(t *testing.T) {
	for _, source := range []string{
		"package demo\nfunc configure(info Options){ _ = Options{Password: info.Password} }\n",
		"package demo\nfunc configure(info Options){ _ = map[string]string{\"password\": info.Password} }\n",
		"package demo\nfunc configure(info Options, password string){ password = info.Password }\n",
		"package demo\nfunc configure(configuredPassword string){ var password = configuredPassword; _ = password }\n",
		`{"files":[{"path":"harness.txt","content":"package main\nfunc forward(value string){ var info struct{ Password string }; info.Password = value }\n"},{"path":"server.py","content":"replacements = {'util.NewTLSConfigWithPassword': 'makeTLS'}\nPREFIX = r'''package main\n'''\nTESTS = r'''\nfunc resolve(a, b string) string { return a }\nfunc f(v string){ authorizedValue := resolve(\"entry\", v); var o struct{ Password string }; o.Password = authorizedValue }\n'''\n"}]}`,
	} {
		if (CredentialMatcher{}).Match([]byte(source)) {
			t.Fatal("syntactically valid Go variable reference was treated as credential material")
		}
	}
}

func TestCredentialMatcherGoReferenceParsingDoesNotHideLiterals(t *testing.T) {
	for _, source := range []string{
		"package demo\nvar options = Options{Password: \"sensitive-value\"}\n",
		"package demo\nvar options = map[string]string{\"password\": \"sensitive-value\"}\n",
		"package demo\nfunc configure(){ password = \"sensitive-value\" }\n",
		"package demo\nvar password = \"sensitive-value\"\n",
		"package demo\nvar value = \"sensitive-value\"\nvar options = Options{Password: value}\n",
		"package demo\nfunc configure(){ value := \"sensitive-value\"; _ = Options{Password: value} }\n",
		"package demo\nfunc configure(value string){ value = \"sensitive-value\"; _ = Options{Password: value} }\n",
		"password: info.Password",
		"package demo\nvar options = Options{Password: info.Password}\nthis is not Go\npassword: sensitive-value",
	} {
		if !(CredentialMatcher{}).Match([]byte(source)) {
			t.Fatal("literal or ambiguous non-Go credential assignment was accepted")
		}
	}
}
