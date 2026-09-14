package execx

import (
	"strings"
	"testing"
)

const (
	urlUnsafe  = "/ \t\n\f\r\"'<>{}|\\"
	fuzzScheme = "https://"
	fuzzPath   = "/x/y.git"
	redactMark = "***@"
)

func FuzzRedactCredentialURL(f *testing.F) {
	f.Add("fatal: could not read from ", "teyhouse", "ghp_SUPERSECRET", "github.com", " for details")
	f.Add("remote: ", "git", "hunter2", "example.com:22", "")
	f.Add("", "user", "p@ss@rd", "github.com", "")
	f.Add("fatal: unable to access ", "u", "sekrit", "example.com", ",mail 'foo@bar.com'")

	f.Fuzz(func(t *testing.T, prefix, user, secret, host, suffix string) {
		if strings.ContainsAny(user, urlUnsafe) || strings.ContainsAny(secret, urlUnsafe) {
			return
		}
		if strings.Contains(host, "@") || strings.Contains(host, "://") {
			return
		}
		if strings.Contains(prefix, "://") || strings.Contains(suffix, "://") {
			return
		}

		line := prefix + fuzzScheme + user + ":" + secret + "@" + host + fuzzPath + suffix
		want := prefix + fuzzScheme + redactMark + host + fuzzPath + suffix
		got := RedactString(line)

		if got != want {
			t.Errorf("wrong redaction\n  in: %q\n got: %q\nwant: %q", line, got, want)
		}
		if secret != "" && !strings.Contains(want, secret) && strings.Contains(got, secret) {
			t.Errorf("secret survived redaction\n in: %q\nout: %q\nsecret: %q", line, got, secret)
		}
	})
}

func FuzzRedactInvariants(f *testing.F) {
	f.Add("fatal: could not read from https://teyhouse:ghp_SECRET@github.com/x/y.git")
	f.Add("fatal: repository 'https://github.com/x/y.git' not found")
	f.Add("see https://github.com/x/y/issues/1@2 for details")
	f.Add("fatal: unable to access 'https://example.com',mail 'foo@bar.com'")
	f.Add("ssh://git:hunter2@example.com:22/repo")
	f.Add("")

	f.Fuzz(func(t *testing.T, in string) {
		got := RedactString(in)

		if again := RedactString(got); again != got {
			t.Errorf("not idempotent\n  in: %q\n out: %q\nagain: %q", in, got, again)
		}
		if b := string(Redact([]byte(in))); b != got {
			t.Errorf("Redact and RedactString disagree\n in: %q\nbytes: %q\nstring: %q", in, b, got)
		}
		if !strings.Contains(in, "@") && got != in {
			t.Errorf("input with no credential was modified\n in: %q\nout: %q", in, got)
		}
		if strings.Count(got, "***@") > strings.Count(in, "@") {
			t.Errorf("redaction invented credentials\n in: %q\nout: %q", in, got)
		}
	})
}
