// Copyright 2011 The Go Authors. All rights reserved.
// Copyright 2026 The tiff Authors.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package lzw implements the Lempel-Ziv-Welch variant used by TIFF.
package lzw

import (
	"bufio"
	"errors"
	"io"
)

const (
	literalWidth = 8
	maxWidth     = 12
	clearCode    = 1 << literalWidth
	eofCode      = clearCode + 1
	invalidCode  = 0xffff
	tableSize    = 1 << maxWidth

	// maxCodesPastFull matches the decoder table slack used by libtiff.
	maxCodesPastFull = 1024
	flushBuffer      = tableSize
)

var (
	errClosed       = errors.New("lzw: reader is closed")
	errInvalidCode  = errors.New("lzw: invalid code")
	errMissingClear = errors.New("lzw: no clear code after dictionary filled")
)

type decoder struct {
	r     io.ByteReader
	bits  uint32
	nBits uint
	width uint
	err   error

	hi, overflow, last uint16
	codesPastFull      int
	suffix             [tableSize]uint8
	prefix             [tableSize]uint16
	output             [2 * tableSize]byte
	o                  int
	toRead             []byte
}

// Read decompresses data into b.
func (d *decoder) Read(b []byte) (int, error) {
	for {
		if len(d.toRead) > 0 {
			n := copy(b, d.toRead)
			d.toRead = d.toRead[n:]
			return n, nil
		}
		if d.err != nil {
			return 0, d.err
		}
		d.decode()
	}
}

func (d *decoder) readCode() (uint16, error) {
	for d.nBits < d.width {
		x, err := d.r.ReadByte()
		if err != nil {
			return 0, err
		}
		d.bits |= uint32(x) << (24 - d.nBits)
		d.nBits += 8
	}
	code := uint16(d.bits >> (32 - d.width))
	d.bits <<= d.width
	d.nBits -= d.width
	return code, nil
}

func (d *decoder) decode() {
	for d.err == nil && d.o < flushBuffer {
		code, err := d.readCode()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			d.err = err
			break
		}
		d.decodeCode(code)
	}
	d.toRead = d.output[:d.o]
	d.o = 0
}

func (d *decoder) decodeCode(code uint16) {
	switch code {
	case clearCode:
		d.resetTable()
		return
	case eofCode:
		d.err = io.EOF
		return
	}

	head, ok := d.expand(code)
	if !ok {
		return
	}
	d.saveEntry(head)
	d.last = code
	d.advanceCode()
}

func (d *decoder) resetTable() {
	d.width = literalWidth + 1
	d.hi = eofCode
	d.overflow = 1 << d.width
	d.last = invalidCode
	d.codesPastFull = 0
}

func (d *decoder) expand(code uint16) (uint8, bool) {
	if code < clearCode {
		d.output[d.o] = uint8(code)
		d.o++
		return uint8(code), true
	}
	if code > d.hi {
		d.err = errInvalidCode
		return 0, false
	}

	c, i := code, len(d.output)-1
	if code == d.hi {
		c = d.last
		head := d.head(c)
		d.output[i] = head
		i--
	}
	for c >= clearCode {
		d.output[i] = d.suffix[c]
		i--
		c = d.prefix[c]
	}
	head := uint8(c)
	d.output[i] = head
	d.o += copy(d.output[d.o:], d.output[i:])
	return head, true
}

func (d *decoder) head(code uint16) uint8 {
	for code >= clearCode {
		code = d.prefix[code]
	}
	return uint8(code)
}

func (d *decoder) saveEntry(head uint8) {
	if d.last == invalidCode || d.hi >= tableSize {
		return
	}
	d.suffix[d.hi] = head
	d.prefix[d.hi] = d.last
}

func (d *decoder) advanceCode() {
	if d.hi < tableSize {
		d.hi++
	} else {
		d.codesPastFull++
		if d.codesPastFull > maxCodesPastFull {
			d.err = errMissingClear
			return
		}
	}
	if d.hi+1 >= d.overflow && d.width < maxWidth {
		d.width++
		d.overflow <<= 1
	}
}

// Close prevents further reads without closing the underlying reader.
func (d *decoder) Close() error {
	d.err = errClosed
	return nil
}

// NewReader creates a reader for MSB-first TIFF LZW data.
func NewReader(r io.Reader) io.ReadCloser {
	br, ok := r.(io.ByteReader)
	if !ok {
		br = bufio.NewReader(r)
	}
	d := &decoder{r: br}
	d.resetTable()
	return d
}
