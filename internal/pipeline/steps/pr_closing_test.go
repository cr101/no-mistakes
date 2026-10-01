package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
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

func readPRBodyFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
