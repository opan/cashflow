package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type fakeResetter struct {
	users    map[string]bool
	username string
	hash     string
}

func (f *fakeResetter) ResetPassword(_ context.Context, username, hash string) (int64, error) {
	if !f.users[username] {
		return 0, pgx.ErrNoRows
	}
	f.username, f.hash = username, hash
	return 2, nil
}

func TestResetPassword(t *testing.T) {
	t.Run("stores a hash of the new password for the normalized username", func(t *testing.T) {
		f := &fakeResetter{users: map[string]bool{"budi": true}}
		var out bytes.Buffer
		if err := resetPassword(context.Background(), f, "  Budi ", "rahasia-baru", &out); err != nil {
			t.Fatal(err)
		}
		if f.username != "budi" {
			t.Errorf("reset %q, want budi", f.username)
		}
		if !checkPassword(f.hash, "rahasia-baru") || checkPassword(f.hash, "something-else") {
			t.Error("stored hash doesn't verify the new password (and only it)")
		}
		if !strings.Contains(out.String(), `"budi"`) || !strings.Contains(out.String(), "2 session(s)") {
			t.Errorf("unexpected output %q", out.String())
		}
	})

	errs := []struct {
		name, username, pw, want string
	}{
		{"unknown user", "nobody", "rahasia-baru", `no user named "nobody"`},
		{"too short", "budi", "pendek", "at least 8"},
		{"too long for bcrypt", "budi", strings.Repeat("x", maxPasswordLen+1), "at most 72"},
	}
	for _, c := range errs {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeResetter{users: map[string]bool{"budi": true}}
			err := resetPassword(context.Background(), f, c.username, c.pw, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
			if f.hash != "" {
				t.Error("nothing may be stored on error")
			}
		})
	}

	t.Run("store errors are passed through", func(t *testing.T) {
		boom := errors.New("db down")
		err := resetPassword(context.Background(), failingResetter{boom}, "budi", "rahasia-baru", &bytes.Buffer{})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
	})
}

type failingResetter struct{ err error }

func (f failingResetter) ResetPassword(context.Context, string, string) (int64, error) {
	return 0, f.err
}

func TestReadPasswordLine(t *testing.T) {
	cases := map[string]string{
		"rahasia-baru\n":    "rahasia-baru",
		"rahasia-baru\r\n":  "rahasia-baru",
		"rahasia-baru":      "rahasia-baru", // no trailing newline
		" spasi di tepi \n": " spasi di tepi ",
		"baris1\nbaris2\n":  "baris1",
	}
	for in, want := range cases {
		got, err := readPasswordLine(strings.NewReader(in))
		if err != nil || got != want {
			t.Errorf("readPasswordLine(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRunCommand_UsageErrors(t *testing.T) {
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"help"}, 0},
		{[]string{"user:reset-password"}, 2},
		{[]string{"user:reset-password", "a", "b"}, 2},
		{[]string{"no-such-command"}, 2},
	}
	for _, c := range cases {
		if got := runCommand(c.args); got != c.code {
			t.Errorf("runCommand(%v) = %d, want %d", c.args, got, c.code)
		}
	}
}
