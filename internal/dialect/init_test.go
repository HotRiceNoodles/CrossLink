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

func TestNewDialect_Kingbase(t *testing.T) {
	d, err := New(DBConfig{Driver: "kingbase", Host: "h", Port: 54321, User: "u", Password: "p", DBName: "d", SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*KingbaseDialect); !ok {
		t.Fatalf("expected *KingbaseDialect, got %T", d)
	}
}

func TestNewDialect_KingbaseES(t *testing.T) {
	d, err := New(DBConfig{Driver: "kingbasees", Host: "h", Port: 54321, User: "u", Password: "p", DBName: "d", SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*KingbaseDialect); !ok {
		t.Fatalf("expected *KingbaseDialect for 'kingbasees', got %T", d)
	}
}

func TestNewDialect_OceanBase(t *testing.T) {
	d, err := New(DBConfig{Driver: "oceanbase", Host: "h", Port: 2883, User: "u", Password: "p", DBName: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*OceanBaseDialect); !ok {
		t.Fatalf("expected *OceanBaseDialect, got %T", d)
	}
}

func TestNewDialect_Unknown(t *testing.T) {
	_, err := New(DBConfig{Driver: "oracle"})
	if err == nil {
		t.Fatal("expected error for unknown driver")
	}
}
