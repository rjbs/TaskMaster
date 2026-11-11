package tasks

import (
	"reflect"
	"testing"

	"github.com/rjbs/taskmaster/internal/store"
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

// checkSearchIndexRebuild stores a task whose index entry has the stale
// "other" text, records the index as built at haveVersion, runs
// EnsureSearchIndex, and checks the "other" text left in the index.
func checkSearchIndexRebuild(t *testing.T, haveVersion int, data map[string]any, stale, want string) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := New(db, Config{AccountID: "a"})

	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.writeTask(tx, &task{ID: "T-1", UID: "u1", Data: data}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tasks_fts SET other = ? WHERE task_id = 'T-1'`, stale); err != nil {
		t.Fatal(err)
	}
	if haveVersion > 0 {
		if _, err := tx.Exec(`INSERT INTO index_versions (name, version) VALUES ('tasks_fts', ?)`, haveVersion); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := svc.EnsureSearchIndex(t.Context()); err != nil {
		t.Fatalf("EnsureSearchIndex: %v", err)
	}

	tx, err = db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var got string
	if err := tx.QueryRow(`SELECT other FROM tasks_fts WHERE task_id = 'T-1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("index built at version %d: other = %q, want %q", haveVersion, got, want)
	}
}

func TestSearchIndexRebuild(t *testing.T) {
	milk := map[string]any{
		"@type":    "Task",
		"uid":      "u1",
		"keywords": map[string]any{"dairy": true},
	}
	checkSearchIndexRebuild(t, 0, milk, "stale", "dairy")
	checkSearchIndexRebuild(t, searchIndexVersion, milk, "stale", "stale")
}
