//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prDraftPromptMarker opens the PR step's drafting prompt.
const prDraftPromptMarker = "Draft a pull request title and summary"

// slowPRDraftScenario answers every turn like the default scenario, but holds
// the PR drafting turn open long enough for the test to edit the live PR body
// on the forge while the pipeline is drafting.
const slowPRDraftScenario = `actions:
  - match: "` + prDraftPromptMarker + `"
    delay_ms: 8000
    structured:
      title: "feat: fakeagent change"
      body: "## What Changed\nfakeagent canned PR body"
  - structured:
      findings: []
      summary: no issues found
      risk_level: low
      risk_rationale: no risks detected in the diff
      risk_scope: source-or-external
      tested: ["fakeagent: simulated test run"]
      testing_summary: simulated tests passed
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: fakeagent change"
      body: "## What Changed\nfakeagent canned PR body"
`

func countPRDraftTurns(t *testing.T, agentLog string) int {
	t.Helper()
	data, err := os.ReadFile(agentLog)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), prDraftPromptMarker)
}

func issuesSection(body string) string {
	i := strings.Index(body, "## Issues")
	if i < 0 {
		return ""
	}
	rest := body[i+len("## Issues"):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// TestClosingIssueRefsAuthorRemovesLineDuringDrafting: the author deletes a
// standalone closing line from the live PR body while an ordinary update is
// drafting. The pre-write re-read is authoritative, so the removed line is
// not written back, while the requested reference survives.
func TestClosingIssueRefsAuthorRemovesLineDuringDrafting(t *testing.T) {
	dir := t.TempDir()
	scenario := filepath.Join(dir, "slow-pr-draft.yaml")
	if err := os.WriteFile(scenario, []byte(slowPRDraftScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/closes-drafting-removal"
	h.CommitChange(branch, "d.txt", "d\n", "add drafting fixture")
	wt := h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "drafting race", "--skip", "ci", "--closes", "95"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "create")

	// The author adds two standalone closing lines on the forge.
	state := readStatefulGH(t, statePath)
	state.PRs[branch].Body += "\n\nFixes #3\nResolves #4\n"
	writeStatefulGH(t, statePath, state)
	saveEvidence(t, "20-drafting-before-update-pr-body.md", state.PRs[branch].Body)

	h.Checkout("main")
	h.RemoveWorktree(wt)
	draftsBefore := countPRDraftTurns(t, h.AgentLog)
	h.CommitChange(branch, "d.txt", "d v2\n", "update drafting fixture")
	h.PushToGate(branch)

	// Once the update is drafting (its first read is done), the author
	// deletes "Fixes #3" from the live body.
	deadline := time.Now().Add(3 * time.Minute)
	for countPRDraftTurns(t, h.AgentLog) <= draftsBefore {
		if time.Now().After(deadline) {
			t.Fatal("ordinary update never started drafting")
		}
		time.Sleep(200 * time.Millisecond)
	}
	state = readStatefulGH(t, statePath)
	if !strings.Contains(state.PRs[branch].Body, "Fixes #3") {
		t.Fatalf("drafting started after the first read lost the author line; body:\n%s", state.PRs[branch].Body)
	}
	state.PRs[branch].Body = strings.Replace(state.PRs[branch].Body, "Fixes #3\n", "", 1)
	writeStatefulGH(t, statePath, state)

	waitPublished(t, h, branch, first.ID, "update during removal")
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "21-drafting-after-update-pr-body.md", body)
	if strings.Contains(body, "Fixes #3") {
		t.Errorf("closing line the author removed during drafting was written back; body:\n%s", body)
	}
	assertLinesOnce(t, "drafting removal", body, "## Issues", "Closes #95", "Resolves #4")
}

// TestClosingIssueRefsNeverCarriesPublishedIntent: run 1 publishes an intent
// containing its own "## Goal" heading and a "Fixes #12" line without
// --closes, which must be published neutralized (never a live closing
// reference); run 2 (a plain gate push) must not turn that published intent
// line into a carried-over closing reference.
func TestClosingIssueRefsNeverCarriesPublishedIntent(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/closes-intent-heading"
	h.CommitChange(branch, "i.txt", "i\n", "add intent fixture")
	wt := h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "## Goal\nRefactor X\nFixes #12", "--skip", "ci"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "intent run")
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "22-intent-heading-run1-pr-body.md", body)
	if issuesSection(body) != "" {
		t.Errorf("run without --closes rendered an Issues section; body:\n%s", body)
	}
	if exactLineCount(body, "Fixes `#12`") != 1 || exactLineCount(body, "Fixes #12") != 0 {
		t.Errorf("published intent line was not neutralized exactly once; body:\n%s", body)
	}

	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "i.txt", "i v2\n", "update intent fixture")
	h.PushToGate(branch)
	waitPublished(t, h, branch, first.ID, "plain push after intent")
	body = livePRBody(t, statePath, branch)
	saveEvidence(t, "23-intent-heading-run2-pr-body.md", body)
	if section := issuesSection(body); section != "" {
		t.Errorf("published intent line was carried into an Issues section %q; body:\n%s", section, body)
	}
	for _, keyword := range []string{"Closes #12"} {
		if strings.Contains(body, keyword) {
			t.Errorf("body contains %q; body:\n%s", keyword, body)
		}
	}
}
