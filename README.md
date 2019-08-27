 # Note

This package is an improved version of [golang.org/x/image/tiff](https://github.com/golang/image/tree/master/tiff) featuring:

* Support for decoding of CCITT Group3/4 compressed images
* Support for LZW compressed images (using an extended version of `compress/lzw` at: [github.com/hhrutter/lzw](https://github.com/hhrutter/lzw)
* Support for CMYK


## Background

As stated in this [golang proposal](https://github.com/golang/go/issues/25409) Go lzw implementations are spread out over the standard library at [compress/lzw](https://github.com/golang/go/tree/master/src/compress/lzw) and [golang.org/x/image/tiff/lzw](https://github.com/golang/image/tree/master/tiff/lzw). As of go1.12 `compress/lzw` works reliably for GIF only. This is also the reason the TIFF package at [golang.org/x/image/tiff](https://github.com/golang/image/tree/master/tiff) provides its own lzw implementation for compression. In addition with PDF there is a third variant of lzw needed.

[pdfcpu](https://github.com/pdfcpu/pdfcpu) supports lzw compression for PDF files and uses an extended version of lzw at [github.com/hhrutter/lzw](https://github.com/hhrutter/lzw) which works for GIF, PDF and TIFF. It not only supports the PDF LZWFilter but also processing PDFs with embedded TIFF images. Therefore it also uses this variant of [golang.org/x/image/tiff](https://github.com/golang/image/tree/master/tiff) leveraging an extended lzw implementation([github.com/hhrutter/lzw](https://github.com/hhrutter/lzw)).

## Goal

An improved version of `x/image/tiff` with full support for CCITT and LZW compression and the CMYK color model.
