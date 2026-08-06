package redact

import (
	"regexp"
	"strings"
	"testing"
)

// A custom rule is written by hand in a config file, and the shapes people
// reach for differ in where they put the group. Whichever they choose, the
// secret must not survive — a rule that half-fires is worse than no rule,
// because it reads as protection.
func TestACustomRuleNeverLeavesTheSecretBehind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		in      string
		secret  string
	}{
		{"no group at all", `\bcorp-[a-z0-9]{8}\b`, "deploy corp-ab12cd34 now", "corp-ab12cd34"},
		{"the group is the value", `api_key=(\S+)`, "curl api_key=s3cret123 x", "s3cret123"},
		{"the group is the label", `(--password )\S+`, "mysql --password hunter2", "hunter2"},
		{"value group, end of string", `token:(\S+)`, "auth token:abc123", "abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New([]Rule{{
				Name:   tc.name,
				Re:     regexp.MustCompile(tc.pattern),
				Action: ActionRedact,
			}})
			out, action, _ := r.Apply(tc.in)
			if action != ActionRedact {
				t.Fatalf("rule did not fire on %q", tc.in)
			}
			if strings.Contains(out, tc.secret) {
				t.Errorf("the secret survived:\n  in:  %q\n  out: %q", tc.in, out)
			}
			if !strings.Contains(out, Placeholder) {
				t.Errorf("no marker in %q", out)
			}
		})
	}
}

// The label is worth keeping where the rule makes one identifiable: seeing that
// you ran mysql --password is the point of keeping the command at all.
func TestALabelGroupIsKept(t *testing.T) {
	r := New([]Rule{{
		Name: "password flag", Re: regexp.MustCompile(`(--password )\S+`), Action: ActionRedact,
	}})
	out, _, _ := r.Apply("mysql --password hunter2")
	if !strings.Contains(out, "--password ") {
		t.Errorf("the label was thrown away with the secret: %q", out)
	}
}
