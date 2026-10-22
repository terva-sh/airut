package agent

import (
	"slices"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// Gate turns the agent guards in c into the core's Gate: the access lists
// by platform, the rate limit with the owners' rate and whether a refused
// turn waits, and the budget of a turn that sets none. A file with no
// [agent] table gives the zero Gate, which refuses every turn.
func Gate(c *config.Config) airut.Gate {
	if c.Agent == nil {
		return airut.Gate{}
	}
	a := c.Agent
	g := airut.Gate{
		Access:     make(map[airut.Platform]airut.Access, len(a.Access)),
		PerUser:    a.Rate.PerUser,
		PerChannel: a.Rate.PerChannel,
		Window:     a.Rate.Window,
		Wait:       a.Rate.Wait,
		Budget:     airut.Budget{Turns: a.Budget.MaxTurns, Tokens: a.Budget.MaxTokens},
	}
	if o := a.Rate.Owners; o != nil {
		g.Owners = &airut.GroupRate{PerUser: o.PerUser, PerChannel: o.PerChannel}
	}
	for p, acc := range a.Access {
		g.Access[airut.Platform(p)] = airut.Access{
			Users: slices.Clone(acc.Users), Roles: slices.Clone(acc.Roles), Channels: slices.Clone(acc.Rooms),
		}
	}
	return g
}

// JudgeRate turns [agent.judge] rate into the core's JudgeRate. A file
// with no [agent.judge] table, or no rate in it, caps nothing.
func JudgeRate(c *config.Config) airut.JudgeRate {
	if c.Agent == nil || c.Agent.Judge == nil {
		return airut.JudgeRate{}
	}
	r := c.Agent.Judge.Rate
	return airut.JudgeRate{PerChannel: r.PerChannel, Window: r.Window}
}
