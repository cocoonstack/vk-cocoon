package guest

import (
	"testing"
)

func TestPosixQuotePlainPassesThrough(t *testing.T) {
	cases := []string{"ls", "/usr/bin/cat", "abc-123", "key=val", "5s"}
	for _, c := range cases {
		if got := PosixQuote(c); got != c {
			t.Errorf("PosixQuote(%q) = %q, want %q", c, got, c)
		}
	}
}

func TestPosixQuoteSpecialChars(t *testing.T) {
	cases := map[string]string{
		"":                "''",
		"hello world":     "'hello world'",
		`it's`:            `'it'\''s'`,
		`$HOME`:           `'$HOME'`,
		`a;b`:             `'a;b'`,
		"tab\there":       "'tab\there'",
		"quotes'inside'x": `'quotes'\''inside'\''x'`,
	}
	for in, want := range cases {
		if got := PosixQuote(in); got != want {
			t.Errorf("PosixQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinShellQuotesPerArg(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"ls"}, "ls"},
		{[]string{"ls", "-la", "/etc"}, "ls -la /etc"},
		{[]string{"journalctl", "--no-pager", "-n", "200"}, "journalctl --no-pager -n 200"},
		{[]string{"echo", "hello world"}, "echo 'hello world'"},
		{[]string{"sh", "-c", "echo $HOME"}, "sh -c 'echo $HOME'"},
	}
	for _, c := range cases {
		if got := JoinShell(c.in); got != c.want {
			t.Errorf("JoinShell(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
