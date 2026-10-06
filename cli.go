package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

const cliUsage = `Usage: cashflow <command>

Without a command, cashflow runs the web server.

Commands:
  user:reset-password <username>
      Set a new password for a user and log out all their sessions.
      Asks for the password twice (hidden); when stdin is not a terminal,
      reads it from the first line of stdin instead.

Uses DATABASE_URL, like the server. In Kubernetes:
  kubectl -n app exec -it deploy/cashflow -- /cashflow user:reset-password <username>
`

// runCommand runs an admin command and returns the process exit code.
func runCommand(args []string) int {
	switch args[0] {
	case "user:reset-password":
		if len(args) != 2 {
			fmt.Fprint(os.Stderr, cliUsage)
			return 2
		}
		if err := cliResetPassword(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	case "help", "-h", "--help":
		fmt.Print(cliUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], cliUsage)
		return 2
	}
}

func cliResetPassword(username string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://cashflow:cashflow@localhost:5432/cashflow?sslmode=disable"))
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	// Connect before asking for the password, so a wrong DATABASE_URL fails fast.
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect db: %w", err)
	}

	pw, err := readNewPassword(os.Stdin, os.Stderr)
	if err != nil {
		return err
	}
	return resetPassword(ctx, &Store{pool: pool}, username, pw, os.Stdout)
}

// readNewPassword asks twice without echo on a terminal; otherwise (piped
// input) it reads the first line of in.
func readNewPassword(in *os.File, prompt io.Writer) (string, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return readPasswordLine(in)
	}
	fmt.Fprint(prompt, "New password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	if err != nil {
		return "", err
	}
	fmt.Fprint(prompt, "Repeat new password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}

func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

type passwordResetter interface {
	ResetPassword(ctx context.Context, username, hash string) (sessionsEnded int64, err error)
}

// resetPassword applies the same password rules as registration.
func resetPassword(ctx context.Context, s passwordResetter, username, pw string, out io.Writer) error {
	username = normalizeUsername(username)
	switch {
	case len(pw) < minPasswordLen:
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	case len(pw) > maxPasswordLen:
		return fmt.Errorf("password must be at most %d bytes", maxPasswordLen)
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	ended, err := s.ResetPassword(ctx, username, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("no user named %q", username)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Password reset for %q; logged out %d session(s).\n", username, ended)
	return nil
}
