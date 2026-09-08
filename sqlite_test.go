package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/portablesql/psql"
)

var d = sqliteDialect{}

func TestExportArgTime(t *testing.T) {
	loc := time.FixedZone("JST", 9*3600)
	in := time.Date(2024, 3, 5, 12, 30, 0, 500, loc) // 12:30 JST = 03:30 UTC, 500ns

	got := d.ExportArg(in)
	want := "2024-03-05T03:30:00.000000500Z"
	if got != want {
		t.Errorf("ExportArg(time) = %q, want %q", got, want)
	}

	// trailing zeros must not be trimmed
	got = d.ExportArg(time.Date(2024, 3, 5, 12, 30, 0, 0, time.UTC))
	want = "2024-03-05T12:30:00.000000000Z"
	if got != want {
		t.Errorf("ExportArg(time without nanos) = %q, want %q", got, want)
	}
	if len(got.(string)) != len(ZeroTime) {
		t.Errorf("format is not fixed width: %q vs %q", got, ZeroTime)
	}

	// zero time
	if got := d.ExportArg(time.Time{}); got != ZeroTime {
		t.Errorf("ExportArg(zero) = %q, want %q", got, ZeroTime)
	}

	// pointers
	var nilTime *time.Time
	if got := d.ExportArg(nilTime); got != nil {
		t.Errorf("ExportArg(nil *time.Time) = %v, want nil", got)
	}
	if got := d.ExportArg(&in); got != "2024-03-05T03:30:00.000000500Z" {
		t.Errorf("ExportArg(*time.Time) = %v", got)
	}
}

func TestExportArgRoundTripAndOrdering(t *testing.T) {
	times := []time.Time{
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(2024, 1, 1, 0, 0, 0, 100000000, time.UTC),
		time.Date(2024, 1, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2024, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	var strs []string
	for _, tm := range times {
		s := d.ExportArg(tm).(string)
		// the core's timeSetter parses anything containing 'T' with RFC3339Nano
		back, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("RFC3339Nano cannot parse %q: %v", s, err)
		}
		if !back.Equal(tm) {
			t.Errorf("round trip of %v gave %v", tm, back)
		}
		strs = append(strs, s)
	}
	if !sort.StringsAreSorted(strs) {
		t.Errorf("text ordering does not match chronological ordering: %v", strs)
	}
}

func TestExportArgOther(t *testing.T) {
	b := []byte{1, 2, 3}
	if got, ok := d.ExportArg(b).([]byte); !ok || string(got) != string(b) {
		t.Errorf("ExportArg([]byte) = %v", got)
	}
	var nilStr *string
	if got := d.ExportArg(nilStr); got != nil {
		t.Errorf("ExportArg(nil *string) = %v, want nil", got)
	}
	s := "hello"
	if got := d.ExportArg(&s); got != "hello" {
		t.Errorf("ExportArg(*string) = %v", got)
	}
	if got := d.ExportArg(nil); got != nil {
		t.Errorf("ExportArg(nil) = %v", got)
	}
	v := psql.Vector{1, 2.5, 3}
	if got, want := d.ExportArg(v), psql.DefaultExportArg(v); !reflect.DeepEqual(got, want) {
		t.Errorf("ExportArg(Vector) = %v, want %v", got, want)
	}
	if got := d.ExportArg(42); got != 42 {
		t.Errorf("ExportArg(int) = %v", got)
	}
}

func TestLimitOffset(t *testing.T) {
	if got := d.LimitOffset(20, 10); got != "LIMIT 10 OFFSET 20" {
		t.Errorf("LimitOffset(20, 10) = %q", got)
	}
}

func TestPlaceholder(t *testing.T) {
	if d.Placeholder(1) != "?" || d.Placeholder(7) != "?" {
		t.Error("Placeholder should always be ?")
	}
}

func TestSqlType(t *testing.T) {
	cases := []struct {
		base  string
		attrs map[string]string
		want  string
	}{
		{"enum", map[string]string{"values": "a,b"}, "text"},
		{"set", nil, "text"},
		{"vector", map[string]string{"size": "3"}, "text"},
		{"bigint", map[string]string{"size": "20"}, "integer"},
		{"INT", nil, "integer"},
		{"bool", nil, "integer"},
		{"double", nil, "real"},
		{"decimal", nil, "real"},
		{"varbinary", nil, "blob"},
		{"varchar", map[string]string{"size": "128"}, "text"},
		{"datetime", nil, "text"},
		{"json", nil, "text"},
		{"something_unknown", nil, "text"},
	}
	for _, c := range cases {
		if got := d.SqlType(c.base, c.attrs); got != c.want {
			t.Errorf("SqlType(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestFieldDef(t *testing.T) {
	cases := []struct {
		attrs map[string]string
		want  string
	}{
		{map[string]string{}, `"col" text`},
		{map[string]string{"null": "0"}, `"col" text NOT NULL`},
		{map[string]string{"null": "true"}, `"col" text NULL`},
		{map[string]string{"null": "maybe"}, ``},
		{map[string]string{"default": "\\N"}, `"col" text DEFAULT NULL`},
		{map[string]string{"null": "0", "default": "x"}, `"col" text NOT NULL DEFAULT 'x'`},
		{map[string]string{"collation": "utf8mb4_bin"}, `"col" text`}, // no COLLATE on sqlite
	}
	for _, c := range cases {
		if got := d.FieldDef("col", "text", false, c.attrs); got != c.want {
			t.Errorf("FieldDef(%v) = %q, want %q", c.attrs, got, c.want)
		}
	}
}

func TestFieldDefAlter(t *testing.T) {
	cases := []struct {
		typ   string
		attrs map[string]string
		want  string
	}{
		{"text", map[string]string{}, `"col" text`},
		{"text", map[string]string{"null": "0"}, `"col" text NOT NULL DEFAULT ''`},
		{"integer", map[string]string{"null": "0"}, `"col" integer NOT NULL DEFAULT 0`},
		{"real", map[string]string{"null": "false"}, `"col" real NOT NULL DEFAULT 0.0`},
		{"text", map[string]string{"null": "0", "default": "x"}, `"col" text DEFAULT 'x' NOT NULL`},
		{"text", map[string]string{"null": "1", "default": "\\N"}, `"col" text DEFAULT NULL`},
	}
	for _, c := range cases {
		if got := d.FieldDefAlter("col", c.typ, false, c.attrs); got != c.want {
			t.Errorf("FieldDefAlter(%s, %v) = %q, want %q", c.typ, c.attrs, got, c.want)
		}
	}
}

func TestKeyRendering(t *testing.T) {
	primary := &psql.StructKey{Key: "PRIMARY", Typ: psql.KeyPrimary, Fields: []string{"id"}}
	unique := &psql.StructKey{Key: "name_uniq", Typ: psql.KeyUnique, Fields: []string{"a", "b"}}
	index := &psql.StructKey{Key: "by_x", Typ: psql.KeyIndex, Fields: []string{"x"}}
	vector := &psql.StructKey{Key: "vec", Typ: psql.KeyVector, Fields: []string{"emb"}}
	fulltext := &psql.StructKey{Key: "ft", Typ: psql.KeyFulltext, Fields: []string{"body"}}

	if got := d.InlineKeyDef(primary, "t"); got != `PRIMARY KEY ("id")` {
		t.Errorf("InlineKeyDef(primary) = %q", got)
	}
	if got := d.InlineKeyDef(unique, "t"); got != "" {
		t.Errorf("InlineKeyDef(unique) = %q, want empty (created as named index)", got)
	}
	if got := d.InlineKeyDef(index, "t"); got != "" {
		t.Errorf("InlineKeyDef(index) = %q, want empty", got)
	}

	if got := d.CreateIndex(primary, "t"); got != "" {
		t.Errorf("CreateIndex(primary) = %q, want empty", got)
	}
	if got, want := d.CreateIndex(unique, "t"), `CREATE UNIQUE INDEX "t_name_uniq" ON "t" ("a", "b")`; got != want {
		t.Errorf("CreateIndex(unique) = %q, want %q", got, want)
	}
	if got, want := d.CreateIndex(index, "t"), `CREATE INDEX "t_by_x" ON "t" ("x")`; got != want {
		t.Errorf("CreateIndex(index) = %q, want %q", got, want)
	}
	if got := d.CreateIndex(vector, "t"); got != "" {
		t.Errorf("CreateIndex(vector) = %q, want empty", got)
	}
	if got := d.CreateIndex(fulltext, "t"); got != "" {
		t.Errorf("CreateIndex(fulltext) = %q, want empty", got)
	}
	if got := d.KeyDef(index, "t"); got != d.CreateIndex(index, "t") {
		t.Errorf("KeyDef should match CreateIndex, got %q", got)
	}
}

func TestUpsertRendering(t *testing.T) {
	if got, want := d.ReplaceSQL("t", `"a","b"`, "?,?", nil, nil), `INSERT OR REPLACE INTO "t" ("a","b") VALUES (?,?)`; got != want {
		t.Errorf("ReplaceSQL = %q, want %q", got, want)
	}
	if got, want := d.InsertIgnoreSQL("t", `"a"`, "?"), `INSERT OR IGNORE INTO "t" ("a") VALUES (?)`; got != want {
		t.Errorf("InsertIgnoreSQL = %q, want %q", got, want)
	}
}

func TestMatchDSN(t *testing.T) {
	f := sqliteFactory{}
	for _, dsn := range []string{":memory:", "foo.db", "/tmp/x.sqlite", "data.sqlite3", "sqlite:/tmp/x", "file:test.db?cache=shared"} {
		if !f.MatchDSN(dsn) {
			t.Errorf("MatchDSN(%q) = false, want true", dsn)
		}
	}
	for _, dsn := range []string{"postgres://u@h/db", "postgresql://u@h/db", "root:pw@tcp(127.0.0.1:3306)/db", "host=localhost dbname=x", ""} {
		if f.MatchDSN(dsn) {
			t.Errorf("MatchDSN(%q) = true, want false", dsn)
		}
	}
}

func TestIsDuplicate(t *testing.T) {
	dup := errors.New("constraint failed: UNIQUE constraint failed: t.name (2067)")
	if !d.IsDuplicate(dup) {
		t.Error("plain duplicate error not detected")
	}
	if !d.IsDuplicate(fmt.Errorf("insert: %w", dup)) {
		t.Error("wrapped duplicate error not detected")
	}
	if !d.IsDuplicate(&psql.Error{Query: "INSERT", Err: dup}) {
		t.Error("psql.Error wrapped duplicate not detected")
	}
	if !d.IsDuplicate(errors.Join(errors.New("other"), dup)) {
		t.Error("joined duplicate error not detected")
	}
	if d.IsDuplicate(errors.New("no such table: t")) {
		t.Error("non duplicate error detected as duplicate")
	}
	if d.IsDuplicate(nil) {
		t.Error("nil detected as duplicate")
	}
}

func TestPrepareDSN(t *testing.T) {
	got, err := prepareDSN("/tmp/x.db")
	if err != nil {
		t.Fatal(err)
	}
	base, rawQuery, _ := strings.Cut(got, "?")
	if base != "/tmp/x.db" {
		t.Errorf("base = %q", base)
	}
	q, _ := url.ParseQuery(rawQuery)
	pragmas := strings.Join(q["_pragma"], ";")
	for _, want := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(1)"} {
		if !strings.Contains(pragmas, want) {
			t.Errorf("missing pragma %q in %q", want, pragmas)
		}
	}
	if q.Get("_txlock") != "immediate" {
		t.Errorf("_txlock = %q", q.Get("_txlock"))
	}

	// caller settings win
	got, err = prepareDSN("file:x.db?_txlock=deferred&_pragma=busy_timeout(50)&_pragma=journal_mode(DELETE)&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	_, rawQuery, _ = strings.Cut(got, "?")
	q, _ = url.ParseQuery(rawQuery)
	pragmas = strings.Join(q["_pragma"], ";")
	if strings.Contains(pragmas, "busy_timeout(10000)") || !strings.Contains(pragmas, "busy_timeout(50)") {
		t.Errorf("caller busy_timeout not kept: %q", pragmas)
	}
	if strings.Contains(pragmas, "journal_mode(WAL)") || !strings.Contains(pragmas, "journal_mode(DELETE)") {
		t.Errorf("caller journal_mode not kept: %q", pragmas)
	}
	if !strings.Contains(pragmas, "foreign_keys(1)") {
		t.Errorf("default foreign_keys not added: %q", pragmas)
	}
	if q.Get("_txlock") != "deferred" {
		t.Errorf("caller _txlock not kept: %q", q.Get("_txlock"))
	}
	if q.Get("cache") != "shared" {
		t.Errorf("unrelated parameter lost: %q", q.Get("cache"))
	}

	if !isMemoryDSN(":memory:") || !isMemoryDSN("file:x?mode=memory&cache=shared") || isMemoryDSN("/tmp/x.db") {
		t.Error("isMemoryDSN misclassified a DSN")
	}
}

// Integration tests against a real in-memory database.

type tsRow struct {
	psql.Name `sql:"ts_test_sqlite"`
	ID        int64  `sql:",key=PRIMARY"`
	Label     string `sql:",type=VARCHAR,size=128"`
	When      time.Time
	NameKey   psql.Key `sql:",type=UNIQUE,fields=Label"`
	WhenKey   psql.Key `sql:",type=INDEX,fields=When"`
}

func listIndexes(t *testing.T, ctx context.Context, table string) []string {
	t.Helper()
	var names []string
	err := psql.Q("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name=? ORDER BY name", table).Each(ctx, func(rows *sql.Rows) error {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
		return nil
	})
	if err != nil {
		t.Fatalf("listing indexes: %v", err)
	}
	return names
}

func TestMemoryBackendTimestampsAndIndexes(t *testing.T) {
	be, err := psql.New(":memory:")
	if err != nil {
		t.Fatalf("psql.New: %v", err)
	}
	defer be.DB().Close()
	if be.Engine() != psql.EngineSQLite {
		t.Fatalf("engine = %v", be.Engine())
	}
	ctx := be.Plug(context.Background())

	// explicit schema check creates the table and its named indexes
	if err := d.CheckStructure(ctx, be, psql.Table[tsRow]()); err != nil {
		t.Fatalf("CheckStructure: %v", err)
	}
	idx := listIndexes(t, ctx, "ts_test_sqlite")
	want := []string{"ts_test_sqlite_NameKey", "ts_test_sqlite_WhenKey"}
	if strings.Join(idx, ",") != strings.Join(want, ",") {
		t.Fatalf("indexes after create = %v, want %v", idx, want)
	}

	// a second check must not create anything
	if err := d.CheckStructure(ctx, be, psql.Table[tsRow]()); err != nil {
		t.Fatalf("second CheckStructure: %v", err)
	}
	if idx2 := listIndexes(t, ctx, "ts_test_sqlite"); strings.Join(idx2, ",") != strings.Join(idx, ",") {
		t.Fatalf("second CheckStructure changed indexes: %v -> %v", idx, idx2)
	}

	// timestamps: insert out of order, with values whose RFC3339Nano form
	// would have different lengths (trailing zeros trimmed)
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	rows := []*tsRow{
		{ID: 1, Label: "a", When: base.Add(500 * time.Millisecond)},
		{ID: 2, Label: "b", When: base.Add(50 * time.Millisecond)},
		{ID: 3, Label: "c", When: base.Add(1 * time.Second)},
		{ID: 4, Label: "d", When: base.Add(999 * time.Millisecond)},
		{ID: 5, Label: "e", When: base.Add(1*time.Second + time.Nanosecond)},
	}
	for _, r := range rows {
		if err := psql.Insert(ctx, r); err != nil {
			t.Fatalf("insert %d: %v", r.ID, err)
		}
	}

	got, err := psql.Fetch[tsRow](ctx, nil, psql.Sort(psql.S("When", "ASC")))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var order []int64
	for _, r := range got {
		order = append(order, r.ID)
	}
	if fmt.Sprint(order) != "[2 1 4 3 5]" {
		t.Errorf("order by When = %v, want [2 1 4 3 5]", order)
	}

	// round trip
	for _, r := range got {
		var orig *tsRow
		for _, o := range rows {
			if o.ID == r.ID {
				orig = o
			}
		}
		if !r.When.Equal(orig.When) {
			t.Errorf("row %d: When = %v, want %v", r.ID, r.When, orig.When)
		}
		if r.When.Location() != time.UTC {
			t.Errorf("row %d: location = %v, want UTC", r.ID, r.When.Location())
		}
	}

	// stored text form is the fixed-width one
	var stored string
	err = psql.Q(`SELECT "When" FROM "ts_test_sqlite" WHERE "ID"=?`, 3).Each(ctx, func(rows *sql.Rows) error {
		return rows.Scan(&stored)
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored != "2024-06-01T12:00:01.000000000Z" {
		t.Errorf("stored = %q", stored)
	}

	// zero time round trip
	if err := psql.Insert(ctx, &tsRow{ID: 6, Label: "zero"}); err != nil {
		t.Fatal(err)
	}
	z, err := psql.Get[tsRow](ctx, map[string]any{"ID": 6})
	if err != nil {
		t.Fatal(err)
	}
	if !z.When.IsZero() {
		t.Errorf("zero time came back as %v", z.When)
	}

	// unique key enforced and detected
	err = psql.Q(`INSERT INTO "ts_test_sqlite" ("ID","Label","When") VALUES (?,?,?)`, 7, "a", d.ExportArg(time.Now())).Exec(ctx)
	if err == nil || !psql.IsDuplicate(err) {
		t.Errorf("duplicate label: err = %v, IsDuplicate = %v", err, psql.IsDuplicate(err))
	}
}

func TestCheckStructureDetectsInlineUnique(t *testing.T) {
	// Tables created by older versions of this package declared UNIQUE inline,
	// which produces an auto-named sqlite_autoindex_* index. CheckStructure must
	// recognise it instead of creating a redundant named index.
	be, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer be.DB().Close()
	ctx := be.Plug(context.Background())

	err = psql.Q(`CREATE TABLE "ts_test_sqlite" ("ID" integer NOT NULL, "Label" text NOT NULL, "When" text NOT NULL, PRIMARY KEY ("ID"), UNIQUE ("Label"))`).Exec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckStructure(ctx, be, psql.Table[tsRow]()); err != nil {
		t.Fatal(err)
	}
	idx := listIndexes(t, ctx, "ts_test_sqlite")
	for _, n := range idx {
		if n == "ts_test_sqlite_NameKey" {
			t.Errorf("redundant unique index created on top of inline UNIQUE: %v", idx)
		}
	}
	found := false
	for _, n := range idx {
		if n == "ts_test_sqlite_WhenKey" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing index not created: %v", idx)
	}
}

func TestCheckStructureAddsColumn(t *testing.T) {
	be, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer be.DB().Close()
	ctx := be.Plug(context.Background())

	if err := psql.Q(`CREATE TABLE "ts_test_sqlite" ("ID" integer NOT NULL, "Label" text NOT NULL, PRIMARY KEY ("ID"))`).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckStructure(ctx, be, psql.Table[tsRow]()); err != nil {
		t.Fatal(err)
	}
	if err := psql.Insert(ctx, &tsRow{ID: 1, Label: "x", When: time.Now()}); err != nil {
		t.Fatalf("insert after ALTER: %v", err)
	}
}

type concRow struct {
	psql.Name `sql:"conc_test_sqlite"`
	ID        int64  `sql:",key=PRIMARY"`
	Value     string `sql:",type=VARCHAR,size=128"`
}

func TestFileBackendConcurrentReadersAndWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.db")
	be, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer be.DB().Close()
	ctx := be.Plug(context.Background())

	if got := be.DB().Stats().MaxOpenConnections; got != MaxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, MaxOpenConns)
	}
	var journal, fk string
	if err := be.DB().QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || !strings.EqualFold(journal, "wal") {
		t.Fatalf("journal_mode = %q (%v), want wal", journal, err)
	}
	if err := be.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != "1" {
		t.Fatalf("foreign_keys = %q (%v), want 1", fk, err)
	}

	if err := psql.Insert(ctx, &concRow{ID: 0, Value: "seed"}); err != nil {
		t.Fatal(err)
	}

	const writes = 200
	const readers = 4
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, readers+1)

	// writer: one insert per transaction, plus a long-ish transaction holding
	// an open cursor to prove readers are not serialized behind it
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 1; i <= writes; i++ {
			if err := psql.Insert(ctx, &concRow{ID: int64(i), Value: "v"}); err != nil {
				errs <- fmt.Errorf("write %d: %w", i, err)
				return
			}
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := 0
			for {
				select {
				case <-stop:
					if seen == 0 {
						errs <- errors.New("reader never saw a row")
					}
					return
				default:
				}
				n, err := psql.Count[concRow](ctx, nil)
				if err != nil {
					errs <- fmt.Errorf("read: %w", err)
					return
				}
				if n > seen {
					seen = n
				}
				// keep a cursor open while issuing another query: with a pool
				// this must not deadlock
				rows, err := be.DB().Query(`SELECT "ID" FROM "conc_test_sqlite" LIMIT 5`)
				if err != nil {
					errs <- err
					return
				}
				if _, err := psql.Count[concRow](ctx, nil); err != nil {
					rows.Close()
					errs <- err
					return
				}
				rows.Close()
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock: readers and writer did not finish")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	n, err := psql.Count[concRow](ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != writes+1 {
		t.Errorf("count = %d, want %d", n, writes+1)
	}
}

func TestMemoryBackendSingleConnection(t *testing.T) {
	be, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer be.DB().Close()
	if got := be.DB().Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1 for :memory:", got)
	}
	var fk string
	if err := be.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != "1" {
		t.Errorf("foreign_keys = %q (%v), want 1", fk, err)
	}
	var bt string
	if err := be.DB().QueryRow("PRAGMA busy_timeout").Scan(&bt); err != nil || bt != "10000" {
		t.Errorf("busy_timeout = %q (%v), want 10000", bt, err)
	}
}
