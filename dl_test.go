package main

// A focused test for the download path.

// The install hangs before printing anything, and the manifest download works
// (easysetup list succeeds), so the difference is the release-asset URL: it
// redirects to objects.githubusercontent.com, which is a different host.

// This checks that path directly with a short timeout, so a hang shows up as a
// test failure rather than a stuck process.

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestReleaseRedirect(t *testing.T) {
	url := releaseBase + "/jq-1.8.2/jq-1.8.2-debian-trixie-amd64.tar.zst"

	// Print what the proxy machinery resolves for THIS url. A bare &http.Request{}
	// panics inside ProxyFromEnvironment because its URL is nil - which is worth
	// knowing, since the symptom being chased is "downloads work but crawl":
	// curl honours HTTPS_PROXY and the first version of this client did not.
	if req0, err := http.NewRequest(http.MethodGet, url, nil); err == nil {
		if u, err := http.ProxyFromEnvironment(req0); err != nil {
			t.Logf("proxy: error %v", err)
		} else if u == nil {
			t.Logf("proxy: NONE - ProxyFromEnvironment returned nil")
		} else {
			t.Logf("proxy: %s", u)
		}
	}
	t.Logf("env HTTPS_PROXY=%q NO_PROXY=%q",
		os.Getenv("HTTPS_PROXY"), os.Getenv("NO_PROXY"))

	// Give it 45 seconds: either it downloads 19 MB or something is wrong.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", userAgent)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := &http.Client{
		// Record every hop, because the thing under test is what happens to the
		// Authorization header when the host changes.
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			t.Logf("redirect %d: host=%s auth=%v",
				len(via), r.URL.Host, r.Header.Get("Authorization") != "")
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	t.Logf("status=%s content-length=%d", resp.Status, resp.ContentLength)

	n, err := io.Copy(io.Discard, resp.Body)
	t.Logf("read %d bytes, err=%v", n, err)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s", resp.Status)
	}
	if n != resp.ContentLength {
		t.Fatalf("short read: %d of %d", n, resp.ContentLength)
	}
}
