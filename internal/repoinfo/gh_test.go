package repoinfo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGH writes a shell script standing in for gh. script receives "$*" and
// decides stdout, stderr and exit code; every call is appended to calls.log.
func fakeGH(t *testing.T, script string) (*Reader, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	body := "#!/bin/sh\necho \"$* | GH_TOKEN=${GH_TOKEN-unset} PROMPT=$GH_PROMPT_DISABLED\" >> " + log + "\n" + script + "\n"
	p := filepath.Join(dir, "gh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New(2)
	r.GHPath = p
	r.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
	return r, log
}

func TestInbox(t *testing.T) {
	ctx := context.Background()
	t.Run("ok, both lanes, titles cut", func(t *testing.T) {
		long := strings.Repeat("t", 300)
		r, log := fakeGH(t, `case "$*" in
*"search prs"*) echo '[{"number":14783,"title":"`+long+`","url":"https://github.com/cli/cli/pull/14783","updatedAt":"2026-10-21T18:17:09Z","isDraft":true,"repository":{"nameWithOwner":"cli/cli"},"author":{"login":"m"}}]';;
*"search issues"*) echo '[{"number":2,"title":"Proposal","url":"javascript:alert(1)","updatedAt":"x","repository":{"nameWithOwner":"NakliTechie/menagerie"},"author":{"login":"N"},"labels":[{"name":"bug"}]}]';;
esac`)
		got := r.Inbox(ctx)
		if got.State != StateOK || len(got.ReviewRequested) != 1 || len(got.Assigned) != 1 || got.Truncated {
			t.Fatalf("got %+v", got)
		}
		pr := got.ReviewRequested[0]
		if pr.Kind != "pr" || pr.Repo != "cli/cli" || pr.Number != 14783 || len(pr.Title) != maxTitle || !pr.Draft {
			t.Fatalf("pr %+v", pr)
		}
		is := got.Assigned[0]
		if is.Kind != "issue" || is.URL != "" || len(is.Labels) != 1 {
			t.Fatalf("issue %+v (a non-https url must be dropped)", is)
		}
		calls, _ := os.ReadFile(log)
		if !strings.Contains(string(calls), "PROMPT=1") || strings.Contains(string(calls), "auth token") {
			t.Fatalf("calls %s", calls)
		}
	})
	t.Run("empty lanes are empty arrays", func(t *testing.T) {
		r, _ := fakeGH(t, `echo '[]'`)
		got := r.Inbox(ctx)
		if got.State != StateOK || got.ReviewRequested == nil || got.Assigned == nil {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("lane cap", func(t *testing.T) {
		row := `{"number":1,"title":"x","url":"https://github.com/a/b/pull/1","repository":{"nameWithOwner":"a/b"},"author":{"login":"a"}}`
		rows := strings.TrimSuffix(strings.Repeat(row+",", MaxInboxItems+1), ",")
		r, _ := fakeGH(t, `echo '[`+rows+`]'`)
		got := r.Inbox(ctx)
		if len(got.ReviewRequested) != MaxInboxItems || !got.Truncated {
			t.Fatalf("len %d truncated %v", len(got.ReviewRequested), got.Truncated)
		}
	})
	cases := []struct{ name, script, want string }{
		{"logged out", `echo 'To get started with GitHub CLI, please run:  gh auth login' >&2; exit 4`, StateNotAuthenticated},
		{"rate limited", `echo 'API rate limit exceeded for user ID 1' >&2; exit 1`, StateRateLimited},
		{"other failure", `echo 'HTTP 502: secret-looking ghp_abc' >&2; exit 1`, StateUnavailable},
		{"bad json", `echo 'not json'`, StateUnavailable},
		{"oversized output", `head -c 2000000 /dev/zero | tr '\0' 'a'`, StateUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := fakeGH(t, c.script)
			got := r.Inbox(ctx)
			if got.State != c.want || got.ReviewRequested != nil {
				t.Fatalf("got %+v, want %s", got, c.want)
			}
		})
	}
	t.Run("gh missing", func(t *testing.T) {
		r := New(1)
		r.GHPath = filepath.Join(t.TempDir(), "gh")
		if got := r.Inbox(ctx).State; got != StateGHMissing {
			t.Fatalf("state %q", got)
		}
	})
}

func TestInboxTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the gh deadline")
	}
	r, _ := fakeGH(t, `exec sleep 30`)
	start := time.Now()
	got := r.Inbox(context.Background())
	if got.State != StateTimeout || time.Since(start) > ghDeadline+3*time.Second {
		t.Fatalf("state %q after %v", got.State, time.Since(start))
	}
}

func TestGetItem(t *testing.T) {
	ctx := context.Background()
	t.Run("validates input before running gh", func(t *testing.T) {
		r, log := fakeGH(t, `echo '{}'`)
		for _, c := range []struct {
			kind, repo string
			n          int
		}{{"issue", "-R/x", 1}, {"issue", "a/b c", 1}, {"issue", "a/b", 0}, {"commit", "a/b", 1}, {"pr", "../x", 1}, {"pr", "a", 1}} {
			if got := r.GetItem(ctx, c.kind, c.repo, c.n).State; got != StateNotFound {
				t.Fatalf("%+v: state %q", c, got)
			}
		}
		if _, err := os.Stat(log); err == nil {
			t.Fatal("gh ran for an invalid request")
		}
	})
	t.Run("forked pull request", func(t *testing.T) {
		r, log := fakeGH(t, `echo '{"number":14783,"title":"t","state":"OPEN","url":"https://github.com/cli/cli/pull/14783","body":"`+strings.Repeat("b", maxBody+10)+`","author":{"login":"m"},"headRefName":"release-download-verify","headRepository":{"nameWithOwner":"mrecachinas/cli"},"isCrossRepository":true,"maintainerCanModify":true}'`)
		got := r.GetItem(ctx, "pr", "cli/cli", 14783)
		it := got.Item
		if got.State != StateOK || it == nil || !it.CrossRepository || it.HeadRepo != "mrecachinas/cli" || !it.MaintainerCanModify || len(it.Body) != maxBody || !it.BodyTruncated {
			t.Fatalf("got %+v %+v", got, it)
		}
		calls, _ := os.ReadFile(log)
		if !strings.Contains(string(calls), "pr view 14783 -R cli/cli --json") {
			t.Fatalf("calls %s", calls)
		}
	})
	t.Run("not found", func(t *testing.T) {
		r, _ := fakeGH(t, `echo 'GraphQL: Could not resolve to an issue or pull request with the number of 9.' >&2; exit 1`)
		if got := r.GetItem(ctx, "issue", "a/b", 9).State; got != StateNotFound {
			t.Fatalf("state %q", got)
		}
	})
}

func TestSessionPR(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	view := `*"pr view"*) echo '{"number":7,"url":"https://github.com/a/b/pull/7","state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","mergeStateStatus":"BLOCKED","headRefName":"feat","headRefOid":"abc","isCrossRepository":false}';;`
	checks := `[{"name":"build","state":"SUCCESS","bucket":"pass","workflow":"CI","link":"https://github.com/a/b/actions/runs/1"},{"name":"lint","state":"FAILURE","bucket":"fail","workflow":"CI","link":"https://x"},{"name":"e2e","state":"PENDING","bucket":"pending","workflow":"CI","link":"file:///etc/passwd"}]`
	t.Run("no directory", func(t *testing.T) {
		r, _ := fakeGH(t, `exit 0`)
		if got := r.SessionPR(ctx, "").State; got != StateNoDirectory {
			t.Fatalf("state %q", got)
		}
	})
	t.Run("no pull request", func(t *testing.T) {
		r, _ := fakeGH(t, `echo 'no pull requests found for branch "main"' >&2; exit 1`)
		if got := r.SessionPR(ctx, dir); got.State != StateNoPR || got.PR != nil {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("no remote", func(t *testing.T) {
		r, _ := fakeGH(t, `echo 'no git remotes found' >&2; exit 1`)
		if got := r.SessionPR(ctx, dir).State; got != StateNoRemote {
			t.Fatalf("state %q", got)
		}
	})
	for _, c := range []struct {
		name string
		exit string
		want map[string]int
	}{
		{"pending checks exit 8", "exit 8", map[string]int{"pass": 1, "fail": 1, "pending": 1}},
		{"failing checks exit 1", "exit 1", map[string]int{"pass": 1, "fail": 1, "pending": 1}},
		{"all reported exit 0", "exit 0", map[string]int{"pass": 1, "fail": 1, "pending": 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, log := fakeGH(t, `case "$*" in
`+view+`
*"pr checks"*) echo '`+checks+`'; `+c.exit+`;;
esac`)
			got := r.SessionPR(ctx, dir)
			if got.State != StateOK || got.PR == nil || got.PR.Number != 7 || got.ChecksTotal != 3 || len(got.Checks) != 3 {
				t.Fatalf("got %+v", got)
			}
			for k, v := range c.want {
				if got.Summary[k] != v {
					t.Fatalf("summary %v", got.Summary)
				}
			}
			if got.Checks[2].Link != "" {
				t.Fatalf("non-https link kept: %q", got.Checks[2].Link)
			}
			calls, _ := os.ReadFile(log)
			if !strings.Contains(string(calls), "pr checks 7 --json") {
				t.Fatalf("calls %s", calls)
			}
		})
	}
	t.Run("no checks reported", func(t *testing.T) {
		r, _ := fakeGH(t, `case "$*" in
`+view+`
*"pr checks"*) echo "no checks reported on the 'feat' branch" >&2; exit 1;;
esac`)
		got := r.SessionPR(ctx, dir)
		if got.State != StateOK || got.ChecksTotal != 0 || got.PR == nil {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("check cap", func(t *testing.T) {
		row := `{"name":"c","state":"SUCCESS","bucket":"pass","link":"https://x"}`
		rows := strings.TrimSuffix(strings.Repeat(row+",", MaxChecks+5), ",")
		r, _ := fakeGH(t, `case "$*" in
`+view+`
*"pr checks"*) echo '[`+rows+`]';;
esac`)
		got := r.SessionPR(ctx, dir)
		if len(got.Checks) != MaxChecks || got.ChecksTotal != MaxChecks+5 || !got.Truncated || got.Summary["pass"] != MaxChecks+5 {
			t.Fatalf("listed %d total %d", len(got.Checks), got.ChecksTotal)
		}
	})
}
