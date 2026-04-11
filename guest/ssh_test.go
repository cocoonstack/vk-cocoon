package guest

import (
	"testing"
)

func TestPosixQuotePlainPassesThrough(t *testing.T) {
	cases := []string{"ls", "/usr/bin/cat", "abc-123", "key=val", "5s"}
	for _, c := range cases {
		if got := posixQuote(c); got != c {
			t.Errorf("posixQuote(%q) = %q, want %q", c, got, c)
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
		if got := posixQuote(in); got != want {
			t.Errorf("posixQuote(%q) = %q, want %q", in, got, want)
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
		if got := joinShell(c.in); got != c.want {
			t.Errorf("joinShell(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNewSSHExecutorDefaults(t *testing.T) {
	e := NewSSHExecutor("root", "pw", 0)
	if e.Port != 22 {
		t.Errorf("Port = %d, want 22", e.Port)
	}
	if e.DialTimeout != defaultDialTimeout {
		t.Errorf("DialTimeout = %v, want %v", e.DialTimeout, defaultDialTimeout)
	}
	if e.User != "root" || e.Password != "pw" {
		t.Errorf("user/password not captured: %+v", e)
	}
}
