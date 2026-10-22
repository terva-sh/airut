package agent

import (
	"context"

	"terva.sh/airut"
)

var _ airut.VersionStore = (*Agent)(nil)

// AnnouncedVersion implements airut.VersionStore.
func (a *Agent) AnnouncedVersion(ctx context.Context, ch airut.Channel) (string, error) {
	return a.db.AnnouncedVersion(ctx, airut.ChannelScope(ch))
}

// SaveAnnouncedVersion implements airut.VersionStore.
func (a *Agent) SaveAnnouncedVersion(ctx context.Context, ch airut.Channel, version string) error {
	return a.db.SaveAnnouncedVersion(ctx, airut.ChannelScope(ch), version)
}
