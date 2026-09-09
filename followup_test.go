package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/portablesql/psql"
)

// the dialect must be usable by psql.IsNotExist / psql.ErrorNumber
var _ psql.ErrorClassifier = sqliteDialect{}

func TestFollowupIsNotExist(t *testing.T) {
	be, err := psql.New(":memory:")
	if err != nil {
		t.Fatalf("New: %s", err)
	}
	ctx := be.Plug(context.Background())

	// missing table
	err = psql.Q(`SELECT * FROM "followup_missing_table"`).Each(ctx, func(*sql.Rows) error { return nil })
	if err == nil {
		t.Fatal("expected an error querying a missing table")
	}
	if !psql.IsNotExist(err) {
		t.Errorf("psql.IsNotExist(%q) = false, want true", err)
	}
	if psql.IsDuplicate(err) {
		t.Errorf("psql.IsDuplicate(%q) = true, want false", err)
	}
	if n := psql.ErrorNumber(err); n != 0xffff {
		t.Errorf("psql.ErrorNumber = %#x, want 0xffff", n)
	}
	// wrapped and joined errors are still recognized
	if !psql.IsNotExist(fmt.Errorf("outer: %w", err)) {
		t.Error("wrapped missing table error not recognized")
	}
	if !psql.IsNotExist(errors.Join(errors.New("unrelated"), err)) {
		t.Error("joined missing table error not recognized")
	}

	// missing column
	if err := psql.Q(`CREATE TABLE "followup_t" ("a" INTEGER)`).Exec(ctx); err != nil {
		t.Fatalf("create: %s", err)
	}
	err = psql.Q(`SELECT b FROM followup_t`).Each(ctx, func(*sql.Rows) error { return nil })
	if err == nil {
		t.Fatal("expected an error querying a missing column")
	}
	if !psql.IsNotExist(err) {
		t.Errorf("psql.IsNotExist(%q) = false, want true", err)
	}

	// other errors are not "not exist"
	err = psql.Q(`SELECT * FROM "followup_t" WHERE`).Each(ctx, func(*sql.Rows) error { return nil })
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	if psql.IsNotExist(err) {
		t.Errorf("psql.IsNotExist(%q) = true, want false", err)
	}
}

func TestFollowupErrorClassifier(t *testing.T) {
	if n := d.ErrorNumber(nil); n != 0 {
		t.Errorf("ErrorNumber(nil) = %d, want 0", n)
	}
	if n := d.ErrorNumber(errors.New("boom")); n != 0xffff {
		t.Errorf("ErrorNumber(err) = %#x, want 0xffff", n)
	}
	if d.IsNotExist(nil) {
		t.Error("IsNotExist(nil) = true")
	}
	if d.IsNotExist(errors.New("boom")) {
		t.Error("IsNotExist(unrelated) = true")
	}
	if !d.IsNotExist(errors.New("no such table: x")) {
		t.Error("IsNotExist(no such table) = false")
	}
	if !d.IsNotExist(&psql.Error{Query: "q", Err: errors.New("no such column: y")}) {
		t.Error("IsNotExist(wrapped no such column) = false")
	}
}
