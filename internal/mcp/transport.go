package mcp

import "context"

type Transport interface {
	Send(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error)
	Close() error
	Ping(ctx context.Context) error
}
