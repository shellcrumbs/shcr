package redact

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRedactsRealSecretShapes(t *testing.T) {
	r := New(nil)
	cases := []struct {
		name   string
		in     string
		secret string // must not survive
		keep   string // must survive, so the entry stays useful
	}{
		{"aws access key", "aws sts get-caller-identity --profile AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE", "get-caller-identity"},
		{"aws secret env", `AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY aws s3 ls`, "wJalrXUtnFEMI", "AWS_SECRET_ACCESS_KEY="},
		{"github pat", "gh auth login --with-token ghp_16CharsAtLeastxxxxxxxxxxxxxxxxxxxx", "ghp_16CharsAtLeast", "gh auth login"},
		{"gitlab pat", "glab auth login -t glpat-abcdefghij1234567890", "glpat-abcdefghij", "glab auth login"},
		{"slack token", "curl -d token=xoxb-123456789012-abcdefghijkl", "xoxb-123456789012", "curl"},
		{"openai style key", "export KEY=sk-abcdefghijklmnopqrstuvwxyz012345", "sk-abcdefghijklmnop", "export"},
		{"bearer header", `curl -H "Authorization: Bearer eyJhbGciOi.eyJzdWIiOi.SflKxwRJSM" https://api.example.com`, "SflKxwRJSM", "api.example.com"},
		{"basic auth", `curl -H "Authorization: Basic dXNlcjpwYXNzd29yZA==" https://x.test`, "dXNlcjpwYXNzd29yZA", "x.test"},
		{"postgres uri", "psql postgres://admin:sup3rs3cret@db.internal:5432/prod", "sup3rs3cret", "db.internal"},
		{"password flag", "mysqldump --password=hunter2 mydb", "hunter2", "mysqldump"},
		{"password flag spaced", "wget --password topsecret123 http://x.test", "topsecret123", "wget"},
		{"mysql -p inline", "mysql -uroot -pMyS3cretPass mydb", "MyS3cretPass", "mysql"},
		{"jwt", "curl -b eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.dBjftJeZ4CVPmB92K", "dBjftJeZ4CVPmB92K", "curl"},
		{"quoted secret", `docker login -u me --password "p@ss w0rd!"`, "p@ss w0rd!", "docker login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, action, fired := r.Apply(tc.in)
			if action != ActionRedact {
				t.Fatalf("action = %v, want redact (fired: %v)\n  got: %s", action, fired, got)
			}
			if strings.Contains(got, tc.secret) {
				t.Errorf("secret survived redaction:\n  in:  %s\n  out: %s", tc.in, got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Errorf("no placeholder in output: %s", got)
			}
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Errorf("redaction destroyed useful context %q:\n  out: %s", tc.keep, got)
			}
		})
	}
}

// A redactor that mangles ordinary commands gets turned off, and then it
// protects nothing. These must pass through untouched.
func TestLeavesOrdinaryCommandsAlone(t *testing.T) {
	r := New(nil)
	ordinary := []string{
		"git commit -m 'fix the thing'",
		"git log --oneline -20",
		"git checkout 4f8a9c2e1b3d5a7f9c1e3b5d7a9f1c3e5b7d9a1f",
		"npm run build:prod",
		"kubectl get pods -n production",
		"docker compose up -d",
		"ssh deploy@build-server",
		"curl https://api.example.com/v1/users",
		"echo 'this is not a secret at all'",
		"grep -rn password ./src",
		"ls -la /etc/ssl/private",
		"tar -xzf archive.tar.gz",
		"make -j8 all",
		"python3 -m pytest tests/",
	}
	for _, cmd := range ordinary {
		got, action, fired := r.Apply(cmd)
		if action != ActionNone {
			t.Errorf("false positive on %q\n  action: %v  rules: %v\n  got:    %s", cmd, action, fired, got)
		}
		if got != cmd {
			t.Errorf("command was altered:\n  want %q\n  got  %q", cmd, got)
		}
	}
}

func TestPrivateKeyBlockIsSkippedEntirely(t *testing.T) {
	r := New(nil)
	cmd := "echo '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n-----END OPENSSH PRIVATE KEY-----' > id_ed25519"
	got, action, fired := r.Apply(cmd)
	if action != ActionSkip {
		t.Fatalf("action = %v, want skip (fired %v)", action, fired)
	}
	if got != "" {
		t.Fatalf("a skipped command must yield no text, got %q", got)
	}
}

func TestMultipleSecretsInOneCommand(t *testing.T) {
	r := New(nil)
	cmd := "AWS_SECRET_ACCESS_KEY=abcd1234efgh5678 aws s3 cp s3://b/k . --password=hunter2"
	got, action, _ := r.Apply(cmd)
	if action != ActionRedact {
		t.Fatalf("action = %v", action)
	}
	for _, secret := range []string{"abcd1234efgh5678", "hunter2"} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived: %s", secret, got)
		}
	}
	if strings.Count(got, Placeholder) < 2 {
		t.Errorf("expected two placeholders, got: %s", got)
	}
}

func TestRepeatedSecretsAllReplaced(t *testing.T) {
	r := New(nil)
	cmd := "cmp <(curl -H 'Bearer aaaaaaaaaaaaaaaa') <(curl -H 'Bearer bbbbbbbbbbbbbbbb')"
	got, _, _ := r.Apply(cmd)
	if strings.Contains(got, "aaaaaaaaaaaaaaaa") || strings.Contains(got, "bbbbbbbbbbbbbbbb") {
		t.Fatalf("not every occurrence was redacted: %s", got)
	}
}

// Redaction must be a fixed point: applying it again changes nothing. The
// daemon re-runs it as a backstop, and that must not corrupt already-clean text.
func TestRedactionIsIdempotent(t *testing.T) {
	r := New(nil)
	for _, cmd := range []string{
		"mysqldump --password=hunter2 mydb",
		"psql postgres://admin:sup3rs3cret@db/prod",
		"aws s3 ls --profile AKIAIOSFODNN7EXAMPLE",
	} {
		once, _, _ := r.Apply(cmd)
		twice, _, _ := r.Apply(once)
		if once != twice {
			t.Errorf("second pass changed the text:\n  once:  %s\n  twice: %s", once, twice)
		}
	}
}

func TestUserRulesFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "redact.conf")
	os.WriteFile(path, []byte(`
# internal tooling
redact \bcorp-[a-z0-9]{8}\b
skip   ^vault write
`), 0o600)

	rules, err := LoadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("loaded %d rules, want 2", len(rules))
	}
	r := New(rules)

	got, action, _ := r.Apply("deploy --key corp-a1b2c3d4 --env prod")
	if action != ActionRedact || strings.Contains(got, "corp-a1b2c3d4") {
		t.Errorf("user redact rule did not fire: %s (%v)", got, action)
	}
	if !strings.Contains(got, "--env prod") {
		t.Errorf("user rule destroyed context: %s", got)
	}

	if _, action, _ := r.Apply("vault write secret/foo value=bar"); action != ActionSkip {
		t.Errorf("user skip rule did not fire, action = %v", action)
	}
}

func TestBadConfigIsAnErrorNotSilence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "redact.conf")

	os.WriteFile(path, []byte("redact [unclosed\n"), 0o600)
	if _, err := LoadRules(path); err == nil {
		t.Error("an invalid regexp must be reported, not skipped")
	}

	os.WriteFile(path, []byte("obliterate foo\n"), 0o600)
	if _, err := LoadRules(path); err == nil {
		t.Error("an unknown action must be reported")
	}

	os.WriteFile(path, []byte("redact\n"), 0o600)
	if _, err := LoadRules(path); err == nil {
		t.Error("a rule with no pattern must be reported")
	}

	if rules, err := LoadRules(filepath.Join(dir, "nope.conf")); err != nil || rules != nil {
		t.Errorf("a missing config file is fine, got %v %v", rules, err)
	}
}

func TestCustomRuleWithExplicitGroup(t *testing.T) {
	r := New([]Rule{{
		Name:   "custom",
		Re:     regexp.MustCompile(`(--license )([A-Z0-9\-]+)`),
		Action: ActionRedact,
		Group:  2,
	}})
	got, action, _ := r.Apply("installer --license ABCD-1234-EFGH --quiet")
	if action != ActionRedact {
		t.Fatalf("action = %v", action)
	}
	if strings.Contains(got, "ABCD-1234-EFGH") {
		t.Errorf("secret survived: %s", got)
	}
	if !strings.Contains(got, "--license ") || !strings.Contains(got, "--quiet") {
		t.Errorf("context lost: %s", got)
	}
}

// A redactor that corrupts everyday commands is one people turn off, so the
// false-positive surface matters as much as the catch rate.
func TestNoFalsePositivesOnCommonCommands(t *testing.T) {
	r := New(nil)
	for _, cmd := range []string{
		"docker run -p8080:80 nginx",
		"docker run -p 8080:80 -p 443:443 nginx",
		"ssh -p2222 deploy@host",
		"tar -pxzf archive.tar.gz",
		"git log -p HEAD~3",
		"ffmpeg -i in.mp4 -preset slow out.mp4",
		"kubectl port-forward svc/api 8080:80",
		"rsync -avz -e ssh src/ host:/dst/",
		"curl -X POST https://api.test/v1/things",
		"npm run build -- --mode=production",
		"psql -h localhost -U postgres -d mydb",
		"aws s3 sync s3://bucket/path ./local",
		"openssl s_client -connect example.com:443",
		"java -Dspring.profiles.active=prod -jar app.jar",
		"cp -pr src dst",
	} {
		got, action, fired := r.Apply(cmd)
		if action != ActionNone {
			t.Errorf("false positive %v:\n  in:  %s\n  out: %s", fired, cmd, got)
		}
	}
}

// The narrowed rule must still catch what it was written for.
func TestMysqlInlinePasswordStillCaught(t *testing.T) {
	r := New(nil)
	for _, cmd := range []string{
		"mysql -uroot -pMyS3cretPass mydb",
		"mysqldump -u admin -pHunter2Hunter2 --all-databases",
		"mariadb -h db -u root -pTopSecret123",
	} {
		got, action, _ := r.Apply(cmd)
		if action != ActionRedact {
			t.Errorf("missed a real password in %q", cmd)
		}
		if strings.Contains(got, "S3cretPass") || strings.Contains(got, "Hunter2Hunter2") || strings.Contains(got, "TopSecret123") {
			t.Errorf("password survived: %s", got)
		}
	}
}
