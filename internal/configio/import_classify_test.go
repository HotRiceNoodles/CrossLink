package configio

import (
	"errors"
	"testing"
)

func TestIsConstraintErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New(`pq: duplicate key value violates unique constraint "providers_name_key"`), true},
		{errors.New("Error 1062 (23000): Duplicate entry 'openai' for key 'name'"), true},
		{errors.New("UNIQUE constraint failed: providers.name"), true},
		{errors.New("create provider: duplicate key"), true},
		// Locale-independent SQLSTATE + the zh_CN localized form (regression:
		// previously matched only English keywords and returned 500).
		{errors.New(`create provider: 错误: 重复键违反唯一约束"providers_name_key" (SQLSTATE 23505)`), true},
		{errors.New(`重复键违反唯一约束`), true},
		{nil, false},
		{errors.New("connection refused"), false},
		{errors.New("some other transient error"), false},
	}
	for _, c := range cases {
		if got := isConstraintErr(c.err); got != c.want {
			t.Errorf("isConstraintErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestIsConnectionErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("dial tcp: connection refused"), true},
		{errors.New("write: broken pipe"), true},
		{errors.New("read: connection reset by peer"), true},
		{errors.New("context deadline exceeded (timeout)"), true},
		{errors.New("unexpected EOF"), true},
		{nil, false},
		{errors.New("duplicate key value"), false},
		{errors.New("invalid input syntax"), false},
	}
	for _, c := range cases {
		if got := isConnectionErr(c.err); got != c.want {
			t.Errorf("isConnectionErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
