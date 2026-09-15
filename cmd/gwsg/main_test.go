package main

import (
	"strings"
	"testing"

	"github.com/astelmach20/gwsgo/internal/discovery"
)

func TestParseArgsSplitsPositionalsFromFlags(t *testing.T) {
	positional, opts, err := parseArgs([]string{
		"drive", "files", "list", "--params", `{"pageSize":10}`, "--format", "table", "--page-all",
	})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if strings.Join(positional, " ") != "drive files list" {
		t.Errorf("got positionals %v", positional)
	}
	if opts.params != `{"pageSize":10}` || opts.format != "table" || !opts.pageAll {
		t.Errorf("flags not parsed: %+v", opts)
	}
}

func TestParseArgsAcceptsInlineFlagValues(t *testing.T) {
	_, opts, err := parseArgs([]string{"drive", "files", "list", "--format=csv"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.format != "csv" {
		t.Errorf("got %q, want csv", opts.format)
	}
}

func TestParseArgsRejectsUnknownFlags(t *testing.T) {
	if _, _, err := parseArgs([]string{"drive", "--nope"}); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}

func TestParseArgsRejectsFlagWithoutValue(t *testing.T) {
	if _, _, err := parseArgs([]string{"drive", "files", "list", "--params"}); err == nil {
		t.Error("expected an error for a flag missing its value")
	}
}

func TestDecodeJSONReportsBadInput(t *testing.T) {
	if _, err := decodeJSON("--params", "{not json"); err == nil {
		t.Error("expected a JSON error")
	}
}

func TestDecodeJSONTreatsEmptyAsNoParams(t *testing.T) {
	got, err := decodeJSON("--params", "  ")
	if err != nil || len(got) != 0 {
		t.Errorf("got (%v, %v)", got, err)
	}
}

// Drive hides shared drive content unless these are set, so we default them on.
func TestSharedDriveDefaultsAreApplied(t *testing.T) {
	method := &discovery.Method{Parameters: map[string]discovery.Parameter{
		"supportsAllDrives":         {Type: "boolean"},
		"includeItemsFromAllDrives": {Type: "boolean"},
	}}
	params := map[string]any{}
	applySharedDriveDefaults(method, params)
	if params["supportsAllDrives"] != true || params["includeItemsFromAllDrives"] != true {
		t.Errorf("expected both defaults to be set, got %v", params)
	}
}

func TestSharedDriveDefaultsNeverOverrideTheCaller(t *testing.T) {
	method := &discovery.Method{Parameters: map[string]discovery.Parameter{
		"supportsAllDrives": {Type: "boolean"},
	}}
	params := map[string]any{"supportsAllDrives": false}
	applySharedDriveDefaults(method, params)
	if params["supportsAllDrives"] != false {
		t.Error("an explicit parameter must win over the default")
	}
}

func TestSharedDriveDefaultsSkipUnsupportedMethods(t *testing.T) {
	method := &discovery.Method{Parameters: map[string]discovery.Parameter{}}
	params := map[string]any{}
	applySharedDriveDefaults(method, params)
	if len(params) != 0 {
		t.Errorf("must not add parameters the method does not declare: %v", params)
	}
}
