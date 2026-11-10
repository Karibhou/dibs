package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	readmePath        = "../../README.md"
	contributingPath  = "../../CONTRIBUTING.md"
	issueChooserPath  = "../../.github/ISSUE_TEMPLATE/config.yml"
	communityMeetings = "https://docs.hivecommons.dev/docs/community/meetings"
)

var chooserURLRe = regexp.MustCompile(`(?m)^\s+url:[ \t]*(\S+)\s*$`)

// Every Hive Commons repo points newcomers at the same community channels from
// both the README and the issue chooser; pin them so none is dropped silently.
func TestReadmeAndIssueChooserLinkEveryCommunityChannel(t *testing.T) {
	links := []struct{ name, url string }{
		{"Discord", "https://hivecommons.dev/discord"},
		{"Twitch live stream", "https://www.twitch.tv/hivecommons"},
		{"community meeting", communityMeetings},
	}
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}
	chooser, err := os.ReadFile(issueChooserPath)
	if err != nil {
		t.Fatalf("read %s: %v", issueChooserPath, err)
	}
	chooserURLs := map[string]bool{}
	for _, m := range chooserURLRe.FindAllStringSubmatch(string(chooser), -1) {
		chooserURLs[m[1]] = true
	}
	for _, l := range links {
		if !strings.Contains(string(readme), "]("+l.url+")") {
			t.Errorf("%s does not link the %s (%s)", readmePath, l.name, l.url)
		}
		if !chooserURLs[l.url] {
			t.Errorf("%s does not link the %s (%s)", issueChooserPath, l.name, l.url)
		}
	}
}

// CONTRIBUTING.md is what GitHub links from the new-issue and new-PR pages, so
// it must tell a newcomer what to pick up and where to ask before they start.
func TestContributingPointsNewcomersAtStarterWorkAndCommunity(t *testing.T) {
	links := []struct{ name, url string }{
		{"good first issue label", "https://github.com/hivecommons/dibs/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22"},
		{"help wanted label", "https://github.com/hivecommons/dibs/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22"},
		{"feature request form", "https://github.com/hivecommons/dibs/issues/new?template=feature_request.yml"},
		{"roadmap", "ROADMAP.md"},
		{"Discord", "https://hivecommons.dev/discord"},
		{"community meeting", communityMeetings},
	}
	contributing, err := os.ReadFile(contributingPath)
	if err != nil {
		t.Fatalf("read %s: %v", contributingPath, err)
	}
	for _, l := range links {
		if !strings.Contains(string(contributing), "]("+l.url+")") {
			t.Errorf("%s does not link the %s (%s)", contributingPath, l.name, l.url)
		}
	}
	if _, err := os.Stat("../../.github/ISSUE_TEMPLATE/feature_request.yml"); err != nil {
		t.Errorf("%s links the feature request form, but its template is missing: %v", contributingPath, err)
	}
}
