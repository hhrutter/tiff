/*
Copyright 2026 The tiff Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tiff

import (
	"errors"
	"io"
	"math/bits"

	"golang.org/x/image/ccitt"
)

var (
	errCCITTRLEInvalidCode = errors.New("tiff: ccitt rle: invalid code")
	errCCITTRLEOverflow    = errors.New("tiff: ccitt rle: run length overflows width")
)

type mhCode struct {
	run  int
	bits string
}

type mhNode struct {
	next [2]int
	run  int
}

type mhBitReader struct {
	data  []byte
	order ccitt.Order
	pos   int
	nbits uint
	b     byte
}

func (r *mhBitReader) align() {
	r.nbits = 0
}

func (r *mhBitReader) nextBit() (uint, error) {
	if r.nbits == 0 {
		if r.pos >= len(r.data) {
			return 0, io.ErrUnexpectedEOF
		}
		r.b = r.data[r.pos]
		r.pos++
		if r.order == ccitt.LSB {
			r.b = bits.Reverse8(r.b)
		}
		r.nbits = 8
	}
	r.nbits--
	return uint(r.b>>r.nbits) & 1, nil
}

func buildMHTree(codes []mhCode) []mhNode {
	tree := []mhNode{{run: -1}}
	for _, c := range codes {
		node := 0
		for _, b := range c.bits {
			i := int(b - '0')
			if tree[node].next[i] == 0 {
				tree = append(tree, mhNode{run: -1})
				tree[node].next[i] = len(tree) - 1
			}
			node = tree[node].next[i]
		}
		tree[node].run = c.run
	}
	return tree
}

func decodeMHCode(r *mhBitReader, tree []mhNode) (int, error) {
	node := 0
	for {
		b, err := r.nextBit()
		if err != nil {
			return 0, err
		}
		node = tree[node].next[b]
		if node == 0 {
			return 0, errCCITTRLEInvalidCode
		}
		if run := tree[node].run; run >= 0 {
			return run, nil
		}
	}
}

func decodeMHRun(r *mhBitReader, tree []mhNode) (int, error) {
	total := 0
	for {
		n, err := decodeMHCode(r, tree)
		if err != nil {
			return 0, err
		}
		total += n
		if n <= 0x3f {
			return total, nil
		}
	}
}

func setMHRun(row []byte, start, n int, bit byte) {
	if bit == 0 {
		return
	}
	for x := start; x < start+n; x++ {
		row[x/8] |= 0x80 >> uint(x&7)
	}
}

func decodeCCITTRLE(r io.Reader, order ccitt.Order, width, height int, whiteIsZero bool, lim int64) ([]byte, error) {
	data, err := readBuf(r, nil, lim)
	if err != nil {
		return nil, err
	}
	br := mhBitReader{data: data, order: order}
	stride := (width + 7) / 8
	dstLen, ok := checkedMul3(stride, height, 1)
	if !ok || dstLen > lim {
		return nil, FormatError("block data size too large")
	}
	dst := make([]byte, dstLen)
	whiteBit, blackBit := byte(0), byte(1)
	if !whiteIsZero {
		whiteBit, blackBit = 1, 0
	}
	for y := 0; y < height; y++ {
		if err := decodeCCITTRLERow(&br, dst[y*stride:(y+1)*stride], width, whiteBit, blackBit); err != nil {
			return nil, err
		}
		br.align()
	}
	return dst, nil
}

func decodeCCITTRLERow(r *mhBitReader, row []byte, width int, whiteBit, blackBit byte) error {
	x := 0
	white := true
	for x < width {
		tree := mhBlackTree
		bit := blackBit
		if white {
			tree = mhWhiteTree
			bit = whiteBit
		}
		n, err := decodeMHRun(r, tree)
		if err != nil {
			return err
		}
		if n > width-x {
			return errCCITTRLEOverflow
		}
		setMHRun(row, x, n, bit)
		x += n
		white = !white
	}
	return nil
}

var mhWhiteTree = buildMHTree(mhWhiteCodes)
var mhBlackTree = buildMHTree(mhBlackCodes)

var mhWhiteCodes = []mhCode{
	{0x0000, "00110101"}, {0x0001, "000111"}, {0x0002, "0111"}, {0x0003, "1000"},
	{0x0004, "1011"}, {0x0005, "1100"}, {0x0006, "1110"}, {0x0007, "1111"},
	{0x0008, "10011"}, {0x0009, "10100"}, {0x000A, "00111"}, {0x000B, "01000"},
	{0x000C, "001000"}, {0x000D, "000011"}, {0x000E, "110100"}, {0x000F, "110101"},
	{0x0010, "101010"}, {0x0011, "101011"}, {0x0012, "0100111"}, {0x0013, "0001100"},
	{0x0014, "0001000"}, {0x0015, "0010111"}, {0x0016, "0000011"}, {0x0017, "0000100"},
	{0x0018, "0101000"}, {0x0019, "0101011"}, {0x001A, "0010011"}, {0x001B, "0100100"},
	{0x001C, "0011000"}, {0x001D, "00000010"}, {0x001E, "00000011"}, {0x001F, "00011010"},
	{0x0020, "00011011"}, {0x0021, "00010010"}, {0x0022, "00010011"}, {0x0023, "00010100"},
	{0x0024, "00010101"}, {0x0025, "00010110"}, {0x0026, "00010111"}, {0x0027, "00101000"},
	{0x0028, "00101001"}, {0x0029, "00101010"}, {0x002A, "00101011"}, {0x002B, "00101100"},
	{0x002C, "00101101"}, {0x002D, "00000100"}, {0x002E, "00000101"}, {0x002F, "00001010"},
	{0x0030, "00001011"}, {0x0031, "01010010"}, {0x0032, "01010011"}, {0x0033, "01010100"},
	{0x0034, "01010101"}, {0x0035, "00100100"}, {0x0036, "00100101"}, {0x0037, "01011000"},
	{0x0038, "01011001"}, {0x0039, "01011010"}, {0x003A, "01011011"}, {0x003B, "01001010"},
	{0x003C, "01001011"}, {0x003D, "00110010"}, {0x003E, "00110011"}, {0x003F, "00110100"},
	{0x0040, "11011"}, {0x0080, "10010"}, {0x00C0, "010111"}, {0x0100, "0110111"},
	{0x0140, "00110110"}, {0x0180, "00110111"}, {0x01C0, "01100100"}, {0x0200, "01100101"},
	{0x0240, "01101000"}, {0x0280, "01100111"}, {0x02C0, "011001100"}, {0x0300, "011001101"},
	{0x0340, "011010010"}, {0x0380, "011010011"}, {0x03C0, "011010100"}, {0x0400, "011010101"},
	{0x0440, "011010110"}, {0x0480, "011010111"}, {0x04C0, "011011000"}, {0x0500, "011011001"},
	{0x0540, "011011010"}, {0x0580, "011011011"}, {0x05C0, "010011000"}, {0x0600, "010011001"},
	{0x0640, "010011010"}, {0x0680, "011000"}, {0x06C0, "010011011"}, {0x0700, "00000001000"},
	{0x0740, "00000001100"}, {0x0780, "00000001101"}, {0x07C0, "000000010010"}, {0x0800, "000000010011"},
	{0x0840, "000000010100"}, {0x0880, "000000010101"}, {0x08C0, "000000010110"}, {0x0900, "000000010111"},
	{0x0940, "000000011100"}, {0x0980, "000000011101"}, {0x09C0, "000000011110"}, {0x0A00, "000000011111"},
}

var mhBlackCodes = []mhCode{
	{0x0000, "0000110111"}, {0x0001, "010"}, {0x0002, "11"}, {0x0003, "10"},
	{0x0004, "011"}, {0x0005, "0011"}, {0x0006, "0010"}, {0x0007, "00011"},
	{0x0008, "000101"}, {0x0009, "000100"}, {0x000A, "0000100"}, {0x000B, "0000101"},
	{0x000C, "0000111"}, {0x000D, "00000100"}, {0x000E, "00000111"}, {0x000F, "000011000"},
	{0x0010, "0000010111"}, {0x0011, "0000011000"}, {0x0012, "0000001000"}, {0x0013, "00001100111"},
	{0x0014, "00001101000"}, {0x0015, "00001101100"}, {0x0016, "00000110111"}, {0x0017, "00000101000"},
	{0x0018, "00000010111"}, {0x0019, "00000011000"}, {0x001A, "000011001010"}, {0x001B, "000011001011"},
	{0x001C, "000011001100"}, {0x001D, "000011001101"}, {0x001E, "000001101000"}, {0x001F, "000001101001"},
	{0x0020, "000001101010"}, {0x0021, "000001101011"}, {0x0022, "000011010010"}, {0x0023, "000011010011"},
	{0x0024, "000011010100"}, {0x0025, "000011010101"}, {0x0026, "000011010110"}, {0x0027, "000011010111"},
	{0x0028, "000001101100"}, {0x0029, "000001101101"}, {0x002A, "000011011010"}, {0x002B, "000011011011"},
	{0x002C, "000001010100"}, {0x002D, "000001010101"}, {0x002E, "000001010110"}, {0x002F, "000001010111"},
	{0x0030, "000001100100"}, {0x0031, "000001100101"}, {0x0032, "000001010010"}, {0x0033, "000001010011"},
	{0x0034, "000000100100"}, {0x0035, "000000110111"}, {0x0036, "000000111000"}, {0x0037, "000000100111"},
	{0x0038, "000000101000"}, {0x0039, "000001011000"}, {0x003A, "000001011001"}, {0x003B, "000000101011"},
	{0x003C, "000000101100"}, {0x003D, "000001011010"}, {0x003E, "000001100110"}, {0x003F, "000001100111"},
	{0x0040, "0000001111"}, {0x0080, "000011001000"}, {0x00C0, "000011001001"}, {0x0100, "000001011011"},
	{0x0140, "000000110011"}, {0x0180, "000000110100"}, {0x01C0, "000000110101"}, {0x0200, "0000001101100"},
	{0x0240, "0000001101101"}, {0x0280, "0000001001010"}, {0x02C0, "0000001001011"}, {0x0300, "0000001001100"},
	{0x0340, "0000001001101"}, {0x0380, "0000001110010"}, {0x03C0, "0000001110011"}, {0x0400, "0000001110100"},
	{0x0440, "0000001110101"}, {0x0480, "0000001110110"}, {0x04C0, "0000001110111"}, {0x0500, "0000001010010"},
	{0x0540, "0000001010011"}, {0x0580, "0000001010100"}, {0x05C0, "0000001010101"}, {0x0600, "0000001011010"},
	{0x0640, "0000001011011"}, {0x0680, "0000001100100"}, {0x06C0, "0000001100101"}, {0x0700, "00000001000"},
	{0x0740, "00000001100"}, {0x0780, "00000001101"}, {0x07C0, "000000010010"}, {0x0800, "000000010011"},
	{0x0840, "000000010100"}, {0x0880, "000000010101"}, {0x08C0, "000000010110"}, {0x0900, "000000010111"},
	{0x0940, "000000011100"}, {0x0980, "000000011101"}, {0x09C0, "000000011110"}, {0x0A00, "000000011111"},
}
