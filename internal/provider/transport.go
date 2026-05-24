package provider

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

const (
	defaultMaxConnsPerHost       = 200
	defaultMaxIdleConnsPerHost   = 20
	defaultMaxIdleConns          = 400
	defaultIdleConnTimeout       = 90 * time.Second
	defaultResponseHeaderTimeout = 60 * time.Second
)

func newStreamTransport() *http.Transport {
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &tls.Dialer{Config: &tls.Config{NextProtos: []string{"http/1.1"}}}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxConnsPerHost:        defaultMaxConnsPerHost,
		MaxIdleConns:           defaultMaxIdleConns,
		MaxIdleConnsPerHost:    defaultMaxIdleConnsPerHost,
		IdleConnTimeout:        defaultIdleConnTimeout,
		ResponseHeaderTimeout:  defaultResponseHeaderTimeout,
	}
}

func newDefaultTransport() *http.Transport {
	return &http.Transport{
		MaxConnsPerHost:     defaultMaxConnsPerHost,
		MaxIdleConns:        defaultMaxIdleConns,
		MaxIdleConnsPerHost: defaultMaxIdleConnsPerHost,
		IdleConnTimeout:     defaultIdleConnTimeout,
	}
}
