// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"fmt"
	"io"
	"sync"
)

// buffer buffers an io.Reader to satisfy io.ReaderAt.
type buffer struct {
	mu  sync.Mutex
	r   io.Reader
	buf []byte
}

// fill reads data from b.r until the buffer contains at least end bytes.
func (b *buffer) fill(end int) error {
	if end < 0 {
		return io.ErrUnexpectedEOF
	}
	var chunk [32 << 10]byte
	for len(b.buf) < end {
		// Read into bounded storage before growing the retained buffer. In particular,
		// an IFD offset must not determine an allocation before the input exists.
		next := min(end-len(b.buf), len(chunk))
		n, err := io.ReadFull(b.r, chunk[:next])
		b.buf = append(b.buf, chunk[:n]...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *buffer) ReadAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off < 0 {
		return 0, fmt.Errorf("tiff: invalid ReadAt offset %d", off)
	}
	maxInt := int(^uint(0) >> 1)
	if off > int64(maxInt) || len(p) > maxInt-int(off) {
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	err := b.fill(int(off) + len(p))
	n := 0
	if int(off) < len(b.buf) {
		n = copy(p, b.buf[int(off):])
	}
	if n == len(p) {
		return n, nil
	}
	if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
		return n, io.EOF
	}
	return n, err
}

// Slice returns a slice of the underlying buffer. The slice contains
// n bytes starting at offset off.
func (b *buffer) Slice(off, n int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off < 0 || n < 0 {
		return nil, fmt.Errorf("tiff: invalid Slice offset %d or length %d", off, n)
	}
	if n > int(^uint(0)>>1)-off {
		return nil, io.ErrUnexpectedEOF
	}
	end := off + n
	if err := b.fill(end); err != nil {
		return nil, err
	}
	return b.buf[off:end], nil
}

// newReaderAt converts an io.Reader into an io.ReaderAt.
func newReaderAt(r io.Reader) io.ReaderAt {
	if ra, ok := r.(io.ReaderAt); ok {
		return ra
	}
	return &buffer{
		r:   r,
		buf: make([]byte, 0, 1024),
	}
}
