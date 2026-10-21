// Command airut-example is airut's sample bot: one plain command and one
// that hands its work to an agent turn, on Discord, Matrix, or both from
// one binary.
//
//	airut-example -config /etc/airut-example/airut.toml
//
// It serves each platform whose table the file has. roll rolls dice, and
// is also a tool the agent may call. ask puts a question to the agent,
// which may roll dice to answer it; it needs the file's [agent] table,
// and only the users and roles its access tables name may use it. On
// Matrix the bot joins a room only when one of the users named in
// [agent.access.matrix] invites it. docs/config.example.toml shows every
// key. Secrets are references to where the deployment put them, never
// values.
//
// With -cross-sign it runs the Matrix cross-signing setup instead, once,
// with the bot stopped, on the same configuration:
//
//	airut-example -config /etc/airut-example/airut.toml -cross-sign
//
// For an account with no cross-signing identity it makes one, signs the
// bot's device, and prints the recovery key to the terminal, once. For one
// that has an identity it asks for the recovery key, read without echo,
// and signs the device with it.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"terva.sh/airut"
	"terva.sh/airut/agent"
	"terva.sh/airut/config"
	"terva.sh/airut/discord"
	"terva.sh/airut/matrix"
)

func main() {
	path := flag.String("config", "/etc/airut-example/airut.toml", "the configuration file")
	crossSign := flag.Bool("cross-sign", false, "run the Matrix cross-signing setup, with the bot stopped, and exit")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *crossSign {
		if err := crossSignSetup(*path); err != nil {
			fmt.Fprintln(os.Stderr, "airut-example:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*path, log); err != nil {
		// The error names keys and paths, never a secret or chat text.
		fmt.Fprintln(os.Stderr, "airut-example:", err)
		os.Exit(1)
	}
}

// crossSignSetup runs matrix.CrossSign for the file's [matrix] table, on
// the operator's terminal. The recovery key goes to stdout only.
func crossSignSetup(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if cfg.Matrix == nil {
		return errors.New("the configuration has no [matrix] table")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err = matrix.CrossSign(ctx, cfg.Matrix, matrix.CrossSignIO{Out: os.Stdout, RecoveryKey: readRecoveryKey})
	return err
}

// readRecoveryKey reads the recovery key from the terminal without echo,
// or one line from stdin when it is not a terminal.
func readRecoveryKey(ctx context.Context) (string, error) {
	defer fmt.Fprintln(os.Stderr)
	return readKey(ctx, os.Stdin, os.Stderr, "Recovery key: ")
}

// readKey shows prompt on out and reads a key from in until ctx is done.
// A read cannot be interrupted, so when ctx is done first the read is
// left blocked, for a process that is about to exit. A terminal is put
// in raw mode here, before the read starts, and restored before readKey
// returns, so the blocked read leaves it as it was.
func readKey(ctx context.Context, in *os.File, out io.Writer, prompt string) (string, error) {
	type read struct {
		key string
		err error
	}
	got := make(chan read, 1)
	if fd := int(in.Fd()); term.IsTerminal(fd) {
		st, err := term.MakeRaw(fd)
		if err != nil {
			return "", err
		}
		defer func() { _ = term.Restore(fd, st) }()
		// In raw mode Ctrl-C is a key, and ReadPassword returns io.EOF.
		t := term.NewTerminal(struct {
			io.Reader
			io.Writer
		}{in, out}, "")
		go func() {
			line, err := t.ReadPassword(prompt)
			got <- read{strings.TrimSpace(line), err}
		}()
	} else {
		fmt.Fprint(out, prompt)
		go func() {
			line, err := bufio.NewReader(in).ReadString('\n')
			if line != "" {
				err = nil
			}
			got <- read{strings.TrimSpace(line), err}
		}()
	}
	select {
	case r := <-got:
		return r.key, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func run(path string, log *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bc := airut.Config{
		Prefix:       cfg.Bot.Prefix,
		Logger:       log,
		MetricsAddr:  cfg.Metrics.Listen,
		SummaryEvery: cfg.Metrics.SummaryEvery,
	}
	// Without an [agent] table, ask answers that it has no agent.
	if cfg.Agent != nil {
		ag, err := agent.Open(ctx, cfg, personas, log)
		if err != nil {
			return err
		}
		defer ag.Close()
		bc.Agent, bc.Gate = ag, agent.Gate(cfg)
	}
	as := adapters(cfg, log)
	if len(as) == 0 {
		return fmt.Errorf("%s has neither a [discord] nor a [matrix] table", path)
	}
	return newBot(bc, nil).Run(ctx, as...)
}

// adapters returns one adapter for each platform the file configures.
func adapters(cfg *config.Config, log *slog.Logger) []airut.Adapter {
	var as []airut.Adapter
	if cfg.Discord != nil {
		as = append(as, discord.New(cfg.Discord, discord.Options{Logger: log, ReplyDepth: cfg.Bot.ReplyDepth}))
	}
	if cfg.Matrix != nil {
		as = append(as, matrix.New(cfg.Matrix, matrix.Options{Logger: log, AcceptInvite: inviters(cfg), ReplyDepth: cfg.Bot.ReplyDepth}))
	}
	return as
}

// inviters lets the bot join a Matrix room when the invite comes from a
// user the file's [agent.access.matrix] names, and from nobody else.
func inviters(cfg *config.Config) func(matrix.Invite) bool {
	allowed := map[string]bool{}
	if cfg.Agent != nil {
		for _, u := range cfg.Agent.Access[string(airut.Matrix)].Users {
			allowed[u] = true
		}
	}
	return func(inv matrix.Invite) bool { return allowed[inv.Inviter] }
}
