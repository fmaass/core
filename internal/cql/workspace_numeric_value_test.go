package cql

import (
	"strings"
	"testing"
)

// generate runs the real tokenizer -> parser -> SQL generator pipeline, the
// same path the /search/items ql parameter takes.
func generate(t *testing.T, query string) (string, []any, error) {
	t.Helper()
	tokens, err := NewTokenizer(query).Tokenize()
	if err != nil {
		t.Fatalf("tokenize(%q): %v", query, err)
	}
	ast, err := NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("parse(%q): %v", query, err)
	}
	return NewSQLGenerator(map[string]int{"INFRA": 3}, nil, "postgres").GenerateSQL(ast)
}

// TestWorkspaceKeyFormStillMatches is the positive control: the key form is
// what INFRA-243's probe showed working (workspace = INFRA -> 26 rows), and it
// must keep generating a name/key comparison.
func TestWorkspaceKeyFormStillMatches(t *testing.T) {
	sql, args, err := generate(t, `workspace = INFRA AND description ~ "Cronicle"`)
	if err != nil {
		t.Fatalf("key form must generate SQL, got error: %v", err)
	}
	if !strings.Contains(sql, "w.key") || !strings.Contains(sql, "w.name") {
		t.Errorf("key form must compare against w.key and w.name, got %q", sql)
	}
	if !strings.Contains(sql, "i.description") {
		t.Errorf("description term must reach i.description, got %q", sql)
	}
	if !containsArg(args, "INFRA") {
		t.Errorf("workspace value must be bound as an argument, got %v", args)
	}
	if !containsArg(args, "%Cronicle%") && !containsArg(args, "Cronicle") {
		t.Errorf("description term must be bound as an argument, got %v", args)
	}
}

// TestWorkspaceNumericIDIsRejected is the defect itself: `workspace = 3` used
// to generate name/key comparisons that no row could satisfy, so the endpoint
// answered an empty 200 that was indistinguishable from "no matches"
// (INFRA-243). It must now be a generator error, which the v1 search handler
// surfaces as HTTP 400.
func TestWorkspaceNumericIDIsRejected(t *testing.T) {
	for _, query := range []string{
		`workspace = 3`,
		`workspace = 3 AND description ~ "Cronicle"`,
		`workspace = 3 AND title ~ "Cronicle"`,
		`workspace != 3`,
		`workspace IN (3, 4)`,
		`workspace NOT IN (3)`,
	} {
		_, _, err := generate(t, query)
		if err == nil {
			t.Errorf("%q must be rejected, but generated SQL", query)
			continue
		}
		msg := err.Error()
		for _, want := range []string{"workspaceId", "workspace = INFRA"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q: error must name %q so the caller knows the working form, got %q", query, want, msg)
			}
		}
	}
}

// TestWorkspaceNumericRejectionLeavesOtherFormsWorking guards the blast
// radius: the dedicated id field, the quoted-digits escape hatch for a
// workspace whose name or key really is numeric, and numeric values on
// unrelated fields must all keep generating SQL.
func TestWorkspaceNumericRejectionLeavesOtherFormsWorking(t *testing.T) {
	sql, _, err := generate(t, `workspaceId = 3`)
	if err != nil {
		t.Fatalf("workspaceId is the id field and must still work: %v", err)
	}
	if !strings.Contains(sql, "i.workspace_id") {
		t.Errorf("workspaceId must compare i.workspace_id, got %q", sql)
	}

	if _, args, err := generate(t, `workspace = "3"`); err != nil {
		t.Errorf(`workspace = "3" is a quoted name/key and must still work: %v`, err)
	} else if !containsArg(args, "3") {
		t.Errorf("quoted digits must be bound as the workspace value, got %v", args)
	}

	if _, _, err := generate(t, `workspace = INFRA AND priorityId = 3`); err != nil {
		t.Errorf("numeric values on non-workspace fields must be unaffected: %v", err)
	}
}

// TestNegativeControlTermGeneratesCleanly is the negative control from the
// INFRA-243 probe table: a term no item contains must still produce valid
// SQL with the term bound. Zero hits there is data, not a generator failure —
// which is what distinguishes it from the numeric-workspace defect.
func TestNegativeControlTermGeneratesCleanly(t *testing.T) {
	sql, args, err := generate(t, `workspace = INFRA AND description ~ "zzqx-nonexistent-term"`)
	if err != nil {
		t.Fatalf("negative control must generate SQL, got error: %v", err)
	}
	if sql == "" {
		t.Fatal("negative control produced empty SQL")
	}
	if !containsArg(args, "%zzqx-nonexistent-term%") && !containsArg(args, "zzqx-nonexistent-term") {
		t.Errorf("negative-control term must be bound as an argument, got %v", args)
	}
}

func containsArg(args []any, want string) bool {
	for _, arg := range args {
		if s, ok := arg.(string); ok && s == want {
			return true
		}
	}
	return false
}
