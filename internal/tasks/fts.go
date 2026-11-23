package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/rjbs/taskmaster/internal/store"
)

// searchIndexVersion identifies what ftsText puts in the index.  Bump it
// whenever that changes, and EnsureSearchIndex will rebuild the entries of
// tasks written under the old rules the next time the server starts.
//
//  1. the initial indexing
//  2. location descriptions are no longer indexed
//  3. nor are virtual location descriptions
const searchIndexVersion = 3

// searchTerms splits a text filter value into terms following the Filtering
// section of the spec: a run in matching single or double quotes is one
// phrase (with \", \' and \\ as escapes inside it), and whitespace separates
// tokens outside quotes.
func searchTerms(s string) []string {
	var terms []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			terms = append(terms, cur.String())
			cur.Reset()
		}
	}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0 && r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
		case quote != 0 && r == quote:
			quote = 0
			flush()
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			flush()
			quote = r
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return terms
}

// ftsMatch builds an FTS5 MATCH expression restricting the search to the
// given columns.  Every term must match (FTS5's implicit AND); phrases keep
// their word order.  The boolean is false when the text yields no
// searchable terms, in which case the filter condition is vacuous.
func ftsMatch(columns []string, text string) (string, bool) {
	var parts []string
	for _, term := range searchTerms(text) {
		if !strings.ContainsFunc(term, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			continue
		}
		parts = append(parts, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	if len(parts) == 0 {
		return "", false
	}
	return "{" + strings.Join(columns, " ") + "} : (" + strings.Join(parts, " ") + ")", true
}

// ftsText extracts the indexable text of a task into the three FTS columns.
// Overridden titles and descriptions in recurrenceOverrides are included,
// since the spec says a filter may match any instance of the task.
func ftsText(obj map[string]any) (title, description, other string) {
	var t, d, o []string
	addStr := func(dst *[]string, v any) {
		if s, ok := v.(string); ok && s != "" {
			*dst = append(*dst, s)
		}
	}
	addStr(&t, obj["title"])
	addStr(&d, obj["description"])

	if overrides, ok := obj["recurrenceOverrides"].(map[string]any); ok {
		for _, v := range overrides {
			if patch, ok := v.(map[string]any); ok {
				addStr(&t, patch["title"])
				addStr(&d, patch["description"])
			}
		}
	}

	eachValue := func(prop string, fn func(map[string]any)) {
		if m, ok := obj[prop].(map[string]any); ok {
			for _, v := range m {
				if sub, ok := v.(map[string]any); ok {
					fn(sub)
				}
			}
		}
	}
	// "description" is reserved on both kinds of location in JSCalendar
	// bis, so the text filter matches a location by name alone.
	eachValue("locations", func(m map[string]any) { addStr(&o, m["name"]) })
	eachValue("virtualLocations", func(m map[string]any) { addStr(&o, m["name"]) })
	eachValue("participants", func(m map[string]any) { addStr(&o, m["name"]); addStr(&o, m["email"]) })
	eachValue("links", func(m map[string]any) { addStr(&o, m["title"]) })
	for _, prop := range []string{"keywords", "categories"} {
		if m, ok := obj[prop].(map[string]any); ok {
			for k := range m {
				o = append(o, k)
			}
		}
	}

	return strings.Join(t, "\n"), strings.Join(d, "\n"), strings.Join(o, "\n")
}

// writeSearchEntry replaces the full-text index entry of one task.
func writeSearchEntry(tx *store.Tx, id string, obj map[string]any) error {
	if _, err := tx.Exec(`DELETE FROM tasks_fts WHERE task_id = ?`, id); err != nil {
		return err
	}
	title, description, other := ftsText(obj)
	_, err := tx.Exec(`INSERT INTO tasks_fts (task_id, title, description, other) VALUES (?, ?, ?, ?)`,
		id, title, description, other)
	return err
}

// EnsureSearchIndex rebuilds the full-text index if it was built by a
// version of ftsText older than this one.  A schema migration cannot do
// this, because the indexed text is computed here, not in SQL.
func (s *Service) EnsureSearchIndex(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var have int
	err = tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM index_versions WHERE name = 'tasks_fts'`).Scan(&have)
	if err != nil {
		return err
	}
	if have >= searchIndexVersion {
		return nil
	}

	rows, err := tx.Query(`SELECT id, data FROM tasks`)
	if err != nil {
		return err
	}
	data := map[string]string{}
	for rows.Next() {
		var id, d string
		if err := rows.Scan(&id, &d); err != nil {
			rows.Close()
			return err
		}
		data[id] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM tasks_fts`); err != nil {
		return err
	}
	for id, d := range data {
		var obj map[string]any
		if err := json.Unmarshal([]byte(d), &obj); err != nil {
			return err
		}
		if err := writeSearchEntry(tx, id, obj); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO index_versions (name, version) VALUES ('tasks_fts', ?)
		ON CONFLICT (name) DO UPDATE SET version = excluded.version`, searchIndexVersion)
	if err != nil {
		return err
	}
	return tx.Commit()
}
