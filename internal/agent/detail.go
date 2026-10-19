package agent

import "strings"

// maxDetailLen bounds the foreground-context string shipped per heartbeat. A
// window title is normally short; the cap keeps a pathological title (a very
// long page title) from bloating the buffer and the ingest payload.
const maxDetailLen = 256

// titleApps is the set of executables for which the agent captures a foreground
// context string from the window title. It is deliberately a small, fixed set,
// not every app: title capture is scoped to the apps whose title carries the
// signal the dashboard wants (which page is open in a browser, which document is
// open in Photoshop), so no other app's window title ever leaves the PC.
//
// Keys are lowercase executable base names (matched case-insensitively).
var titleApps = map[string]bool{
	// Browsers → the active page/site.
	"chrome.exe":  true,
	"msedge.exe":  true,
	"firefox.exe": true,
	"brave.exe":   true,
	"opera.exe":   true,
	"vivaldi.exe": true,
	// Photoshop → the open document name.
	"photoshop.exe": true,
}

// browserApps is the subset of titleApps whose title is "<page> - <browser>".
var browserApps = map[string]bool{
	"chrome.exe":  true,
	"msedge.exe":  true,
	"firefox.exe": true,
	"brave.exe":   true,
	"opera.exe":   true,
	"vivaldi.exe": true,
}

// browserSuffixes are the trailing "<sep><browser name>" strings browsers append
// to the window title. ExtractDetail strips the first one that matches, leaving
// the page title. Both the hyphen (Chromium family) and em dash (Firefox) forms
// are covered.
var browserSuffixes = []string{
	" - Google Chrome",
	" - Microsoft​ Edge", // Edge inserts a zero-width space in some builds
	" - Microsoft Edge",
	" — Mozilla Firefox",
	" - Mozilla Firefox",
	" - Brave",
	" - Opera",
	" - Vivaldi",
}

// ShouldCaptureTitle reports whether app is one the agent captures a title for.
// Matching is case-insensitive on the executable base name.
func ShouldCaptureTitle(app string) bool {
	return titleApps[strings.ToLower(app)]
}

// ExtractDetail turns a raw foreground window title into the compact, app-scoped
// context string shipped with a heartbeat. For a browser it returns the active
// page/site (the browser-name suffix stripped); for Photoshop it returns the
// open document name. It never returns the browser chrome or editor brand, and
// returns "" when nothing meaningful can be extracted. The result is truncated
// to maxDetailLen runes.
func ExtractDetail(app, title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	key := strings.ToLower(app)
	switch {
	case browserApps[key]:
		return truncate(browserDetail(title))
	case key == "photoshop.exe":
		return truncate(photoshopDetail(title))
	default:
		return truncate(title)
	}
}

// browserDetail strips the trailing browser-name suffix from a browser window
// title, leaving the page title (which is what identifies the site/page in use).
func browserDetail(title string) string {
	for _, sfx := range browserSuffixes {
		if strings.HasSuffix(title, sfx) {
			return strings.TrimSpace(title[:len(title)-len(sfx)])
		}
	}
	// Unknown suffix (a browser build we don't special-case): fall back to the
	// part before the last " - ", which is the page title for every Chromium
	// browser. If there is no separator, keep the whole title.
	if i := strings.LastIndex(title, " - "); i > 0 {
		return strings.TrimSpace(title[:i])
	}
	return title
}

// photoshopDetail extracts the open document name from a Photoshop window title.
// Observed shapes across versions:
//
//	"Adobe Photoshop 2024 - [Untitled-1 @ 100% (RGB/8)]"
//	"job-42.psd @ 66,7% (Layer 1, RGB/8) - Adobe Photoshop 2023"
//	"Adobe Photoshop 2021 - banner.psd @ 25% (RGB/8)"
//
// The brand prefix/suffix is removed, surrounding brackets are dropped, and the
// zoom/layer tail after " @ " is cut, leaving just the document name.
func photoshopDetail(title string) string {
	s := title
	// Drop a trailing " - Adobe Photoshop ..." brand suffix.
	if i := strings.Index(s, " - Adobe Photoshop"); i >= 0 {
		s = s[:i]
	}
	// Drop a leading "Adobe Photoshop ... - " brand prefix.
	if strings.HasPrefix(s, "Adobe Photoshop") {
		if i := strings.Index(s, " - "); i >= 0 {
			s = s[i+len(" - "):]
		}
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	// Cut the "@ <zoom> (<layer>, <mode>)" tail.
	if i := strings.Index(s, " @ "); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// truncate limits s to maxDetailLen runes without splitting a rune.
func truncate(s string) string {
	if len(s) <= maxDetailLen {
		return s
	}
	r := []rune(s)
	if len(r) <= maxDetailLen {
		return s
	}
	return string(r[:maxDetailLen])
}
