package airut

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"terva.sh/airut/changelog"
)

// VersionStore records the last version of the bot announced in each of
// Config.Changelog's channels. terva.sh/airut/agent's Agent is one, and
// FileVersionStore is one for a bot without an agent. Its methods must
// be safe for concurrent use.
type VersionStore interface {
	// AnnouncedVersion returns the version last recorded for ch, or ""
	// when there is none.
	AnnouncedVersion(ctx context.Context, ch Channel) (string, error)
	// SaveAnnouncedVersion records version for ch, replacing the record
	// there was.
	SaveAnnouncedVersion(ctx context.Context, ch Channel, version string) error
}

// versionStore is the store announcements are recorded in, or nil for
// none.
func (b *Bot) versionStore() VersionStore {
	if b.cfg.Changelog != nil && b.cfg.Changelog.Store != nil {
		return b.cfg.Changelog.Store
	}
	s, _ := b.cfg.Agent.(VersionStore)
	return s
}

// FileVersionStore returns a VersionStore kept in a small JSON file at
// path, for a bot without an agent. A missing file holds no records, and
// its directory must exist. Each save writes a new file beside it and
// renames it into place, so a crash leaves the old records or the new
// ones, never half of either. It is safe for concurrent use within one
// process; two processes must not share a file.
func FileVersionStore(path string) VersionStore { return &fileVersionStore{path: path} }

type fileVersionStore struct {
	path string
	mu   sync.Mutex
}

// versionFile is FileVersionStore's file: the version announced in
// each room, by the room's ChannelScope.
type versionFile struct {
	Announced map[string]string `json:"announced"`
}

func (s *fileVersionStore) read() (versionFile, error) {
	f := versionFile{Announced: map[string]string{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("airut: the version store %s: %w", s.path, err)
	}
	if f.Announced == nil {
		f.Announced = map[string]string{}
	}
	return f, nil
}

func (s *fileVersionStore) AnnouncedVersion(_ context.Context, ch Channel) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return "", err
	}
	return f.Announced[ChannelScope(ch)], nil
}

func (s *fileVersionStore) SaveAnnouncedVersion(_ context.Context, ch Channel, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	f.Announced[ChannelScope(ch)] = version
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".*")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	done = true
	return nil
}

// announcing reports whether Config.Changelog lists a channel on p.
func (b *Bot) announcing(p Platform) bool {
	if b.changelog == nil {
		return false
	}
	for _, ch := range b.cfg.Changelog.Channels {
		if ch.Platform == p {
			return true
		}
	}
	return false
}

// The outcomes airut_changelog_posts_total counts, one per listed
// channel each time its platform connects.
const (
	announcePosted      = "posted"        // the notes were posted and recorded
	announceSendFailed  = "send_failed"   // the post failed, so nothing was recorded
	announceRecordFail  = "record_failed" // the post went out and its record failed
	announceStoreFailed = "store_failed"  // the record could not be read
	announceFirstRun    = "first_run"     // a room with no record was recorded
	announceNoSection   = "no_section"    // the notes have no section for the version
	announceNotRelease  = "not_release"   // the version is a build, not a release
	announceNotNewer    = "not_newer"     // the version is not above the record
)

// announce tells each channel listed for conn's platform what changed
// since the version last announced there, for ready.
func (b *Bot) announce(ctx context.Context, conn Conn) {
	b.announceMu.Lock()
	defer b.announceMu.Unlock()
	store := b.versionStore()
	version := strings.TrimSpace(b.cfg.Version)
	for _, ch := range b.cfg.Changelog.Channels {
		if ch.Platform != conn.Platform() {
			continue
		}
		outcome := b.announceIn(ctx, conn, store, ch, version)
		b.metrics.Add("airut_changelog_posts_total", 1, "platform", string(ch.Platform), "outcome", outcome)
	}
}

// announceIn applies the announcement rules to one channel and returns
// the outcome. Only versions and room ids reach the log: the notes are
// the bot's own text, but they are chat all the same.
func (b *Bot) announceIn(ctx context.Context, conn Conn, store VersionStore, ch Channel, version string) string {
	log := b.cfg.Logger.With("platform", string(ch.Platform), "channel", ch.ID)
	if !changelog.IsRelease(version) {
		// A build between releases runs quietly and leaves every record
		// alone, so the release after it is announced in full.
		return announceNotRelease
	}
	// Canonical drops build metadata, so a hyphen left marks a
	// prerelease.
	v := changelog.Canonical(version)
	_, covered := b.changelog.Section(v)
	if !covered && strings.Contains(v, "-") {
		// A prerelease with no notes of its own leaves the record where
		// it is, so its release posts properly later.
		log.Info("changelog not announced: no section for the prerelease", "version", v)
		return announceNoSection
	}
	recorded, err := store.AnnouncedVersion(ctx, ch)
	if err != nil {
		log.Warn("changelog record unreadable", "version", v, "error_kind", errorKind(err))
		return announceStoreFailed
	}
	recorded = changelog.Canonical(recorded) // "" for none, or for a record that is not a version
	save := func(outcome string) string {
		if err := store.SaveAnnouncedVersion(ctx, ch, v); err != nil {
			log.Warn("changelog record not saved", "version", v, "error_kind", errorKind(err))
			if outcome == announcePosted {
				return announceRecordFail
			}
			return announceStoreFailed
		}
		return outcome
	}
	switch {
	case recorded == "":
		log.Info("changelog recorded without an announcement: the room has no record", "version", v)
		return save(announceFirstRun)
	case changelog.Compare(v, recorded) <= 0:
		return announceNotNewer
	case !covered:
		log.Warn("changelog not announced: no section for the release", "version", v, "from", recorded)
		return save(announceNoSection)
	}
	sections := b.changelog.Between(recorded, v)
	if !strings.Contains(v, "-") {
		// A release's own notes cover its prereleases', so a room is not
		// told them twice: only a prerelease announces prerelease notes.
		sections = slices.DeleteFunc(sections, func(s changelog.Section) bool { return strings.Contains(s.Version, "-") })
	}
	text := b.cfg.Changelog.announcement(v, sections)
	key := "airut-changelog/" + v + "/" + ChannelScope(ch)
	if _, err := conn.Send(ctx, ch, Message{Text: text, Key: key}); err != nil {
		log.Warn("changelog announcement failed", "version", v, "from", recorded, "error_kind", errorKind(err))
		return announceSendFailed
	}
	log.Info("changelog announced", "version", v, "from", recorded, "text_bytes", len(text))
	return save(announcePosted)
}

// announcement is the one message that tells a room of an update to
// version, from sections, newest first. One section reads as the
// changelog command shows it; several each open with their version.
func (c *Changelog) announcement(version string, sections []changelog.Section) string {
	var out strings.Builder
	out.WriteString(c.heading(version))
	if len(sections) == 1 {
		if n := sections[0].Notes; n != "" {
			out.WriteString("\n" + n)
		}
		return out.String()
	}
	for _, s := range sections {
		out.WriteString("\n\n**" + s.Version + "**")
		if s.Notes != "" {
			out.WriteString("\n" + s.Notes)
		}
	}
	return out.String()
}
