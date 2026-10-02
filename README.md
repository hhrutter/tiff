# tiff

This package extends [golang.org/x/image/tiff](https://github.com/golang/image/tree/master/tiff) with CCITT, LZW,
JPEG, CMYK, and multi-page support. It originated in [pdfcpu](https://github.com/pdfcpu/pdfcpu).

## Compression support

| Compression | Decode | Encode |
| --- | --- | --- |
| Uncompressed | Yes | Yes |
| PackBits | Yes | No |
| Deflate, including the legacy tag when decoding | Yes | Yes |
| LZW | Yes | Yes |
| CCITT RLE (Modified Huffman) | Yes | No |
| CCITT Group 3 (1-D) and Group 4 | Yes | No |
| JPEG (compression 6 and 7) | Yes | No |

CCITT decoding uses [x/image/ccitt](https://github.com/golang/image/tree/master/ccitt). Group 3 byte alignment is
supported; Group 3 2-D and CCITT uncompressed modes are not. LZW decoding uses
[x/image/tiff/lzw](https://github.com/golang/image/tree/master/tiff/lzw), with encoding provided locally.

## Images and pages

The package reads and writes 8-bit CMYK. It decodes unspecified four-component RGB as associated-alpha RGBA and
handles padded edge tiles for packed and 16-bit samples.

`Encode` writes one image and `EncodeAll` writes multiple pages. `Decode` uses the first IFD; `DecodeAt` uses an IFD
byte offset. Configuration variants follow the same rule. LZW and Deflate support horizontal prediction.

## Decoder checks

Checked IFD counts, offsets, and block sizes bound parsing and decompression. JPEG frame dimensions must match their
strip or tile. These checks do not impose an absolute image-size cap.

## YCbCr JPEG decoding

YCbCr JPEG uses three interleaved 8-bit components and returns `image.RGBA`. Strips, tiles, complete streams, shared
`JPEGTables`, and sequential Huffman frames with one interleaved scan are supported.

Subsampling `[1,1]`, `[2,1]`, and `[2,2]` is supported with centered chroma where applicable. Standard defaults apply
to optional YCbCr fields. Missing `ReferenceBlackWhite` defaults to `[0,255,128,255,128,255]`; explicit values must
match. Other compression, component layouts, color parameters, and JPEG processes return an error.
