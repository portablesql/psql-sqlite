// Package sqlite registers the SQLite dialect and backend factory for psql.
//
// Import this package with a blank identifier to enable SQLite support:
//
//	import _ "github.com/portablesql/psql-sqlite"
//
// # Connections and locking
//
// SQLite has no server; every *sql.DB connection is a separate handle on the
// same database file. [New] configures the pool depending on the DSN:
//
//   - In-memory databases (":memory:" or any DSN containing "mode=memory") are
//     private to the connection that opened them, so the pool is limited to a
//     single connection (SetMaxOpenConns(1)). With one connection, any query
//     issued while a *sql.Rows is still open, or issued from outside a
//     transaction while that transaction is active, blocks forever waiting for
//     the connection. Always close rows (or use Each/All helpers) before running
//     the next query and keep all work of a transaction inside it.
//   - File databases get a small pool (see [MaxOpenConns]) opened in WAL mode
//     with "_txlock=immediate" and a busy timeout, so several readers can run
//     concurrently with one writer. A second writer waits up to the busy
//     timeout for the lock and then fails with SQLITE_BUSY instead of
//     deadlocking.
//
// Every connection gets "PRAGMA busy_timeout", "PRAGMA journal_mode=WAL" and
// "PRAGMA foreign_keys=ON" through DSN "_pragma" parameters; parameters already
// present in the caller's DSN are left untouched.
//
// # Time values
//
// time.Time values are stored as TEXT in the fixed-width UTC form
// "2006-01-02T15:04:05.000000000Z" so that lexical comparison and ordering of
// stored timestamps match chronological order. The zero time is stored as
// "0001-01-01T00:00:00.000000000Z".
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/portablesql/psql"
	_ "modernc.org/sqlite"
)

// TimeFormat is the layout used to store time.Time values. It is fixed-width
// (nanosecond precision, always in UTC with a "Z" suffix) so that text ordering
// of stored values matches chronological ordering.
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// ZeroTime is the stored representation of the zero time.Time.
const ZeroTime = "0001-01-01T00:00:00.000000000Z"

// MaxOpenConns is the size of the connection pool used for file-based
// databases. In-memory databases always use a single connection.
const MaxOpenConns = 8

// BusyTimeout is the default "PRAGMA busy_timeout" applied to each connection
// unless the DSN already specifies one.
const BusyTimeout = 10 * time.Second

func init() {
	psql.RegisterDialect(psql.EngineSQLite, sqliteDialect{})
	psql.RegisterBackendFactory(&sqliteFactory{})

	// Register engine-specific magic types
	psql.DefineMagicTypeEngine(psql.EngineSQLite, "DATETIME", "type=TEXT")
	psql.DefineMagicTypeEngine(psql.EngineSQLite, "JSON", "type=TEXT,format=json")
}

// sqliteDialect implements psql.Dialect and optional interfaces for SQLite.
type sqliteDialect struct{}

func (sqliteDialect) Placeholder(_ int) string { return "?" }

// LimitOffset renders "LIMIT count OFFSET offset". The arguments are
// (offset, count), matching psql's QueryBuilder.Limit(offset, count).
//
// Deprecated: the core renders LIMIT/OFFSET itself and no longer calls this.
func (sqliteDialect) LimitOffset(offset, count int) string {
	return "LIMIT " + strconv.Itoa(count) + " OFFSET " + strconv.Itoa(offset)
}

func (sqliteDialect) ExportArg(v any) any {
	switch val := v.(type) {
	case time.Time:
		return formatTime(val)
	case *time.Time:
		if val == nil {
			return nil
		}
		return formatTime(*val)
	}
	return psql.DefaultExportArg(v)
}

// formatTime renders t in the fixed-width UTC TimeFormat.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ZeroTime
	}
	return t.UTC().Format(TimeFormat)
}

// TypeMapper implementation

func (sqliteDialect) SqlType(baseType string, attrs map[string]string) string {
	switch baseType {
	case "enum", "set":
		return "text"
	case "vector":
		return "text"
	default:
		return sqliteTypeAffinity(baseType)
	}
}

func (sqliteDialect) FieldDef(column, sqlType string, nullable bool, attrs map[string]string) string {
	mydef := psql.QuoteName(column) + " " + sqlType

	if null, ok := attrs["null"]; ok {
		switch null {
		case "0", "false":
			mydef += " NOT NULL"
		case "1", "true":
			mydef += " NULL"
		default:
			return ""
		}
	}
	if def, ok := attrs["default"]; ok {
		if def == "\\N" {
			mydef += " DEFAULT NULL"
		} else {
			mydef += " DEFAULT " + psql.Escape(def)
		}
	}

	// SQLite only supports BINARY, NOCASE, RTRIM collations — skip COLLATE

	return mydef
}

func (sqliteDialect) FieldDefAlter(column, sqlType string, nullable bool, attrs map[string]string) string {
	mydef := psql.QuoteName(column) + " " + sqlType

	hasDefault := false
	isNotNull := false

	if null, ok := attrs["null"]; ok {
		switch null {
		case "0", "false":
			isNotNull = true
		}
	}

	if def, ok := attrs["default"]; ok {
		hasDefault = true
		if def == "\\N" {
			mydef += " DEFAULT NULL"
		} else {
			mydef += " DEFAULT " + psql.Escape(def)
		}
	}

	// SQLite requires a DEFAULT for NOT NULL columns added via ALTER TABLE
	if isNotNull {
		if !hasDefault {
			switch sqlType {
			case "integer":
				mydef += " NOT NULL DEFAULT 0"
			case "real":
				mydef += " NOT NULL DEFAULT 0.0"
			default:
				mydef += " NOT NULL DEFAULT ''"
			}
		} else {
			mydef += " NOT NULL"
		}
	}

	return mydef
}

// KeyRenderer implementation

func (sqliteDialect) KeyDef(k *psql.StructKey, tableName string) string {
	return createIndexSQLite(k, tableName)
}

// InlineKeyDef renders the PRIMARY KEY clause for CREATE TABLE. UNIQUE keys are
// created as named standalone indexes (see CreateIndex) so that CheckStructure
// can find them by name; they are therefore not rendered inline.
func (sqliteDialect) InlineKeyDef(k *psql.StructKey, tableName string) string {
	if k.Typ != psql.KeyPrimary {
		return "" // unique and other indexes are created as named indexes
	}
	s := &strings.Builder{}
	s.WriteString("PRIMARY KEY (")
	for i, f := range k.Fields {
		if i > 0 {
			s.WriteString(", ")
		}
		s.WriteString(psql.QuoteName(f))
	}
	s.WriteByte(')')
	return s.String()
}

func (sqliteDialect) CreateIndex(k *psql.StructKey, tableName string) string {
	return createIndexSQLite(k, tableName)
}

// indexName returns the name used for a standalone index on tableName.
func indexName(k *psql.StructKey, tableName string) string {
	return tableName + "_" + k.Key
}

func createIndexSQLite(k *psql.StructKey, tableName string) string {
	s := &strings.Builder{}

	switch k.Typ {
	case psql.KeyPrimary:
		return "" // handled inline
	case psql.KeyUnique:
		s.WriteString("CREATE UNIQUE INDEX ")
	case psql.KeyIndex:
		s.WriteString("CREATE INDEX ")
	default:
		// FULLTEXT, SPATIAL, VECTOR not supported in SQLite
		return ""
	}

	s.WriteString(psql.QuoteName(indexName(k, tableName)))
	s.WriteString(" ON ")
	s.WriteString(psql.QuoteName(tableName))
	s.WriteString(" (")
	for n, f := range k.Fields {
		if n > 0 {
			s.WriteString(", ")
		}
		s.WriteString(psql.QuoteName(f))
	}
	s.WriteByte(')')
	return s.String()
}

// UpsertRenderer implementation

func (sqliteDialect) ReplaceSQL(tableName, fldStr, placeholders string, mainKey *psql.StructKey, fields []*psql.StructField) string {
	return "INSERT OR REPLACE INTO " + psql.QuoteName(tableName) + " (" + fldStr + ") VALUES (" + placeholders + ")"
}

func (sqliteDialect) InsertIgnoreSQL(tableName, fldStr, placeholders string) string {
	return "INSERT OR IGNORE INTO " + psql.QuoteName(tableName) + " (" + fldStr + ") VALUES (" + placeholders + ")"
}

// DuplicateChecker implementation

// IsDuplicate reports whether err (or any error it wraps, including joined
// errors) is a SQLite UNIQUE constraint violation.
func (sqliteDialect) IsDuplicate(err error) bool {
	return errorTreeContains(err, "UNIQUE constraint failed")
}

// errorTreeContains walks the error tree (Unwrap() error and Unwrap() []error)
// and reports whether any error message contains needle.
func errorTreeContains(err error, needle string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), needle) {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return errorTreeContains(u.Unwrap(), needle)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if errorTreeContains(e, needle) {
				return true
			}
		}
	}
	return false
}

// SchemaChecker implementation

func (sqliteDialect) CheckStructure(ctx context.Context, be *psql.Backend, tv psql.TableView) error {
	return checkStructureSQLite(ctx, be, tv)
}

// sqliteFactory implements psql.BackendFactory for SQLite DSNs.
type sqliteFactory struct{}

func (sqliteFactory) MatchDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "sqlite:") ||
		strings.HasPrefix(dsn, "file:") ||
		strings.HasSuffix(dsn, ".db") ||
		strings.HasSuffix(dsn, ".sqlite") ||
		strings.HasSuffix(dsn, ".sqlite3") ||
		dsn == ":memory:"
}

func (sqliteFactory) CreateBackend(dsn string) (*psql.Backend, error) {
	return New(strings.TrimPrefix(dsn, "sqlite:"))
}

// isMemoryDSN reports whether dsn opens an in-memory database, which is
// private to a single connection.
func isMemoryDSN(dsn string) bool {
	return strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
}

// prepareDSN adds the default connection parameters (busy timeout, WAL,
// foreign keys, immediate transactions) to dsn unless the caller already set
// them. modernc.org/sqlite strips the query string from non-"file:" DSNs after
// applying the parameters, so this is safe for plain paths and ":memory:".
func prepareDSN(dsn string) (string, error) {
	base, rawQuery, _ := strings.Cut(dsn, "?")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid sqlite DSN parameters: %w", err)
	}

	hasPragma := func(name string) bool {
		for _, p := range q["_pragma"] {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), name) {
				return true
			}
		}
		return false
	}
	if !hasPragma("busy_timeout") {
		q.Add("_pragma", "busy_timeout("+strconv.FormatInt(BusyTimeout.Milliseconds(), 10)+")")
	}
	if !hasPragma("journal_mode") {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	if !hasPragma("foreign_keys") {
		q.Add("_pragma", "foreign_keys(1)")
	}
	if q.Get("_txlock") == "" {
		q.Set("_txlock", "immediate")
	}
	return base + "?" + q.Encode(), nil
}

// New creates a psql.Backend connected to a SQLite database at the given path
// or "file:" URI. Pass ":memory:" for an in-memory database.
//
// Each connection is opened with WAL journaling, foreign keys enabled, a busy
// timeout of [BusyTimeout] and "_txlock=immediate"; any of these already
// present in the DSN query string are kept as given.
//
// In-memory databases are private to one connection, so the pool is limited to
// exactly one connection: a query issued while a *sql.Rows is still open, or a
// query issued outside of an active transaction, waits forever for that
// connection. Close rows before issuing the next query and keep all work of a
// transaction inside it. File databases use a pool of up to [MaxOpenConns]
// connections, so concurrent readers do not block each other or the writer.
func New(dsn string) (*psql.Backend, error) {
	memory := isMemoryDSN(dsn)

	connDSN, err := prepareDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", connDSN)
	if err != nil {
		return nil, fmt.Errorf("sqlite connection failed: %w", err)
	}

	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if memory {
		// An in-memory database only exists in the connection that opened it:
		// never open a second one, and never let the first one be closed.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(MaxOpenConns)
		db.SetMaxIdleConns(MaxOpenConns)
	}

	// Open the first connection now so DSN/pragma errors surface here rather
	// than on the first query.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite connection failed: %w", err)
	}

	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		slog.Warn(fmt.Sprintf("[sqlite] failed to read journal mode: %s", err), "event", "psql:init:sqlite_wal")
	} else if !memory && !strings.EqualFold(journal, "wal") {
		slog.Warn(fmt.Sprintf("[sqlite] journal mode is %s, not WAL", journal), "event", "psql:init:sqlite_wal")
	}

	be := psql.NewBackend(psql.EngineSQLite, db)
	return be, nil
}

// sqliteTypeAffinity maps SQL types to SQLite type affinity names.
func sqliteTypeAffinity(typ string) string {
	typ = strings.ToLower(typ)
	switch typ {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "boolean", "bool":
		return "integer"
	case "float", "double", "real", "double precision", "numeric", "decimal":
		return "real"
	case "blob", "binary", "varbinary", "longblob", "mediumblob", "tinyblob":
		return "blob"
	case "char", "varchar", "text", "longtext", "mediumtext", "tinytext",
		"timestamp", "datetime", "date", "time",
		"jsonb", "json", "uuid", "xml", "cidr", "inet":
		return "text"
	default:
		return "text"
	}
}

// existingIndexes describes the indexes currently defined on a table.
type existingIndexes struct {
	names   map[string]bool // index names (including sqlite_autoindex_*)
	uniques map[string]bool // column lists ("a,b") of unique indexes, any origin
}

func (ei *existingIndexes) has(k *psql.StructKey, tableName string) bool {
	if ei.names[indexName(k, tableName)] {
		return true
	}
	if k.Typ == psql.KeyUnique && ei.uniques[strings.Join(k.Fields, ",")] {
		// An inline UNIQUE constraint (sqlite_autoindex_*) or PRIMARY KEY
		// already covers exactly these columns.
		return true
	}
	return false
}

// readIndexes lists the indexes of tableName via PRAGMA index_list, resolving
// the columns of unique indexes via PRAGMA index_info so that inline UNIQUE
// constraints (created by older versions of this package) are recognised.
func readIndexes(ctx context.Context, tableName string) (*existingIndexes, error) {
	ei := &existingIndexes{names: map[string]bool{}, uniques: map[string]bool{}}

	var uniqueNames []string
	err := psql.Q(fmt.Sprintf("PRAGMA index_list(%s)", psql.QuoteName(tableName))).Each(ctx, func(rows *sql.Rows) error {
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return err
		}
		ei.names[name] = true
		if unique == 1 && partial == 0 {
			uniqueNames = append(uniqueNames, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("while reading index list: %w", err)
	}

	for _, name := range uniqueNames {
		var cols []string
		err := psql.Q(fmt.Sprintf("PRAGMA index_info(%s)", psql.QuoteName(name))).Each(ctx, func(rows *sql.Rows) error {
			var seqno, cid int
			var col *string
			if err := rows.Scan(&seqno, &cid, &col); err != nil {
				return err
			}
			if col != nil {
				cols = append(cols, *col)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("while reading index %s: %w", name, err)
		}
		if len(cols) > 0 {
			ei.uniques[strings.Join(cols, ",")] = true
		}
	}
	return ei, nil
}

// checkStructureSQLite checks and creates the SQLite table structure.
func checkStructureSQLite(ctx context.Context, be *psql.Backend, tv psql.TableView) error {
	if v, ok := tv.TableAttrs()["check"]; ok && v == "0" {
		return nil
	}

	tableName := tv.FormattedName(be)

	var count int
	err := psql.Q("SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?", tableName).Each(ctx, func(rows *sql.Rows) error {
		return rows.Scan(&count)
	})
	if err != nil {
		return fmt.Errorf("while checking table existence: %w", err)
	}

	if count == 0 {
		return createTableSQLite(ctx, be, tv)
	}

	// Table exists, check columns
	type pragmaCol struct {
		CID        int
		Name       string
		Type       string
		NotNull    int
		DefaultVal *string
		PK         int
	}

	var existingCols []pragmaCol
	err = psql.Q(fmt.Sprintf("PRAGMA table_info(%s)", psql.QuoteName(tableName))).Each(ctx, func(rows *sql.Rows) error {
		var c pragmaCol
		if err := rows.Scan(&c.CID, &c.Name, &c.Type, &c.NotNull, &c.DefaultVal, &c.PK); err != nil {
			return err
		}
		existingCols = append(existingCols, c)
		return nil
	})
	if err != nil {
		return fmt.Errorf("while reading table info: %w", err)
	}

	colSet := make(map[string]bool)
	for _, c := range existingCols {
		colSet[c.Name] = true
	}

	for _, f := range tv.AllFields() {
		if colSet[f.Column] {
			continue
		}
		colDef := f.DefStringAlter(be)
		if colDef == "" {
			continue
		}
		alterSQL := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", psql.QuoteName(tableName), colDef)
		slog.Debug(fmt.Sprintf("[psql] SQLite ALTER: %s", alterSQL), "event", "psql:check:alter_sqlite", "table", tv.TableName())
		if err := psql.Q(alterSQL).Exec(ctx); err != nil {
			return fmt.Errorf("while adding column to %s: %w", tv.TableName(), err)
		}
	}

	// Check for missing indexes
	existing, err := readIndexes(ctx, tableName)
	if err != nil {
		return err
	}

	for _, k := range tv.AllKeys() {
		if k.Typ == psql.KeyPrimary || len(k.Fields) == 0 {
			continue
		}
		if existing.has(k, tableName) {
			continue
		}
		createSQL := createIndexSQLite(k, tableName)
		if createSQL == "" {
			continue
		}
		slog.Debug(fmt.Sprintf("[psql] Creating SQLite index: %s", createSQL), "event", "psql:check:create_index_sqlite", "table", tv.TableName())
		if err := psql.Q(createSQL).Exec(ctx); err != nil {
			return fmt.Errorf("while creating index on %s: %w", tv.TableName(), err)
		}
	}

	return nil
}

func createTableSQLite(ctx context.Context, be *psql.Backend, tv psql.TableView) error {
	tableName := tv.FormattedName(be)

	sb := &strings.Builder{}
	sb.WriteString("CREATE TABLE ")
	sb.WriteString(psql.QuoteName(tableName))
	sb.WriteString(" (")

	for n, f := range tv.AllFields() {
		if n > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(f.DefString(be))
	}

	d := sqliteDialect{}
	for _, k := range tv.AllKeys() {
		if len(k.Fields) == 0 {
			continue
		}
		if inline := d.InlineKeyDef(k, tableName); inline != "" {
			sb.WriteString(", ")
			sb.WriteString(inline)
		}
	}

	sb.WriteByte(')')

	if err := psql.Q(sb.String()).Exec(ctx); err != nil {
		return fmt.Errorf("while creating table: %w", err)
	}

	// UNIQUE and INDEX keys are created as named indexes so CheckStructure can
	// find them again by name on the next start.
	for _, k := range tv.AllKeys() {
		if len(k.Fields) == 0 {
			continue
		}
		createSQL := createIndexSQLite(k, tableName)
		if createSQL == "" {
			continue
		}
		if err := psql.Q(createSQL).Exec(ctx); err != nil {
			return fmt.Errorf("while creating index: %w", err)
		}
	}

	return nil
}
