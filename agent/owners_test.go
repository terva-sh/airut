package agent

import (
	"context"
	"path/filepath"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/agent/sessions"
	"terva.sh/airut/airuttest"
)

// A claim recorded by another process on the same session database, as
// airut claim records one beside a running bot, reaches the bot through
// its Agent.
func TestAgentReadsOwnerClaims(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	e := newEnv(t, openDB(t, path), nil, airut.Config{})

	cli, err := sessions.Open(ctx, path, sessions.Options{NoSweep: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, _, err := cli.ClaimOwner(ctx, string(airuttest.Platform), "owner", "ops"); err != nil {
		t.Fatal(err)
	}

	got, err := e.agent.ClaimedOwners(ctx)
	if err != nil || len(got) != 1 || got[0].Platform != airuttest.Platform || got[0].ID != "owner" || got[0].At.IsZero() {
		t.Fatalf("ClaimedOwners = %+v, %v", got, err)
	}
	if has, err := e.bot.HasOwner(ctx, airuttest.Platform); !has || err != nil {
		t.Errorf("HasOwner with a claim in the store = %v, %v", has, err)
	}
}
