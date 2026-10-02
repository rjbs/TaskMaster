package tasks

import (
	"reflect"
	"testing"
)

func checkTerms(t *testing.T, input string, want []string) {
	t.Helper()
	if got := searchTerms(input); !reflect.DeepEqual(got, want) {
		t.Errorf("searchTerms(%q) = %q, want %q", input, got, want)
	}
}

func TestSearchTerms(t *testing.T) {
	checkTerms(t, `foo bar`, []string{"foo", "bar"})
	checkTerms(t, `  foo   bar `, []string{"foo", "bar"})
	checkTerms(t, `"foo bar" baz`, []string{"foo bar", "baz"})
	checkTerms(t, `'foo bar' baz`, []string{"foo bar", "baz"})
	checkTerms(t, `"it's fine"`, []string{"it's fine"})
	checkTerms(t, `"say \"hi\""`, []string{`say "hi"`})
	checkTerms(t, `"back\\slash"`, []string{`back\slash`})
	checkTerms(t, `"unterminated phrase`, []string{"unterminated phrase"})
	checkTerms(t, ``, nil)
}

func checkMatch(t *testing.T, columns []string, input, want string, wantOK bool) {
	t.Helper()
	got, ok := ftsMatch(columns, input)
	if ok != wantOK || got != want {
		t.Errorf("ftsMatch(%q) = %q, %v; want %q, %v", input, got, ok, want, wantOK)
	}
}

func TestFTSMatch(t *testing.T) {
	checkMatch(t, []string{"title"}, `buy milk`, `{title} : ("buy" "milk")`, true)
	checkMatch(t, []string{"title", "other"}, `"the ficus"`, `{title other} : ("the ficus")`, true)
	checkMatch(t, []string{"title"}, `say "hi"`, `{title} : ("say" "hi")`, true)
	checkMatch(t, []string{"title"}, `--- ...`, ``, false)
	checkMatch(t, []string{"title"}, ``, ``, false)
}
