// Command airut holds the tools for a bot's configuration that need no
// bot to run them.
//
//	airut seal --recipient age1... --at providers.cpa.api_key < value
//
// seal reads one secret value from stdin, seals it to the age recipient
// for the key path --at names, and prints the field to write there as
// { sealed = "..." }. It needs only the recipient, which is public, so
// the person who holds a secret can seal it without the key that opens
// it. On a terminal the value is read without echo; from a pipe, one
// trailing line ending is trimmed, as Load trims a file's. --recipient
// may be repeated, so a value opens with either key while one replaces
// the other.
//
//	airut claim -config /etc/mybot/bot.toml @you:example.org
//	airut unclaim -config /etc/mybot/bot.toml @you:example.org
//
// claim makes a user an owner of the bot the config file describes, and
// unclaim removes a claim. They record it in the file's [store] session
// database, which the running bot reads again within a minute. Package
// terva.sh/airut/agent/claim documents them, and a bot can mount them as
// its own subcommands from there.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"filippo.io/age"
	"golang.org/x/term"

	"terva.sh/airut/agent/claim"
	"terva.sh/airut/config"
	"terva.sh/seal"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `usage: airut seal --recipient age1... --at KEY < value
       airut claim -config FILE [PLATFORM] USER
       airut unclaim -config FILE [PLATFORM] USER

seal reads a secret from stdin and prints it sealed for KEY, a key path
in the configuration file such as providers.ID.api_key, discord.token,
or app.booru.token. A key that is not only A-Z, a-z, 0-9, _ and - is
quoted, as in app.tokens."a.b", and an array index is bracketed, as in
app.mirrors[0].key. Write the output there as { sealed = "..." }.

claim and unclaim add and remove an owner of the bot the config file
describes; airut claim -h says more.
`

// run is the command, with its streams, and returns the exit status:
// 0 on success, 1 when the command fails, 2 for a usage mistake.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "claim" || args[0] == "unclaim") {
		return claim.Main(args, stdout, stderr)
	}
	if len(args) == 0 || args[0] != "seal" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	err := sealCmd(args[1:], stdin, stdout, stderr)
	var u usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stderr, usage)
		return 0
	case errors.As(err, &u):
		fmt.Fprintln(stderr, "airut seal:", err)
		fmt.Fprint(stderr, usage)
		return 2
	}
	fmt.Fprintln(stderr, "airut seal:", err)
	return 1
}

// usageError is a mistake in the command line rather than in sealing.
type usageError struct{ error }

// badArg says which argument fs.Parse refused, by position only: an
// unknown flag, or a flag missing its value.
func badArg(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch name {
		case "recipient", "at":
			if !hasValue {
				if i+1 == len(args) {
					return fmt.Errorf("argument %d is a flag with no value after it", i+1)
				}
				i++
			}
		default:
			return fmt.Errorf("argument %d is not a flag airut seal takes", i+1)
		}
	}
	return errors.New("the arguments are not ones airut seal takes")
}

// recipients collects repeated --recipient flags.
type recipients []string

func (r *recipients) String() string     { return strings.Join(*r, ",") }
func (r *recipients) Set(v string) error { *r = append(*r, v); return nil }

// maxValue bounds what seal reads, so a file piped in by mistake is
// refused rather than sealed into one enormous line.
const maxValue = 64 << 10

func sealCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("airut seal", flag.ContinueOnError)
	// The flag package names an argument it does not know, and that
	// argument may be a secret pasted on the command line by mistake.
	// It prints nothing; badArg says what was wrong, by position.
	fs.SetOutput(io.Discard)
	var rs recipients
	fs.Var(&rs, "recipient", "an age1... recipient to seal for; repeat for more than one")
	at := fs.String("at", "", "the key path the value is sealed for, such as providers.cpa.api_key")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{badArg(args)}
	}
	if fs.NArg() > 0 {
		return usageError{errors.New("the value is read from stdin, not given as an argument")}
	}
	if len(rs) == 0 {
		return usageError{errors.New("--recipient is required")}
	}
	if *at == "" {
		return usageError{errors.New("--at is required")}
	}
	var to []age.Recipient
	for i, r := range rs {
		rcpt, err := seal.ParseRecipient(r)
		if err != nil {
			// Which one, by position: a recipient is public, but a key
			// pasted here by mistake is not.
			return usageError{fmt.Errorf("--recipient %d is not an age1... recipient", i+1)}
		}
		to = append(to, rcpt)
	}
	// Check the key path before asking for the value.
	if err := config.CheckSealKey(*at); err != nil {
		return usageError{strippedErr(err)}
	}
	value, err := readValue(stdin, stderr)
	if err != nil {
		return err
	}
	field, err := config.Seal(*at, value, to...)
	if err != nil {
		return strippedErr(err)
	}
	_, err = fmt.Fprintln(stdout, field)
	return err
}

// strippedErr drops config's "config: " prefix, which says nothing to
// someone running the command.
func strippedErr(err error) error {
	return errors.New(strings.TrimPrefix(err.Error(), "config: "))
}

// readValue reads the secret: from a terminal without echo, otherwise
// all of stdin with one trailing line ending trimmed. No error carries
// any of what was read.
func readValue(stdin io.Reader, stderr io.Writer) (string, error) {
	var raw []byte
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Value (not echoed): ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", errors.New("cannot read the value from the terminal")
		}
		raw = b
	} else {
		b, err := io.ReadAll(io.LimitReader(stdin, maxValue+1))
		if err != nil {
			return "", errors.New("cannot read the value from stdin")
		}
		if len(b) > maxValue {
			return "", fmt.Errorf("the value on stdin is longer than %d bytes", maxValue)
		}
		raw = b
	}
	v, crlf := strings.CutSuffix(string(raw), "\r\n")
	if !crlf {
		v = strings.TrimSuffix(v, "\n")
	}
	switch {
	case v == "":
		return "", errors.New("no value on stdin: pipe the secret in, as in `airut seal ... < file`, or run it on a terminal to type it")
	case !utf8.ValidString(v):
		return "", errors.New("the value is not valid UTF-8 text")
	case seal.IsSealed(v):
		return "", errors.New("the value on stdin is already sealed")
	}
	return v, nil
}
