# tiff

This package extends [golang.org/x/image/tiff](https://github.com/golang/image/tree/master/tiff) with additional
compression, color, and multi-page support. It grew out of [pdfcpu](https://github.com/pdfcpu/pdfcpu)'s need to process
TIFF images, including monochrome CCITT and CMYK images.

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

Group 3 and Group 4 decoding uses [x/image/ccitt](https://github.com/golang/image/tree/master/ccitt). Group 3
byte-aligned data is supported. Group 3 2-D and uncompressed modes, and the Group 4 uncompressed mode, return an
error. LZW decoding uses [x/image/tiff/lzw](https://github.com/golang/image/tree/master/tiff/lzw); encoding uses
this package's TIFF LZW writer. The former `hhrutter/lzw` dependency is no longer needed.

## Images and pages

The package reads and writes 8-bit CMYK images. It also decodes four-component RGB images whose `ExtraSamples` tag is
absent or unspecified as associated-alpha RGBA. Padded edge tiles are decoded with their full tile dimensions while only
visible pixels are copied into the image, including packed and 16-bit samples.

`Encode` writes one image; `EncodeAll` writes multiple pages with linked Image File Directories (IFDs). `Decode` and
`DecodeConfig` use the first IFD. `DecodeAt` and `DecodeConfigAt` read an IFD at a supplied byte offset; the offset is
not a page number. The writer supports horizontal prediction with LZW and Deflate.

## Decoder checks

The decoder caps parsed IFD value counts and uses checked block sizes to bound decompressed buffers. For an input that
only implements `io.Reader`, the buffered `io.ReaderAt` adapter grows as bytes arrive and returns errors for invalid
offsets and short reads.
Before a JPEG strip or tile is fully decoded, its frame dimensions are checked against the expected TIFF
block, including short final strips, padded edge tiles, and streams prepared with APP14 or `JPEGTables`.
These checks do not impose an absolute image-size cap; the destination image is allocated from the TIFF dimensions.
IFD parsing, pixel conversion, and compression dispatch are kept in focused helpers.

## YCbCr JPEG decoding

YCbCr images are supported with JPEG compression (7), three 8-bit components, and interleaved component storage.
The decoder returns an `image.RGBA`; configuration reads report `color.RGBAModel`.
It reads single or multiple strips and tiles, with complete JPEG streams or shared `JPEGTables`.
Supported JPEG frames are 8-bit sequential Huffman frames with one interleaved scan.

Supported subsampling factors are `[1,1]`, `[2,1]`, and `[2,2]`. Chroma positioning must be centered, except that
positioning is immaterial without subsampling. The color coefficients must equal `[299/1000,587/1000,114/1000]`.
The standard defaults apply when these fields or `PlanarConfiguration` are absent.
`ReferenceBlackWhite` must be present and equal `[0,255,128,255,128,255]`.

Other compression methods for YCbCr, separate component planes, extra components, other color conversion parameters,
and other JPEG processes return an error. Applications do not need to import `image/jpeg` separately.
