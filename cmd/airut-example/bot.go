package main

import (
	"math/rand/v2"
	"strconv"
	"strings"

	"terva.sh/airut"
)

// personas are the system prompts ask's turns may use. The empty name is
// the one a turn gets when it names none.
var personas = map[string]string{
	"": "You are a helpful assistant in a group chat. Answer briefly. " +
		"When a question needs dice, call the roll tool rather than " +
		"inventing a result.",
}

type rollArgs struct {
	Dice string `desc:"Dice to roll, like 2d6"`
}

type askArgs struct {
	Question string `airut:",rest" desc:"What to ask"`
}

// Bounds on a roll, so one command cannot ask for a reply longer than a
// message or a loop longer than a moment.
const (
	maxDice  = 20
	maxSides = 1000
)

// newBot registers the example's commands. die returns a number in
// [1, sides]; nil means a random one.
func newBot(cfg airut.Config, die func(sides int) int) *airut.Bot {
	if die == nil {
		die = func(sides int) int { return rand.IntN(sides) + 1 }
	}
	b := airut.New(cfg)
	b.Command("roll", "Roll dice", func(c *airut.Ctx, a rollArgs) error {
		n, sides, ok := parseDice(a.Dice)
		if !ok {
			return airut.Errorf("Roll dice written like 2d6: up to %d dice of 2 to %d sides.", maxDice, maxSides)
		}
		rolls := make([]string, n)
		total := 0
		for i := range rolls {
			r := die(sides)
			rolls[i] = strconv.Itoa(r)
			total += r
		}
		_, err := c.Reply(a.Dice + ": " + strings.Join(rolls, " + ") + " = " + strconv.Itoa(total))
		return err
	}).AsTool()
	// An agent that keeps memories brings the remember tool, so "remember
	// that I prefer metric" asks the model to save it.
	tools := []string{"roll"}
	if _, ok := cfg.Agent.(airut.MemoryKeeper); ok {
		tools = append(tools, "remember")
	}
	b.Command("ask", "Ask the assistant", func(c *airut.Ctx, a askArgs) error {
		return c.Agent(airut.Turn{
			Prompt:  a.Question,
			Tools:   b.Tools(tools...),
			Session: airut.PerChannel,
		}).StreamTo(c)
	})
	return b
}

// parseDice reads NdS, such as 2d6 or d20, within the bounds above.
func parseDice(s string) (n, sides int, ok bool) {
	count, faces, found := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "d")
	if !found {
		return 0, 0, false
	}
	n = 1
	if count != "" {
		var err error
		if n, err = strconv.Atoi(count); err != nil {
			return 0, 0, false
		}
	}
	sides, err := strconv.Atoi(faces)
	if err != nil || n < 1 || n > maxDice || sides < 2 || sides > maxSides {
		return 0, 0, false
	}
	return n, sides, true
}
