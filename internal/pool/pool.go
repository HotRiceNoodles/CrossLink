package pool

import (
	"bytes"
	"sync"
)

var ScannerBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 64*1024)
		return &buf
	},
}

func GetScannerBuffer() *[]byte {
	return ScannerBufferPool.Get().(*[]byte)
}

func PutScannerBuffer(buf *[]byte) {
	*buf = (*buf)[:0]
	ScannerBufferPool.Put(buf)
}

var BytesBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

func GetBuffer() *bytes.Buffer {
	buf := BytesBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func PutBuffer(buf *bytes.Buffer) {
	if buf.Cap() > 1024*1024 {
		return
	}
	BytesBufferPool.Put(buf)
}
