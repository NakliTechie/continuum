package repoinfo

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxInboxItems = 50
	MaxChecks     = 100
	maxTitle      = 200
	maxBody       = 16 << 10
)

// InboxItem is one row in a github_inbox_result lane.
type InboxItem struct {
	Kind      string   `json:"kind"`
	Repo      string   `json:"repo"`
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	URL       string   `json:"url"`
	Author    string   `json:"author,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	Draft     bool     `json:"draft,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

type Inbox struct {
	State           string      `json:"state"`
	ReviewRequested []InboxItem `json:"review_requested"`
	Assigned        []InboxItem `json:"assigned"`
	Truncated       bool        `json:"truncated"`
}

type Item struct {
	Kind                string `json:"kind"`
	Repo                string `json:"repo"`
	Number              int    `json:"number"`
	Title               string `json:"title"`
	URL                 string `json:"url"`
	Author              string `json:"author,omitempty"`
	State               string `json:"state"`
	Body                string `json:"body"`
	BodyTruncated       bool   `json:"body_truncated"`
	HeadRef             string `json:"head_ref,omitempty"`
	HeadRepo            string `json:"head_repo,omitempty"`
	CrossRepository     bool   `json:"cross_repository,omitempty"`
	MaintainerCanModify bool   `json:"maintainer_can_modify,omitempty"`
}

type ItemResult struct {
	State string `json:"state"`
	Item  *Item  `json:"item,omitempty"`
}

type PR struct {
	Number          int    `json:"number"`
	URL             string `json:"url"`
	State           string `json:"state"`
	Draft           bool   `json:"draft"`
	ReviewDecision  string `json:"review_decision,omitempty"`
	MergeState      string `json:"merge_state,omitempty"`
	HeadRef         string `json:"head_ref"`
	HeadOID         string `json:"head_oid,omitempty"`
	CrossRepository bool   `json:"cross_repository"`
}

type Check struct {
	Name     string `json:"name"`
	Workflow string `json:"workflow,omitempty"`
	Bucket   string `json:"bucket"`
	State    string `json:"state"`
	Link     string `json:"link,omitempty"`
}

type PRResult struct {
	State       string         `json:"state"`
	PR          *PR            `json:"pr,omitempty"`
	Checks      []Check        `json:"checks,omitempty"`
	Summary     map[string]int `json:"summary,omitempty"`
	ChecksTotal int            `json:"checks_total"`
	Truncated   bool           `json:"truncated"`
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// ValidRepo reports whether s is an owner/name pair safe to pass to gh -R.
func ValidRepo(s string) bool {
	return repoRe.MatchString(s) && !strings.HasPrefix(s, "-") && !strings.Contains(s, "..")
}

// ghEnv keeps the daemon's environment (so gh finds its own login) but makes
// every call non-interactive and colourless, and stops gh from consulting a
// pager, browser or update check.
func (r *Reader) ghEnv() []string {
	env := withoutPrefix(r.baseEnv(), "GIT_", "GH_PROMPT", "GH_PAGER", "GH_BROWSER", "GH_DEBUG")
	return append(env,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_NO_EXTENSION_UPDATE_NOTIFIER=1",
		"GH_SPINNER_DISABLED=1",
		"GH_PAGER=cat",
		"NO_COLOR=1",
		"CLICOLOR=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

func (r *Reader) ghBinary() string {
	if r.GHPath != "" {
		return r.GHPath
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	// A launchd service often runs with a minimal PATH.
	for _, p := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh", "/usr/bin/gh"} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return ""
}

// classify maps a failed gh run to a fixed state. It reads stderr only to
// choose the state; the text is never returned.
func classifyGH(res runResult) string {
	if res.timedOut {
		return StateTimeout
	}
	if res.startErr != nil {
		return StateGHMissing
	}
	e := strings.ToLower(res.stderr)
	switch {
	case res.code == 4 || strings.Contains(e, "gh auth login") || strings.Contains(e, "not logged in") || strings.Contains(e, "authentication"):
		return StateNotAuthenticated
	case strings.Contains(e, "rate limit"):
		return StateRateLimited
	case strings.Contains(e, "no pull requests found"):
		return StateNoPR
	case strings.Contains(e, "not a git repository"):
		return StateNotRepository
	case strings.Contains(e, "no git remotes") || strings.Contains(e, "none of the git remotes") || strings.Contains(e, "could not determine base repo"):
		return StateNoRemote
	case strings.Contains(e, "could not resolve to") || strings.Contains(e, "not found"):
		return StateNotFound
	}
	return StateUnavailable
}

func (r *Reader) gh(ctx context.Context, dir string, args ...string) (runResult, string) {
	bin := r.ghBinary()
	if bin == "" {
		return runResult{}, StateGHMissing
	}
	if dir == "" {
		dir = "/"
	}
	res := r.run(ctx, ghDeadline, dir, r.ghEnv(), bin, args...)
	if res.timedOut {
		return res, StateTimeout
	}
	if res.overflow {
		return res, StateUnavailable
	}
	return res, ""
}

// safeURL keeps only https links, so a client never opens another scheme.
func safeURL(u string) string {
	if strings.HasPrefix(u, "https://") && len(u) <= 2048 {
		return u
	}
	return ""
}

func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s, true
}

type ghSearchRow struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	UpdatedAt  string `json:"updatedAt"`
	IsDraft    bool   `json:"isDraft"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (r *Reader) search(ctx context.Context, kind string) ([]InboxItem, bool, string) {
	var args []string
	if kind == "pr" {
		args = []string{"search", "prs", "--review-requested=@me", "--state=open", "--sort=updated", "--limit", strconv.Itoa(MaxInboxItems + 1),
			"--json", "number,title,repository,url,updatedAt,isDraft,author"}
	} else {
		args = []string{"search", "issues", "--assignee=@me", "--state=open", "--sort=updated", "--limit", strconv.Itoa(MaxInboxItems + 1),
			"--json", "number,title,repository,url,updatedAt,labels,author"}
	}
	res, state := r.gh(ctx, "", args...)
	if state != "" {
		return nil, false, state
	}
	if res.code != 0 {
		return nil, false, classifyGH(res)
	}
	var rows []ghSearchRow
	if err := json.Unmarshal(res.stdout, &rows); err != nil {
		return nil, false, StateUnavailable
	}
	truncated := len(rows) > MaxInboxItems
	if truncated {
		rows = rows[:MaxInboxItems]
	}
	items := make([]InboxItem, 0, len(rows))
	for _, row := range rows {
		title, _ := cut(row.Title, maxTitle)
		it := InboxItem{Kind: kind, Repo: row.Repository.NameWithOwner, Number: row.Number, Title: title,
			URL: safeURL(row.URL), Author: row.Author.Login, UpdatedAt: row.UpdatedAt, Draft: row.IsDraft}
		if kind == "issue" {
			it.Labels = []string{}
			for _, l := range row.Labels {
				it.Labels = append(it.Labels, l.Name)
			}
		}
		items = append(items, it)
	}
	return items, truncated, ""
}

// Inbox runs the two inbox searches: pull requests awaiting the login's
// review, and issues assigned to it. Explicit refresh only; the search API
// allows 30 requests a minute.
func (r *Reader) Inbox(ctx context.Context) Inbox {
	if !r.acquire() {
		return Inbox{State: StateBusy}
	}
	defer r.release()
	prs, t1, st := r.search(ctx, "pr")
	if st != "" {
		return Inbox{State: st}
	}
	issues, t2, st := r.search(ctx, "issue")
	if st != "" {
		return Inbox{State: st}
	}
	return Inbox{State: StateOK, ReviewRequested: prs, Assigned: issues, Truncated: t1 || t2}
}

// GetItem fetches one issue or pull request for a handoff draft.
func (r *Reader) GetItem(ctx context.Context, kind, repo string, number int) ItemResult {
	if (kind != "issue" && kind != "pr") || !ValidRepo(repo) || number <= 0 {
		return ItemResult{State: StateNotFound}
	}
	if !r.acquire() {
		return ItemResult{State: StateBusy}
	}
	defer r.release()
	fields := "number,title,state,author,url,body"
	sub := "issue"
	if kind == "pr" {
		sub = "pr"
		fields += ",headRefName,headRepository,isCrossRepository,maintainerCanModify"
	}
	res, st := r.gh(ctx, "", sub, "view", strconv.Itoa(number), "-R", repo, "--json", fields)
	if st != "" {
		return ItemResult{State: st}
	}
	if res.code != 0 {
		return ItemResult{State: classifyGH(res)}
	}
	var v struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		URL    string `json:"url"`
		Body   string `json:"body"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		HeadRefName    string `json:"headRefName"`
		HeadRepository *struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"headRepository"`
		IsCrossRepository   bool `json:"isCrossRepository"`
		MaintainerCanModify bool `json:"maintainerCanModify"`
	}
	if err := json.Unmarshal(res.stdout, &v); err != nil {
		return ItemResult{State: StateUnavailable}
	}
	title, _ := cut(v.Title, maxTitle)
	body, bodyCut := cut(v.Body, maxBody)
	it := &Item{Kind: kind, Repo: repo, Number: v.Number, Title: title, URL: safeURL(v.URL), Author: v.Author.Login,
		State: v.State, Body: body, BodyTruncated: bodyCut}
	if kind == "pr" {
		it.HeadRef = v.HeadRefName
		if v.HeadRepository != nil {
			it.HeadRepo = v.HeadRepository.NameWithOwner
		}
		it.CrossRepository = v.IsCrossRepository
		it.MaintainerCanModify = v.MaintainerCanModify
	}
	return ItemResult{State: StateOK, Item: it}
}

// SessionPR finds the pull request for the branch checked out in dir and
// reads its checks. `gh pr checks` exits 8 while checks are pending and 1 when
// one failed; both still print the JSON, so both are data.
func (r *Reader) SessionPR(ctx context.Context, dir string) PRResult {
	if dir == "" || !filepath.IsAbs(dir) {
		return PRResult{State: StateNoDirectory}
	}
	if !r.acquire() {
		return PRResult{State: StateBusy}
	}
	defer r.release()
	res, st := r.gh(ctx, dir, "pr", "view", "--json", "number,url,state,isDraft,reviewDecision,mergeStateStatus,headRefName,headRefOid,isCrossRepository")
	if st != "" {
		return PRResult{State: st}
	}
	if res.code != 0 {
		return PRResult{State: classifyGH(res)}
	}
	var v struct {
		Number            int    `json:"number"`
		URL               string `json:"url"`
		State             string `json:"state"`
		IsDraft           bool   `json:"isDraft"`
		ReviewDecision    string `json:"reviewDecision"`
		MergeStateStatus  string `json:"mergeStateStatus"`
		HeadRefName       string `json:"headRefName"`
		HeadRefOid        string `json:"headRefOid"`
		IsCrossRepository bool   `json:"isCrossRepository"`
	}
	if err := json.Unmarshal(res.stdout, &v); err != nil || v.Number <= 0 {
		return PRResult{State: StateUnavailable}
	}
	out := PRResult{State: StateOK, PR: &PR{Number: v.Number, URL: safeURL(v.URL), State: v.State, Draft: v.IsDraft,
		ReviewDecision: v.ReviewDecision, MergeState: v.MergeStateStatus, HeadRef: v.HeadRefName,
		HeadOID: v.HeadRefOid, CrossRepository: v.IsCrossRepository},
		Summary: map[string]int{"pass": 0, "fail": 0, "pending": 0, "skipping": 0, "cancel": 0}}

	res, st = r.gh(ctx, dir, "pr", "checks", strconv.Itoa(v.Number), "--json", "name,state,bucket,workflow,link")
	if st != "" {
		// The pull request itself was read; report it with no checks.
		return out
	}
	if res.code != 0 && res.code != 1 && res.code != 8 {
		return out
	}
	var checks []Check
	if err := json.Unmarshal(res.stdout, &checks); err != nil {
		// gh prints a plain sentence and exits 1 when a branch has no checks.
		return out
	}
	out.ChecksTotal = len(checks)
	for i, c := range checks {
		if _, ok := out.Summary[c.Bucket]; ok {
			out.Summary[c.Bucket]++
		}
		if i < MaxChecks {
			c.Name, _ = cut(c.Name, maxTitle)
			c.Workflow, _ = cut(c.Workflow, maxTitle)
			c.Link = safeURL(c.Link)
			out.Checks = append(out.Checks, c)
		}
	}
	out.Truncated = len(checks) > MaxChecks
	return out
}
