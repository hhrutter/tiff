# Padded tile fixtures

The 13 `tiled-*.tiff.bz2` files are copied unchanged from
`golang.org/x/image/tiff/testdata` at v0.46.0 under the upstream BSD license
in this repository's `LICENSE`. The adapted fixture test retains the original
2011 Go Authors notice from upstream `reader_test.go`.

Each file contains a little-endian, uncompressed 15x15 image with a black
diagonal on a white background, stored in one 16x16 tile. The bzip2 wrapper
compresses the fixture file, not the TIFF pixel data.

Tests also crop ImageWidth to 7 without changing the stored tile. This checks
whole-byte padding in the 1-bit gray and palette fixtures.
