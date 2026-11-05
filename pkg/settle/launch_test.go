package settle

import (
	"net/url"
	"strings"
	"testing"
)

// TestNewIssueURL: the prefilled URL targets the right repo, round-trips
// title/body/label through URL encoding, and carries the Dibs footer.
func TestNewIssueURL(t *testing.T) {
	title := "💡 Great idea & more"
	body := LaunchBody("Line one.\n\nSecond ¶ with specials: ?&=#+%", true)
	got, truncated := NewIssueURL("kubestellar/dibs", title, body, true)
	if truncated {
		t.Fatal("small body must not be truncated")
	}
	if !strings.HasPrefix(got, "https://github.com/kubestellar/dibs/issues/new?") {
		t.Fatalf("url prefix: %q", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing built url: %v", err)
	}
	q := u.Query()
	if q.Get("title") != title {
		t.Fatalf("title round-trip: %q", q.Get("title"))
	}
	if q.Get("body") != body {
		t.Fatalf("body round-trip: %q", q.Get("body"))
	}
	if q.Get("labels") != Label {
		t.Fatalf("labels: %q", q.Get("labels"))
	}
	if !strings.Contains(q.Get("body"), Footer) {
		t.Fatalf("body missing the Dibs footer:\n%s", q.Get("body"))
	}
}

// TestLaunchBody: appends the footer exactly once.
func TestLaunchBody(t *testing.T) {
	b := LaunchBody("Body.", true)
	if !strings.HasSuffix(b, Footer) {
		t.Fatalf("missing footer: %q", b)
	}
	if again := LaunchBody(b, true); again != b {
		t.Fatalf("footer must be idempotent:\n%q\nvs\n%q", b, again)
	}
	if strings.Count(LaunchBody(LaunchBody("x", true), true), Footer) != 1 {
		t.Fatal("footer duplicated")
	}
}

// TestLaunchBodyForMarkerWithExistingFooter: a body that already ends with
// the footer still ends with THIS idea's marker when ideaID is set —
// VerifyFiledIssue needs it for ideators without a GitHub login — and a
// trailing marker naming another idea is replaced, not kept. A body already
// carrying the right marker, or launched without an ideaID, is unchanged.
func TestLaunchBodyForMarkerWithExistingFooter(t *testing.T) {
	const id = "idea-1"
	footerOnly := LaunchBody("Body.", true)
	got := LaunchBodyFor(footerOnly, true, id)
	if want := footerOnly + "\n" + IdeaMarker(id); got != want {
		t.Fatalf("marker not appended after an existing footer:\n%q\nwant\n%q", got, want)
	}
	if strings.Count(got, Footer) != 1 {
		t.Fatalf("footer duplicated: %q", got)
	}
	if again := LaunchBodyFor(got, true, id); again != got {
		t.Fatalf("footer+marker must be idempotent:\n%q\nvs\n%q", got, again)
	}
	if ext := LaunchBodyFor(LaunchBody("Body.", false), false, id); !strings.HasSuffix(ext, ExternalFooter+"\n"+IdeaMarker(id)) {
		t.Fatalf("external footer not followed by the marker: %q", ext)
	}
	stale := LaunchBodyFor("Body.", true, "other-idea")
	fixed := LaunchBodyFor(stale, true, id)
	if !HasIdeaMarker(fixed, id) || HasIdeaMarker(fixed, "other-idea") {
		t.Fatalf("stale marker not replaced: %q", fixed)
	}
	if strings.Count(fixed, markerPrefix) != 1 {
		t.Fatalf("expected exactly one marker: %q", fixed)
	}
	if noID := LaunchBodyFor(footerOnly, true, ""); noID != footerOnly {
		t.Fatalf("empty ideaID must leave a footered body unchanged: %q", noID)
	}
}

// TestNewIssueURLTruncation: an over-budget body is cut to fit, keeps the
// truncation note + footer, and reports truncated=true so the UI offers
// copy-to-clipboard of the full text.
func TestNewIssueURLTruncation(t *testing.T) {
	huge := LaunchBody(strings.Repeat("An idea so grand it overflows URLs. ", 400), true) // ~15k chars
	got, truncated := NewIssueURL("org/repo", "Big one", huge, true)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(got) > MaxIssueURLLen {
		t.Fatalf("still over budget: %d > %d", len(got), MaxIssueURLLen)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing built url: %v", err)
	}
	body := u.Query().Get("body")
	if !strings.Contains(body, "truncated") {
		t.Fatalf("truncated body missing the paste-the-rest note:\n%.200s", body)
	}
	if !strings.HasSuffix(body, Footer) {
		t.Fatal("truncated body must still end with the Dibs footer")
	}
	if !strings.HasPrefix(body, "An idea so grand") {
		t.Fatalf("truncated body lost its beginning: %.80s", body)
	}
}

// TestNewIssueURLTruncationMultibyte: rune-safe cutting — no torn UTF-8.
func TestNewIssueURLTruncationMultibyte(t *testing.T) {
	huge := strings.Repeat("蜂蜜と🐝のアイデア ", 800)
	got, truncated := NewIssueURL("org/repo", "多バイト", huge, true)
	if !truncated {
		t.Fatal("expected truncation")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing built url: %v", err)
	}
	if body := u.Query().Get("body"); strings.ContainsRune(body, '\uFFFD') {
		t.Fatal("torn multibyte rune in truncated body")
	}
}

// TestValidateIssueURL pins the confirmation validation: only a real
// https://github.com/{org}/{repo}/issues/N URL on the accepting repo passes.
func TestValidateIssueURL(t *testing.T) {
	repo := "kubestellar/dibs"
	valid := []string{
		"https://github.com/kubestellar/dibs/issues/42",
		"  https://github.com/kubestellar/dibs/issues/1 ", // whitespace trimmed
		"https://github.com/KubeStellar/Dibs/issues/7",    // GitHub is case-insensitive
		"https://www.github.com/kubestellar/dibs/issues/3",
	}
	for _, u := range valid {
		if err := ValidateIssueURL(u, repo); err != nil {
			t.Errorf("ValidateIssueURL(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"",
		"not a url at all ://",
		"http://github.com/kubestellar/dibs/issues/42",        // not https
		"https://gitlab.com/kubestellar/dibs/issues/42",       // wrong host
		"https://github.com/other/repo/issues/42",             // wrong repo
		"https://github.com/kubestellar/dibs/pull/42",         // a PR, not an issue
		"https://github.com/kubestellar/dibs/issues/new",      // the form, not a filed issue
		"https://github.com/kubestellar/dibs/issues/0",        // not a positive number
		"https://github.com/kubestellar/dibs/issues/+5",       // sign: non-canonical
		"https://github.com/kubestellar/dibs/issues/05",       // leading zero: non-canonical
		"https://github.com/kubestellar/dibs/issues/-5",       // negative
		"https://github.com/kubestellar/dibs/issues/42/extra", // trailing junk
		"https://github.com/kubestellar/dibs/issues",          // no number
		"https://evil.com/https://github.com/kubestellar/dibs/issues/42",
	}
	for _, u := range invalid {
		if err := ValidateIssueURL(u, repo); err == nil {
			t.Errorf("ValidateIssueURL(%q) = nil, want error", u)
		}
	}
}

// TestLaunchBodyExternal: issues aimed at NON-hive repos carry the
// "request a hive" growth CTA instead of the short attribution footer —
// and the hive footer never carries the pitch.
func TestLaunchBodyExternal(t *testing.T) {
	ext := LaunchBody("Body.", false)
	if !strings.HasSuffix(ext, ExternalFooter) {
		t.Fatalf("external body missing the growth footer: %q", ext)
	}
	if !strings.Contains(ext, "requesting a hive") || !strings.Contains(ext, "hive.hivecommons.dev") || !strings.Contains(ext, "dibs.hivecommons.dev") {
		t.Fatalf("external footer missing the hive CTA: %q", ext)
	}
	// Idempotent: relaunching an already-footered body adds nothing.
	if again := LaunchBody(ext, false); again != ext {
		t.Fatalf("external footer must be idempotent:\n%q\nvs\n%q", ext, again)
	}
	if strings.Count(LaunchBody(LaunchBody("x", false), false), "requesting a hive") != 1 {
		t.Fatal("external footer duplicated")
	}
	// The hive footer never pitches a hive (they already have one).
	if hive := LaunchBody("Body.", true); strings.Contains(hive, "requesting a hive") {
		t.Fatalf("hive-managed footer must not carry the pitch: %q", hive)
	}
}

// TestNewIssueURLExternalTruncation: an over-budget EXTERNAL body still
// fits the URL budget and keeps the growth CTA at the end.
func TestNewIssueURLExternalTruncation(t *testing.T) {
	huge := LaunchBody(strings.Repeat("An idea so grand it overflows URLs. ", 400), false)
	got, truncated := NewIssueURL("someorg/somerepo", "Big one", huge, false)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(got) > MaxIssueURLLen {
		t.Fatalf("still over budget: %d > %d", len(got), MaxIssueURLLen)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing built url: %v", err)
	}
	body := u.Query().Get("body")
	if !strings.HasSuffix(body, ExternalFooter) {
		t.Fatalf("truncated external body must still end with the growth CTA:\n%.200s", body)
	}
}

// TestValidateRepoID pins the org/repo shape check used for external
// (non-hive) offer targets.
func TestValidateRepoID(t *testing.T) {
	valid := []string{"kubernetes/kubernetes", "a/b", "my-org/my.repo_name", "Org-1/Repo-2"}
	for _, id := range valid {
		if err := ValidateRepoID(id); err != nil {
			t.Errorf("ValidateRepoID(%q) = %v, want nil", id, err)
		}
	}
	invalid := []string{"", "norepo", "org/", "/repo", "org/repo/extra", "-org/repo",
		"org/re po", "https://github.com/org/repo", "org//repo"}
	for _, id := range invalid {
		if err := ValidateRepoID(id); err == nil {
			t.Errorf("ValidateRepoID(%q) = nil, want error", id)
		}
	}
}

// TestFootersUseCanonicalHost pins the outbound branding hosts. These footer
// strings are written permanently into third-party issue trackers, so they
// cannot be corrected after the fact: a regression here is not a stale
// default but a wrong URL published on someone else's repo forever. The
// canonical hosts are dibs.hivecommons.dev and hive.hivecommons.dev; the
// retired kubestellar.io hosts survive only as redirects we do not control.
func TestFootersUseCanonicalHost(t *testing.T) {
	const retiredHost = "kubestellar.io"
	for name, footer := range map[string]string{
		"Footer":         Footer,
		"ExternalFooter": ExternalFooter,
	} {
		if strings.Contains(footer, retiredHost) {
			t.Errorf("%s advertises the retired host %q: %q", name, retiredHost, footer)
		}
		if !strings.Contains(footer, "dibs.hivecommons.dev") {
			t.Errorf("%s missing canonical dibs host: %q", name, footer)
		}
	}
	// The growth CTA must point at the canonical hub, not the redirect.
	if !strings.Contains(ExternalFooter, "https://hive.hivecommons.dev") {
		t.Errorf("ExternalFooter missing canonical hub URL: %q", ExternalFooter)
	}
	// Both footers as actually appended to a launched body.
	for _, hiveManaged := range []bool{true, false} {
		if body := LaunchBody("Body.", hiveManaged); strings.Contains(body, retiredHost) {
			t.Errorf("LaunchBody(hiveManaged=%v) advertises %q: %q", hiveManaged, retiredHost, body)
		}
	}
}
