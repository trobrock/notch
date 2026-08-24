package main

import "testing"

func TestCurrentBuildInfoUsesInjectedValues(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	version, commit, buildDate = "v1.2.3", "abc123", "2026-01-02T03:04:05Z"
	t.Cleanup(func() { version, commit, buildDate = oldVersion, oldCommit, oldBuildDate })

	info := currentBuildInfo()
	if info.Version != "v1.2.3" || info.Commit != "abc123" || info.BuildDate != "2026-01-02T03:04:05Z" {
		t.Fatalf("build info = %#v", info)
	}
	if info.GoVersion == "" || info.Platform == "" {
		t.Fatalf("incomplete build info = %#v", info)
	}
}
