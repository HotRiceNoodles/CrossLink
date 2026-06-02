package dialect

import "testing"

func TestNewDialect_Postgres(t *testing.T) {
	d, err := New(DBConfig{Driver: "postgres", Host: "h", Port: 5432, User: "u", Password: "p", DBName: "d", SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*PostgresDialect); !ok {
		t.Fatalf("expected *PostgresDialect, got %T", d)
	}
}

func TestNewDialect_SQLite(t *testing.T) {
	d, err := New(DBConfig{Driver: "sqlite", SQLitePath: "/tmp/test.db"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*SQLiteDialect); !ok {
		t.Fatalf("expected *SQLiteDialect, got %T", d)
	}
}

func TestNewDialect_MySQL(t *testing.T) {
	d, err := New(DBConfig{Driver: "mysql", Host: "h", Port: 3306, User: "u", Password: "p", DBName: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*MySQLDialect); !ok {
		t.Fatalf("expected *MySQLDialect, got %T", d)
	}
}

func TestNewDialect_Unknown(t *testing.T) {
	_, err := New(DBConfig{Driver: "oracle"})
	if err == nil {
		t.Fatal("expected error for unknown driver")
	}
}
