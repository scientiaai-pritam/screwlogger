package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"screwlogger/internal/agent"
)

func writeRules(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const testRules = `rules:
  - {pattern: excel.exe, category: Office}
  - {pattern: "chrome.exe", category: Browser}
  - {pattern: "mes_*.exe", category: Production}
`

func TestExactMatchCaseInsensitive(t *testing.T) {
	rules, err := agent.LoadRules(writeRules(t, testRules))
	if err != nil {
		t.Fatal(err)
	}
	if got := rules.Categorize("EXCEL.EXE"); got != "Office" {
		t.Fatalf("got %q", got)
	}
}

func TestGlobMatch(t *testing.T) {
	rules, _ := agent.LoadRules(writeRules(t, testRules))
	if got := rules.Categorize("mes_line3.exe"); got != "Production" {
		t.Fatalf("got %q", got)
	}
}

func TestUnknownAppUncategorized(t *testing.T) {
	rules, _ := agent.LoadRules(writeRules(t, testRules))
	if got := rules.Categorize("notepad.exe"); got != "Uncategorized" {
		t.Fatalf("got %q", got)
	}
}

func TestExactBeatsGlob(t *testing.T) {
	rules := agent.Rules{
		{Pattern: "*", Category: "CatchAll"},
		{Pattern: "chrome.exe", Category: "Browser"},
	}
	if got := rules.Categorize("chrome.exe"); got != "Browser" {
		t.Fatalf("exact must win regardless of order; got %q", got)
	}
}
