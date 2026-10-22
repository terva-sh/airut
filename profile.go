package airut

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Profile is the bot's own profile on one platform, as it is now.
type Profile struct {
	// Name is the name the platform shows for the bot.
	Name string
	// About is the bot's description, empty on a platform that has none.
	About string
	// Avatar is where the platform serves the bot's picture, empty when
	// it has none.
	Avatar string
}

// ProfileChange changes the bot's profile. A nil field is left as it is.
type ProfileChange struct {
	Name  *string
	About *string
	// Avatar is the new picture.
	Avatar *Image
}

// Image is a picture's bytes and media type. A profile's avatar is one,
// and so is a picture an agent turn sees.
type Image struct {
	Data []byte
	// Type is image/png, image/jpeg, image/gif, or image/webp, and must
	// be what Data holds. An agent turn reads the type from Data itself,
	// so a turn's image may leave it empty.
	Type string
}

// The bounds every platform accepts on a profile: Discord's, which are
// the tighter.
const (
	// MaxProfileName is the most characters a name may have.
	MaxProfileName = 32
	// MaxProfileAbout is the most characters a description may have.
	MaxProfileAbout = 400
	// MaxAvatarBytes is the largest picture a profile may take.
	MaxAvatarBytes = 8 << 20
)

// avatarTypes are the media types an avatar may be.
var avatarTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// ErrUnsupported is returned, wrapped, for something the platform does
// not have, such as a description on Matrix.
var ErrUnsupported = errors.New("airut: the platform does not support this")

// Check reports whether every platform would accept the change: a name
// of 1 to MaxProfileName characters with no control characters and
// not blank, a description of at most MaxProfileAbout, and a picture of
// at most MaxAvatarBytes whose bytes are the type it claims. An adapter
// calls it before changing anything.
func (p ProfileChange) Check() error {
	if p.Name != nil {
		n := utf8.RuneCountInString(*p.Name)
		switch {
		case strings.TrimSpace(*p.Name) == "" || n > MaxProfileName:
			return fmt.Errorf("a name has 1 to %d characters, and this one has %d", MaxProfileName, n)
		case strings.ContainsFunc(*p.Name, unicode.IsControl):
			return errors.New("a name cannot hold a line break or other control character")
		}
	}
	if p.About != nil {
		if n := utf8.RuneCountInString(*p.About); n > MaxProfileAbout {
			return fmt.Errorf("a description has at most %d characters, and this one has %d", MaxProfileAbout, n)
		}
	}
	if p.Avatar != nil {
		a := p.Avatar
		switch {
		case len(a.Data) == 0 || len(a.Data) > MaxAvatarBytes:
			return fmt.Errorf("a picture has 1 byte to %d MiB, and this one has %d bytes", MaxAvatarBytes>>20, len(a.Data))
		case !slices.Contains(avatarTypes, a.Type):
			return fmt.Errorf("a picture is PNG, JPEG, GIF, or WebP, not %q", a.Type)
		case http.DetectContentType(a.Data) != a.Type:
			return fmt.Errorf("the picture's bytes are not the %s it claims to be", a.Type)
		}
	}
	return nil
}

// Fields names the fields the change sets, in a fixed order.
func (p ProfileChange) Fields() []string {
	var out []string
	if p.Name != nil {
		out = append(out, "name")
	}
	if p.About != nil {
		out = append(out, "about")
	}
	if p.Avatar != nil {
		out = append(out, "avatar")
	}
	return out
}

type profileArgs struct {
	Field *string     `choices:"name,about,avatar" desc:"What to change; leave out to show the profile"`
	Image *Attachment `desc:"The new picture, for avatar"`
	Value *string     `airut:",rest" desc:"The new name or description; empty clears the description"`
}

// profileCommand is the built-in profile. It has an attachment
// parameter, so it can never be a tool, and no agent turn can change
// the bot's name or face.
func profileCommand(b *Bot) *Command {
	c, err := newCommand(b, "profile", "Show the bot's profile here, or change its name, description, or picture", func(c *Ctx, a profileArgs) error {
		conn, err := c.Conn()
		if err != nil {
			return err
		}
		if a.Field == nil {
			p, err := conn.Profile(c.ctx)
			if err != nil {
				return err
			}
			_, err = c.ReplyPrivate(showProfile(p))
			return err
		}
		var ch ProfileChange
		value := ""
		if a.Value != nil {
			value = strings.TrimSpace(*a.Value)
		}
		switch *a.Field {
		case "name":
			ch.Name = &value
		case "about":
			ch.About = &value
		case "avatar":
			if a.Image == nil {
				return Errorf("Attach the new picture to the command.")
			}
			img, err := readAvatar(c, conn, *a.Image)
			if err != nil {
				return err
			}
			ch.Avatar = img
		default:
			return Errorf("Change one of name, about, or avatar.")
		}
		if err := ch.Check(); err != nil {
			return Errorf("That is not a profile %s every platform takes: %v.", *a.Field, err)
		}
		if err := conn.SetProfile(c.ctx, ch); err != nil {
			if errors.Is(err, ErrUnsupported) {
				return Errorf("%s has no profile %s.", c.Platform(), *a.Field)
			}
			return err
		}
		size := 0
		if ch.Avatar != nil {
			size = len(ch.Avatar.Data)
		}
		b.metrics.Add("airut_profile_changes_total", 1, "platform", string(c.Platform()), "field", *a.Field)
		b.cfg.Logger.Info("profile changed", "invocation", c.inv.ID, "platform", string(c.Platform()), "field", *a.Field, "avatar_bytes", size)
		_, err = c.ReplyPrivate("The bot's " + *a.Field + " here is changed.")
		return err
	})
	if err != nil {
		panic("airut: the built-in profile command: " + err.Error())
	}
	return c
}

// readAvatar reads an attached picture, up to MaxAvatarBytes, and takes
// its type from its bytes rather than from what the platform reported.
func readAvatar(c *Ctx, conn Conn, at Attachment) (*Image, error) {
	if at.Size > MaxAvatarBytes {
		return nil, Errorf("The picture is larger than %d MiB.", MaxAvatarBytes>>20)
	}
	r, err := conn.Open(c.ctx, at)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, MaxAvatarBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxAvatarBytes {
		return nil, Errorf("The picture is larger than %d MiB.", MaxAvatarBytes>>20)
	}
	return &Image{Data: data, Type: http.DetectContentType(data)}, nil
}

func showProfile(p Profile) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Name: %s", p.Name)
	if p.About != "" {
		fmt.Fprintf(&out, "\nAbout: %s", p.About)
	}
	if p.Avatar != "" {
		fmt.Fprintf(&out, "\nPicture: %s", p.Avatar)
	} else {
		out.WriteString("\nNo picture.")
	}
	out.WriteString("\nChange one with: profile name TEXT, profile about TEXT, or profile avatar with a picture attached.")
	return out.String()
}
