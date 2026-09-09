package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/portablesql/psql"
)

// compile-time checks of the optional interfaces the dialect implements
var (
	_ psql.ReturningRenderer = sqliteDialect{}
	_ psql.RetryableChecker  = sqliteDialect{}
)

func TestSupportsReturning(t *testing.T) {
	if !d.SupportsReturning() {
		t.Error("SupportsReturning must be true (SQLite >= 3.35)")
	}
	be := psql.NewBackend(psql.EngineSQLite, nil)
	if !be.Supports(psql.FeatureReturning) || be.Supports(psql.FeatureAdvisoryLocks) || be.Supports(psql.FeatureFullText) {
		t.Error("feature defaults for SQLite are wrong")
	}
	ctx := be.Plug(context.Background())
	q, err := psql.B().Update("t").Set(map[string]any{"a": 1}).Where(map[string]any{"id": 1}).Returning("*").Render(ctx)
	if err != nil || q != `UPDATE "t" SET "a"=1 WHERE ("id"=1) RETURNING *` {
		t.Errorf("UPDATE RETURNING = %q, %v", q, err)
	}
}

func TestIsRetryable(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	locked := errors.New("database table is locked: t (6) (SQLITE_LOCKED)")
	for _, err := range []error{
		busy, locked,
		fmt.Errorf("tx: %w", busy),
		&psql.Error{Query: "UPDATE", Err: locked},
		errors.Join(errors.New("ctx"), &psql.Error{Query: "COMMIT", Err: busy}),
	} {
		if !d.IsRetryable(err) || !psql.IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = false", err)
		}
	}
	for _, err := range []error{nil, errors.New("UNIQUE constraint failed: t.id"), errors.New("no such table: t")} {
		if d.IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = true", err)
		}
	}
}

func TestFieldDefAutoInc(t *testing.T) {
	attrs := map[string]string{"null": "0", "autoinc": "1"}
	// whatever the declared type, an autoinc column is exactly "integer" so
	// that it aliases the rowid
	for _, typ := range []string{"integer", "bigint", "text"} {
		if got := d.FieldDef("ID", typ, false, attrs); got != `"ID" integer NOT NULL` {
			t.Errorf("FieldDef(%s autoinc) = %q", typ, got)
		}
	}
	if got := d.FieldDefAlter("ID", "bigint", false, attrs); got != `"ID" integer NOT NULL DEFAULT 0` {
		t.Errorf("FieldDefAlter(autoinc) = %q", got)
	}

	type autoItem struct {
		psql.Name `sql:"auto_item"`
		ID        uint64 `sql:",key=PRIMARY,autoinc"`
		Label     string `sql:",type=VARCHAR,size=32"`
	}
	be := psql.NewBackend(psql.EngineSQLite, nil)
	f := psql.Table[autoItem]().AllFields()[0]
	if got := f.SqlType(be); got != "integer" {
		t.Errorf("SqlType(uint64 autoinc) = %q, want integer", got)
	}
	if got := f.DefString(be); got != `"ID" integer NOT NULL` {
		t.Errorf("DefString = %q", got)
	}
}

func TestExpressionAndGinKeys(t *testing.T) {
	expr := &psql.StructKey{Key: "lower_name", Typ: psql.KeyIndex, Attrs: map[string]string{"expression": "lower({Name})"}}
	uexpr := &psql.StructKey{Key: "u", Typ: psql.KeyUnique, Attrs: map[string]string{"expression": "{A} || {B}"}}
	gin := &psql.StructKey{Key: "g", Typ: psql.KeyGIN, Fields: []string{"Data"}, Attrs: map[string]string{}}
	gist := &psql.StructKey{Key: "s", Typ: psql.KeyGIST, Fields: []string{"Range"}, Attrs: map[string]string{}}

	if got, want := d.CreateIndex(expr, "t"), `CREATE INDEX "t_lower_name" ON "t" (lower("Name"))`; got != want {
		t.Errorf("CreateIndex(expression) = %q, want %q", got, want)
	}
	if got, want := d.CreateIndex(uexpr, "t"), `CREATE UNIQUE INDEX "t_u" ON "t" ("A" || "B")`; got != want {
		t.Errorf("CreateIndex(unique expression) = %q, want %q", got, want)
	}
	noFields := &psql.StructKey{Key: "n", Typ: psql.KeyIndex, Attrs: map[string]string{}}
	for _, k := range []*psql.StructKey{gin, gist, noFields} {
		if got := d.CreateIndex(k, "t"); got != "" {
			t.Errorf("CreateIndex(%s) = %q, want empty", k.Key, got)
		}
	}
}

// Integration on an in-memory database: autoinc keys, RETURNING on Insert,
// InsertIgnore and Replace, expression indexes.
type featItem struct {
	psql.Name `sql:"feat_item"`
	ID        int64    `sql:",key=PRIMARY,autoinc"`
	Title     string   `sql:",type=VARCHAR,size=32"`
	Data      string   `sql:",import=JSON"`
	LowerIdx  psql.Key `sql:",expression=\"lower({Title})\""`
	DataIdx   psql.Key `sql:",type=GIN,fields='Data'"`
}

func TestMemoryBackendReturningAndAutoInc(t *testing.T) {
	be, err := psql.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := be.Plug(context.Background())

	a := &featItem{Title: "Alpha", Data: "{}"}
	if err := psql.Insert(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := &featItem{Title: "Beta", Data: "{}"}
	if err := psql.Insert(ctx, b); err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Errorf("autoinc ids not populated: %d, %d", a.ID, b.ID)
	}

	// INSERT OR IGNORE ... RETURNING yields no row on a conflict
	dup := &featItem{ID: a.ID, Title: "Dup", Data: "{}"}
	if err := psql.InsertIgnore(ctx, dup); err != nil {
		t.Fatal(err)
	}
	got, err := psql.Get[featItem](ctx, map[string]any{"ID": a.ID})
	if err != nil || got.Title != "Alpha" {
		t.Errorf("InsertIgnore overwrote the row: %v, %v", got, err)
	}
	// INSERT OR REPLACE ... RETURNING refreshes the object
	rep := &featItem{ID: a.ID, Title: "Alpha2", Data: "{}"}
	if err := psql.Replace(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if got, err := psql.Get[featItem](ctx, map[string]any{"ID": a.ID}); err != nil || got.Title != "Alpha2" {
		t.Errorf("Replace failed: %v, %v", got, err)
	}

	// the single connection is not held by RETURNING rows: transactions and
	// further queries keep working
	err = psql.Tx(ctx, func(tx context.Context) error {
		c := &featItem{Title: "Gamma", Data: "{}"}
		if err := psql.Insert(tx, c); err != nil {
			return err
		}
		if c.ID == 0 {
			return errors.New("id not populated inside a transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cnt, err := psql.Count[featItem](ctx, nil)
	if err != nil || cnt != 3 {
		t.Errorf("count = %d, %v", cnt, err)
	}

	// the expression index exists, the GIN key was skipped
	var names []string
	if err := psql.Q("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='feat_item'").Each(ctx, func(rows *sql.Rows) error {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "feat_item_LowerIdx" {
		t.Errorf("indexes = %v, want only feat_item_LowerIdx", names)
	}

	// the schema check finds the expression index again on a second pass
	if err := be.CheckStructure(ctx, psql.Table[featItem]()); err != nil {
		t.Fatal(err)
	}
}
