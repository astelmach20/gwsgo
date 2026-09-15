package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseAcceptsKnownFormats(t *testing.T) {
	for input, want := range map[string]Format{
		"": JSON, "json": JSON, "JSON": JSON, "ndjson": NDJSON, "table": Table, "csv": CSV,
	} {
		got, err := Parse(input)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = (%v, %v), want %v", input, got, err, want)
		}
	}
}

func TestParseRejectsUnknownFormat(t *testing.T) {
	if _, err := Parse("yaml"); err == nil {
		t.Error("expected an error for an unsupported format")
	}
}

func TestRowsOfPrefersKnownCollectionKeys(t *testing.T) {
	payload := map[string]any{
		"files":         []any{map[string]any{"id": "1"}},
		"nextPageToken": "abc",
	}
	rows, key, ok := rowsOf(payload)
	if !ok || key != "files" || len(rows) != 1 {
		t.Fatalf("got (%v, %q, %v)", rows, key, ok)
	}
}

func TestRowsOfFallsBackToTheOnlyArray(t *testing.T) {
	payload := map[string]any{"somethingUnusual": []any{map[string]any{"id": "1"}}}
	_, key, ok := rowsOf(payload)
	if !ok || key != "somethingUnusual" {
		t.Fatalf("got (%q, %v)", key, ok)
	}
}

func TestRowsOfDeclinesWhenAmbiguous(t *testing.T) {
	payload := map[string]any{"a": []any{1}, "b": []any{2}}
	if _, _, ok := rowsOf(payload); ok {
		t.Error("two candidate arrays should not resolve to a collection")
	}
}

func TestColumnsOfSkipsNestedValues(t *testing.T) {
	rows := []any{map[string]any{"id": "1", "owners": []any{"x"}, "meta": map[string]any{"a": 1}}}
	for _, column := range columnsOf(rows, 0) {
		if column == "owners" || column == "meta" {
			t.Errorf("nested field %q must not become a table column", column)
		}
	}
}

func TestColumnsOfFrontLoadsIdentifiers(t *testing.T) {
	rows := []any{map[string]any{"zzz": "1", "name": "n", "id": "i"}}
	got := columnsOf(rows, 0)
	if got[0] != "id" {
		t.Errorf("expected id first, got %v", got)
	}
}

// Paginated table output must print the header once, not per page.
func TestTableHeaderIsWrittenOnlyOnce(t *testing.T) {
	var buffer bytes.Buffer
	writer := New(&buffer, Table)
	page := map[string]any{"files": []any{map[string]any{"id": "1", "name": "a"}}}
	for range 3 {
		if err := writer.Write(page); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if count := strings.Count(buffer.String(), "id"); count != 1 {
		t.Errorf("header appeared %d times, want 1:\n%s", count, buffer.String())
	}
}

func TestCSVHeaderIsWrittenOnlyOnce(t *testing.T) {
	var buffer bytes.Buffer
	writer := New(&buffer, CSV)
	page := map[string]any{"files": []any{map[string]any{"id": "1"}}}
	for range 2 {
		if err := writer.Write(page); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if count := strings.Count(buffer.String(), "id"); count != 1 {
		t.Errorf("CSV header repeated: %s", buffer.String())
	}
}

func TestCellFormatsIntegersCleanly(t *testing.T) {
	if got := cell(float64(42)); got != "42" {
		t.Errorf("got %q, want \"42\"", got)
	}
}

func TestCellFlattensNewlines(t *testing.T) {
	if got := cell("a\nb\tc"); strings.ContainsAny(got, "\n\t") {
		t.Errorf("cell must not contain raw newlines or tabs: %q", got)
	}
}

func TestWriteRawBytesPassThrough(t *testing.T) {
	var buffer bytes.Buffer
	if err := New(&buffer, JSON).Write([]byte("binary")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buffer.String() != "binary" {
		t.Errorf("got %q", buffer.String())
	}
}
