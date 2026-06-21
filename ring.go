package main

import "sync"

// ringBuffer keeps the last N bytes written to it. It is safe for concurrent
// use: the console pump writes while the HTTP layer snapshots via String.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity <= 0 {
		capacity = 64 * 1024
	}
	return &ringBuffer{cap: capacity}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		// Re-slice to the tail and copy so the backing array can be freed.
		tail := r.buf[len(r.buf)-r.cap:]
		b := make([]byte, len(tail))
		copy(b, tail)
		r.buf = b
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
