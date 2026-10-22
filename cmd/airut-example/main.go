// Command airut-example is airut's sample bot: one plain command and one
// that hands its work to an agent turn, on Discord, Matrix, or both from
// one binary.
//
//	airut-example -config /etc/airut-example/airut.toml
//
// It serves each platform whose table the file has. roll rolls dice, and
// is also a tool the agent may call. Its roll.max_dice setting is listed
// by settings, and the owners [bot] owners names may change it with set,
// and the bot's name, description, and picture with profile. status
// tells anyone whether the bot has an owner. ask puts a question to the
// agent, which may roll dice to answer it; it needs the file's [agent]
// table, and only the users and roles its access tables name may use it.
// On Matrix the bot joins a room only when one of its owners, listed in
// [bot] owners or claimed, or one of the users named in
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
//
// With -verify it verifies the bot's Matrix account to its owner instead,
// with emoji, also with the bot stopped, on the same configuration:
//
//	airut-example -config /etc/airut-example/airut.toml -verify @owner:example.org
//
// The owner must already share an encrypted direct chat with the bot, and
// the bot's device must hold its cross-signing keys, which -cross-sign
// restores. The owner accepts the request in Element, the terminal shows
// the emoji, and the operator types yes only if Element shows the same
// ones. Then the owner's Element signs the bot, and the bot signs the
// owner, so the owner's clients show the bot as verified.
//
// claim and unclaim add and remove an owner from the host, as airut
// claim and airut unclaim do, on the same configuration, which the bot
// reads again within a minute:
//
//	airut-example claim @you:example.org
//	airut-example unclaim -config /etc/airut-example/airut.toml @you:example.org
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
	"terva.sh/airut/agent/claim"
	"terva.sh/airut/config"
	"terva.sh/airut/discord"
	"terva.sh/airut/matrix"
)

// defaultConfig is the configuration file -config defaults to.
const defaultConfig = "/etc/airut-example/airut.toml"

func main() {
	if code, ok := subcommand(os.Args[1:], os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	path := flag.String("config", defaultConfig, "the configuration file")
	crossSign := flag.Bool("cross-sign", false, "run the Matrix cross-signing setup, with the bot stopped, and exit")
	verify := flag.String("verify", "", "verify the bot's Matrix account to `USER`, its owner, with emoji compared on this terminal, with the bot stopped, and exit")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *crossSign && *verify != "" {
		fmt.Fprintln(os.Stderr, "airut-example: -cross-sign and -verify run one at a time")
		os.Exit(2)
	}
	if *verify != "" {
		if err := verifyOwner(*path, *verify); err != nil {
			fmt.Fprintln(os.Stderr, "airut-example:", err)
			os.Exit(1)
		}
		return
	}
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

// subcommand runs claim or unclaim when args name one, and reports
// whether they did. Each takes its own -config, defaulting to the bot's.
func subcommand(args []string, stdout, stderr io.Writer) (int, bool) {
	if len(args) == 0 || args[0] != "claim" && args[0] != "unclaim" {
		return 0, false
	}
	return claim.Command{Prog: "airut-example", Config: defaultConfig}.Run(args, stdout, stderr), true
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

// verifyOwner runs matrix.Verify for the file's [matrix] table, on the
// operator's terminal: the emoji go to stdout, and the answer is read
// from stdin.
func verifyOwner(path, user string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if cfg.Matrix == nil {
		return errors.New("the configuration has no [matrix] table")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err = matrix.Verify(ctx, cfg.Matrix, user, matrix.VerifyIO{
		Out:     os.Stdout,
		Confirm: func(ctx context.Context) (bool, error) { return readYes(ctx, os.Stdin, os.Stdout) },
	})
	return err
}

// readYes asks whether the emoji match and reads one line from in until
// ctx is done. Only "yes" or "y" is a yes; anything else, an empty line
// included, is a no. As with readKey, a read ctx outlives is left
// blocked, for a process that is about to exit.
func readYes(ctx context.Context, in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "Do they match the emoji in Element, in the same order? Type yes to confirm [no]: ")
	got := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(in).ReadString('\n')
		// A last line without a newline is an answer; any other failed
		// read is not, whatever text came with it.
		if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
			errc <- err
			return
		}
		got <- line
	}()
	select {
	case line := <-got:
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "yes" || a == "y", nil
	case err := <-errc:
		return false, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
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
	bc := botConfig(cfg, log)
	// Without an [agent] table, ask answers that it has no agent.
	if cfg.Agent != nil {
		ag, err := agent.Open(ctx, cfg, personas, log, agent.WithCharacter(character))
		if err != nil {
			return err
		}
		defer ag.Close()
		bc.Agent, bc.Gate, bc.MemoryWrites = ag, agent.Gate(cfg), agent.MemoryWrites(cfg)
	}
	// The bot is built before the adapters, so the Matrix invite check
	// can ask it who owns it.
	b := newBot(bc, nil)
	as := adapters(cfg, log, b)
	if len(as) == 0 {
		return fmt.Errorf("%s has neither a [discord] nor a [matrix] table", path)
	}
	return b.Run(ctx, as...)
}

// adapters returns one adapter for each platform the file configures,
// for b to run.
func adapters(cfg *config.Config, log *slog.Logger, b *airut.Bot) []airut.Adapter {
	var as []airut.Adapter
	if cfg.Discord != nil {
		as = append(as, discord.New(cfg.Discord, discord.Options{Logger: log, ReplyDepth: cfg.Bot.ReplyDepth}))
	}
	if cfg.Matrix != nil {
		as = append(as, matrix.New(cfg.Matrix, matrix.Options{Logger: log, CheckInvite: inviters(cfg, b), ReplyDepth: cfg.Bot.ReplyDepth}))
	}
	return as
}

// inviters lets the bot join a Matrix room when the invite comes from
// one of b's owners, listed in [bot] owners or claimed with claim, or
// from a user the file's [agent.access.matrix] names, and from nobody
// else. An owner check that fails refuses the invite.
func inviters(cfg *config.Config, b *airut.Bot) func(context.Context, matrix.Invite) (bool, error) {
	allowed := map[string]bool{}
	if cfg.Agent != nil {
		for _, u := range cfg.Agent.Access[string(airut.Matrix)].Users {
			allowed[u] = true
		}
	}
	return func(ctx context.Context, inv matrix.Invite) (bool, error) {
		if allowed[inv.Inviter] {
			return true, nil
		}
		return b.IsOwner(ctx, airut.User{Platform: airut.Matrix, ID: inv.Inviter})
	}
}

// botConfig is the core's configuration from the file's. The owners
// [bot] owners names may change the settings and the bot's profile from
// chat; neither built-in is open to anyone else.
func botConfig(cfg *config.Config, log *slog.Logger) airut.Config {
	return airut.Config{
		Prefix:         cfg.Bot.Prefix,
		Logger:         log,
		MetricsAddr:    cfg.Metrics.Listen,
		SummaryEvery:   cfg.Metrics.SummaryEvery,
		Owners:         airut.ByPlatform(cfg.Bot.Owners),
		Settings:       airut.Settings{Owner: airut.Owner, Values: cfg.Settings},
		ProfileEditors: airut.Owner,
	}
}
