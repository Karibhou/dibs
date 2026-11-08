package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	readmePath        = "../../README.md"
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
