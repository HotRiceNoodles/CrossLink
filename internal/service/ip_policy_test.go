package service

import (
	"testing"

	"github.com/crosslink/internal/model"
)

func TestNoopPolicyAlwaysAllows(t *testing.T) {
	p := NoopPolicy{}
	key := &model.APIKey{ID: 1}
	if err := p.Check(key, "1.2.3.4", ""); err != nil {
		t.Fatalf("NoopPolicy must always return nil, got %v", err)
	}
	// empty IP also allowed (Noop ignores everything)
	if err := p.Check(key, "", ""); err != nil {
		t.Fatalf("NoopPolicy must allow empty IP, got %v", err)
	}
}
