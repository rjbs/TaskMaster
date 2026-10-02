package tasks

import (
	"strings"
	"unicode"
)

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
	eachValue("locations", func(m map[string]any) { addStr(&o, m["name"]); addStr(&o, m["description"]) })
	eachValue("virtualLocations", func(m map[string]any) { addStr(&o, m["name"]); addStr(&o, m["description"]) })
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
