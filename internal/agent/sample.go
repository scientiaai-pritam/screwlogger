package agent

import (
	"encoding/json"
	"fmt"
)

// Sample is one in-session observation the watcher ships to the service over
// the local pipe (spec §4.1).
type Sample struct {
	App    string `json:"app"`
	Active bool   `json:"active"`
}

// EncodeSample renders one sample as a JSON line (trailing newline included).
func EncodeSample(s Sample) []byte {
	b, _ := json.Marshal(s) // Sample has no marshal failure mode
	return append(b, '\n')
}

// DecodeSample parses one JSON line. Empty lines, malformed JSON, and samples
// without an app name are rejected; the caller logs and skips them.
func DecodeSample(line []byte) (Sample, error) {
	var s Sample
	if len(line) == 0 {
		return s, fmt.Errorf("empty sample line")
	}
	if err := json.Unmarshal(line, &s); err != nil {
		return s, fmt.Errorf("decode sample: %w", err)
	}
	if s.App == "" {
		return s, fmt.Errorf("sample without app")
	}
	return s, nil
}
