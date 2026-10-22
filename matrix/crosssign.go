package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"terva.sh/rihma"

	"terva.sh/airut/config"
)

// CrossSigningState is what the account's cross-signing says of the
// bot's device.
type CrossSigningState struct {
	// Identity reports that the account has a cross-signing identity.
	Identity bool
	// DeviceSigned reports that the bot's device is signed by it, so
	// clients show the bot's messages as sent from a verified device, and
	// a client that shares room keys only with verified devices shares
	// them with the bot.
	DeviceSigned bool
}

// ErrNotRunning is returned by Adapter.CrossSigning when the adapter is
// not connected.
var ErrNotRunning = errors.New("matrix: the adapter is not running")

// CrossSigning reads, from the server, whether the account has a
// cross-signing identity and whether the running bot's device is signed
// by it, for a bot that shows its state on a status command.
func (a *Adapter) CrossSigning(ctx context.Context) (CrossSigningState, error) {
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil {
		return CrossSigningState{}, ErrNotRunning
	}
	return crossSigningState(ctx, c)
}

func crossSigningState(ctx context.Context, c *rihma.Client) (CrossSigningState, error) {
	v, err := c.Verification(ctx)
	if err != nil {
		return CrossSigningState{}, err
	}
	return CrossSigningState{Identity: v != rihma.NoIdentity, DeviceSigned: v == rihma.Verified}, nil
}

// checkCrossSigning runs at start. It logs whether the device is signed
// and, when it is not and the configuration holds the recovery key, signs
// it from the account's secret storage, so a device made afresh signs
// itself. Nothing here stops the bot, and no line holds the key.
func checkCrossSigning(ctx context.Context, c *rihma.Client, key config.Secret, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, err := c.Verification(ctx)
	if err != nil {
		log.Warn("could not read the device's cross-signing state", "error_kind", errKind(err))
		return
	}
	switch {
	case v == rihma.Verified:
		log.Info("the device is cross-signed", "device", string(c.DeviceID))
		return
	case v == rihma.NoIdentity:
		log.Warn("the account has no cross-signing identity, so clients show the bot's messages as unverified; run the cross-signing setup",
			"device", string(c.DeviceID))
		return
	case key.IsZero():
		log.Warn("the device is not cross-signed; run the cross-signing setup, or set [matrix] recovery_key", "device", string(c.DeviceID))
		return
	}
	if err := c.RestoreFromRecoveryKey(ctx, key.Value()); err != nil {
		log.Warn("could not sign the device with the recovery key", "device", string(c.DeviceID), "error_kind", errKind(err))
		return
	}
	signed, err := c.DeviceVerified(ctx)
	if err != nil {
		log.Warn("signed the device, and could not read it back", "device", string(c.DeviceID), "error_kind", errKind(err))
		return
	}
	log.Info("signed the device with the recovery key", "device", string(c.DeviceID), "signed", signed)
}

// CrossSignIO is the operator's terminal, for CrossSign.
type CrossSignIO struct {
	// Out receives what the setup tells the operator, a new recovery key
	// included. It must be the operator's terminal, never a log.
	Out io.Writer
	// RecoveryKey asks the operator for the account's recovery key, when
	// the account already has an identity this device is not signed by.
	// It should return when ctx is done. Nil means the setup says so and
	// stops.
	RecoveryKey func(ctx context.Context) (string, error)
}

// ErrNeedRecoveryKey is returned by CrossSign when the account already
// has a cross-signing identity and no recovery key was given to restore
// it with.
var ErrNeedRecoveryKey = errors.New("matrix: the account has a cross-signing identity; restore it with its recovery key")

// CrossSign is the one-time cross-signing setup, which an operator runs
// with the bot stopped, against the bot's own configuration, so that it
// signs the device the bot uses. Running it beside the bot would make two
// processes write one device's crypto store.
//
//   - For an account with no identity it creates one, signs the device,
//     and writes the recovery key to io.Out, once. Keep it: it is the only
//     way to sign a later device, and with [matrix] recovery_key set the
//     bot uses it to sign a device made afresh.
//   - For an account with an identity it never replaces it, because
//     everyone who trusts the bot would stop. It asks io.RecoveryKey for
//     the key and signs the device with it.
//   - A device already signed is left as it is.
//
// It logs nothing. It returns the state it leaves, and an error when
// io.Out could not be written. It writes to io.Out before it creates an
// identity, so an output that fails stops it with the account unchanged.
func CrossSign(ctx context.Context, cfg *config.Matrix, io CrossSignIO) (CrossSigningState, error) {
	c, err := open(ctx, cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		return CrossSigningState{}, err
	}
	defer c.Close()
	out := &operator{w: io.Out}
	st, err := crossSign(ctx, c, cfg.Password, io.RecoveryKey, out)
	if err == nil && out.err != nil {
		err = fmt.Errorf("matrix: writing to the operator: %w", out.err)
	}
	return st, err
}

// operator writes lines to the operator and keeps the first error, after
// which it writes nothing.
type operator struct {
	w   io.Writer
	err error
}

func (o *operator) say(format string, args ...any) {
	if o.err == nil {
		_, o.err = fmt.Fprintf(o.w, format+"\n", args...)
	}
}

func crossSign(ctx context.Context, c *rihma.Client, password config.Secret, recoveryKey func(context.Context) (string, error), out *operator) (CrossSigningState, error) {
	say := out.say
	v, err := c.Verification(ctx)
	if err != nil {
		return CrossSigningState{}, fmt.Errorf("matrix: reading the device's cross-signing state: %w", err)
	}
	switch v {
	case rihma.Verified:
		say("Device %s of %s is already signed by the account's cross-signing identity. Nothing to do.", c.DeviceID, c.UserID)
		return CrossSigningState{Identity: true, DeviceSigned: true}, nil
	case rihma.NoIdentity:
		say("%s has no cross-signing identity. Creating one, and signing device %s with it.", c.UserID, c.DeviceID)
		if out.err != nil {
			return CrossSigningState{}, fmt.Errorf("matrix: writing to the operator, before creating anything: %w", out.err)
		}
		key, err := c.CreateRecoveryKey(ctx, password.Value())
		if err == nil {
			say("Created the identity and signed the device.")
			say("")
			say("Recovery key, shown this once:")
			say("")
			say("    %s", key)
			say("")
			say("Keep it as you keep the bot's password. It signs any later device of the")
			say("bot's, and with [matrix] recovery_key set the bot signs a new device itself.")
			if out.err != nil {
				// The key was in no other place, and running the setup
				// again finds the device signed and has no key to show.
				return CrossSigningState{Identity: true, DeviceSigned: true}, fmt.Errorf(
					"matrix: created a cross-signing identity and could not write its recovery key, which is now lost; reset cross-signing from a client logged in as the bot, then run the setup again: %w", out.err)
			}
			return crossSigningState(ctx, c)
		}
		if !errors.Is(err, rihma.ErrIdentityExists) {
			return CrossSigningState{}, fmt.Errorf("matrix: creating a cross-signing identity: %w", err)
		}
		// Another device made one meanwhile; restore it as below.
	}
	say("%s already has a cross-signing identity, and device %s is not signed by it.", c.UserID, c.DeviceID)
	say("The identity is never replaced: everyone who trusts the bot would stop trusting it.")
	if recoveryKey == nil {
		return CrossSigningState{Identity: true}, ErrNeedRecoveryKey
	}
	key, err := recoveryKey(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return CrossSigningState{Identity: true}, err
	}
	if err := c.RestoreFromRecoveryKey(ctx, key); err != nil {
		return CrossSigningState{Identity: true}, fmt.Errorf("matrix: signing the device with the recovery key: %w", err)
	}
	st, err := crossSigningState(ctx, c)
	if err == nil && st.DeviceSigned {
		say("Signed device %s with the recovery key.", c.DeviceID)
	}
	return st, err
}
