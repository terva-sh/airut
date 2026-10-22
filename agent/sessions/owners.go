package sessions

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"terva.sh/airut/agent/sessions/dbq"
)

// OwnerClaim is an owner recorded with airut claim: a user id on a
// platform, when it was claimed, and who ran the command.
type OwnerClaim struct {
	// Platform is the platform's name, such as matrix or discord.
	Platform string
	// ID is the owner's user id on Platform.
	ID string
	At time.Time
	// Actor is free text naming who claimed it.
	Actor string
}

// ClaimOwner records platform and id as an owner, claimed by actor now.
// It returns the claim in force afterwards, and whether this call made
// it: an id already claimed keeps its first claim.
func (d *DB) ClaimOwner(ctx context.Context, platform, id, actor string) (OwnerClaim, bool, error) {
	var (
		got   OwnerClaim
		added bool
	)
	err := d.tx(ctx, func(q *dbq.Queries) error {
		n, err := q.ClaimOwner(ctx, dbq.ClaimOwnerParams{Platform: platform, UserID: id, ClaimedAt: d.now().UnixMilli(), Actor: actor})
		if err != nil {
			return err
		}
		added = n > 0
		row, err := q.OwnerClaim(ctx, dbq.OwnerClaimParams{Platform: platform, UserID: id})
		if err != nil {
			return err
		}
		got = ownerClaim(row)
		return nil
	})
	if err != nil {
		return OwnerClaim{}, false, err
	}
	return got, added, nil
}

// UnclaimOwner removes the claim on platform and id, and reports whether
// there was one.
func (d *DB) UnclaimOwner(ctx context.Context, platform, id string) (bool, error) {
	n, err := d.q.UnclaimOwner(ctx, dbq.UnclaimOwnerParams{Platform: platform, UserID: id})
	return n > 0, err
}

// ClaimedOwner returns the claim on platform and id, and whether there
// is one.
func (d *DB) ClaimedOwner(ctx context.Context, platform, id string) (OwnerClaim, bool, error) {
	row, err := d.q.OwnerClaim(ctx, dbq.OwnerClaimParams{Platform: platform, UserID: id})
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerClaim{}, false, nil
	}
	if err != nil {
		return OwnerClaim{}, false, err
	}
	return ownerClaim(row), true, nil
}

// OwnerClaims returns every claim, by platform and then id.
func (d *DB) OwnerClaims(ctx context.Context) ([]OwnerClaim, error) {
	rows, err := d.q.OwnerClaims(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]OwnerClaim, len(rows))
	for i, r := range rows {
		out[i] = ownerClaim(r)
	}
	return out, nil
}

func ownerClaim(r dbq.OwnerClaim) OwnerClaim {
	return OwnerClaim{Platform: r.Platform, ID: r.UserID, At: time.UnixMilli(r.ClaimedAt), Actor: r.Actor}
}
