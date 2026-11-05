package settle

// This file is the DEFAULT settlement flow — Dibs as pure matchmaker.
// Instead of opening the issue server-side, Dibs hands the ideator a
// prefilled GitHub new-issue URL; they file it with their OWN GitHub
// account, so GitHub natively attributes the issue to them. Dibs then
// records the issue URL the ideator pastes back
// (accepted → issue_launched → settled).

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Footer is the attribution line issues filed on HIVE-MANAGED repos end
// with — they already have agent capacity, so no pitch is needed. It is
// public-facing text on real repos: one plain line, no flourish.
const Footer = "Listed via dibs.hivecommons.dev"

// ExternalFooter is the growth-loop call-to-action appended to issues filed
// on repos NOT managed by a hive: every idea filed externally advertises
// hive to that repo's maintainers.
const ExternalFooter = "Listed via dibs.hivecommons.dev.\n" +
	"This idea arrived via DIBS (https://dibs.hivecommons.dev). Give this repo agent capacity by requesting a hive at https://hive.hivecommons.dev; clankers, powered by donated AI tokens, implement accepted ideas. Once the repo has a hive, this issue can be assigned to it and built."

// footerFor picks the footer by hive membership of the target repo.
func footerFor(hiveManaged bool) string {
	if hiveManaged {
		return Footer
	}
	return ExternalFooter
}

// footerBlockFor is footerFor with its separator, as appended to bodies.
// A non-empty ideaID appends IdeaMarker on its own line after the footer.
func footerBlockFor(hiveManaged bool, ideaID string) string {
	block := "\n\n---\n" + footerFor(hiveManaged)
	if ideaID != "" {
		block += "\n" + IdeaMarker(ideaID)
	}
	return block
}

// markerPrefix/markerSuffix delimit the HTML comment IdeaMarker emits.
// GitHub does not render HTML comments but returns them verbatim through
// the issues API, so the marker ties a filed issue to one Dibs idea.
const (
	markerPrefix = "<!-- dibs-idea: "
	markerSuffix = " -->"
)

// IdeaMarker is the hidden line the launch footer carries so settlement can
// verify that the pasted issue was filed for this idea and not merely on
// the right repo (see VerifyFiledIssue).
func IdeaMarker(ideaID string) string { return markerPrefix + ideaID + markerSuffix }

// HasIdeaMarker reports whether body carries IdeaMarker(ideaID).
func HasIdeaMarker(body, ideaID string) bool {
	return ideaID != "" && strings.Contains(body, IdeaMarker(ideaID))
}

// stripTrailingMarker removes a trailing IdeaMarker line so footer
// idempotence checks see the footer itself.
func stripTrailingMarker(body string) string {
	if !strings.HasSuffix(body, markerSuffix) {
		return body
	}
	if i := strings.LastIndex(body, "\n"+markerPrefix); i >= 0 {
		return strings.TrimRight(body[:i], "\n ")
	}
	return body
}

// MaxIssueURLLen is the budget for a prefilled new-issue URL. Browsers and
// GitHub tolerate roughly 8k characters; stay safely under it.
const MaxIssueURLLen = 7500

// truncationNote is appended (before the footer) when the body had to be
// cut to fit the URL budget.
const truncationNote = "\n\n_(Draft truncated to fit the URL. Paste the full text from Dibs over this section.)_"

// LaunchBody returns body terminated by the Dibs footer (idempotent).
// hiveManaged selects the footer: the short attribution for hive-managed
// repos, the "request a hive" growth CTA for external ones.
func LaunchBody(body string, hiveManaged bool) string {
	return LaunchBodyFor(body, hiveManaged, "")
}

// LaunchBodyFor is LaunchBody with the IdeaMarker for ideaID appended after
// the footer (omitted when ideaID is empty). A body that already ends with
// the footer keeps that footer; when ideaID is set it still ends with this
// idea's marker (VerifyFiledIssue needs it for ideators without a GitHub
// login), replacing a trailing marker that names another idea.
func LaunchBodyFor(body string, hiveManaged bool, ideaID string) string {
	body = strings.TrimRight(body, "\n ")
	bare := stripTrailingMarker(body)
	if strings.HasSuffix(bare, Footer) || strings.HasSuffix(bare, ExternalFooter) {
		if ideaID == "" || strings.HasSuffix(body, "\n"+IdeaMarker(ideaID)) {
			return body
		}
		return bare + "\n" + IdeaMarker(ideaID)
	}
	return body + footerBlockFor(hiveManaged, ideaID)
}

// NewIssueURL builds the prefilled GitHub new-issue URL for repoID
// ("org/name") with the ideated label. When the encoded URL would exceed
// MaxIssueURLLen the body is truncated rune-safely (a note and the footer
// matching hiveManaged are preserved) and truncated=true — callers should
// then offer the full body via copy-to-clipboard.
func NewIssueURL(repoID, title, body string, hiveManaged bool) (issueURL string, truncated bool) {
	return NewIssueURLFor(repoID, title, body, hiveManaged, "")
}

// NewIssueURLFor is NewIssueURL whose truncation tail also preserves the
// IdeaMarker for ideaID (omitted when empty), so a shortened body still
// identifies the idea it was filed for.
func NewIssueURLFor(repoID, title, body string, hiveManaged bool, ideaID string) (issueURL string, truncated bool) {
	build := func(b string) string {
		q := url.Values{}
		q.Set("title", title)
		q.Set("body", b)
		q.Set("labels", Label)
		return "https://github.com/" + repoID + "/issues/new?" + q.Encode()
	}
	full := build(body)
	if len(full) <= MaxIssueURLLen {
		return full, false
	}
	tail := truncationNote + footerBlockFor(hiveManaged, ideaID)
	runes := []rune(body)
	// Binary-search the longest body prefix whose encoded URL fits.
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if len(build(string(runes[:mid])+"…"+tail)) <= MaxIssueURLLen {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return build(string(runes[:lo]) + "…" + tail), true
}

// ValidateIssueURL checks that raw is a real GitHub issue URL on repoID —
// https://github.com/{org}/{repo}/issues/{N} — the shape the ideator pastes
// back to confirm they filed the issue.
func ValidateIssueURL(raw, repoID string) error {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("settle: not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("settle: issue URL must use https")
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return fmt.Errorf("settle: issue URL must be on github.com")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return fmt.Errorf("settle: expected an URL like https://github.com/%s/issues/<number>", repoID)
	}
	if !strings.EqualFold(parts[0]+"/"+parts[1], repoID) {
		return fmt.Errorf("settle: issue URL is not on the accepting repo %s", repoID)
	}
	if _, err := parseIssueNumber(parts[3]); err != nil {
		return err
	}
	return nil
}

// repoIDPattern approximates GitHub's "org/name" shape: owners are
// alphanumeric with hyphens (max 39 chars); repo names add dots and
// underscores (max 100 chars).
var repoIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// ValidateRepoID checks that repoID looks like a real GitHub "org/name" —
// the shape required when an ideator targets an EXTERNAL (non-hive) repo.
func ValidateRepoID(repoID string) error {
	if repoIDPattern.MatchString(repoID) {
		return nil
	}
	return fmt.Errorf("settle: %q is not a GitHub repo — use the org/repo format", repoID)
}
