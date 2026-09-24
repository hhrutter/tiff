// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// readOnly hides optional interfaces implemented by the underlying reader.
type readOnly struct{ io.Reader }

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(1, len(p))])
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type measuredReader struct {
	io.Reader
	maxRead int
}

func (r *measuredReader) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	return r.Reader.Read(p)
}

var readAtTests = []struct {
	n   int
	off int64
	s   string
	err error
}{
	{2, 0, "ab", nil},
	{6, 0, "abcdef", nil},
	{3, 3, "def", nil},
	{3, 5, "f", io.EOF},
	{3, 6, "", io.EOF},
	{2, 0, "ab", nil},
	{3, 1, "bcd", nil},
	{2, 4, "ef", nil},
}

func TestReadAt(t *testing.T) {
	r := newReaderAt(readOnly{strings.NewReader("abcdef")})
	if _, ok := r.(*buffer); !ok {
		t.Fatal("test did not exercise the buffered adapter")
	}
	b := make([]byte, 10)
	for _, test := range readAtTests {
		n, err := r.ReadAt(b[:test.n], test.off)
		s := string(b[:n])
		if s != test.s || err != test.err {
			t.Errorf("buffer.ReadAt(<%v bytes>, %v): got %v, %q; want %v, %q", test.n, test.off, err, s, test.err, test.s)
		}
	}
}

func TestReadAtShortReader(t *testing.T) {
	r := newReaderAt(readOnly{oneByteReader{strings.NewReader("abcdef")}})
	p := make([]byte, 8)
	n, err := r.ReadAt(p, 0)
	if n != 6 || err != io.EOF || string(p[:n]) != "abcdef" {
		t.Fatalf("ReadAt = %d, %v, %q; want 6, EOF, abcdef", n, err, p[:n])
	}
	if n, err = r.ReadAt(p[:3], 2); n != 3 || err != nil || string(p[:n]) != "cde" {
		t.Fatalf("cached ReadAt = %d, %v, %q; want 3, nil, cde", n, err, p[:n])
	}
}

func TestReadAtSourceError(t *testing.T) {
	wantErr := errors.New("source failure")
	r := newReaderAt(readOnly{io.MultiReader(strings.NewReader("x"), errorReader{wantErr})})
	p := make([]byte, 3)
	n, err := r.ReadAt(p, 0)
	if n != 1 || !errors.Is(err, wantErr) || string(p[:n]) != "x" {
		t.Fatalf("ReadAt = %d, %v, %q; want 1, source error, x", n, err, p[:n])
	}
}

func TestReadAtConcurrent(t *testing.T) {
	data := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 100)
	r := newReaderAt(readOnly{oneByteReader{strings.NewReader(data)}})
	var wg sync.WaitGroup
	results := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := int64(i * 31)
			p := make([]byte, 17)
			n, err := r.ReadAt(p, off)
			if err != nil || n != len(p) || string(p) != data[off:off+int64(len(p))] {
				results <- fmt.Errorf("ReadAt(%d) = %d, %v, %q", off, n, err, p[:n])
			}
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		t.Error(err)
	}
}

func TestBufferInvalidOffsets(t *testing.T) {
	b := newReaderAt(readOnly{strings.NewReader("abc")}).(*buffer)
	p := make([]byte, 3)
	maxInt := int(^uint(0) >> 1)
	for _, off := range []int64{-1, -1 << 63, int64(maxInt), 1<<63 - 1} {
		n, err := b.ReadAt(p, off)
		if n != 0 || err == nil {
			t.Fatalf("ReadAt offset %d = %d, %v; want an error with no bytes", off, n, err)
		}
	}
	for _, tc := range []struct{ off, n int }{{-1, 1}, {0, -1}, {maxInt, 1}} {
		if _, err := b.Slice(tc.off, tc.n); err == nil {
			t.Fatalf("Slice(%d, %d) accepted an invalid range", tc.off, tc.n)
		}
	}
	if got, err := b.Slice(1, 2); err != nil || string(got) != "bc" {
		t.Fatalf("Slice(1, 2) = %q, %v; want bc", got, err)
	}
	if _, err := b.Slice(2, 2); err == nil {
		t.Fatal("Slice accepted data past EOF")
	}
	if _, err := b.Slice(1<<30, 1); err == nil || cap(b.buf) > 64<<10 {
		t.Fatalf("Slice at a large strip offset = %v with capacity %d; want bounded EOF", err, cap(b.buf))
	}
}

func TestBufferedReaderLargeIFDOffset(t *testing.T) {
	var header [8]byte
	copy(header[:4], leHeader)
	binary.LittleEndian.PutUint32(header[4:], 1<<30)

	source := &measuredReader{Reader: bytes.NewReader(header[:])}
	if _, err := Decode(readOnly{source}); err == nil {
		t.Fatal("Decode accepted an IFD beyond the input")
	}
	if source.maxRead > 32<<10 {
		t.Fatalf("Decode requested %d bytes from an eight-byte source", source.maxRead)
	}

	// Exercise the same decoder path with an observable adapter to bound retained storage.
	b := newReaderAt(readOnly{bytes.NewReader(header[:])}).(*buffer)
	d := decoder{r: b}
	if err := d.readImageIFD(0); err == nil {
		t.Fatal("readImageIFD accepted an IFD beyond the input")
	}
	if cap(b.buf) > 64<<10 || len(b.buf) != len(header) {
		t.Fatalf("buffered %d bytes with capacity %d for an eight-byte input", len(b.buf), cap(b.buf))
	}
}
