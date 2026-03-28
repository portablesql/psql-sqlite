[![Go Reference](https://pkg.go.dev/badge/github.com/portablesql/psql-sqlite.svg)](https://pkg.go.dev/github.com/portablesql/psql-sqlite)

# psql-sqlite

SQLite driver for [portablesql/psql](https://github.com/portablesql/psql).

## Installation

```bash
go get github.com/portablesql/psql-sqlite
```

## Usage

Import with a blank identifier to register the driver automatically:

```go
import (
    "github.com/portablesql/psql"
    _ "github.com/portablesql/psql-sqlite"
)

// In-memory database
be, err := psql.New(":memory:")
ctx := be.Plug(context.Background())

// File-based database
be, err := psql.New("file:myapp.db")
be, err := psql.New("myapp.sqlite3")
```

Recognized DSN patterns: `:memory:`, `sqlite:` prefix, `file:` prefix, and `.db` / `.sqlite` / `.sqlite3` file extensions.

## Features

- WAL (Write-Ahead Logging) mode enabled by default
- Foreign keys enforced via `PRAGMA foreign_keys=ON`
- Single-connection mode (`MaxOpenConns=1`) for safe concurrent access
- `INSERT OR REPLACE INTO` and `INSERT OR IGNORE INTO` for upserts
- SQLite type affinity mapping (`TEXT`, `INTEGER`, `REAL`, `BLOB`)
- `COLLATE NOCASE` for case-insensitive LIKE
- `MAX()`/`MIN()` substitution for `GREATEST()`/`LEAST()`
- `datetime()` function for portable timestamp arithmetic
- Automatic table creation and schema migration via `PRAGMA table_info`
- `FOR UPDATE` silently omitted (SQLite uses file/WAL-level locking)
- Timestamps stored as RFC 3339 text

## Underlying Driver

[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO required).

## License

MIT - see [LICENSE](LICENSE).
