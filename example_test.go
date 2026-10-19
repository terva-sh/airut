package airut_test

import (
	"context"
	"fmt"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

type DiceArgs struct {
	Sides int  `desc:"Sides on the die" min:"2" max:"100"`
	Count *int `desc:"How many dice" min:"1" max:"10"`
}

func Example() {
	bot := airut.New(airut.Config{Prefix: "!"})

	// One definition: a slash command on Discord, "!roll 6 2" on Matrix,
	// and a tool an agent turn may call.
	bot.Command("roll", "Roll dice", func(c *airut.Ctx, a DiceArgs) error {
		count := 1
		if a.Count != nil {
			count = *a.Count
		}
		_, err := c.Replyf("Rolling %dd%d.", count, a.Sides)
		return err
	}).AsTool()

	// The fake adapter runs the bot in process, as a test would.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := airuttest.New("")
	go bot.Run(ctx, a)
	<-a.Ready()

	for _, line := range []string{"!roll 6 2", "!roll 1", "just chatting"} {
		for _, e := range a.Text(line) {
			fmt.Println(e.Text)
		}
	}
	// Output:
	// Rolling 2d6.
	// sides: must be at least 2
	// Usage: !roll <sides> [count]
}
