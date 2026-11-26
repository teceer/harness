package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	gosync "sync"
	"time"
)

// The board's deploy queue: recent Vercel deployments of the team the Vercel
// CLI is logged into. The token stays on this machine; the page only sees
// the list. HARNESS_VERCEL_TEAM overrides the CLI's current team.

type vercelDeploy struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	State     string `json:"state"`  // QUEUED, INITIALIZING, BUILDING, READY, ERROR, CANCELED
	Target    string `json:"target"` // production or "" (preview)
	Branch    string `json:"branch,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Author    string `json:"author,omitempty"`
	Created   int64  `json:"created"` // ms
	BuildAt   int64  `json:"buildingAt,omitempty"`
	ReadyAt   int64  `json:"ready,omitempty"`
	Inspector string `json:"inspector,omitempty"`
}

var (
	vercelMu    gosync.Mutex
	vercelAt    time.Time
	vercelCache []byte
)

func vercelCLIDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "com.vercel.cli")
}

func vercelCredentials() (token, team string, err error) {
	var auth struct {
		Token string `json:"token"`
	}
	raw, err := os.ReadFile(filepath.Join(vercelCLIDir(), "auth.json"))
	if err != nil {
		return "", "", fmt.Errorf("vercel CLI not logged in: %w", err)
	}
	json.Unmarshal(raw, &auth)
	var cfg struct {
		CurrentTeam string `json:"currentTeam"`
	}
	if raw, err := os.ReadFile(filepath.Join(vercelCLIDir(), "config.json")); err == nil {
		json.Unmarshal(raw, &cfg)
	}
	team = cfg.CurrentTeam
	if t := os.Getenv("HARNESS_VERCEL_TEAM"); t != "" {
		team = t
	}
	if auth.Token == "" {
		return "", "", fmt.Errorf("vercel CLI has no token")
	}
	return auth.Token, team, nil
}

// vercel returns the last deployments, cached for a few seconds so several
// open boards do not multiply the API calls.
func (s *webServer) vercel(w http.ResponseWriter, r *http.Request) {
	vercelMu.Lock()
	defer vercelMu.Unlock()
	if time.Since(vercelAt) < 8*time.Second && vercelCache != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write(vercelCache)
		return
	}
	out, err := fetchVercel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	data, _ := json.Marshal(map[string]any{"deployments": out, "at": time.Now().UnixMilli()})
	vercelAt, vercelCache = time.Now(), data
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func fetchVercel() ([]vercelDeploy, error) {
	token, team, err := vercelCredentials()
	if err != nil {
		return nil, err
	}
	q := url.Values{"limit": {"30"}}
	if team != "" {
		q.Set("teamId", team)
	}
	req, _ := http.NewRequest("GET", "https://api.vercel.com/v6/deployments?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vercel: %s", res.Status)
	}
	var body struct {
		Deployments []struct {
			UID          string            `json:"uid"`
			Name         string            `json:"name"`
			State        string            `json:"state"`
			Target       *string           `json:"target"`
			Created      int64             `json:"created"`
			BuildingAt   int64             `json:"buildingAt"`
			Ready        int64             `json:"ready"`
			InspectorURL string            `json:"inspectorUrl"`
			Meta         map[string]string `json:"meta"`
			Creator      struct {
				Username string `json:"username"`
			} `json:"creator"`
		} `json:"deployments"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]vercelDeploy, 0, len(body.Deployments))
	for _, d := range body.Deployments {
		target := ""
		if d.Target != nil {
			target = *d.Target
		}
		author := d.Meta["githubCommitAuthorName"]
		if author == "" {
			author = d.Creator.Username
		}
		out = append(out, vercelDeploy{
			ID: d.UID, Project: d.Name, State: d.State, Target: target,
			Branch: d.Meta["githubCommitRef"], Commit: firstLine(d.Meta["githubCommitMessage"]),
			Author: author, Created: d.Created, BuildAt: d.BuildingAt, ReadyAt: d.Ready,
			Inspector: d.InspectorURL,
		})
	}
	return out, nil
}
