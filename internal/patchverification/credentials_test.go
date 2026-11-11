package patchverification

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCredentialMatcher(test *testing.T) {
	cases := []struct {
		name, content string
		want          bool
	}{
		{"empty document", "", false},
		{"identifiers", "var password string\nfunc getPassword() {}\n", false},
		{"schema", `{"password":{"type":"string"}}`, false},
		{"schema metadata", `{"password":{"type":"string","description":"Password input","minLength":8,"writeOnly":true}}`, false},
		{"schema empty default", `{"password":{"type":"string","default":""}}`, false},
		{"schema placeholder default", `{"password":{"type":"string","default":"${PASSWORD}"}}`, false},
		{"multiline schema", "{\n\"password\":\n{\"type\":\"string\"}\n}", false},
		{"schema array", `{"password":[]}`, false},
		{"empty strings", `{"password":"","client_secret":""}`, false},
		{"empty single quote", `password = ''`, false},
		{"empty raw string", "password := ``", false},
		{"null", `{"password":null}`, false},
		{"nil", "password := nil", false},
		{"identifier reference", "password := candidate", false},
		{"qualified reference", "password := config.Password", false},
		{"go environment", `password := os.Getenv("PASSWORD")`, false},
		{"go environment lookup", `password, ok := os.LookupEnv("PASSWORD")`, false},
		{"python environment", `password = os.environ["PASSWORD"]`, false},
		{"python environment lookup", `password = os.getenv('PASSWORD')`, false},
		{"python environment get", `password = os.environ.get("PASSWORD")`, false},
		{"javascript environment", "clientSecret = process.env.CLIENT_SECRET;", false},
		{"javascript bracket environment", `apiKey = process.env["API_KEY"]`, false},
		{"C environment", `password = getenv("PASSWORD");`, false},
		{"java environment", `password = System.getenv("PASSWORD");`, false},
		{"rust environment", `password = std::env::var("PASSWORD")`, false},
		{"shell environment", "PASSWORD=$PASSWORD", false},
		{"shell braced environment", `password="${PASSWORD}"`, false},
		{"single quoted placeholder", `password='${PASSWORD}'`, false},
		{"windows environment", "password=%PASSWORD%", false},
		{"authorization environment", "Authorization: Bearer ${ACCESS_TOKEN}", false},
		{"JSON authorization environment", `{"Authorization":"Bearer ${ACCESS_TOKEN}"}`, false},
		{"short literal", `password = "x"`, true},
		{"literal", `password = "synthetic-fixture-only"`, true},
		{"Go literal", `password := "synthetic-fixture-only"`, true},
		{"Go raw literal", "password := `synthetic-fixture-only`", true},
		{"passphrase", `password = "synthetic fixture only"`, true},
		{"whitespace literal", `password = " "`, true},
		{"bare literal", "password=synthetic-fixture-only", true},
		{"ambiguous bare identifier", "password=candidate", true},
		{"quoted null is a literal", `password="null"`, true},
		{"dotenv null is a literal", "password=null", true},
		{"dotenv nil is a literal", "password=nil", true},
		{"dotenv None is a literal", "password=None", true},
		{"dotenv undefined is a literal", "password=undefined", true},
		{"dotenv braced literal", "password={synthetic-fixture-only}", true},
		{"dotenv bracketed literal", "password=[synthetic-fixture-only]", true},
		{"quoted identifier is a literal", `password="config.Password"`, true},
		{"quoted call is a literal", `password="os.Getenv('PASSWORD')"`, true},
		{"escaped quote literal", `password="\"synthetic-fixture-only\""`, true},
		{"multiline literal", "password =\n\"synthetic-fixture-only\"", true},
		{"JSON literal", `{"password":"synthetic-fixture-only"}`, true},
		{"prefixed JSON literal", `{"DB_PASSWORD":"synthetic-fixture-only"}`, true},
		{"api key", `api_key: synthetic-fixture-only`, true},
		{"access token", `accessToken = "synthetic-fixture-only"`, true},
		{"refresh token", `refresh-token: synthetic-fixture-only`, true},
		{"client secret", `client_secret = "synthetic-fixture-only"`, true},
		{"authorization bearer", "Authorization: Bearer synthetic-fixture-only", true},
		{"authorization basic", "Authorization: Basic c3ludGhldGljOm9ubHk=", true},
		{"authorization lookup literal", `{"Authorization":"Bearer os.Getenv('TOKEN')"}`, true},
		{"JSON lookup literal", `{"password":"os.Getenv('PASSWORD')"}`, true},
		{"shell default", `password="${PASSWORD:-synthetic-fixture-only}"`, true},
		{"shell suffix", `password="${PASSWORD}-synthetic-fixture-only"`, true},
		{"lookup default", `password = os.getenv("PASSWORD", "synthetic-fixture-only")`, true},
		{"lookup alternative", `password = os.getenv("PASSWORD") or "synthetic-fixture-only"`, true},
		{"lookup suffix", `password := os.Getenv("PASSWORD") + "synthetic-fixture-only"`, true},
		{"placeholder concatenation", `password = "${PASSWORD}" "synthetic-fixture-only"`, true},
		{"empty concatenation", `password = "" "synthetic-fixture-only"`, true},
		{"escaped newline suffix", `password=$PASSWORD\nsynthetic-fixture-only`, true},
		{"escaped carriage return suffix", `password=$PASSWORD\rsynthetic-fixture-only`, true},
		{"schema followed by literal", "{\"password\":{\"type\":\"string\"}}\npassword=\"synthetic-fixture-only\"", true},
		{"reference followed by literal", "password=os.Getenv(\"PASSWORD\"); api_key=\"synthetic-fixture-only\"", true},
		{"nested literal", `{"password":{"password":"synthetic-fixture-only"}}`, true},
		{"schema default", `{"password":{"type":"string","default":"synthetic-fixture-only"}}`, true},
		{"schema constant", `{"password":{"type":"string","const":"synthetic-fixture-only"}}`, true},
		{"schema enum", `{"password":{"type":"string","enum":["synthetic-fixture-only"]}}`, true},
		{"schema example", `{"password":{"type":"string","examples":["synthetic-fixture-only"]}}`, true},
		{"nested value", `{"password":{"value":"synthetic-fixture-only"}}`, true},
		{"array value", `{"password":["synthetic-fixture-only"]}`, true},
		{"private key", "-----BEGIN " + "PRIVATE KEY-----\nsynthetic-fixture-only", true},
		{"RSA private key", "-----BEGIN RSA " + "PRIVATE KEY-----\nsynthetic-fixture-only", true},
		{"EC private key", "-----BEGIN EC " + "PRIVATE KEY-----\nsynthetic-fixture-only", true},
		{"OpenSSH private key", "-----BEGIN OPENSSH " + "PRIVATE KEY-----\nsynthetic-fixture-only", true},
		{"personal token", "gh" + "p_" + strings.Repeat("a", 20), true},
		{"OAuth token", "gh" + "o_" + strings.Repeat("a", 20), true},
		{"user token", "gh" + "u_" + strings.Repeat("a", 20), true},
		{"server token", "gh" + "s_" + strings.Repeat("a", 20), true},
		{"refresh token format", "gh" + "r_" + strings.Repeat("a", 20), true},
		{"fine grained token", "github" + "_pat_" + strings.Repeat("a", 20), true},
		{"API token format", "s" + "k-" + strings.Repeat("a", 20), true},
		{"project API token", "s" + "k-proj-" + strings.Repeat("a", 20), true},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			encoded, err := json.Marshal(map[string]string{"text": scenario.content})
			if err != nil {
				test.Fatal(err)
			}
			for index, content := range []string{scenario.content, string(encoded), "\x00" + scenario.content + "\x00"} {
				matcher := CredentialMatcher{}
				if matcher.MatchString(content) != scenario.want || matcher.Match([]byte(content)) != scenario.want {
					test.Fatalf("credential screening disagrees with the expected classification for encoding %d", index)
				}
			}
		})
	}
}
