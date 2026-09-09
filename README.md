[![Go Reference](https://pkg.go.dev/badge/github.com/portablesql/psql-sqlite.svg)](https://pkg.go.dev/github.com/portablesql/psql-sqlite)
[![Tests](https://github.com/portablesql/psql-sqlite/actions/workflows/test.yml/badge.svg)](https://github.com/portablesql/psql-sqlite/actions/workflows/test.yml)

# psql-sqlite

SQLite driver for [portablesql/psql](https://github.com/portablesql/psql), built on
[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO).

## Installation

```bash
go get github.com/portablesql/psql github.com/portablesql/psql-sqlite
```

Requires Go 1.25 or later.

## Usage

The package registers its dialect and backend factory in `init()`. Import it with a blank
identifier and `psql.New(dsn)` auto-detects SQLite DSNs:

```go
import (
    "context"

    "github.com/portablesql/psql"
    _ "github.com/portablesql/psql-sqlite"
)

be, err := psql.New(":memory:")          // in-memory database
be, err := psql.New("file:app.db")       // file URI
be, err := psql.New("app.sqlite3")       // plain path, detected by extension
if err != nil {
    return err
}
ctx := be.Plug(context.Background())
```

### DSN formats

`MatchDSN` accepts:

- `:memory:`;
- strings starting with `sqlite:` (the prefix is stripped before opening, e.g. `sqlite:/tmp/x`);
- strings starting with `file:` (SQLite URI, e.g. `file:test.db?cache=shared`);
- strings ending in `.db`, `.sqlite` or `.sqlite3`.

A plain path with another extension is not detected by `psql.New`; open it with
`sqlite.New(path)` or prefix it with `sqlite:`.

### Explicit construction

```go
import psqlsqlite "github.com/portablesql/psql-sqlite"

be, err := psqlsqlite.New("file:app.db?_pragma=busy_timeout(5000)")   // func New(dsn string) (*psql.Backend, error)
```

`New` opens the first connection immediately so that DSN or pragma errors surface at startup
rather than on the first query. There is no `InitCfg`; assign `psql.DefaultBackend = be` if you
want a global backend.

### Connections and defaults

Every connection is opened with these DSN parameters, each added only if the DSN does not
already set it:

| Parameter | Default |
|-----------|---------|
| `_pragma=busy_timeout(...)` | `BusyTimeout` = 10 s |
| `_pragma=journal_mode(WAL)` | WAL journaling (a warning is logged if a file database ends up in another mode) |
| `_pragma=foreign_keys(1)` | foreign keys enforced |
| `_txlock` | `immediate` (write transactions take the lock at `BEGIN`) |

Pool size depends on the DSN:

- **In-memory** (`:memory:` or any DSN containing `mode=memory`): the database only exists in
  the connection that opened it, so the pool is limited to exactly one connection that is never
  closed. With a single connection, a query issued while a `*sql.Rows` is still open, or issued
  outside a transaction while that transaction is active, waits forever for the connection.
  Close rows (or use `Each`/`All`/`Fetch`) before running the next query and keep all work of a
  transaction inside it.
- **File databases**: a pool of up to `MaxOpenConns` = 8 connections in WAL mode, so several
  readers run concurrently with one writer. A second writer waits up to the busy timeout and
  then fails with `SQLITE_BUSY` instead of deadlocking.

Connections have no max lifetime or idle time.

## Dialect behavior

- Placeholders are `?`; `Limit(offset, count)` renders `LIMIT count OFFSET offset`.
- `Replace` renders `INSERT OR REPLACE INTO`, `InsertIgnore` renders `INSERT OR IGNORE INTO`;
  the query builder's `OnConflict(cols...).DoUpdate(...)` / `DoNothing()` render
  `ON CONFLICT ... DO UPDATE` / `DO NOTHING`.
- `FOR UPDATE` (and `SKIP LOCKED` / `NOWAIT`) is silently omitted; SQLite locks at the
  database level.
- `CILike` renders `LOWER(col) LIKE LOWER(pattern)`; `Greatest`/`Least` render multi-argument
  `MAX()`/`MIN()`; `DateAdd`/`DateSub` render `datetime(expr, '+N unit')`; `Now()` renders
  `CURRENT_TIMESTAMP`.

### Timestamps

`time.Time` values are stored as `TEXT` in the fixed-width UTC form
`TimeFormat` = `2006-01-02T15:04:05.000000000Z07:00`, always rendered with a `Z` suffix and
nanosecond precision (for example `2024-05-01T12:34:56.000000000Z`). Because every value has the
same width, lexical ordering and comparison of stored timestamps match chronological order. The
zero time is stored as `ZeroTime` = `0001-01-01T00:00:00.000000000Z`.

### Type mapping (`SqlType`)

SQLite uses type affinity, so declared types collapse to five storage classes and `size=` is
ignored:

| Struct tag type | Column type |
|-----------------|-------------|
| `enum`, `set`, `VECTOR` | `text` |
| `tinyint`, `smallint`, `mediumint`, `int`, `integer`, `bigint`, `boolean`, `bool` | `integer` |
| `float`, `double`, `real`, `double precision`, `numeric`, `decimal` | `real` |
| `blob`, `binary`, `varbinary`, `longblob`, `mediumblob`, `tinyblob` | `blob` |
| `char`, `varchar`, `text`, `*text`, `timestamp`, `datetime`, `date`, `time`, `json`, `jsonb`, `uuid`, `xml`, `cidr`, `inet` and anything else | `text` |
| `type=DATETIME` (magic type) | `TEXT` |
| `type=JSON` (magic type) | `TEXT` with `format=json` |

Column definitions honor `null=0/1` and `default=...` (`default=\N` gives `DEFAULT NULL`);
the `collation` attribute is ignored. When a `NOT NULL` column is added to an existing table
with `ALTER TABLE`, SQLite requires a default, so `DEFAULT 0` (integer), `DEFAULT 0.0` (real)
or `DEFAULT ''` (other) is added when the struct declares none.

Two things SQLite does **not** do:

- **Enum values are not validated.** `enum` columns are plain `text` with no `CHECK`
  constraint; call `psql.ValidateEnum` yourself if you need enforcement.
- **Vectors are stored as text** (`[1,2,3]`) and can be read back into `psql.Vector`, but there
  is no distance operator: rendering a query that uses `VecL2Distance`, `VecCosineDistance` or
  `VecInnerProduct` returns an error. Vector similarity search needs PostgreSQL or CockroachDB.

### Keys and indexes

- `PRIMARY KEY (...)` is created inline in `CREATE TABLE`.
- `UNIQUE` and `INDEX` keys are created as named standalone indexes
  (`CREATE UNIQUE INDEX "table_key" ...` / `CREATE INDEX "table_key" ...`) so the schema check
  can find them again by name.
- `FULLTEXT`, `SPATIAL` and `VECTOR` keys are not supported and are ignored.

### Schema check

On first use of a table (unless the backend was created with `psql.WithSchemaCheck(false)`,
in which case call `be.CheckStructure` yourself) the driver looks the table up in
`sqlite_master`:

- missing table: `CREATE TABLE` followed by the `CREATE INDEX` statements;
- existing table: columns missing from `PRAGMA table_info` are added with
  `ALTER TABLE ... ADD COLUMN`; keys missing from `PRAGMA index_list` are created. A `UNIQUE`
  key is considered present when an index of any name (including the `sqlite_autoindex_*`
  created by an inline constraint in older versions of this driver) already covers exactly
  its columns.

Existing columns are never modified or dropped (SQLite has very limited `ALTER TABLE`). A table
declared with `psql.Name \`sql:"name,check=0"\`` is never modified.

## Error classification

SQLite errors carry no numeric code that psql can classify, so `psql.ErrorNumber` returns
`0xffff` and `psql.IsNotExist` returns `false` for SQLite errors.

`psql.IsDuplicate(err)` is true when any error in the tree (wrapped and joined errors included)
contains `UNIQUE constraint failed`.

## Testing

Everything runs in-process; no server is needed:

```bash
go test ./...
```

The unit tests cover DSN handling, timestamp round trips, the schema check and concurrent
access to file databases. The integration suite in
[portablesql/psql-test](https://github.com/portablesql/psql-test) uses in-memory SQLite by
default (no `PSQL_TEST_DSN` needed):

```bash
go work init ./psql ./psql-sqlite ./psql-test
cd psql-test
go test -race -count=1 ./...
```

## License

MIT, same as [psql](https://github.com/portablesql/psql/blob/master/LICENSE).
