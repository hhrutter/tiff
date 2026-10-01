// Copyright 2026 The tiff Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzw

import (
	"bytes"
	"io"
	"testing"
)

type codeWriter struct {
	buf   bytes.Buffer
	bits  uint32
	nBits uint
	width uint
	next  int
}

func newCodeWriter() *codeWriter {
	w := &codeWriter{width: literalWidth + 1, next: eofCode + 1}
	w.put(clearCode)
	return w
}

func (w *codeWriter) put(code int) {
	w.bits = w.bits<<w.width | uint32(code)
	w.nBits += w.width
	for w.nBits >= 8 {
		w.nBits -= 8
		w.buf.WriteByte(byte(w.bits >> w.nBits))
	}
}

func (w *codeWriter) emit(code int) {
	w.put(code)
	if w.next < tableSize {
		w.next++
	}
	if w.next+1 >= 1<<w.width && w.width < maxWidth {
		w.width++
	}
}

func (w *codeWriter) bytes() []byte {
	if w.nBits > 0 {
		w.buf.WriteByte(byte(w.bits << (8 - w.nBits)))
	}
	return w.buf.Bytes()
}

func TestMissingClearAfterFullTable(t *testing.T) {
	w := newCodeWriter()
	w.put(0)
	for i := 1; w.next < tableSize; i++ {
		w.emit(i % clearCode)
	}
	for i := 0; i <= maxCodesPastFull; i++ {
		w.emit(i % clearCode)
	}

	_, err := io.ReadAll(NewReader(bytes.NewReader(w.bytes())))
	if err != errMissingClear {
		t.Fatalf("ReadAll error = %v, want %v", err, errMissingClear)
	}
}
