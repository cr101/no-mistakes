package steps

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/closingissues"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

// Closing references in a PR body come from exactly two places, and nothing
// else may add one:
//
//   - explicit `axi run --closes` values, persisted on the run and claimed
//     once by the PR step (sctx.ClosingIssueRefs);
//   - standalone closing-keyword lines already in a live PR body that an
//     ordinary (unowned) update is about to replace, or in the Issues section
//     of an owned body's appendix that an owned update is about to replace
//     (sctx.PreservedClosingLines).
//
// Both render in one stable `## Issues` section. An author-preserving
// (pr.template) body keeps its author text verbatim, so only the explicit
// references the author text does not already close are added to its
// generated appendix. Closure is never inferred from intent, commits, or
// branch names.

const issuesSectionHeading = "## Issues"

const closingKeywordPattern = `(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved):?\s+(?:#[1-9][0-9]*|[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*)`

// closingKeywordLinePattern matches a line that consists only of GitHub
// closing keywords and their targets, optionally as a bullet or ordered list
// item and with trailing sentence punctuation. A reference
// inside prose ("this fixes #4 partly") is deliberately not preserved: it is
// not a standalone closing declaration.
var closingKeywordLinePattern = regexp.MustCompile(`(?i)^(?:(?:[-*+]|[0-9]+[.)])\s+)?` + closingKeywordPattern + `(?:\s*,\s*` + closingKeywordPattern + `)*[.;!]?$`)

var closingReferencePattern = regexp.MustCompile(`(?i)(?:[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*|#[1-9][0-9]*)`)

// extractClosingKeywordLines returns the distinct standalone closing-keyword
// lines of body, outside fenced and indented code blocks.
func extractClosingKeywordLines(body string) []string {
	seen := map[string]struct{}{}
	var lines []string
	var fence markdownFence
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		inFence := fence.marker != 0
		fence.consume(raw)
		if inFence || fence.marker != 0 || strings.HasPrefix(raw, "\t") || strings.HasPrefix(raw, "    ") {
			continue
		}
		line := strings.TrimSpace(raw)
		if !closingKeywordLinePattern.MatchString(line) {
			continue
		}
		key := strings.ToLower(line)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		lines = append(lines, line)
	}
	return lines
}

// closingTargets returns the canonical refs ("42", "owner/repo#42") closed
// by the given closing-keyword lines. A reference qualified with repo, the
// PR's own repository, is the bare number.
func closingTargets(lines []string, repo string) map[string]struct{} {
	targets := map[string]struct{}{}
	for _, line := range lines {
		for _, target := range closingReferencePattern.FindAllString(line, -1) {
			target = closingissues.Localize(strings.TrimPrefix(target, "#"), repo)
			targets[strings.ToLower(target)] = struct{}{}
		}
	}
	return targets
}

// prRepository returns the owner/repository the PR lives in, or "" when it
// is unknown.
func prRepository(sctx *pipeline.StepContext) string {
	if sctx == nil {
		return ""
	}
	if sctx.Repo != nil {
		if repo := github.RepoSlug(sctx.Repo.UpstreamURL); repo != "" {
			return repo
		}
	}
	if sctx.Run != nil && sctx.Run.PRURL != nil {
		return github.RepoSlug(*sctx.Run.PRURL)
	}
	return ""
}

func closingLine(ref string) string {
	return "Closes " + closingissues.Target(ref)
}

// issuesSection renders the stable Issues section, or "" when there is
// nothing to render. Requested refs already closed by a preserved line or by
// authorText (live author content kept verbatim around it) are not repeated,
// so each reference appears exactly once.
func issuesSection(sctx *pipeline.StepContext, authorText string) string {
	if sctx == nil {
		return ""
	}
	lines := append([]string(nil), sctx.PreservedClosingLines...)
	present := closingTargets(append(append([]string(nil), lines...), extractClosingKeywordLines(authorText)...), prRepository(sctx))
	for _, ref := range sctx.ClosingIssueRefs {
		key := strings.ToLower(ref)
		if _, exists := present[key]; exists {
			continue
		}
		present[key] = struct{}{}
		lines = append(lines, closingLine(ref))
	}
	if len(lines) == 0 {
		return ""
	}
	return issuesSectionHeading + "\n\n" + strings.Join(lines, "\n")
}

// appendIssuesSection appends section to body as its final block.
func appendIssuesSection(body, section string) string {
	if section == "" {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return section
	}
	return body + "\n\n" + section
}

// claimClosingIssueRefs samples the run's --closes references for this PR
// body. The claim is what makes a late reattach (`axi run --closes` against a
// run already composing its PR) fail closed instead of reporting a reference
// that never reaches the body.
func claimClosingIssueRefs(sctx *pipeline.StepContext, host scm.Host, provider scm.Provider) error {
	if sctx.DB == nil {
		return nil
	}
	refs, err := sctx.DB.ClaimClosingIssueRefsForPRBody(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("resolve closing issue references: %w", err)
	}
	// The PR's own repository names an issue one way, so `95` and
	// `owner/repo#95` render once.
	repo := prRepository(sctx)
	for i, ref := range refs {
		refs[i] = closingissues.Localize(ref, repo)
	}
	refs, err = closingissues.Normalize(refs)
	if err != nil {
		return fmt.Errorf("resolve closing issue references: %w", err)
	}
	sctx.ClosingIssueRefs = refs
	if len(refs) == 0 {
		return nil
	}
	if provider != scm.ProviderGitHub {
		return fmt.Errorf("render closing issues: --closes currently supports GitHub repositories only")
	}
	if _, ok := host.(scm.PRContentReader); !ok {
		return fmt.Errorf("verify closing issues: provider cannot read the current pull request body")
	}
	return nil
}

// refuseSkipWithClosingIssues fails a PR step that would be skipped while the
// run carries --closes references: skipping publishes no body that closes
// them. The claim makes a concurrent reattach fail closed too.
func refuseSkipWithClosingIssues(sctx *pipeline.StepContext, reason string) error {
	if sctx.DB == nil {
		return nil
	}
	refs, err := sctx.DB.ClaimClosingIssueRefsForPRBody(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("resolve closing issue references: %w", err)
	}
	if len(refs) == 0 {
		return nil
	}
	return fmt.Errorf("render closing issues: --closes requires publishing a pull request, but PR creation is unavailable: %s", reason)
}

// carryOverLatestClosingLines makes the re-read live body authoritative for
// which of the author's standalone closing lines still exist: lines added
// since the first read are carried over, and lines the author removed are
// dropped rather than written back. sctx.PreservedClosingLines is replaced
// with the latest extract and body's trailing Issues section is re-rendered.
func carryOverLatestClosingLines(sctx *pipeline.StepContext, body, latestBody string, bodyLimit int) (string, error) {
	latest := extractClosingKeywordLines(latestBody)
	if sameClosingLines(sctx.PreservedClosingLines, latest) {
		return body, nil
	}
	previous := issuesSection(sctx, "")
	if previous != "" {
		if !strings.HasSuffix(body, previous) {
			// Never guess where the section is; refuse rather than publish
			// a closing line the author removed.
			return "", fmt.Errorf("verify closing issues: cannot locate the Issues section to reconcile closing lines changed during drafting")
		}
		body = strings.TrimRight(strings.TrimSuffix(body, previous), "\n")
	}
	sctx.PreservedClosingLines = latest
	body = appendIssuesSection(body, issuesSection(sctx, ""))
	if len(body) > maxPullRequestBodyBytes || (bodyLimit > 0 && scm.PRBodyLen(body) > bodyLimit) {
		return "", fmt.Errorf("verify closing issues: PR body exceeds provider budget after carrying over closing lines added during drafting")
	}
	return body, nil
}

func sameClosingLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// verifyClosingIssues re-reads the live PR body and fails unless it still
// carries every requested reference and every preserved closing line. A
// missing reference is invisible at review time (the issue simply stays open
// after merge), so the step must not report success without this check.
func verifyClosingIssues(ctx context.Context, host scm.Host, pr *scm.PR, sctx *pipeline.StepContext) error {
	if sctx == nil || len(sctx.ClosingIssueRefs) == 0 && len(sctx.PreservedClosingLines) == 0 {
		return nil
	}
	reader, ok := host.(scm.PRContentReader)
	if !ok {
		return fmt.Errorf("verify closing issues: provider cannot read the current pull request body")
	}
	if pr == nil {
		return fmt.Errorf("verify closing issues: pull request identity is unavailable")
	}
	content, err := reader.GetPRContent(ctx, pr)
	if err != nil {
		return fmt.Errorf("verify closing issues: %w", err)
	}
	return verifyClosingIssuesInBody(content.Body, sctx)
}

func verifyClosingIssuesInBody(body string, sctx *pipeline.StepContext) error {
	if sctx == nil {
		return nil
	}
	present := extractClosingKeywordLines(body)
	presentLines := make(map[string]struct{}, len(present))
	for _, line := range present {
		presentLines[strings.ToLower(line)] = struct{}{}
	}
	for _, line := range sctx.PreservedClosingLines {
		if _, ok := presentLines[strings.ToLower(line)]; !ok {
			return fmt.Errorf("verify closing issues: pull request body dropped closing line %q", line)
		}
	}
	targets := closingTargets(present, prRepository(sctx))
	for _, ref := range sctx.ClosingIssueRefs {
		if _, ok := targets[strings.ToLower(ref)]; !ok {
			return fmt.Errorf("verify closing issues: pull request body is missing %s", closingLine(ref))
		}
	}
	return nil
}

// assembleDraftPRBody assembles an ordinary drafted body and appends the
// Issues section last, reserving its room up front so body-limit truncation
// sheds generated evidence rather than a closing reference.
func assembleDraftPRBody(sctx *pipeline.StepContext, whatChanged, riskLine, testingMD, pipelineMD string, bodyLimit int, provider scm.Provider) string {
	section := issuesSection(sctx, "")
	reserve := 0
	if section != "" {
		reserve = len("\n\n" + section)
	}
	if bodyLimit > 0 {
		if section != "" {
			reserve = scm.PRBodyLen("\n\n" + section)
		}
		if bodyLimit-reserve <= 0 {
			// The section alone does not fit. Omit it rather than pass a
			// non-positive (unlimited) budget; the pre-publication check then
			// refuses a body that would drop a closing reference.
			return assemblePRBody(sctx, whatChanged, riskLine, testingMD, pipelineMD, bodyLimit, provider)
		}
		return appendIssuesSection(assemblePRBody(sctx, whatChanged, riskLine, testingMD, pipelineMD, bodyLimit-reserve, provider), section)
	}
	return appendIssuesSection(buildPRBodyWithin(whatChanged, riskLine, testingMD, pipelineMD, sctx, provider, maxPullRequestBodyBytes-reserve), section)
}

// ownedPreservedClosingLines returns the closing lines of the Issues section
// an owned body's previous appendix rendered, minus those whose targets the
// verbatim author text already closes. Replacing that appendix must not
// silently unlink an issue a previous run's --closes added.
func ownedPreservedClosingLines(previousAppendix, authorText, repo string) []string {
	start := strings.LastIndex("\n"+previousAppendix, "\n"+issuesSectionHeading+"\n")
	if start < 0 {
		return nil
	}
	closed := closingTargets(extractClosingKeywordLines(authorText), repo)
	var lines []string
	for _, line := range extractClosingKeywordLines(previousAppendix[start:]) {
		covered := true
		for target := range closingTargets([]string{line}, repo) {
			if _, ok := closed[target]; !ok {
				covered = false
			}
		}
		if !covered {
			lines = append(lines, line)
		}
	}
	return lines
}
