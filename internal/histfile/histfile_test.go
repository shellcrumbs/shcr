package histfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"
)

func write(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func parse(t *testing.T, name string, body []byte) *Source {
	t.Helper()
	src, err := Parse(write(t, name, body))
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// The format is sniffed from the contents, not the filename: HISTFILE gets
// pointed elsewhere all the time, and a zsh history read as bash would import
// every command with its `: <epoch>:<elapsed>;` prefix glued to the front.
func TestFormatIsSniffedNotAssumedFromTheName(t *testing.T) {
	zshInBashName := parse(t, ".bash_history", []byte(": 1711202296:0;ls -al\n: 1711202354:5;make\n"))
	if zshInBashName.Kind != Zsh {
		t.Fatalf("kind = %s, want zsh", zshInBashName.Kind)
	}
	if got := zshInBashName.Entries[0].Command; got != "ls -al" {
		t.Errorf("timestamp prefix leaked into the command: %q", got)
	}

	plain := parse(t, ".zsh_history", []byte("ls -al\nmake\n"))
	if plain.Kind != Bash {
		t.Errorf("a file with no zsh headers should read as bash, got %s", plain.Kind)
	}
}

func TestZshTimestampsAndElapsed(t *testing.T) {
	src := parse(t, "h", []byte(": 1711202296:0;ls -al\n: 1711202354:12;make -j8\n"))
	if len(src.Entries) != 2 {
		t.Fatalf("got %d entries", len(src.Entries))
	}
	if src.Entries[0].StartTime != 1711202296000 {
		t.Errorf("start time = %d", src.Entries[0].StartTime)
	}
	if src.Entries[0].Approximate {
		t.Error("a time read from the file is not approximate")
	}
	if src.Entries[1].DurationMS != 12000 {
		t.Errorf("elapsed = %d, want 12000", src.Entries[1].DurationMS)
	}
	if src.Entries[0].DurationMS != 0 {
		t.Error("zero elapsed must not become a duration")
	}
}

// zsh escapes bytes 0x80-0x9f as 0x83 followed by the byte XOR 0x20. Those are
// exactly the continuation bytes of ordinary punctuation pasted from a browser,
// so taking the file literally yields invalid UTF-8.
func TestZshMetafiedBytesAreRecovered(t *testing.T) {
	// "phpstan analyze —configuration" — the em dash is U+2014, e2 80 94, and
	// zsh stores the 0x94 as 0x83 0xb4.
	raw := []byte(": 1754238515:0;phpstan analyze \xe2\x80\x83\xb4configuration\n")
	if utf8.Valid(raw) {
		t.Fatal("test fixture should be invalid UTF-8 before unmetafying")
	}
	src := parse(t, "h", raw)
	if len(src.Entries) != 1 {
		t.Fatalf("got %d entries", len(src.Entries))
	}
	got := src.Entries[0].Command
	if !utf8.ValidString(got) {
		t.Fatalf("still invalid UTF-8: %q", got)
	}
	if got != "phpstan analyze —configuration" {
		t.Errorf("got %q, want the em dash restored", got)
	}
}

func TestZshMultilineCommands(t *testing.T) {
	raw := []byte(": 1711205259:0;awk '\\\n/INFO/ {print}\\\n'\n: 1711205300:0;ls\n")
	src := parse(t, "h", raw)
	if len(src.Entries) != 2 {
		t.Fatalf("got %d entries: %+v", len(src.Entries), src.Entries)
	}
	want := "awk '\n/INFO/ {print}\n'"
	if src.Entries[0].Command != want {
		t.Errorf("multiline reconstruction:\n want %q\n got  %q", want, src.Entries[0].Command)
	}
	if src.Entries[1].Command != "ls" {
		t.Errorf("the entry after a multiline one was lost: %q", src.Entries[1].Command)
	}
}

func TestBashWithTimestampComments(t *testing.T) {
	src := parse(t, ".bash_history", []byte("#1711202296\nls -al\n#1711202354\nmake\n"))
	if len(src.Entries) != 2 {
		t.Fatalf("got %d entries", len(src.Entries))
	}
	if src.Entries[0].StartTime != 1711202296000 || src.Entries[0].Approximate {
		t.Errorf("a real timestamp was not used: %+v", src.Entries[0])
	}
	// A command that merely starts with # is not a timestamp line.
	withComment := parse(t, ".bash_history", []byte("#not-a-timestamp\nls\n"))
	if len(withComment.Entries) != 2 {
		t.Errorf("a comment-looking command should be kept: %+v", withComment.Entries)
	}
}

// An undated history still has an order, and order is most of what makes it
// useful. Entries are spaced backwards from the file's mtime and flagged, so
// nothing pretends the time was measured.
func TestUndatedBashKeepsOrderAndIsFlagged(t *testing.T) {
	p := write(t, ".bash_history", []byte("first\nsecond\nthird\n"))
	mod := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	src, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Entries) != 3 {
		t.Fatalf("got %d entries", len(src.Entries))
	}
	for i, e := range src.Entries {
		if !e.Approximate {
			t.Errorf("entry %d should be flagged approximate", i)
		}
		if i > 0 && e.StartTime <= src.Entries[i-1].StartTime {
			t.Errorf("order not preserved at %d", i)
		}
	}
	if last := src.Entries[2].StartTime; last > mod.UnixMilli() {
		t.Errorf("approximate times should end at the file's mtime, got %d > %d", last, mod.UnixMilli())
	}
}

func TestFishHistory(t *testing.T) {
	raw := []byte("- cmd: git status\n  when: 1711202296\n- cmd: make -j8\n  when: 1711202354\n  paths:\n    - Makefile\n")
	src := parse(t, "fish_history", raw)
	if src.Kind != Fish {
		t.Fatalf("kind = %s", src.Kind)
	}
	if len(src.Entries) != 2 {
		t.Fatalf("got %d entries: %+v", len(src.Entries), src.Entries)
	}
	if src.Entries[0].Command != "git status" || src.Entries[0].StartTime != 1711202296000 {
		t.Errorf("first entry wrong: %+v", src.Entries[0])
	}
	if src.Entries[1].Command != "make -j8" {
		t.Errorf("the paths block confused the parser: %+v", src.Entries[1])
	}
}

// fish writes a newline as \n and a literal backslash as \\, and both have to
// come back out as the characters they stand for.
func TestFishEscapesAreDecoded(t *testing.T) {
	for _, tc := range []struct{ written, want string }{
		{`echo a\nb`, "echo a\nb"},
		{`grep '\\'`, `grep '\'`},
		{`echo \\n`, `echo \n`}, // escaped backslash, then the letter n
		{`awk '{print $1}'`, `awk '{print $1}'`},
		{`sed 's/\t/ /'`, `sed 's/\t/ /'`}, // not an escape fish writes; left alone
	} {
		src := parse(t, "fish_history", []byte("- cmd: "+tc.written+"\n  when: 1711202296\n"))
		if len(src.Entries) != 1 {
			t.Fatalf("%q: got %d entries", tc.written, len(src.Entries))
		}
		if got := src.Entries[0].Command; got != tc.want {
			t.Errorf("fish %q decoded to %q, want %q", tc.written, got, tc.want)
		}
	}
}

// A history where HISTTIMEFORMAT was switched on partway through has its
// undated entries at the top, which are the oldest. Dating them from the
// file's mtime would put the oldest commands after the newest.
func TestMixedDatedAndUndatedBashKeepsFileOrder(t *testing.T) {
	p := write(t, ".bash_history", []byte("old-one\nold-two\n#1600000000\nnewer-dated\n"))
	mod := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	src, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Entries) != 3 {
		t.Fatalf("got %d entries: %+v", len(src.Entries), src.Entries)
	}
	for i := 1; i < len(src.Entries); i++ {
		if src.Entries[i].StartTime <= src.Entries[i-1].StartTime {
			t.Errorf("entry %d (%s, %d) is not after entry %d (%s, %d)",
				i, src.Entries[i].Command, src.Entries[i].StartTime,
				i-1, src.Entries[i-1].Command, src.Entries[i-1].StartTime)
		}
	}
	if !src.Entries[0].Approximate || !src.Entries[1].Approximate {
		t.Error("undated entries should be flagged approximate")
	}
	if src.Entries[2].Approximate || src.Entries[2].StartTime != 1600000000000 {
		t.Errorf("the dated entry should keep its own time: %+v", src.Entries[2])
	}
}

func TestBlankAndMalformedLinesAreIgnored(t *testing.T) {
	src := parse(t, "h", []byte("\n\n   \nls\n\n"))
	if len(src.Entries) != 1 || src.Entries[0].Command != "ls" {
		t.Errorf("got %+v", src.Entries)
	}
	// A colon-prefixed line that is not a zsh header is a real command.
	notHeader := parse(t, "h", []byte(": this is a no-op command\n"))
	if len(notHeader.Entries) != 1 || notHeader.Entries[0].Command != ": this is a no-op command" {
		t.Errorf("got %+v", notHeader.Entries)
	}
}

func TestEmptyFile(t *testing.T) {
	src := parse(t, "h", nil)
	if len(src.Entries) != 0 {
		t.Errorf("got %d entries from an empty file", len(src.Entries))
	}
}
