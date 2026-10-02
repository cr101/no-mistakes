package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A full-body update without --closes must still carry an author's
// standalone closing line over; dropping it silently unlinks the issue.
func TestPRStep_PreservesAuthorClosingLineWithoutCloses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "https://github.com/test/repo/pull/99")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte("## Summary\n\nCloses owner/repo#7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, bodyFile)
	if strings.Count(body, "Closes owner/repo#7") != 1 || !strings.Contains(body, "## Issues") {
		t.Fatalf("author closing line not preserved exactly once:\n%s", body)
	}
}

// Without --closes and without author closing lines, the body never gains a
// closing reference, even though the intent names an issue.
func TestPRStep_NeverInfersClosingReferenceFromIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.UserIntent = "Implement issue #95"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, bodyFile)
	if strings.Contains(body, "## Issues") || len(extractClosingKeywordLines(body)) != 0 {
		t.Fatalf("closing reference inferred without --closes:\n%s", body)
	}
}

// pr.template bodies keep author text verbatim, so the requested reference
// lives in the regenerated appendix, and is not repeated once the author's
// own text closes the same issue.
func TestPRTemplate_ClosesRendersOnceAcrossCreateAndAuthorEditedUpdate(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"42", "owner/repo#5"}); err != nil {
		t.Fatal(err)
	}
	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	created := readPRBodyFile(t, bodyFile)
	parts, err := parsePROwnedBody(created)
	if err != nil {
		t.Fatalf("created body ownership: %v\n%s", err, created)
	}
	for _, line := range []string{"Closes #42", "Closes owner/repo#5"} {
		if strings.Count(created, line) != 1 || !strings.Contains(parts.appendix, line) {
			t.Fatalf("created body should carry %q once in the appendix:\n%s", line, created)
		}
	}

	// The author now closes #42 in their own text; the next refresh keeps it
	// verbatim and stops rendering a second reference to the same issue.
	edited := strings.Replace(created, "# Overview\n", "# Overview\n\nFixes #42\n", 1)
	if err := os.WriteFile(bodyFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ = fakeGH(t, "https://github.com/test/repo/pull/99")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated := readPRBodyFile(t, bodyFile)
	if strings.Count(updated, "Fixes #42") != 1 || strings.Contains(updated, "Closes #42") {
		t.Fatalf("#42 should be closed once, by the author's line:\n%s", updated)
	}
	if strings.Count(updated, "Closes owner/repo#5") != 1 {
		t.Fatalf("requested reference dropped on update:\n%s", updated)
	}
}

// A later run on the same branch carries no --closes of its own, but its
// owned update replaces the appendix that rendered the earlier reference; the
// reference must survive exactly once instead of being silently unlinked.
func TestPRTemplate_LaterRunWithoutClosesKeepsAppendixReference(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95"}); err != nil {
		t.Fatal(err)
	}
	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if body := readPRBodyFile(t, bodyFile); strings.Count(body, "Closes #95") != 1 {
		t.Fatalf("created body should close #95 once:\n%s", body)
	}

	next, _, _ := templateTestContext(t)
	env, _ = fakeGH(t, "https://github.com/test/repo/pull/99")
	next.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	if _, err := (&PRStep{}).Execute(next); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated := readPRBodyFile(t, bodyFile)
	parts, err := parsePROwnedBody(updated)
	if err != nil {
		t.Fatalf("updated body ownership: %v\n%s", err, updated)
	}
	if strings.Count(updated, "Closes #95") != 1 || !strings.Contains(parts.appendix, "Closes #95") {
		t.Fatalf("later run dropped or repeated the closing reference:\n%s", updated)
	}
}

// The Issues section is derived from the author text actually being written:
// an author removing their own closing line between reads gets the requested
// reference back in the appendix instead of a body that closes nothing.
func TestPROwnershipUpdateRecomputesIssuesFromLatestAuthorText(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	content, err := composeOwnedPRContent(prOwnedBody{before: "## Summary\n\nFixes #95"}, "", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	host := &ownershipRaceHost{body: content.Body, read: func(h *ownershipRaceHost) error {
		if h.reads == 1 {
			h.body = strings.Replace(h.body, "Fixes #95", "No longer closing here.", 1)
		}
		return nil
	}}
	sctx := &pipeline.StepContext{Ctx: context.Background(), ClosingIssueRefs: []string{"95"}}
	if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent(content), "", "", appendix, 0); err != nil {
		t.Fatal(err)
	}
	if host.writes != 1 || strings.Contains(host.body, "Fixes #95") || strings.Count(host.body, "Closes #95") != 1 {
		t.Fatalf("written body must close #95 once after the author's edit: writes=%d\n%s", host.writes, host.body)
	}
}

func readPRBodyFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// An author can add a closing line while the update is drafting. The
// full-body write must carry it over from the re-read rather than erase it.
func TestPRStep_CarriesOverClosingLineAddedDuringDrafting(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "https://github.com/test/repo/pull/99")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte("## Summary\n\nFixes #42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(bodyFile, []byte("## Summary\n\nFixes #42\nFixes #4.\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("the author edit must land while the update is drafting")
	}
	lines := extractClosingKeywordLines(readPRBodyFile(t, bodyFile))
	if got := strings.Join(lines, "|"); got != "Fixes #42|Fixes #4." {
		t.Fatalf("closing lines = %q, want both author lines exactly once", got)
	}
}

// The pre-write re-read is authoritative both ways: a closing line the author
// removed while the update was drafting must not be written back, or the PR
// would close an issue the author no longer means to close.
func TestPRStep_DropsClosingLineRemovedDuringDrafting(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "https://github.com/test/repo/pull/99")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte("## Summary\n\nFixes #42\nCloses #7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(bodyFile, []byte("## Summary\n\nCloses #7\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95"}); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("the author edit must land while the update is drafting")
	}
	body := readPRBodyFile(t, bodyFile)
	if got := strings.Join(extractClosingKeywordLines(body), "|"); got != "Closes #7|Closes #95" {
		t.Fatalf("closing lines = %q, want the removed Fixes #42 gone and the rest once:\n%s", got, body)
	}
}

// A run carrying --closes must not skip publication because the PR host is
// unavailable: nothing would close the requested issues. Without --closes the
// step still skips.
func TestPRStep_ClosesFailsInsteadOfSkippingWhenHostUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *pipeline.StepContext)
	}{
		{"unauthenticated", func(t *testing.T, sctx *pipeline.StepContext) {
			sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_AUTH_ERR=not logged in")
		}},
		{"no host", func(_ *testing.T, sctx *pipeline.StepContext) {
			sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
		}},
	} {
		for _, refs := range [][]string{nil, {"42"}} {
			t.Run(fmt.Sprintf("%s/closes=%v", tc.name, refs), func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, headSHA := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
				tc.setup(t, sctx)
				if refs != nil {
					if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, refs); err != nil {
						t.Fatal(err)
					}
				}
				outcome, err := (&PRStep{}).Execute(sctx)
				if refs == nil {
					if err != nil || outcome == nil || !outcome.Skipped {
						t.Fatalf("outcome = %+v, err = %v; want skipped", outcome, err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "--closes requires publishing a pull request") {
					t.Fatalf("outcome = %+v, err = %v; want failure", outcome, err)
				}
			})
		}
	}
}

// Trailing punctuation and ordered-list items are ordinary ways to write a
// standalone closing line; both extraction and verification must see them.
func TestClosingKeywordLinesAcceptPunctuationAndOrderedLists(t *testing.T) {
	body := "Fixes #4.\n1. Closes #5\n2) Resolves owner/repo#6;\nThis fixes #7 partly.\n"
	got := strings.Join(extractClosingKeywordLines(body), "|")
	if got != "Fixes #4.|1. Closes #5|2) Resolves owner/repo#6;" {
		t.Fatalf("extractClosingKeywordLines() = %q", got)
	}
	sctx := &pipeline.StepContext{
		ClosingIssueRefs:      []string{"4", "5", "owner/repo#6"},
		PreservedClosingLines: []string{"Fixes #4.", "1. Closes #5"},
	}
	if err := verifyClosingIssuesInBody(body, sctx); err != nil {
		t.Fatalf("verifyClosingIssuesInBody() = %v", err)
	}
	if err := verifyClosingIssuesInBody("Fixes #5.\n", sctx); err == nil {
		t.Fatal("verifyClosingIssuesInBody() accepted a body missing preserved lines")
	}
}

// `95` and `<own repo>#95` name the same issue, so it renders exactly once,
// and an author's qualified line for the PR's own repository covers `95`.
func TestPRStep_OwnRepositoryQualifiedReferenceRendersOnce(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95", "Test/Repo#95", "other/repo#95"}); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	lines := extractClosingKeywordLines(readPRBodyFile(t, bodyFile))
	if got := strings.Join(lines, "|"); got != "Closes #95|Closes other/repo#95" {
		t.Fatalf("closing lines = %q, want #95 and other/repo#95 exactly once each", got)
	}

	authored := &pipeline.StepContext{ClosingIssueRefs: []string{"95"}, Repo: sctx.Repo}
	if got := issuesSection(authored, "Closes TEST/repo#95"); got != "" {
		t.Fatalf("issuesSection() = %q, want the author line to cover #95", got)
	}
	if err := verifyClosingIssuesInBody("Closes test/repo#95\n", authored); err != nil {
		t.Fatalf("verifyClosingIssuesInBody() = %v", err)
	}
}
