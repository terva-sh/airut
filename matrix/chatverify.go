package matrix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut"
)

// In-chat verification, which [matrix] verify_in_chat turns on, lets an
// owner verify the running bot from Element. Element sends the request
// into the owner's direct chat with the bot. The bot accepts it, posts
// the emoji in that chat, and confirms only when the owner replies yes
// there. It trusts the homeserver, which could forge the key exchange
// and rewrite the chat to match, so it is off by default. Verify, which
// shows the emoji on the host's terminal, does not trust the homeserver.

// errNoOwnersFunc is Run's error when verify_in_chat is on and
// Options.Owners is nil, so the bot cannot know whom to admit.
var errNoOwnersFunc = errors.New("matrix: [matrix] verify_in_chat is on, but Options.Owners is nil, so the bot cannot tell whose verification to answer")

// sasControl is what in-chat verification needs of rihma's
// SASController, so tests can stand in for it.
type sasControl interface {
	Changed() <-chan struct{}
	Snapshot() rihma.SASSnapshot
	Accept(ctx context.Context, txn id.VerificationTransactionID) error
	Confirm(ctx context.Context, txn id.VerificationTransactionID, match bool) error
	Cancel(ctx context.Context, txn id.VerificationTransactionID) error
}

// enableChatVerify turns on rihma's SAS controller for the bot's owners,
// in-room, when on is set, and returns it. When on is false it returns
// nil and calls neither owners nor enable, so a bot that leaves
// verify_in_chat out runs no SAS controller, as before the option
// existed. With no owner to list it logs a warning and returns nil:
// rihma fixes the list before the first sync, so an owner claimed later
// is admitted only after a restart. An error from owners does the same,
// so a claims read that fails does not stop the bot.
func enableChatVerify(ctx context.Context, on bool, owners func(context.Context) ([]string, error), self id.UserID, enable func(rihma.SASOptions) (sasControl, error), log *slog.Logger) (sasControl, error) {
	if !on {
		return nil, nil
	}
	if owners == nil {
		return nil, errNoOwnersFunc
	}
	ids, err := owners(ctx)
	if err != nil {
		log.Warn("in-chat verification is off: the owners could not be read", "error_kind", errKind(err))
		return nil, nil
	}
	var peers []id.UserID
	skipped := 0
	for _, s := range ids {
		u := id.UserID(s)
		if _, _, err := u.ParseAndValidateRelaxed(); err != nil || u == self {
			skipped++
			continue
		}
		if !slices.Contains(peers, u) {
			peers = append(peers, u)
		}
	}
	if skipped > 0 {
		log.Warn("in-chat verification skips owners that are the bot or not Matrix user ids", "count", skipped)
	}
	if len(peers) == 0 {
		log.Warn("in-chat verification is off: the bot has no Matrix owner; set [bot] owners or claim it, then restart")
		return nil, nil
	}
	ctl, err := enable(rihma.SASOptions{Peers: peers, InRoom: true})
	if err != nil {
		return nil, fmt.Errorf("matrix: enabling in-chat verification: %w", err)
	}
	log.Info("in-chat verification is on", "owners", len(peers))
	return ctl, nil
}

// chatVerify runs in-chat verification for one Run. One goroutine,
// watch, reads the controller's snapshots and hands each to the attempt
// it belongs to. Each attempt runs on a goroutine of its own, and takes
// the owner's answer from onMessage through answer.
type chatVerify struct {
	ctl  sasControl
	self id.UserID
	// owners lists the bot's Matrix owners now; each request is checked
	// against it, so an owner removed since the start is refused.
	owners func(context.Context) ([]string, error)
	// direct reports whether room is a direct chat private to the bot
	// and u. dmOf returns an existing such chat with u, or "", and
	// never creates one.
	direct func(ctx context.Context, room id.RoomID, u id.UserID) bool
	dmOf   func(ctx context.Context, u id.UserID) id.RoomID
	// post sends text to room.
	post  func(ctx context.Context, room id.RoomID, text string) error
	spawn func(func())
	log   *slog.Logger
	// timeout bounds one attempt, from the request to the outcome.
	timeout time.Duration

	quit     chan struct{}
	quitOnce sync.Once

	mu  sync.Mutex
	cur *chatAttempt
}

// chatAttempt is one verification with an owner.
type chatAttempt struct {
	txn   id.VerificationTransactionID
	owner id.UserID
	room  id.RoomID
	// device is the owner's device, from the request.
	device id.DeviceID
	// updates carries the latest snapshot for this attempt, and answers
	// the owner's yes or no. Each holds one value; a newer snapshot
	// replaces an unread one.
	updates chan rihma.SASSnapshot
	answers chan bool
	// replaced is closed when rihma has admitted another request, which
	// it does only once this one has ended.
	replaced chan struct{}
	// asking is set, under chatVerify.mu, while the emoji are posted and
	// the owner's answer is awaited.
	asking bool
}

func (v *chatVerify) shutdown() { v.quitOnce.Do(func() { close(v.quit) }) }

// watch reads the controller until it stops, ctx ends, or shutdown.
func (v *chatVerify) watch(ctx context.Context) {
	for {
		select {
		case _, ok := <-v.ctl.Changed():
			if !ok {
				v.shutdown()
				return
			}
		case <-ctx.Done():
			return
		case <-v.quit:
			return
		}
		v.route(v.ctl.Snapshot())
	}
}

// route hands snap to its attempt, or starts one for a new request.
func (v *chatVerify) route(snap rihma.SASSnapshot) {
	if snap.TransactionID == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if a := v.cur; a != nil && a.txn == snap.TransactionID {
		select {
		case <-a.updates:
		default:
		}
		a.updates <- snap
		return
	}
	if snap.State != rihma.SASRequested {
		// The end of an attempt that is no longer followed.
		return
	}
	if old := v.cur; old != nil {
		close(old.replaced)
	}
	a := &chatAttempt{
		txn: snap.TransactionID, owner: snap.OtherUser, room: snap.Room, device: snap.OtherDevice,
		updates: make(chan rihma.SASSnapshot, 1), answers: make(chan bool, 1), replaced: make(chan struct{}),
	}
	v.cur = a
	v.spawn(func() { v.run(a) })
}

// answer takes msg as the owner's reply to the emoji, and reports
// whether it did. Only an encrypted text message from the owner whose
// attempt is asking, in that attempt's room, whose whole body is yes, y,
// no, or n in any case, is taken, and only the first: the check and the
// claim are one step under the lock, so of two replies racing, one is
// taken. A message taken never reaches the bot's commands or handlers.
// Every other message does, as usual, a yes or no after the first among
// them.
func (v *chatVerify) answer(evt *event.Event) bool {
	if evt.Sender == "" || !evt.Mautrix.WasEncrypted {
		return false
	}
	c, _ := evt.Content.Parsed.(*event.MessageEventContent)
	if c == nil || c.MsgType != event.MsgText || c.RelatesTo.GetReplaceID() != "" {
		return false
	}
	yes, ok := parseYesNo(c.Body)
	if !ok {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	a := v.cur
	if a == nil || !a.asking || evt.RoomID != a.room || evt.Sender != a.owner {
		return false
	}
	// Claimed: asking is cleared before the lock is released, so no
	// other reply is taken, and answers, which holds one, is empty.
	a.asking = false
	a.answers <- yes
	return true
}

// parseYesNo reads an answer to the emoji: yes or y, no or n, in any
// case, with surrounding space.
func parseYesNo(s string) (yes, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y":
		return true, true
	case "no", "n":
		return false, true
	}
	return false, false
}

func (v *chatVerify) setAsking(a *chatAttempt, on bool) {
	v.mu.Lock()
	a.asking = on
	v.mu.Unlock()
}

// finish forgets a, unless another attempt has replaced it.
func (v *chatVerify) finish(a *chatAttempt) {
	v.mu.Lock()
	defer v.mu.Unlock()
	a.asking = false
	if v.cur == a {
		v.cur = nil
	}
}

// isOwner reports whether u is an owner now. A read that fails refuses.
func (v *chatVerify) isOwner(ctx context.Context, u id.UserID) bool {
	ids, err := v.owners(ctx)
	if err != nil {
		v.log.Warn("could not read the owners for a verification request", "error_kind", errKind(err))
		return false
	}
	return slices.Contains(ids, string(u))
}

// The outcomes run logs, besides the snapshot states.
const (
	outcomeNotOwner   = "not_owner"
	outcomeOwnAccount = "own_account"
	outcomeToDevice   = "to_device"
	outcomeNotDirect  = "not_direct"
	outcomeAccept     = "accept_failed"
	outcomePost       = "post_failed"
	outcomeConfirm    = "confirm_failed"
	outcomeTimeout    = "timeout"
	outcomeReplaced   = "replaced"
	outcomeStopped    = "stopped"
)

// run is one attempt, from the request to its outcome. It never confirms
// unless the owner said yes.
func (v *chatVerify) run(a *chatAttempt) {
	defer v.finish(a)
	ctx, cancel := context.WithTimeout(context.Background(), v.timeout)
	defer cancel()
	go func() {
		select {
		case <-v.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	log := v.log.With("room", string(a.room), "user", string(a.owner), "txn", string(a.txn))
	end := func(outcome string, attrs ...any) {
		log.Info("in-chat verification ended", append([]any{"outcome", outcome}, attrs...)...)
	}
	// refuse cancels, and posts note, if any, in a private direct chat
	// with the owner other than the attempt's room: one only when the
	// attempt's room is not private, or it would not be refused.
	refuse := func(outcome string, note string) {
		_ = v.ctl.Cancel(ctx, a.txn)
		if note != "" {
			if dm := v.dmOf(ctx, a.owner); dm != "" && dm != a.room {
				_ = v.post(ctx, dm, note)
			}
		}
		end(outcome)
	}
	// say posts text in the attempt's room only while that room is
	// still private to the bot and the owner, checked at each post,
	// because someone may join, or the join rule change, at any time.
	say := func(text string) {
		if v.direct(ctx, a.room, a.owner) {
			_ = v.post(ctx, a.room, text)
		}
	}

	switch {
	case a.owner == v.self:
		// Another device of the bot's own account; the bot never
		// confirms by itself, so it does not take part.
		refuse(outcomeOwnAccount, "")
		return
	case !v.isOwner(ctx, a.owner):
		refuse(outcomeNotOwner, "")
		return
	case a.room == "":
		refuse(outcomeToDevice, textElsewhere)
		return
	case !v.direct(ctx, a.room, a.owner):
		refuse(outcomeNotDirect, textElsewhere)
		return
	}
	log.Info("in-chat verification requested")
	if err := v.ctl.Accept(ctx, a.txn); err != nil {
		// rihma has cancelled a request that failed its checks; cancel
		// any other that is still open.
		_ = v.ctl.Cancel(ctx, a.txn)
		say(acceptText(err))
		end(outcomeAccept, "error_kind", errKind(err))
		return
	}
	posted, answered := false, false
	for {
		select {
		case snap := <-a.updates:
			switch snap.State {
			case rihma.SASShowing:
				if posted {
					continue
				}
				posted = true
				if !v.direct(ctx, a.room, a.owner) {
					// The room stopped being private after Accept: the
					// emoji go nowhere.
					refuse(outcomeNotDirect, textNoLongerPrivate)
					return
				}
				if err := v.post(ctx, a.room, emojiText(snap.Emoji, a.device)); err != nil {
					// The owner cannot compare what they were not shown.
					_ = v.ctl.Cancel(ctx, a.txn)
					end(outcomePost, "error_kind", errKind(err))
					return
				}
				v.setAsking(a, true)
			case rihma.SASDone:
				say(doneText(snap.MasterKey))
				end(string(snap.State))
				return
			case rihma.SASCancelled, rihma.SASFailed:
				say(reasonText(snap.Reason))
				end(string(snap.State), "reason", reasonKind(snap.Reason))
				return
			case rihma.SASStopped:
				end(outcomeStopped)
				return
			}
		case yes := <-a.answers:
			v.setAsking(a, false)
			if answered {
				// answer takes one reply; never confirm twice.
				continue
			}
			answered = true
			if !v.direct(ctx, a.room, a.owner) {
				// The answer came from a room that is no longer private,
				// so it may not be the owner's alone to give.
				refuse(outcomeNotDirect, textNoLongerPrivate)
				return
			}
			if err := v.ctl.Confirm(ctx, a.txn, yes); err != nil {
				_ = v.ctl.Cancel(ctx, a.txn)
				say(textConfirmFailed)
				end(outcomeConfirm, "error_kind", errKind(err))
				return
			}
			log.Info("in-chat verification answered", "match", yes)
			if yes {
				say(textConfirmed)
			}
		case <-a.replaced:
			say(textReplaced)
			end(outcomeReplaced)
			return
		case <-ctx.Done():
			select {
			case <-v.quit:
				// The bot is stopping; rihma drops the attempt with it.
				end(outcomeStopped)
				return
			default:
			}
			late, cancelLate := context.WithTimeout(context.Background(), 30*time.Second)
			_ = v.ctl.Cancel(late, a.txn)
			if v.direct(late, a.room, a.owner) {
				_ = v.post(late, a.room, reasonText("timeout"))
			}
			cancelLate()
			end(outcomeTimeout)
			return
		}
	}
}

// The fixed texts the bot posts in the owner's direct chat. None carries
// text from the server or the other side.
const (
	textElsewhere       = "A verification request from you reached me outside this chat, so I cancelled it. To verify me, start it here: open my profile in this chat and choose Verify."
	textConfirmed       = "Confirmed on my side. Confirm in Element as well if you have not, and the verification completes."
	textConfirmFailed   = "I could not record your answer, so I cancelled the verification. Nothing was signed. Start it again from my profile."
	textNoLongerPrivate = "Our chat where you started a verification is no longer private to the two of us, so I cancelled the verification there and signed nothing. Start it again from a private direct chat with me."
	textReplaced        = "A newer verification request replaced this one. Compare the emoji of the newest one only."
	textTrust           = "This check runs in chat, so it trusts the homeserver: a homeserver that tampers with the exchange could also change this message. Running -verify on the bot's host is the stronger check."
)

// emojiText is the message that shows the emoji and asks for the answer.
func emojiText(emoji []rihma.SASEmoji, device id.DeviceID) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Verification in progress with your device %s.\n\n", device)
	fmt.Fprintf(&b, "Compare these %d emoji with the ones Element shows, in the same order:\n\n", len(emoji))
	for i, e := range emoji {
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, e.Emoji, e.Description)
	}
	b.WriteString("\nReply **yes** here if they match, or **no** if they do not, and answer Element the same way.\n\n")
	b.WriteString(textTrust)
	return b.String()
}

// doneText reports a completed verification and the key the bot signed.
func doneText(key id.Ed25519) string {
	return fmt.Sprintf("Verified. I signed your master key:\n\n    %s\n\nElement signs me on its side, and then shows me as verified on each of your devices.", fingerprint(string(key)))
}

// acceptText explains why the bot could not accept a request.
func acceptText(err error) string {
	switch {
	case errors.Is(err, rihma.ErrNoCrossSigningKeys):
		return "I cannot verify you yet: my device does not hold my account's cross-signing keys. Whoever runs me must run the cross-signing setup on my host first. Nothing was signed."
	case errors.Is(err, rihma.ErrNoUserIdentity):
		return "I cannot verify you: your account has no cross-signing identity, so there is nothing to sign. Set up cross-signing (secure backup) in Element, then try again. Nothing was signed."
	}
	return "I could not start the verification, so I cancelled it. Nothing was signed. Try again, or verify me with -verify on my host."
}

// reasonKind is reason when it is one rihma documents, and "other"
// otherwise, so a log line never carries an unexpected value.
func reasonKind(reason string) string {
	switch reason {
	case "cancelled", "mismatch", "timeout", "rejected", "operation_failed", "overflow", "unsigned", "identity_changed":
		return reason
	}
	return "other"
}

// reasonText is the fixed text for an attempt that ended unverified,
// keyed by rihma's fixed reason. It never shows a server's reason.
func reasonText(reason string) string {
	switch reason {
	case "mismatch":
		return "The emoji were not confirmed as matching, so I cancelled the verification and signed nothing. If they did differ, someone may be interfering with the exchange: do not trust my account until a verification matches, and prefer -verify on my host."
	case "timeout":
		return "The verification timed out, and nothing was signed. Start it again from my profile when you are ready."
	case "rejected":
		return "The verification was cancelled from Element, and nothing was signed."
	case "cancelled":
		return "The verification was cancelled, and nothing was signed."
	case "operation_failed":
		return "The verification failed on my side, and nothing was signed. Try again, or verify me with -verify on my host."
	case "overflow":
		return "Too many verification events arrived at once, so I stopped this verification. Nothing was signed. Try again."
	case "unsigned":
		return "Your device finished the verification without covering your master key, so nothing was signed. Verify from an Element with cross-signing set up."
	case "identity_changed":
		return "Your cross-signing identity changed during the verification, so I did not sign it. If you reset it on purpose, verify again."
	}
	return "The verification ended without completing, and nothing was signed."
}

// chatVerify is the session's in-chat verification on ctl.
func (s *session) chatVerify(ctl sasControl) *chatVerify {
	return &chatVerify{
		ctl:     ctl,
		self:    s.c.UserID,
		owners:  s.opts.Owners,
		direct:  s.isDirect,
		dmOf:    s.existingDirect,
		post:    s.postText,
		spawn:   s.spawn,
		log:     s.log,
		timeout: verifyTimeout,
		quit:    make(chan struct{}),
	}
}

// existingDirect returns a direct chat with u that is private to the
// two of them, or "" when there is none. Unlike direct, it creates
// nothing.
func (s *session) existingDirect(ctx context.Context, u id.UserID) id.RoomID {
	s.dmMu.Lock()
	defer s.dmMu.Unlock()
	if room, ok := s.dms[u]; ok && s.privateRoom(ctx, room, u) {
		return room
	}
	room, err := findDirect(ctx, s.c, u)
	if err != nil {
		return ""
	}
	return room
}

// postText sends text to room.
func (s *session) postText(ctx context.Context, room id.RoomID, text string) error {
	_, err := s.send(ctx, room, "", "", airut.Message{Text: text})
	return err
}
