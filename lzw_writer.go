// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bufio"
	"errors"
	"io"
)

type lzwByteWriter interface {
	io.ByteWriter
	Flush() error
}

type lzwWriter struct {
	w         lzwByteWriter
	bits      uint32
	nBits     uint
	width     uint
	hi        uint32
	overflow  uint32
	savedCode uint32
	err       error
	table     [lzwTableSize]uint32
}

const (
	lzwMaxCode      = 1<<12 - 1
	lzwInvalidCode  = 1<<32 - 1
	lzwTableSize    = 4 * 1 << 12
	lzwTableMask    = lzwTableSize - 1
	lzwInvalidEntry = 0
	lzwLitWidth     = 8
	lzwClear        = 1 << lzwLitWidth
	lzwEOF          = lzwClear + 1
)

var (
	errLZWClosed     = errors.New("lzw: reader/writer is closed")
	errLZWOutOfCodes = errors.New("lzw: out of codes")
)

func (w *lzwWriter) writeCode(c uint32) error {
	w.bits |= c << (32 - w.width - w.nBits)
	w.nBits += w.width
	for w.nBits >= 8 {
		if err := w.w.WriteByte(uint8(w.bits >> 24)); err != nil {
			return err
		}
		w.bits <<= 8
		w.nBits -= 8
	}
	return nil
}

func (w *lzwWriter) resetTable() {
	w.width = lzwLitWidth + 1
	w.hi = lzwClear + 1
	w.overflow = lzwClear << 1
	for i := range w.table {
		w.table[i] = lzwInvalidEntry
	}
}

func (w *lzwWriter) incHi() error {
	w.hi++
	if w.hi+1 == w.overflow {
		w.width++
		w.overflow <<= 1
	}
	if w.hi+1 != lzwMaxCode {
		return nil
	}
	if err := w.writeCode(lzwClear); err != nil {
		return err
	}
	w.resetTable()
	return errLZWOutOfCodes
}

func (w *lzwWriter) find(key uint32) (uint32, uint32, bool) {
	hash := (key>>12 ^ key) & lzwTableMask
	for h, t := hash, w.table[hash]; t != lzwInvalidEntry; {
		if key == t>>12 {
			return hash, t & lzwMaxCode, true
		}
		h = (h + 1) & lzwTableMask
		t = w.table[h]
	}
	return hash, 0, false
}

func (w *lzwWriter) insert(hash uint32, key uint32) {
	for {
		if w.table[hash] == lzwInvalidEntry {
			w.table[hash] = (key << 12) | w.hi
			return
		}
		hash = (hash + 1) & lzwTableMask
	}
}

func (w *lzwWriter) writeLiteral(code uint32, literal uint32) error {
	key := code<<8 | literal
	hash, _, _ := w.find(key)
	if err := w.writeCode(code); err != nil {
		return err
	}
	if err := w.incHi(); err != nil {
		return err
	}
	w.insert(hash, key)
	return nil
}

func (w *lzwWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := len(p)
	code, p := w.firstCode(p)
	for _, x := range p {
		literal := uint32(x)
		key := code<<8 | literal
		if _, next, ok := w.find(key); ok {
			code = next
			continue
		}
		if err := w.writeLiteral(code, literal); err != nil && err != errLZWOutOfCodes {
			w.err = err
			return 0, err
		}
		code = literal
	}
	w.savedCode = code
	return n, nil
}

func (w *lzwWriter) firstCode(p []byte) (uint32, []byte) {
	if w.savedCode != lzwInvalidCode {
		return w.savedCode, p
	}
	return uint32(p[0]), p[1:]
}

func (w *lzwWriter) Close() error {
	if w.err != nil {
		if w.err == errLZWClosed {
			return nil
		}
		return w.err
	}
	w.err = errLZWClosed
	if err := w.closeCodes(); err != nil {
		return err
	}
	return w.w.Flush()
}

func (w *lzwWriter) closeCodes() error {
	if w.savedCode != lzwInvalidCode {
		if err := w.writeCode(w.savedCode); err != nil {
			return err
		}
		if err := w.incHi(); err != nil && err != errLZWOutOfCodes {
			return err
		}
	}
	if err := w.writeCode(lzwEOF); err != nil {
		return err
	}
	if w.nBits == 0 {
		return nil
	}
	w.bits >>= 24
	return w.w.WriteByte(uint8(w.bits))
}

func newTIFFLZWWriter(dst io.Writer) io.WriteCloser {
	bw, ok := dst.(lzwByteWriter)
	if !ok {
		bw = bufio.NewWriter(dst)
	}
	w := &lzwWriter{
		w:         bw,
		savedCode: lzwInvalidCode,
	}
	w.resetTable()
	_ = w.writeCode(lzwClear)
	return w
}
