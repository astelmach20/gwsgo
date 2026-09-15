// Package output renders API responses in the formats the CLI supports.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Format is an output encoding.
type Format string

const (
	// JSON is indented JSON, the default.
	JSON Format = "json"
	// NDJSON is one compact JSON value per line, suited to streaming.
	NDJSON Format = "ndjson"
	// Table is aligned columns for humans.
	Table Format = "table"
	// CSV is comma-separated values with a single header row.
	CSV Format = "csv"
)

// Parse validates a format name.
func Parse(name string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(name))) {
	case JSON, "":
		return JSON, nil
	case NDJSON:
		return NDJSON, nil
	case Table:
		return Table, nil
	case CSV:
		return CSV, nil
	default:
		return "", fmt.Errorf("unknown format %q; use json, ndjson, table or csv", name)
	}
}

// collectionKeys are the response fields Google uses to hold result arrays,
// checked before falling back to whatever array the payload happens to have.
var collectionKeys = []string{
	"items", "files", "messages", "threads", "events", "values", "spaces",
	"members", "users", "groups", "labels", "tasks", "documents", "drives",
	"calendars", "people", "connections", "courses", "forms", "notes",
	"conferenceRecords", "matters", "subscriptions", "operations",
}

// rowsOf finds the primary collection inside a response payload.
func rowsOf(payload any) ([]any, string, bool) {
	object, ok := payload.(map[string]any)
	if !ok {
		if rows, ok := payload.([]any); ok {
			return rows, "", true
		}
		return nil, "", false
	}
	for _, key := range collectionKeys {
		if rows, ok := object[key].([]any); ok {
			return rows, key, true
		}
	}
	// Fall back to the only array present, if there is exactly one.
	var found []any
	var name string
	count := 0
	for key, value := range object {
		if rows, ok := value.([]any); ok {
			found, name, count = rows, key, count+1
		}
	}
	if count == 1 {
		return found, name, true
	}
	return nil, "", false
}

// columnsOf picks a stable, readable column order across all rows.
func columnsOf(rows []any, limit int) []string {
	seen := map[string]bool{}
	var names []string
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			continue
		}
		for key, value := range object {
			// Nested objects and arrays do not belong in a flat table.
			switch value.(type) {
			case map[string]any, []any:
				continue
			}
			if !seen[key] {
				seen[key] = true
				names = append(names, key)
			}
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return priority(names[i]) < priority(names[j]) ||
			(priority(names[i]) == priority(names[j]) && names[i] < names[j])
	})
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	return names
}

// priority front-loads the fields a human actually scans for.
func priority(name string) int {
	switch name {
	case "id", "name":
		return 0
	case "title", "subject", "summary", "displayName", "emailAddress":
		return 1
	case "date", "createdTime", "modifiedTime", "startTime", "updated":
		return 2
	default:
		return 3
	}
}

// cell renders a scalar for tabular output.
func cell(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.ReplaceAll(strings.ReplaceAll(typed, "\n", " "), "\t", " ")
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

// Writer renders payloads, tracking whether a header has been emitted so that
// paginated output does not repeat column headers on every page.
type Writer struct {
	Out          io.Writer
	Format       Format
	wroteHeader  bool
	tableColumns []string
}

// New builds a Writer.
func New(out io.Writer, format Format) *Writer {
	return &Writer{Out: out, Format: format}
}

// Write renders one payload (one page, when paginating).
func (w *Writer) Write(payload any) error {
	if payload == nil {
		return nil
	}
	if raw, ok := payload.([]byte); ok {
		_, err := w.Out.Write(raw)
		return err
	}
	switch w.Format {
	case NDJSON:
		encoder := json.NewEncoder(w.Out)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(payload)
	case Table:
		return w.writeTable(payload)
	case CSV:
		return w.writeCSV(payload)
	default:
		encoder := json.NewEncoder(w.Out)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(payload)
	}
}

func (w *Writer) writeTable(payload any) error {
	rows, _, ok := rowsOf(payload)
	if !ok {
		encoder := json.NewEncoder(w.Out)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(payload)
	}
	if !w.wroteHeader {
		w.tableColumns = columnsOf(rows, 6)
		w.wroteHeader = true
		_, _ = fmt.Fprintln(w.Out, strings.Join(w.tableColumns, "\t"))
	}
	tab := tabwriter.NewWriter(w.Out, 0, 4, 2, ' ', 0)
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			_, _ = fmt.Fprintln(tab, cell(row))
			continue
		}
		cells := make([]string, 0, len(w.tableColumns))
		for _, column := range w.tableColumns {
			cells = append(cells, cell(object[column]))
		}
		_, _ = fmt.Fprintln(tab, strings.Join(cells, "\t"))
	}
	return tab.Flush()
}

func (w *Writer) writeCSV(payload any) error {
	rows, _, ok := rowsOf(payload)
	if !ok {
		return fmt.Errorf("response has no tabular collection to render as CSV")
	}
	writer := csv.NewWriter(w.Out)
	defer writer.Flush()
	if !w.wroteHeader {
		w.tableColumns = columnsOf(rows, 0)
		w.wroteHeader = true
		if err := writer.Write(w.tableColumns); err != nil {
			return err
		}
	}
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			continue
		}
		cells := make([]string, 0, len(w.tableColumns))
		for _, column := range w.tableColumns {
			cells = append(cells, cell(object[column]))
		}
		if err := writer.Write(cells); err != nil {
			return err
		}
	}
	return writer.Error()
}
