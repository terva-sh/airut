package discord

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

// appCommand is an application command as Discord's API takes it. It is
// declared here rather than taken from discordgo, whose option type
// drops a max_value of zero.
type appCommand struct {
	Type        discordgo.ApplicationCommandType `json:"type"`
	Name        string                           `json:"name"`
	Description string                           `json:"description"`
	Options     []appOption                      `json:"options,omitempty"`
}

type appOption struct {
	Type        discordgo.ApplicationCommandOptionType `json:"type"`
	Name        string                                 `json:"name"`
	Description string                                 `json:"description"`
	Required    bool                                   `json:"required,omitempty"`
	Choices     []appChoice                            `json:"choices,omitempty"`
	MinValue    *float64                               `json:"min_value,omitempty"`
	MaxValue    *float64                               `json:"max_value,omitempty"`
	MinLength   *int                                   `json:"min_length,omitempty"`
	MaxLength   *int                                   `json:"max_length,omitempty"`
}

type appChoice struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// optionTypes maps each parameter kind to its option type. The kinds
// were chosen to match Discord's one for one.
var optionTypes = map[airut.Kind]discordgo.ApplicationCommandOptionType{
	airut.KindString:     discordgo.ApplicationCommandOptionString,
	airut.KindInteger:    discordgo.ApplicationCommandOptionInteger,
	airut.KindNumber:     discordgo.ApplicationCommandOptionNumber,
	airut.KindBoolean:    discordgo.ApplicationCommandOptionBoolean,
	airut.KindUser:       discordgo.ApplicationCommandOptionUser,
	airut.KindChannel:    discordgo.ApplicationCommandOptionChannel,
	airut.KindRole:       discordgo.ApplicationCommandOptionRole,
	airut.KindAttachment: discordgo.ApplicationCommandOptionAttachment,
}

// commands describes specs as application commands. The core holds every
// command to Discord's limits when it is registered, so the only error
// is a kind this package does not know.
func commands(specs []airut.Spec) ([]appCommand, error) {
	out := make([]appCommand, 0, len(specs))
	for _, s := range specs {
		c := appCommand{Type: discordgo.ChatApplicationCommand, Name: s.Name, Description: s.Description}
		for _, p := range s.Params {
			t, ok := optionTypes[p.Kind]
			if !ok {
				return nil, fmt.Errorf("discord: command %s: parameter %s has kind %s, which has no Discord option type", s.Name, p.Name, p.Kind)
			}
			o := appOption{Type: t, Name: p.Name, Description: p.Description, Required: p.Required}
			for _, v := range p.Choices {
				o.Choices = append(o.Choices, appChoice{Name: v, Value: v})
			}
			if p.Kind == airut.KindString {
				o.MinLength, o.MaxLength = length(p.Min), length(p.Max)
			} else {
				o.MinValue, o.MaxValue = p.Min, p.Max
			}
			c.Options = append(c.Options, o)
		}
		out = append(out, c)
	}
	return out, nil
}

// length turns a string's bound, a whole number the core has checked,
// into a length.
func length(v *float64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// register replaces the application's commands with cmds: in each of
// guilds, or globally when guilds is empty. A bulk overwrite replaces the
// whole set in one request, so a restart neither registers a command
// twice nor leaves behind one the bot no longer has.
//
// Guild commands appear at once and suit development. Global ones reach
// every guild the bot is in and suit production. Switching a bot from
// guilds to global leaves the commands it registered in those guilds in
// place until they are overwritten there.
func register(ctx context.Context, s *discordgo.Session, appID string, guilds []string, cmds []appCommand) error {
	put := func(endpoint string) error {
		_, err := s.RequestWithBucketID("PUT", endpoint, cmds, endpoint, discordgo.WithContext(ctx))
		return err
	}
	if len(guilds) == 0 {
		if err := put(discordgo.EndpointApplicationGlobalCommands(appID)); err != nil {
			return fmt.Errorf("discord: registering commands globally: %w", err)
		}
		return nil
	}
	for _, g := range guilds {
		if err := put(discordgo.EndpointApplicationGuildCommands(appID, g)); err != nil {
			return fmt.Errorf("discord: registering commands in guild %s: %w", g, err)
		}
	}
	return nil
}
