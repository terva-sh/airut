package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut/config"
)

// VerifyIO is the operator's terminal, for Verify.
type VerifyIO struct {
	// Out receives what the verification tells the operator, the emoji
	// included. It must be the operator's terminal, never a log or chat.
	Out io.Writer
	// Confirm asks the operator whether the emoji Verify has just written
	// to Out are the ones the user's Element shows, in the same order. It
	// returns true only for an explicit yes. Any other answer, an error,
	// or ctx ending is a mismatch, and nothing is signed. It should return
	// when ctx is done. A nil Confirm makes Verify refuse before it sends
	// anything, because the bot never confirms on its own.
	Confirm func(ctx context.Context) (bool, error)
}

// Verified is what a completed Verify reports.
type Verified struct {
	// User is the user the bot verified.
	User string
	// MasterKey is the user's cross-signing master key, unpadded base64,
	// which the bot has signed with its user-signing key. It is public.
	MasterKey string
}

// ErrNoDirectRoom is returned by Verify when the bot shares no private,
// encrypted direct chat with the user. Element answers a verification
// request only from a user whose devices it tracks, which it does for
// the members of an encrypted room it shares with them.
var ErrNoDirectRoom = errors.New("matrix: the bot shares no private encrypted direct chat with the user")

// ErrNoConfirm is returned by Verify when VerifyIO.Confirm is nil.
var ErrNoConfirm = errors.New("matrix: the verification needs the operator to confirm the emoji")

// verifyTimeout bounds one verification, from the request to the last
// signature. A Matrix verification request lapses after ten minutes.
const verifyTimeout = 10 * time.Minute

// Verify asks user, the bot's owner, to verify the bot's account with
// emoji (SAS), and has the operator compare the emoji on io.Out, so that
// the comparison never crosses the homeserver. The operator runs it with
// the bot stopped, against the bot's own configuration, as CrossSign is
// run: it syncs the bot's device itself, and fails while the bot syncs.
//
// It refuses before it sends anything when the user is the bot itself,
// when the bot and the user share no direct chat that is private,
// encrypted, and invite-only (see ErrNoDirectRoom), when the bot's device
// lacks the account's cross-signing keys, which CrossSign restores, or
// when the user has no cross-signing identity. Otherwise it sends the
// request to every device of the user's, shows the emoji on io.Out, and
// asks io.Confirm. Only a yes confirms.
//
// When both people have confirmed, the bot signs the user's master key
// with its user-signing key, the user's Element signs the bot, and Verify
// returns the key it signed. Each error names its cause for the operator
// and wraps the rihma error it comes from. It logs nothing.
func Verify(ctx context.Context, cfg *config.Matrix, user string, io VerifyIO) (Verified, error) {
	peer := id.UserID(user)
	if _, _, err := peer.ParseAndValidateRelaxed(); err != nil {
		return Verified{}, fmt.Errorf("matrix: %q is not a Matrix user id, such as @owner:example.org", user)
	}
	if io.Confirm == nil {
		return Verified{}, ErrNoConfirm
	}
	if peer == id.UserID(cfg.UserID) {
		return Verified{}, verifyError(rihma.ErrOwnUser, peer, "")
	}
	c, err := open(ctx, cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		return Verified{}, err
	}
	defer c.Close()
	out := &operator{w: io.Out}
	v, err := verifyWith(ctx, rihmaVerifier{c}, peer, io.Confirm, out)
	if err == nil && out.err != nil {
		err = fmt.Errorf("matrix: writing to the operator: %w", out.err)
	}
	return v, err
}

// verifier is what verify needs of the client: rihma's VerifyUser, and
// the check that the bot shares a private direct chat with the user.
type verifier interface {
	verifyUser(ctx context.Context, user id.UserID, confirm func([]rihma.SASEmoji) bool) (id.Ed25519, error)
	direct(ctx context.Context, user id.UserID) (id.RoomID, error)
	ids() (id.UserID, id.DeviceID)
}

type rihmaVerifier struct{ c *rihma.Client }

func (r rihmaVerifier) verifyUser(ctx context.Context, user id.UserID, confirm func([]rihma.SASEmoji) bool) (id.Ed25519, error) {
	return r.c.VerifyUser(ctx, user, confirm)
}

// direct reads the room state the bot stored while it ran, so it opens
// the crypto store first. Connect sends nothing to the user.
func (r rihmaVerifier) direct(ctx context.Context, user id.UserID) (id.RoomID, error) {
	if err := r.c.Connect(ctx); err != nil {
		return "", fmt.Errorf("matrix: connecting: %w", err)
	}
	return findDirect(ctx, r.c, user)
}

func (r rihmaVerifier) ids() (id.UserID, id.DeviceID) { return r.c.UserID, r.c.DeviceID }

// verifyWith is Verify's work on an open client, after the checks that
// need none.
func verifyWith(ctx context.Context, v verifier, peer id.UserID, confirm func(context.Context) (bool, error), out *operator) (Verified, error) {
	say := out.say
	bot, device := v.ids()
	say("Verifying %s with the bot's account %s, device %s.", peer, bot, device)
	room, err := v.direct(ctx, peer)
	if err != nil {
		return Verified{}, fmt.Errorf("matrix: looking for a direct chat with %s: %w", peer, err)
	}
	if room == "" {
		return Verified{}, verifyError(ErrNoDirectRoom, peer, device)
	}
	say("The bot and %s share the private encrypted chat %s.", peer, room)
	say("Sending the request. Accept it in Element when it asks, choose to compare emoji, and compare them with the ones shown here.")
	if out.err != nil {
		return Verified{}, fmt.Errorf("matrix: writing to the operator, before sending anything: %w", out.err)
	}

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	var answered, confirmed bool
	var answerErr error
	key, err := v.verifyUser(ctx, peer, func(emoji []rihma.SASEmoji) bool {
		answered = true
		confirmed, answerErr = askEmoji(ctx, peer, emoji, confirm, out)
		return confirmed
	})
	switch {
	case err == nil:
		if !confirmed {
			// rihma never completes without the confirm's yes; refuse
			// to report success if that ever changed.
			return Verified{}, fmt.Errorf("matrix: the verification completed without the operator's confirmation")
		}
		say("")
		say("%s is verified. The bot signed their master key:", peer)
		say("")
		say("    %s", fingerprint(string(key)))
		say("")
		say("Element signs the bot on its side, and then shows it as verified on each of %s's devices.", peer)
		return Verified{User: string(peer), MasterKey: string(key)}, nil
	case answered && !confirmed && answerErr != nil:
		return Verified{}, fmt.Errorf("matrix: reading the operator's answer: %w; the verification was cancelled and nothing was signed", answerErr)
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
		return Verified{}, verifyError(fmt.Errorf("%w: %w", rihma.ErrSASCancelled, err), peer, device)
	}
	return Verified{}, verifyError(err, peer, device)
}

// askEmoji shows the emoji on out and asks confirm. It confirms only when
// every line was written and the operator said yes.
func askEmoji(ctx context.Context, peer id.UserID, emoji []rihma.SASEmoji, confirm func(context.Context) (bool, error), out *operator) (bool, error) {
	say := out.say
	say("")
	say("Element should show these %d emoji, in this order:", len(emoji))
	say("")
	for i, e := range emoji {
		say("    %d. %s  %s", i+1, e.Emoji, e.Description)
	}
	say("")
	if out.err != nil {
		// The operator cannot have compared what they were not shown.
		return false, out.err
	}
	ok, err := confirm(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil || !ok {
		say("Not confirmed. Cancelling the verification; nothing is signed.")
		return false, err
	}
	say("Confirmed. Waiting for %s to confirm in Element.", peer)
	return true, nil
}

// fingerprint groups a base64 key in fours, as Element shows a key.
func fingerprint(key string) string {
	var b strings.Builder
	for i, r := range key {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// verifyError turns an error from the verification into one that tells
// the operator what happened and what to do, and wraps the original.
func verifyError(err error, peer id.UserID, device id.DeviceID) error {
	var msg string
	switch {
	case errors.Is(err, rihma.ErrSyncInProgress):
		msg = "the bot is running: stop it first, as for the cross-signing setup, because the verification syncs the bot's device itself"
	case errors.Is(err, rihma.ErrOwnUser):
		msg = fmt.Sprintf("%s is the bot's own account; name the owner's account instead", peer)
	case errors.Is(err, ErrNoDirectRoom):
		msg = fmt.Sprintf("the bot shares no private encrypted direct chat with %s, and Element ignores a verification request from a user it shares no encrypted room with. Nothing was sent. From %s's Element, start a direct chat with the bot, encrypted, and let the running bot join it; then stop the bot and verify again", peer, peer)
	case errors.Is(err, rihma.ErrNoCrossSigningKeys):
		msg = fmt.Sprintf("device %s does not hold the bot's cross-signing keys, so it cannot sign %s. Nothing was sent. Run the cross-signing setup first, which restores them with the recovery key", device, peer)
	case errors.Is(err, rihma.ErrNoUserIdentity):
		msg = fmt.Sprintf("%s has no cross-signing identity, so there is nothing to sign. Nothing was sent. Set up cross-signing (secure backup) in Element first", peer)
	case errors.Is(err, rihma.ErrPeerDeviceIDConflict):
		msg = fmt.Sprintf("one of %s's devices has the bot's device id %s, which the Matrix library the bot uses cannot tell apart from the bot's own. Nothing was sent. Sign out that device, or verify from one with another id", peer, device)
	case errors.Is(err, rihma.ErrSASMismatch):
		msg = "the emoji were not confirmed as matching, so the verification was cancelled and nothing was signed. If they did differ, someone may be interfering with the exchange; do not trust the bot's account until a verification matches"
	case errors.Is(err, rihma.ErrSASCancelled):
		msg = fmt.Sprintf("%s cancelled the verification, or it was not answered in time, and nothing was signed. If Element never showed the request, the likely cause is that it does not track the bot's devices: check that the direct chat with the bot is encrypted and that %s is in it, then try again", peer, peer)
	case errors.Is(err, rihma.ErrSASUnsigned):
		msg = fmt.Sprintf("%s's device finished the verification without covering their master key, so nothing was signed. Verify from Element with cross-signing set up", peer)
	case errors.Is(err, rihma.ErrMasterKeyMismatch):
		msg = fmt.Sprintf("%s's cross-signing identity changed during the verification, so it was not signed. If they reset it on purpose, verify again", peer)
	default:
		return fmt.Errorf("matrix: verifying %s: %w", peer, err)
	}
	return fmt.Errorf("matrix: %s: %w", msg, err)
}
