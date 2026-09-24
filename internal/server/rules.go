package server

import "time"

// RuleRow is one categorization rule for the admin rules editor.
type RuleRow struct {
	ExePattern string
	Category   string
	UpdatedAt  int64
}

// ListRules returns all rules, ordered by pattern.
func (s *Store) ListRules() ([]RuleRow, error) {
	rows, err := s.db.Query(`SELECT exe_pattern, category, updated_at FROM rules ORDER BY exe_pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuleRow
	for rows.Next() {
		var r RuleRow
		if err := rows.Scan(&r.ExePattern, &r.Category, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRule inserts or replaces a categorization rule, bumping updated_at.
func (s *Store) UpsertRule(pattern, category string) error {
	_, err := s.db.Exec(`INSERT INTO rules (exe_pattern, category, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(exe_pattern) DO UPDATE SET category = excluded.category, updated_at = excluded.updated_at`,
		pattern, category, time.Now().Unix())
	return err
}

// DeleteRule removes a rule by its exe pattern.
func (s *Store) DeleteRule(pattern string) error {
	_, err := s.db.Exec(`DELETE FROM rules WHERE exe_pattern = ?`, pattern)
	return err
}

// Recategorize re-maps stored heartbeats matching pattern (case-insensitive
// GLOB on app) to category, returning the number of rows changed.
func (s *Store) Recategorize(pattern, category string) (int64, error) {
	res, err := s.db.Exec(`UPDATE heartbeats SET category = ? WHERE lower(app) GLOB lower(?)`, category, pattern)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
