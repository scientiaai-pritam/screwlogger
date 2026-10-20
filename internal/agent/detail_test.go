package agent_test

import (
	"strings"
	"testing"

	"screwlogger/internal/agent"
)

func TestShouldCaptureTitle(t *testing.T) {
	for _, app := range []string{"chrome.exe", "CHROME.EXE", "msedge.exe", "photoshop.exe", "firefox.exe"} {
		if !agent.ShouldCaptureTitle(app) {
			t.Errorf("ShouldCaptureTitle(%q) = false, want true", app)
		}
	}
	for _, app := range []string{"excel.exe", "notepad.exe", "", "explorer.exe"} {
		if agent.ShouldCaptureTitle(app) {
			t.Errorf("ShouldCaptureTitle(%q) = true, want false", app)
		}
	}
}

func TestExtractDetailBrowser(t *testing.T) {
	cases := []struct {
		app, title, want string
	}{
		{"chrome.exe", "Inbox (3) - Gmail - Google Chrome", "Inbox (3) - Gmail"},
		{"msedge.exe", "Dashboard - Microsoft Edge", "Dashboard"},
		{"firefox.exe", "Wikipedia — Mozilla Firefox", "Wikipedia"},
		{"brave.exe", "Example - Brave", "Example"},
		{"chrome.exe", "No Suffix Page", "No Suffix Page"},
		{"chrome.exe", "", ""},
	}
	for _, c := range cases {
		if got := agent.ExtractDetail(c.app, c.title); got != c.want {
			t.Errorf("ExtractDetail(%q, %q) = %q, want %q", c.app, c.title, got, c.want)
		}
	}
}

func TestExtractDetailPhotoshop(t *testing.T) {
	cases := []struct {
		title, want string
	}{
		{"Adobe Photoshop 2024 - [Untitled-1 @ 100% (RGB/8)]", "Untitled-1"},
		{"job-42.psd @ 66,7% (Layer 1, RGB/8) - Adobe Photoshop 2023", "job-42.psd"},
		{"Adobe Photoshop 2021 - banner.psd @ 25% (RGB/8)", "banner.psd"},
		// CS6 document-window title (no brand text, just the doc).
		{"1791205261076__000000250_0.jpg @ 97.4% (RGB/8#)", "1791205261076__000000250_0.jpg"},
		// Not documents — must be dropped (no " @ " zoom marker):
		{"Adobe Photoshop CS6 Extended", ""}, // empty app frame
		{"New", ""},                          // File > New dialog
		{"Open", ""},                         // File > Open dialog
		{"Save As", ""},                      // Save dialog
	}
	for _, c := range cases {
		if got := agent.ExtractDetail("photoshop.exe", c.title); got != c.want {
			t.Errorf("ExtractDetail(photoshop, %q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestExtractDetailTruncates(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := agent.ExtractDetail("chrome.exe", long)
	if len([]rune(got)) != 256 {
		t.Fatalf("truncated length = %d, want 256", len([]rune(got)))
	}
}
