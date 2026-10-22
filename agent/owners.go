package agent

import (
	"context"

	"terva.sh/airut"
)

var _ airut.OwnerClaims = (*Agent)(nil)

// ClaimedOwners implements airut.OwnerClaims: the owners claimed with
// airut claim, kept in the session store.
func (a *Agent) ClaimedOwners(ctx context.Context) ([]airut.Claim, error) {
	list, err := a.db.OwnerClaims(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]airut.Claim, len(list))
	for i, c := range list {
		out[i] = airut.Claim{Platform: airut.Platform(c.Platform), ID: c.ID, At: c.At}
	}
	return out, nil
}
