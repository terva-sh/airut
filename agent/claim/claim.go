// Package claim is the claim and unclaim commands, which record and
// remove a bot's owners from its host. airut claim and airut unclaim run
// them, and a bot mounts them as its own subcommands with Command:
//
//	case "claim", "unclaim":
//		os.Exit(claim.Command{Prog: "mybot", Config: "/etc/mybot/bot.toml"}.Run(os.Args[1:], os.Stdout, os.Stderr))
//
// The form is
//
//	claim [-config FILE] [-actor NAME] [PLATFORM] USER
//	unclaim [-config FILE] [PLATFORM] USER
//
// PLATFORM is matrix or discord, and may be left out when USER's form
// names it: a Matrix id such as @you:example.org, or a Discord id, which
// is digits. The command reads the config file's [bot] owners and
// [store] and no secret, and records the claim in the [store] session
// database, which is safe to write beside the running bot. The bot
// honours a claim, or drops a removed one, within a minute, without a
// restart.
//
// Claiming an owner [bot] owners lists, or one already claimed, changes
// nothing and says so. Unclaiming an owner [bot] owners lists is refused:
// edit the file instead. A bot with no [store] keeps no claims, and its
// owners are only those [bot] owners lists.
//
// Proof of control is running the command on the host. No chat command
// adds or removes an owner.
package claim

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"slices"
	"strings"
	"time"

	"terva.sh/airut/agent/sessions"
	"terva.sh/airut/config"
)

// Command is claim and unclaim, for a program to mount as its own
// subcommands. The zero value is airut's.
type Command struct {
	// Prog names the program in usage and errors, such as "mybot".
	// Empty means "airut".
	Prog string
	// Config is the config file -config defaults to. Empty makes
	// -config required.
	Config string
}

// Main runs airut claim or airut unclaim: args[0] names the command and
// the rest are its arguments. It returns the exit status, as Run does.
func Main(args []string, stdout, stderr io.Writer) int {
	return Command{}.Run(args, stdout, stderr)
}

// Exit statuses.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	claimName   = "claim"
	unclaimName = "unclaim"
)

// noStore is what a bot with no session store is told.
const noStore = "this bot has no [store]; list owners in [bot] owners"

// Run runs args[0], "claim" or "unclaim", with the rest of args. It
// returns 0 when the owners are as asked, a claim that changed nothing
// included; 1 when the command is refused or fails; and 2 for a mistake
// in the command line.
func (c Command) Run(args []string, stdout, stderr io.Writer) int {
	prog := c.Prog
	if prog == "" {
		prog = "airut"
	}
	if len(args) == 0 || args[0] != claimName && args[0] != unclaimName {
		fmt.Fprint(stderr, c.usage(prog))
		return exitUsage
	}
	name := args[0]
	r := &runner{name: name, prog: prog + " " + name, stdout: stdout, stderr: stderr}
	fl := flag.NewFlagSet(r.prog, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	path := fl.String("config", c.Config, "the bot's config file")
	actor := ""
	if name == claimName {
		fl.StringVar(&actor, "actor", "", "who is claiming, recorded with the claim; defaults to the user running the command")
	}
	if err := fl.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, c.usage(prog))
			return exitOK
		}
		return r.usageErr(c.usage(prog), err.Error())
	}
	if *path == "" {
		return r.usageErr(c.usage(prog), "-config is required")
	}
	platform, id, msg := owner(fl.Args())
	if msg != "" {
		return r.usageErr(c.usage(prog), msg)
	}
	o, err := config.LoadOwners(*path)
	if err != nil {
		return r.fail(strings.TrimPrefix(err.Error(), "config: "))
	}
	ctx := context.Background()
	if name == claimName {
		if actor == "" {
			actor = currentUser()
		}
		return r.claim(ctx, o, platform, id, actor)
	}
	return r.unclaim(ctx, o, platform, id)
}

func (c Command) usage(prog string) string {
	return fmt.Sprintf(`usage: %[1]s claim [-config FILE] [-actor NAME] [PLATFORM] USER
       %[1]s unclaim [-config FILE] [PLATFORM] USER

claim makes USER an owner of the bot the config file describes, and
unclaim removes an owner claim made. PLATFORM is matrix or discord, and
may be left out when USER's form names it: @you:example.org is a Matrix
id, and a Discord id is digits. The claim is kept in the file's [store]
session database, and the running bot honours it within a minute.
%[2]s`, prog, c.configLine())
}

func (c Command) configLine() string {
	if c.Config == "" {
		return "-config is required.\n"
	}
	return fmt.Sprintf("-config defaults to %s.\n", c.Config)
}

// owner reads [PLATFORM] USER, and returns a usage message when they are
// not a platform and a user id in its form.
func owner(args []string) (platform, id, msg string) {
	switch len(args) {
	case 1:
		id = args[0]
		for _, p := range []string{"matrix", "discord"} {
			if config.CheckOwnerID(p, id) == nil {
				return p, id, ""
			}
		}
		return "", "", "USER is neither a Matrix id, such as @you:example.org, nor a Discord id; give the platform before it"
	case 2:
		platform, id = args[0], args[1]
		if err := config.CheckOwnerID(platform, id); err != nil {
			if platform != "matrix" && platform != "discord" {
				return "", "", "PLATFORM is " + err.Error()
			}
			return "", "", "USER is " + err.Error()
		}
		return platform, id, ""
	case 0:
		return "", "", "USER is required"
	}
	return "", "", "too many arguments: give [PLATFORM] USER, after the flags"
}

// currentUser names who runs the command, for a claim's actor.
func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

type runner struct {
	name, prog     string
	stdout, stderr io.Writer
}

func (r *runner) usageErr(usage, msg string) int {
	fmt.Fprintf(r.stderr, "%s: %s\n", r.prog, msg)
	fmt.Fprint(r.stderr, usage)
	return exitUsage
}

func (r *runner) fail(msg string) int {
	fmt.Fprintf(r.stderr, "%s: %s\n", r.prog, msg)
	return exitFailed
}

func (r *runner) say(format string, args ...any) {
	fmt.Fprintf(r.stdout, format+"\n", args...)
}

// open opens the store beside the running bot. It sweeps nothing: what
// the bot's sessions expire by is the bot's to decide.
func open(ctx context.Context, path string) (*sessions.DB, error) {
	return sessions.Open(ctx, path, sessions.Options{NoSweep: true})
}

func (r *runner) claim(ctx context.Context, o *config.Owners, platform, id, actor string) int {
	if slices.Contains(o.Config[platform], id) {
		r.say("%s %s is already an owner in [bot] owners; nothing to claim", platform, id)
		return exitOK
	}
	if o.Store.Path == "" {
		return r.fail(noStore)
	}
	db, err := open(ctx, o.Store.Path)
	if err != nil {
		return r.fail("opening the store: " + err.Error())
	}
	defer db.Close()
	got, added, err := db.ClaimOwner(ctx, platform, id, actor)
	if err != nil {
		return r.fail("recording the claim: " + err.Error())
	}
	if !added {
		r.say("%s %s was already claimed at %s; nothing changed", platform, id, got.At.UTC().Format(time.RFC3339))
		return exitOK
	}
	r.say("claimed %s %s in %s; the running bot honours it within a minute", platform, id, o.Store.Path)
	return exitOK
}

func (r *runner) unclaim(ctx context.Context, o *config.Owners, platform, id string) int {
	configured := slices.Contains(o.Config[platform], id)
	if o.Store.Path == "" {
		if configured {
			return r.fail(fmt.Sprintf("%s %s is an owner in [bot] owners; edit [bot] owners to remove them", platform, id))
		}
		return r.fail(noStore)
	}
	// A store that is not there holds no claim, and unclaim does not
	// make one.
	if _, err := os.Stat(o.Store.Path); errors.Is(err, fs.ErrNotExist) {
		if configured {
			return r.fail(fmt.Sprintf("%s %s is an owner in [bot] owners; edit [bot] owners to remove them", platform, id))
		}
		r.say("%s %s has no claim; nothing changed", platform, id)
		return exitOK
	}
	db, err := open(ctx, o.Store.Path)
	if err != nil {
		return r.fail("opening the store: " + err.Error())
	}
	defer db.Close()
	removed, err := db.UnclaimOwner(ctx, platform, id)
	if err != nil {
		return r.fail("removing the claim: " + err.Error())
	}
	if configured {
		// A claim made before the owner was listed would come back to
		// life when the list drops them, so it goes either way.
		if removed {
			r.say("removed the claim on %s %s", platform, id)
		}
		return r.fail(fmt.Sprintf("%s %s is an owner in [bot] owners; edit [bot] owners to remove them", platform, id))
	}
	if !removed {
		r.say("%s %s has no claim; nothing changed", platform, id)
		return exitOK
	}
	r.say("unclaimed %s %s; the running bot drops it within a minute", platform, id)
	return exitOK
}
