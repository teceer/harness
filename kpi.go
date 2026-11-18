package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
)

// The KPI strip of the board: how much of the Claude limits is used, how
// many tokens all sessions burn per minute and how long a task takes from
// its first commit to the merge. A source that cannot be read gives an
// error the page shows as "brak danych", never an empty strip.

type kpiReport struct {
	Usage  []kpiUsage `json:"usage"`
	UsageE string     `json:"usageError,omitempty"`
	Tokens []kpiRate  `json:"tokens"`
	PRs    *kpiPRs    `json:"prs,omitempty"`
	PRsE   string     `json:"prsError,omitempty"`
}

// ---- usage of the Claude limits ----

// The status line (~/.claude/statusline-command.sh) writes the rate_limits
// object it gets from Claude Code to ~/.harness/usage-<account>.json, the
// account being the base name of CLAUDE_CONFIG_DIR.

type kpiLimit struct {
	Pct     float64 `json:"pct"`
	ResetAt string  `json:"resetAt,omitempty"` // RFC 3339
}

type kpiUsage struct {
	Account  string    `json:"account"`
	At       string    `json:"at"` // when the status line wrote it, RFC 3339
	FiveHour *kpiLimit `json:"fiveHour,omitempty"`
	Week     *kpiLimit `json:"week,omitempty"`
}

// parseUsage reads one usage file: {"at": <unix s>, "rate_limits": {...}}.
func parseUsage(account string, data []byte, modTime time.Time) (kpiUsage, error) {
	var raw struct {
		At         json.RawMessage `json:"at"`
		RateLimits map[string]struct {
			Used    *float64        `json:"used_percentage"`
			ResetAt json.RawMessage `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return kpiUsage{}, err
	}
	u := kpiUsage{Account: account}
	at := parseStamp(raw.At)
	if at.IsZero() {
		at = modTime
	}
	u.At = at.UTC().Format(time.RFC3339)
	limit := func(key string) *kpiLimit {
		l, ok := raw.RateLimits[key]
		if !ok || l.Used == nil {
			return nil
		}
		out := &kpiLimit{Pct: *l.Used}
		if t := parseStamp(l.ResetAt); !t.IsZero() {
			out.ResetAt = t.UTC().Format(time.RFC3339)
		}
		return out
	}
	u.FiveHour, u.Week = limit("five_hour"), limit("seven_day")
	if u.FiveHour == nil && u.Week == nil {
		return kpiUsage{}, errors.New("no rate limits")
	}
	return u, nil
}

// parseStamp reads a time given as unix seconds, milliseconds or RFC 3339.
func parseStamp(raw json.RawMessage) time.Time {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return time.Time{}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f > 1e12 {
			return time.UnixMilli(int64(f))
		}
		return time.Unix(int64(f), 0)
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

// readUsage reads every account's usage file in dir.
func readUsage(dir string) ([]kpiUsage, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "usage-*.json"))
	sort.Strings(paths)
	var out []kpiUsage
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		info, _ := os.Stat(p)
		var mod time.Time
		if info != nil {
			mod = info.ModTime()
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "usage-"), ".json")
		if u, err := parseUsage(name, data, mod); err == nil {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("status line has not written " + filepath.Join(dir, "usage-*.json") + " yet")
	}
	return out, nil
}

// ---- tokens per minute ----

// kpiTokens splits what the model read into fresh input (with cache
// writes) and cache reads, which cost a tenth and dwarf everything else.
type kpiTokens struct {
	In    int `json:"in"`
	Cache int `json:"cache"`
	Out   int `json:"out"`
}

// kpiKeep is how many minutes of per-minute buckets a scan keeps.
const kpiKeep = 90

// prunePerMin drops the minute buckets older than kpiKeep.
func (sc *transcriptScan) prunePerMin(now time.Time) {
	from := now.Unix()/60 - kpiKeep
	for m := range sc.perMin {
		if m < from {
			delete(sc.perMin, m)
		}
	}
}

type kpiRate struct {
	Minutes int       `json:"minutes"`
	Sum     kpiTokens `json:"sum"`    // tokens in the window
	PerMin  kpiTokens `json:"perMin"` // the same, per minute
}

// tokenRates sums the minute buckets of every scan over the whole minutes
// of each window before now (the unfinished minute would lower the rate).
func tokenRates(scans []*transcriptScan, now time.Time, windows ...int) []kpiRate {
	out := make([]kpiRate, 0, len(windows))
	cur := now.Unix() / 60
	for _, w := range windows {
		r := kpiRate{Minutes: w}
		for _, sc := range scans {
			for m, b := range sc.perMin {
				if m >= cur-int64(w) && m < cur {
					r.Sum.In += b.In
					r.Sum.Cache += b.Cache
					r.Sum.Out += b.Out
				}
			}
		}
		r.PerMin = kpiTokens{In: r.Sum.In / w, Cache: r.Sum.Cache / w, Out: r.Sum.Out / w}
		out = append(out, r)
	}
	return out
}

// recentTranscripts lists the transcripts of every session that moved
// since since, with its subagents (<dir>/<id>/subagents/*.jsonl).
func recentTranscripts(all []store.Session, since time.Time) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, x := range all {
		if x.TranscriptPath == "" || x.UpdatedAt.Before(since) {
			continue
		}
		add(x.TranscriptPath)
		subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(x.TranscriptPath, ".jsonl"), "subagents", "*.jsonl"))
		for _, p := range subs {
			if info, err := os.Stat(p); err == nil && !info.ModTime().Before(since) {
				add(p)
			}
		}
	}
	return out
}

// ---- time to merge ----

type kpiPR struct {
	Repo        string    `json:"repo"`
	Number      int       `json:"number"`
	FirstCommit time.Time `json:"-"`
	Opened      time.Time `json:"-"`
	Merged      time.Time `json:"-"`
}

type kpiPRs struct {
	Count      int      `json:"count"`
	Repos      []string `json:"repos"`
	MedianLead float64  `json:"medianLead"` // first commit → merge, minutes
	MeanLead   float64  `json:"meanLead"`   // first commit → merge, minutes
	MedianOpen float64  `json:"medianOpen"` // PR opened → merge, minutes
	CheckedAt  string   `json:"checkedAt"`  // RFC 3339
}

// prStats is the lead time of the PRs merged in the 24 h before now.
func prStats(prs []kpiPR, repos []string, now time.Time) kpiPRs {
	out := kpiPRs{Repos: repos, CheckedAt: now.UTC().Format(time.RFC3339)}
	var lead, open []float64
	for _, p := range prs {
		if p.Merged.IsZero() || p.Merged.Before(now.Add(-24*time.Hour)) || p.Merged.After(now) {
			continue
		}
		// The work started with the first commit, or with the PR when it
		// was opened before any commit (or gh gave none).
		start := p.FirstCommit
		if start.IsZero() || !p.Opened.IsZero() && p.Opened.Before(start) {
			start = p.Opened
		}
		if !start.IsZero() {
			lead = append(lead, p.Merged.Sub(start).Minutes())
		}
		if !p.Opened.IsZero() {
			open = append(open, p.Merged.Sub(p.Opened).Minutes())
		}
		out.Count++
	}
	out.MedianLead, out.MeanLead = median(lead), mean(lead)
	out.MedianOpen = median(open)
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

var ghRemoteRe = regexp.MustCompile(`github\.com[:/]([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

// githubRepo turns a remote URL into owner/name; "" for other hosts.
func githubRepo(remote string) string {
	if m := ghRemoteRe.FindStringSubmatch(strings.TrimSpace(remote)); m != nil {
		return m[1] + "/" + m[2]
	}
	return ""
}

// prQuery asks for the merged PRs of a search with the first commit of
// each (`gh pr list --json commits` asks for every commit's authors and
// GitHub refuses it on a busy repository).
const prQuery = `query($q:String!,$endCursor:String){search(query:$q,type:ISSUE,first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{... on PullRequest{number createdAt mergedAt commits(first:1){nodes{commit{authoredDate committedDate}}}}}}}`

// parseGhPRs reads the pages of `gh api graphql --paginate --slurp` with prQuery.
func parseGhPRs(repo string, data []byte) ([]kpiPR, error) {
	var pages []struct {
		Data struct {
			Search struct {
				Nodes []struct {
					Number  int       `json:"number"`
					Created time.Time `json:"createdAt"`
					Merged  time.Time `json:"mergedAt"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								Authored  time.Time `json:"authoredDate"`
								Committed time.Time `json:"committedDate"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &pages); err != nil {
		return nil, err
	}
	var out []kpiPR
	for _, pg := range pages {
		for _, r := range pg.Data.Search.Nodes {
			if r.Number == 0 {
				continue
			}
			p := kpiPR{Repo: repo, Number: r.Number, Opened: r.Created, Merged: r.Merged}
			for _, c := range r.Commits.Nodes {
				for _, t := range []time.Time{c.Commit.Authored, c.Commit.Committed} {
					if !t.IsZero() && (p.FirstCommit.IsZero() || t.Before(p.FirstCommit)) {
						p.FirstCommit = t
					}
				}
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// prCache keeps the merged PRs a few minutes: gh is slow and rate limited.
var prCache struct {
	gosync.Mutex
	at      time.Time
	val     *kpiPRs
	err     string
	running bool
	done    chan struct{}
}

const prCacheFor = 5 * time.Minute

// liveRepos is the GitHub repositories the live sessions work in.
func liveRepos(all []store.Session) []string {
	dirs := map[string]bool{}
	for _, x := range all {
		if x.Live() && x.Cwd != "" {
			dirs[x.Cwd] = true
		}
	}
	set := map[string]bool{}
	for d := range dirs {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := exec.CommandContext(ctx, "git", "-C", d, "remote", "get-url", "origin").Output()
		cancel()
		if err == nil {
			if r := githubRepo(string(out)); r != "" {
				set[r] = true
			}
		}
	}
	repos := make([]string, 0, len(set))
	for r := range set {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos
}

// fetchPRs asks gh for the PRs merged in the last day of every repo.
func fetchPRs(repos []string, now time.Time) (*kpiPRs, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, errors.New("gh is not installed")
	}
	if len(repos) == 0 {
		return nil, errors.New("no live session works in a GitHub repository")
	}
	var all []kpiPR
	var failed []string
	day := now.Add(-24 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	for _, repo := range repos {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := exec.CommandContext(ctx, "gh", "api", "graphql", "--paginate", "--slurp",
			"-f", "q=repo:"+repo+" is:pr is:merged merged:>="+day, "-f", "query="+prQuery).Output()
		cancel()
		if err != nil {
			failed = append(failed, repo)
			continue
		}
		prs, err := parseGhPRs(repo, out)
		if err != nil {
			failed = append(failed, repo)
			continue
		}
		all = append(all, prs...)
	}
	if len(failed) == len(repos) {
		return nil, errors.New("gh failed for " + strings.Join(failed, ", "))
	}
	st := prStats(all, repos, now)
	return &st, nil
}

// cachedPRs returns the last PR stats and refreshes them in the background
// when they are older than prCacheFor. The
// first call waits a little so the board does not start empty.
func cachedPRs(all []store.Session) (*kpiPRs, string) {
	prCache.Lock()
	stale := time.Since(prCache.at) > prCacheFor
	if (stale || prCache.val == nil && prCache.err == "") && !prCache.running {
		prCache.running = true
		prCache.done = make(chan struct{})
		done := prCache.done
		go func() {
			val, err := fetchPRs(liveRepos(all), time.Now())
			prCache.Lock()
			// A failed refresh keeps the last good stats (their CheckedAt
			// shows their age); the error shows only while there are none.
			prCache.at = time.Now()
			if err == nil {
				prCache.val, prCache.err = val, ""
			} else if prCache.val == nil {
				prCache.err = err.Error()
			}
			prCache.running = false
			prCache.Unlock()
			close(done)
		}()
	}
	first := prCache.val == nil && prCache.err == "" && prCache.running
	done := prCache.done
	prCache.Unlock()
	if first {
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
	}
	prCache.Lock()
	defer prCache.Unlock()
	if prCache.val == nil && prCache.err == "" {
		return nil, "still asking gh"
	}
	return prCache.val, prCache.err
}

// boardKPI is the strip without the PRs (they come from cachedPRs, outside
// the scan lock). The scans are the cached ones, so only what transcripts
// added since the last refresh is read.
func boardKPI(all []store.Session, now time.Time, scan func(string) *transcriptScan) kpiReport {
	var r kpiReport
	var err error
	if r.Usage, err = readUsage(config.HarnessHome()); err != nil {
		r.UsageE = err.Error()
	}
	var scans []*transcriptScan
	for _, p := range recentTranscripts(all, now.Add(-time.Hour)) {
		scans = append(scans, scan(p))
	}
	r.Tokens = tokenRates(scans, now, 15, 60)
	return r
}
