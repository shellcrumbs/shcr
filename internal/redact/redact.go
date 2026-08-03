// Package redact strips credentials out of command text.
//
// It runs in the hook-side sender, before the event reaches the socket — which
// means before the spool file too. Redacting in the daemon alone would still
// leave plaintext secrets on disk whenever the daemon happened to be down, and
// "the daemon was restarting" is not an acceptable reason for a secret to be
// written to a file.
package redact

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Action says what to do with a command that matched.
type Action int

const (
	// ActionNone means nothing matched.
	ActionNone Action = iota
	// ActionRedact records the command with the secret replaced.
	ActionRedact
	// ActionSkip does not record the command at all. Reserved for material
	// where even the surrounding text is not worth keeping.
	ActionSkip
)

func (a Action) String() string {
	switch a {
	case ActionRedact:
		return "redact"
	case ActionSkip:
		return "skip"
	}
	return "none"
}

const Placeholder = "[REDACTED]"

// Rule matches one shape of secret. Group selects which capture group is the
// secret itself; 0 means the whole match. Keeping the label visible is
// deliberate — `--password=[REDACTED]` still tells you what the command did.
type Rule struct {
	Name   string
	Re     *regexp.Regexp
	Action Action
	Group  int
}

// builtinRules covers the credential shapes that actually turn up in shell
// history. Each is anchored on its distinctive prefix rather than on entropy,
// because entropy heuristics flag git hashes and base64 payloads constantly and
// a redactor people switch off protects nobody.
var builtinRules = []Rule{
	{
		Name:   "aws-access-key-id",
		Re:     regexp.MustCompile(`\b((?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16})\b`),
		Action: ActionRedact, Group: 1,
	},
	{
		Name:   "assignment-of-secret-env-var",
		Re:     regexp.MustCompile(`(?i)\b((?:AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|GITHUB_TOKEN|GH_TOKEN|NPM_TOKEN|DOCKER_PASSWORD|[A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY))\s*=\s*)(?:"([^"]*)"|'([^']*)'|([^\s;|&]+))`),
		Action: ActionRedact, Group: 0,
	},
	{
		Name:   "github-token",
		Re:     regexp.MustCompile(`\b((?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,})\b`),
		Action: ActionRedact, Group: 1,
	},
	{
		Name:   "gitlab-token",
		Re:     regexp.MustCompile(`\b(glpat-[A-Za-z0-9_\-]{16,})\b`),
		Action: ActionRedact, Group: 1,
	},
	{
		Name:   "slack-token",
		Re:     regexp.MustCompile(`\b(xox[abprs]-[A-Za-z0-9\-]{10,})\b`),
		Action: ActionRedact, Group: 1,
	},
	{
		Name:   "sk-style-api-key",
		Re:     regexp.MustCompile(`\b(sk-[A-Za-z0-9_\-]{16,})\b`),
		Action: ActionRedact, Group: 1,
	},
	{
		Name:   "bearer-token",
		Re:     regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._\-~+/]{12,}=*)`),
		Action: ActionRedact, Group: 2,
	},
	{
		Name:   "basic-auth-header",
		Re:     regexp.MustCompile(`(?i)(basic\s+)([A-Za-z0-9+/]{12,}=*)`),
		Action: ActionRedact, Group: 2,
	},
	{
		Name:   "password-in-connection-uri",
		Re:     regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@]+:)([^\s@/]+)(@)`),
		Action: ActionRedact, Group: 2,
	},
	{
		Name:   "password-flag",
		Re:     regexp.MustCompile(`(?i)(--?(?:password|passwd|token|api-?key|secret)[= ])(?:"([^"]*)"|'([^']*)'|([^\s;|&]+))`),
		Action: ActionRedact, Group: 0,
	},
	{
		// Anchored on the client that actually uses this form. A bare `-p`
		// followed by text is far too common to redact on sight: it would mangle
		// `docker run -p8080:80`, `ssh -p2222`, `tar -pxzf` and `ffmpeg -preset`,
		// and a redactor that corrupts everyday commands is one people turn off.
		Name:   "mysql-style-inline-password",
		Re:     regexp.MustCompile(`(?i)\b(?:mysql|mysqldump|mysqladmin|mariadb|mariadb-dump)\b[^;|&]*?(\s-p)([^\s;|&]{3,})`),
		Action: ActionRedact, Group: 2,
	},
	{
		Name:   "jwt",
		Re:     regexp.MustCompile(`\b(eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]+)\b`),
		Action: ActionRedact, Group: 1,
	},
	{
		// Key material pasted into a terminal is not worth keeping in any form —
		// the surrounding text carries no useful history either.
		Name:   "private-key-block",
		Re:     regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`),
		Action: ActionSkip,
	},
}

type Redactor struct {
	rules []Rule
}

// New returns a redactor with the built-in rules plus any user rules.
func New(extra []Rule) *Redactor {
	rules := make([]Rule, 0, len(builtinRules)+len(extra))
	rules = append(rules, builtinRules...)
	rules = append(rules, extra...)
	return &Redactor{rules: rules}
}

// Apply returns the text to record, what happened, and which rules fired.
// When the action is ActionSkip the returned text is empty and the caller must
// record nothing at all.
func (r *Redactor) Apply(command string) (string, Action, []string) {
	var fired []string

	// Skip wins outright, so check for it before spending effort on rewriting.
	for _, rule := range r.rules {
		if rule.Action == ActionSkip && rule.Re.MatchString(command) {
			return "", ActionSkip, []string{rule.Name}
		}
	}

	out := command
	for _, rule := range r.rules {
		if rule.Action != ActionRedact {
			continue
		}
		replaced, n := replaceGroup(rule, out)
		if n > 0 {
			out = replaced
			fired = append(fired, rule.Name)
		}
	}
	if len(fired) == 0 {
		return command, ActionNone, nil
	}
	return out, ActionRedact, fired
}

// replaceGroup rewrites the secret-bearing part of each match. Rules with
// Group 0 use whichever alternation group actually captured — that is how a
// quoted, single-quoted and bare value can share one pattern while the flag or
// variable name in front of them survives.
func replaceGroup(rule Rule, s string) (string, int) {
	matches := rule.Re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, 0
	}
	var b strings.Builder
	last := 0
	count := 0
	for _, m := range matches {
		start, end := secretSpan(rule, m)
		if start < 0 || end < 0 || start < last {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(Placeholder)
		last = end
		count++
	}
	if count == 0 {
		return s, 0
	}
	b.WriteString(s[last:])
	return b.String(), count
}

// secretSpan locates the bytes to blank out for one match.
func secretSpan(rule Rule, m []int) (int, int) {
	if rule.Group > 0 {
		i := 2 * rule.Group
		if i+1 < len(m) {
			return m[i], m[i+1]
		}
		return -1, -1
	}
	// Group 0: keep group 1 (the label) and blank whichever value group matched.
	if len(m) >= 4 && m[2] >= 0 {
		for g := 2; 2*g+1 < len(m); g++ {
			if m[2*g] >= 0 {
				return m[2*g], m[2*g+1]
			}
		}
		// Label captured but no value group matched: blank everything after it.
		return m[3], m[1]
	}
	return m[0], m[1]
}

// LoadRules reads user rules from a config file. Each line is
//
//	redact <regexp>     record the command with the match replaced
//	skip   <regexp>     do not record the command at all
//
// Blank lines and lines starting with # are ignored. A bad pattern is an error
// rather than a silent omission — a redaction rule that quietly does nothing is
// worse than no rule.
func LoadRules(path string) ([]Rule, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var rules []Rule
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		verb, pattern, ok := strings.Cut(text, " ")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected `redact <regexp>` or `skip <regexp>`", path, line)
		}
		pattern = strings.TrimSpace(pattern)
		var action Action
		switch strings.ToLower(verb) {
		case "redact":
			action = ActionRedact
		case "skip":
			action = ActionSkip
		default:
			return nil, fmt.Errorf("%s:%d: unknown action %q (want redact or skip)", path, line, verb)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		rules = append(rules, Rule{
			Name:   fmt.Sprintf("user:%s:%d", strings.ToLower(verb), line),
			Re:     re,
			Action: action,
		})
	}
	return rules, sc.Err()
}
