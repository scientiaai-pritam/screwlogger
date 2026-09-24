package agent

import (
	"os"
	"path"

	"gopkg.in/yaml.v3"
)

// Category used when no rule matches (spec §3.2).
const Uncategorized = "Uncategorized"

// Rule maps one executable-name pattern to a category. Patterns support
// exact names and globs (* ?), compared case-insensitively.
type Rule struct {
	Pattern  string `yaml:"pattern"`
	Category string `yaml:"category"`
}

// Rules is an ordered rule set; Categorize resolves exact matches before
// globs regardless of file order.
type Rules []Rule

type rulesFile struct {
	Rules []Rule `yaml:"rules"`
}

// LoadRules reads and validates a rules YAML file.
func LoadRules(path string) (Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf rulesFile
	if err := yaml.Unmarshal(b, &rf); err != nil {
		return nil, err
	}
	return Rules(rf.Rules), nil
}

// Categorize returns the category for app: exact match wins over glob,
// first rule wins among equals, Uncategorized when nothing matches.
func (r Rules) Categorize(app string) string {
	appLower := lower(app)
	for _, rule := range r {
		if lower(rule.Pattern) == appLower {
			return rule.Category
		}
	}
	for _, rule := range r {
		if ok, err := path.Match(lower(rule.Pattern), appLower); err == nil && ok {
			return rule.Category
		}
	}
	return Uncategorized
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
