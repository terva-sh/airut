package matrix

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"maunium.net/go/mautrix"
	"terva.sh/rihma"

	"terva.sh/airut"
)

// OpenDirect refuses a user it cannot open a room with before it makes
// any request. The client here has no homeserver; the live suite covers
// opening a room.
func TestOpenDirectRefuses(t *testing.T) {
	s := &session{
		c:   &rihma.Client{Client: &mautrix.Client{UserID: "@bot:example.org"}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, u := range []airut.User{
		{Platform: airut.Discord, ID: "@alice:example.org"},
		{Platform: airut.Matrix},
		{Platform: airut.Matrix, ID: "alice"},
		{Platform: airut.Matrix, ID: "@bot:example.org"},
	} {
		if _, err := s.OpenDirect(context.Background(), u); err == nil {
			t.Errorf("OpenDirect(%+v) succeeded", u)
		}
	}
}
