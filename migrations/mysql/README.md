# MySQL migrations — EXPERIMENTAL

MySQL is an **experimental** backend. This directory only contains migrations
`000001`–`000002`; the first-class PostgreSQL set (see `../postgres/`, 74+
migrations) is not ported here, so a MySQL database will be missing most tables
and is suitable for evaluation only.

The server prints a warning at startup when running against MySQL.

Do not add new migrations to this directory unless the dialect is being
re-adopted (full port of the PostgreSQL set + CI coverage). See
[docs/migration-guidelines.md](../../docs/migration-guidelines.md).
