package repoinfo

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// TestLiveReadOnlyGH runs the real gh against GitHub, read-only, when
// REPOINFO_LIVE=1. REPOINFO_LIVE_DIR names a checkout for the session PR read.
// It prints the shapes it got; it never writes to GitHub.
func TestLiveReadOnlyGH(t *testing.T) {
	if os.Getenv("REPOINFO_LIVE") != "1" {
		t.Skip("set REPOINFO_LIVE=1 to call the real gh (read-only)")
	}
	r := New(2)
	ctx := context.Background()
	show := func(name string, v any) {
		b, _ := json.Marshal(v)
		if len(b) > 600 {
			b = append(b[:600], "…"...)
		}
		t.Logf("%s: %s", name, b)
	}
	in := r.Inbox(ctx)
	show("inbox", in)
	if in.State != StateOK {
		t.Fatalf("inbox state %q", in.State)
	}
	it := r.GetItem(ctx, "pr", "cli/cli", 14783)
	show("item cli/cli#14783", it)
	if it.State != StateOK || it.Item == nil || it.Item.CrossRepository == nil || !*it.Item.CrossRepository || it.Item.HeadRepo == "" {
		t.Fatalf("item %+v", it)
	}
	is := r.GetItem(ctx, "issue", "NakliTechie/menagerie", 2)
	show("item menagerie#2", is)
	if is.State != StateOK {
		t.Fatalf("issue state %q", is.State)
	}
	if dir := os.Getenv("REPOINFO_LIVE_DIR"); dir != "" {
		pr := r.SessionPR(ctx, dir)
		show("session_pr", pr)
		g := r.GitStatus(ctx, dir)
		show("git_status", g)
	}
}
