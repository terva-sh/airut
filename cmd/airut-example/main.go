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
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"terva.sh/airut"
	"terva.sh/airut/agent"
	"terva.sh/airut/config"
	"terva.sh/airut/discord"
	"terva.sh/airut/matrix"
)

func main() {
	path := flag.String("config", "/etc/airut-example/airut.toml", "the configuration file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*path, log); err != nil {
		// The error names keys and paths, never a secret or chat text.
		fmt.Fprintln(os.Stderr, "airut-example:", err)
		os.Exit(1)
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
		as = append(as, discord.New(cfg.Discord, discord.Options{Logger: log}))
	}
	if cfg.Matrix != nil {
		as = append(as, matrix.New(cfg.Matrix, matrix.Options{Logger: log, AcceptInvite: inviters(cfg)}))
	}
	return as
}

// inviters lets the bot join a Matrix room when the invite comes from a
// user the file's [agent.access.matrix] names, and from nobody else.
func inviters(cfg *config.Config) func(room, inviter string) bool {
	allowed := map[string]bool{}
	if cfg.Agent != nil {
		for _, u := range cfg.Agent.Access[string(airut.Matrix)].Users {
			allowed[u] = true
		}
	}
	return func(_, inviter string) bool { return allowed[inviter] }
}
