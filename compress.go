// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bufio"
	"errors"
	"io"
)

type byteReader interface {
	io.Reader
	io.ByteReader
}

// unpackBits decodes the PackBits-compressed data in src and returns the
// uncompressed data.
//
// The PackBits compression format is described in section 9 (p. 42)
// of the TIFF spec.
func unpackBits(r io.Reader, lim int64) ([]byte, error) {
	if lim < 0 {
		return nil, errors.New("tiff: invalid PackBits data limit")
	}
	buf := make([]byte, 128)
	capacity := min(lim, int64(1024))
	dst := make([]byte, 0, int(capacity))
	br, ok := r.(byteReader)
	if !ok {
		br = bufio.NewReader(r)
	}

	for {
		if int64(len(dst)) == lim {
			return dst, nil
		}
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				return dst, nil
			}
			return nil, err
		}
		code := int(int8(b))
		switch {
		case code >= 0:
			n, err := io.ReadFull(br, buf[:code+1])
			if err != nil {
				return nil, err
			}
			remaining := int(lim - int64(len(dst)))
			if n > remaining {
				dst = append(dst, buf[:remaining]...)
				return dst, nil
			}
			dst = append(dst, buf[:n]...)
		case code == -128:
			// No-op.
		default:
			if b, err = br.ReadByte(); err != nil {
				return nil, err
			}
			n := 1 - code
			remaining := int(lim - int64(len(dst)))
			if n > remaining {
				n = remaining
			}
			for j := 0; j < n; j++ {
				buf[j] = b
			}
			dst = append(dst, buf[:n]...)
		}
	}
}
