package acp

import (
	"strings"
	"testing"
)

func TestScanStderrReportsNewestAuthURL(t *testing.T) {
	in := strings.Join([]string{
		"starting",
		"Open the following link to authenticate the ACP server: https://accounts.google.com/o/oauth2/v2/auth?state=old",
		"Open the following link to authenticate the ACP server: https://accounts.google.com/o/oauth2/v2/auth?state=new ",
		"https://symbolize.corp.google.com/r/?trace=1",
	}, "\n")
	urls := make(chan string, 1)
	scanStderr(strings.NewReader(in), nil, urls)
	if got := <-urls; got != "https://accounts.google.com/o/oauth2/v2/auth?state=new" {
		t.Fatalf("got %q", got)
	}
}
