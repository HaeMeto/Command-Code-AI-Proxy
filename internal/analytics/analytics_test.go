package analytics

import (
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRecordAndSummary(t *testing.T) {
	db := newTestDB(t)

	for i, e := range []Entry{
		{Method: "POST", Path: "/alpha/generate", StatusCode: 200, DurationMs: 100, Provider: "command-code", Model: "moonshotai/Kimi-K2.5", InputTokens: 7280, OutputTokens: 10},
		{Method: "POST", Path: "/v1/chat/completions", StatusCode: 200, DurationMs: 90, Provider: "command-code", Model: "moonshotai/Kimi-K2.5", InputTokens: 100, OutputTokens: 5},
		{Method: "POST", Path: "/alpha/generate", StatusCode: 502, DurationMs: 30, Provider: "command-code", Model: "moonshotai/Kimi-K2.5", ErrorMessage: "timeout"},
	} {
		if _, err := db.Record(e); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	s, err := db.Summary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.Totals.TotalRequests != 3 {
		t.Errorf("total_requests = %d, want 3", s.Totals.TotalRequests)
	}
	if s.Totals.SuccessCount != 2 {
		t.Errorf("success = %d, want 2", s.Totals.SuccessCount)
	}
	if s.Totals.ErrorCount != 1 {
		t.Errorf("errors = %d, want 1", s.Totals.ErrorCount)
	}
	if s.Totals.TotalInputTokens != 7380 {
		t.Errorf("total_input_tokens = %d, want 7380", s.Totals.TotalInputTokens)
	}
	if s.Totals.TotalOutputTokens != 15 {
		t.Errorf("total_output_tokens = %d, want 15", s.Totals.TotalOutputTokens)
	}
	if len(s.ByModel) != 1 || s.ByModel[0].Model != "moonshotai/Kimi-K2.5" || s.ByModel[0].Count != 3 {
		t.Errorf("by_model = %+v", s.ByModel)
	}
}

func TestDistinctModels_OrderedByRecency(t *testing.T) {
	db := newTestDB(t)
	models := []string{"alpha", "beta", "gamma", "alpha", "delta"}
	for _, m := range models {
		if _, err := db.Record(Entry{Method: "POST", Path: "/v1/chat/completions", StatusCode: 200, Model: m}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	got, err := db.DistinctModels(50)
	if err != nil {
		t.Fatalf("distinct: %v", err)
	}
	// Most recent first; "alpha" was used last in the loop's order, so it
	// should come first; "beta" and "gamma" never reused so original order
	// (newest first); "delta" was the very last record, so it's #1.
	want := []string{"delta", "alpha", "gamma", "beta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestDistinctModels_SkipsEmpty(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Record(Entry{Method: "POST", Path: "/v1/chat/completions", StatusCode: 200, Model: ""}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := db.Record(Entry{Method: "POST", Path: "/v1/chat/completions", StatusCode: 200, Model: "real"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, _ := db.DistinctModels(50)
	if len(got) != 1 || got[0] != "real" {
		t.Errorf("got %v, want [real]", got)
	}
}

func TestRecent(t *testing.T) {
	db := newTestDB(t)
	for i := 0; i < 5; i++ {
		if _, err := db.Record(Entry{Method: "POST", Path: "/alpha/generate", StatusCode: 200, DurationMs: int64(i)}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	items, err := db.Recent(3)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0].DurationMs != 4 {
		t.Errorf("recent[0] duration = %d, want 4 (most recent)", items[0].DurationMs)
	}
}
