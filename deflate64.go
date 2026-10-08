// Package deflate64 implements a decompressor for Deflate64 ("Enhanced
// Deflate"), the PKWARE extension of the DEFLATE format used by zip method 9.
//
// Deflate64 is identical to DEFLATE (RFC 1951) except that:
//   - the sliding window is 64 KiB instead of 32 KiB,
//   - length code 285 carries 16 extra bits (lengths 3–65538) instead of
//     meaning the fixed length 258,
//   - distance codes 30 and 31 are valid, reaching back up to 65536 bytes.
//
// The API mirrors [compress/flate]'s reader, and [Decompressor] can be
// registered with [archive/zip] to read Deflate64 entries.
package deflate64

import (
	"archive/zip"
	"io"
)

// ZipMethod is the zip compression method ID assigned to Deflate64.
const ZipMethod uint16 = 9

// Resetter resets a ReadCloser returned by [NewReader] or [NewReaderDict]
// to switch to a new underlying Reader, avoiding a fresh allocation.
// It has the same shape as [compress/flate.Resetter].
type Resetter interface {
	Reset(r io.Reader, dict []byte) error
}

// NewReader returns a new ReadCloser that decompresses the Deflate64 stream
// read from r. If r does not also implement [io.ByteReader], the
// decompressor may read more data than necessary from r.
//
// Malformed input yields a [compress/flate.CorruptInputError], and input that
// ends early yields [io.ErrUnexpectedEOF].
//
// The ReadCloser returned by NewReader also implements [Resetter].
func NewReader(r io.Reader) io.ReadCloser {
	return NewReaderDict(r, nil)
}

// NewReaderDict is like [NewReader] but initializes the reader with a preset
// dictionary, of which only the last 64 KiB is used.
func NewReaderDict(r io.Reader, dict []byte) io.ReadCloser {
	d := new(decompressor)
	d.Reset(r, dict)
	return d
}

// Decompressor is a [zip.Decompressor] for Deflate64 entries. Register it on a
// single archive with
//
//	zr.RegisterDecompressor(deflate64.ZipMethod, deflate64.Decompressor)
//
// or globally with [RegisterZip].
func Decompressor(r io.Reader) io.ReadCloser {
	return NewReader(r)
}

// RegisterZip registers [Decompressor] with [archive/zip] for all archives.
// Like [zip.RegisterDecompressor], it panics if called more than once.
func RegisterZip() {
	zip.RegisterDecompressor(ZipMethod, Decompressor)
}
