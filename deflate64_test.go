package deflate64

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"flag"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/iotest"
)

var generate = flag.Bool("generate", false, "regenerate testdata zip fixtures using a local 7z binary")

func randomBytes(seed byte, n int) []byte {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{seed}).Read(b)
	return b
}

var (
	textInput   = bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. Deflate64 says hi!\n"), 50)
	randomInput = randomBytes(1, 70_000)
	// Incompressible data on its own makes 7z fall back to the Store method,
	// so a compressible tail keeps the entry Deflate64.
	mixedInput = append(randomBytes(1, 70_000), textInput...)
	runInput   = bytes.Repeat([]byte{'a'}, 300_000)
	// A random block repeated at a distance between 32 KiB and 48 KiB
	// requires distance code 30; between 48 KiB and 64 KiB, code 31.
	dist30Input = bytes.Repeat(randomBytes(2, 40_000), 2)
	dist31Input = bytes.Repeat(randomBytes(3, 60_000), 2)
)

type fixture struct {
	zipName string
	files   map[string][]byte
}

var fixtures = []fixture{
	{"text.zip", map[string][]byte{"text.txt": textInput}},
	{"mixed.zip", map[string][]byte{"mixed.bin": mixedInput}},
	{"run.zip", map[string][]byte{"run.bin": runInput}},
	{"dist30.zip", map[string][]byte{"dist30.bin": dist30Input}},
	{"dist31.zip", map[string][]byte{"dist31.bin": dist31Input}},
	{"multi.zip", map[string][]byte{
		"empty.txt":  {},
		"text.txt":   textInput,
		"run.bin":    runInput,
		"dist30.bin": dist30Input,
	}},
}

// TestGenerateFixtures rebuilds testdata/*.zip with:
//
//	go test -run TestGenerateFixtures -generate
func TestGenerateFixtures(t *testing.T) {
	if !*generate {
		t.Skip("pass -generate to rebuild fixtures")
	}
	for _, fx := range fixtures {
		dir := t.TempDir()
		args := []string{"a", "-tzip", "-mm=Deflate64", "-mx=9", filepath.Join(dir, fx.zipName)}
		for name, data := range fx.files {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
			args = append(args, name)
		}
		cmd := exec.Command("7z", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("7z: %v\n%s", err, out)
		}
		zipData, err := os.ReadFile(filepath.Join(dir, fx.zipName))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", fx.zipName), zipData, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func openFixture(t *testing.T, name string) *zip.ReadCloser {
	t.Helper()
	zr, err := zip.OpenReader(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { zr.Close() })
	zr.RegisterDecompressor(ZipMethod, Decompressor)
	return zr
}

func rawStream(t *testing.T, f *zip.File) []byte {
	t.Helper()
	r, err := f.OpenRaw()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestZipFixtures(t *testing.T) {
	for _, fx := range fixtures {
		t.Run(fx.zipName, func(t *testing.T) {
			zr := openFixture(t, fx.zipName)
			if len(zr.File) != len(fx.files) {
				t.Fatalf("got %d files, want %d", len(zr.File), len(fx.files))
			}
			for _, f := range zr.File {
				want, ok := fx.files[f.Name]
				if !ok {
					t.Fatalf("unexpected file %q", f.Name)
				}
				if len(want) > 0 && f.Method != ZipMethod {
					t.Errorf("%s: method %d, want Deflate64 (%d)", f.Name, f.Method, ZipMethod)
				}
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				// archive/zip verifies the CRC-32 when the entry is read to EOF.
				got, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatalf("%s: %v", f.Name, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s: decompressed %d bytes, mismatch with expected %d bytes", f.Name, len(got), len(want))
				}
			}
		})
	}
}

func TestSmallReads(t *testing.T) {
	zr := openFixture(t, "multi.zip")
	wrappers := []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{"OneByteReader", iotest.OneByteReader},
		{"HalfReader", iotest.HalfReader},
		{"DataErrReader", iotest.DataErrReader},
	}
	for _, f := range zr.File {
		if f.Method != ZipMethod {
			continue
		}
		raw := rawStream(t, f)
		want := fixtures[len(fixtures)-1].files[f.Name]
		for _, w := range wrappers {
			t.Run(f.Name+"/"+w.name, func(t *testing.T) {
				d := NewReader(w.wrap(bytes.NewReader(raw)))
				got, err := io.ReadAll(w.wrap(d))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("output mismatch: got %d bytes, want %d", len(got), len(want))
				}
			})
		}
	}
}

func TestReset(t *testing.T) {
	zr := openFixture(t, "multi.zip")
	d := NewReader(bytes.NewReader(nil))
	for _, f := range zr.File {
		if f.Method != ZipMethod {
			continue
		}
		if err := d.(Resetter).Reset(bytes.NewReader(rawStream(t, f)), nil); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if !bytes.Equal(got, fixtures[len(fixtures)-1].files[f.Name]) {
			t.Errorf("%s: output mismatch after Reset", f.Name)
		}
	}
}

// bitWriter builds DEFLATE bitstreams by hand for crafted test cases.
type bitWriter struct {
	out   []byte
	bits  uint64
	nbits uint
}

func (w *bitWriter) writeBits(v uint64, n uint) {
	w.bits |= v << w.nbits
	w.nbits += n
	for w.nbits >= 8 {
		w.out = append(w.out, byte(w.bits))
		w.bits >>= 8
		w.nbits -= 8
	}
}

// writeCode writes a Huffman code, which DEFLATE packs MSB-first.
func (w *bitWriter) writeCode(code uint64, n uint) {
	var rev uint64
	for i := range n {
		rev |= (code >> i & 1) << (n - 1 - i)
	}
	w.writeBits(rev, n)
}

func (w *bitWriter) fixedLitLen(sym int) {
	switch {
	case sym < 144:
		w.writeCode(uint64(0x30+sym), 8)
	case sym < 256:
		w.writeCode(uint64(0x190+sym-144), 9)
	case sym < 280:
		w.writeCode(uint64(sym-256), 7)
	default:
		w.writeCode(uint64(0xc0+sym-280), 8)
	}
}

func (w *bitWriter) bytes() []byte {
	if w.nbits > 0 {
		w.writeBits(0, 8-w.nbits)
	}
	return w.out
}

type token struct {
	lit       byte
	lenCode   int // 0 means a literal; otherwise 257–285
	lenExtra  uint64
	distCode  int
	distExtra uint64
}

func lit(s string) []token {
	toks := make([]token, len(s))
	for i := range s {
		toks[i] = token{lit: s[i]}
	}
	return toks
}

func match(lenCode int, lenExtra uint64, distCode int, distExtra uint64) token {
	return token{lenCode: lenCode, lenExtra: lenExtra, distCode: distCode, distExtra: distExtra}
}

func (w *bitWriter) fixedBlock(final bool, toks []token) {
	w.writeBits(boolBit(final), 1)
	w.writeBits(1, 2)
	w.writeTokens(toks, w.fixedLitLen, func(sym int) { w.writeCode(uint64(sym), 5) })
}

func (w *bitWriter) writeTokens(toks []token, writeLitLen, writeDist func(sym int)) {
	for _, tok := range toks {
		if tok.lenCode == 0 {
			writeLitLen(int(tok.lit))
			continue
		}
		writeLitLen(tok.lenCode)
		w.writeBits(tok.lenExtra, testLengthCodes.extra[tok.lenCode-257])
		writeDist(tok.distCode)
		w.writeBits(tok.distExtra, testDistCodes.extra[tok.distCode])
	}
	writeLitLen(256)
}

func (w *bitWriter) storedBlock(final bool, data []byte) {
	w.writeBits(boolBit(final), 1)
	w.writeBits(0, 2)
	w.bytes()
	n := uint16(len(data))
	w.out = append(w.out, byte(n), byte(n>>8), byte(^n), byte(^n>>8))
	w.out = append(w.out, data...)
}

func boolBit(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func concat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func TestCraftedStreams(t *testing.T) {
	history := randomBytes(4, 60_000)
	tests := []struct {
		name  string
		build func(w *bitWriter)
		want  []byte
	}{
		{
			name:  "empty fixed block",
			build: func(w *bitWriter) { w.fixedBlock(true, nil) },
			want:  []byte{},
		},
		{
			name:  "fixed literals",
			build: func(w *bitWriter) { w.fixedBlock(true, lit("hello")) },
			want:  []byte("hello"),
		},
		{
			name:  "stored block",
			build: func(w *bitWriter) { w.storedBlock(true, []byte("stored")) },
			want:  []byte("stored"),
		},
		{
			name: "stored then fixed",
			build: func(w *bitWriter) {
				w.storedBlock(false, []byte("abc"))
				w.fixedBlock(true, []token{match(257, 0, 1, 0)})
			},
			want: []byte("abcbcb"),
		},
		{
			name: "short overlapping match",
			build: func(w *bitWriter) {
				w.fixedBlock(true, append(lit("ab"), match(265, 1, 1, 0)))
			},
			want: bytes.Repeat([]byte("ab"), 7),
		},
		{
			name: "length code 284 max is 257",
			build: func(w *bitWriter) {
				w.fixedBlock(true, append(lit("z"), match(284, 30, 0, 0)))
			},
			want: bytes.Repeat([]byte("z"), 1+257),
		},
		{
			name: "length code 285 with zero extra is 3 not 258",
			build: func(w *bitWriter) {
				w.fixedBlock(true, append(lit("q"), match(285, 0, 0, 0)))
			},
			want: []byte("qqqq"),
		},
		{
			name: "length code 285 with 16 extra bits",
			build: func(w *bitWriter) {
				w.fixedBlock(true, append(lit("x"), match(285, 1000, 0, 0)))
			},
			want: bytes.Repeat([]byte("x"), 1+1003),
		},
		{
			name: "max length 65538 wraps window",
			build: func(w *bitWriter) {
				w.fixedBlock(true, append(lit("y"), match(285, 65535, 0, 0), match(285, 65535, 0, 0)))
			},
			want: bytes.Repeat([]byte("y"), 1+2*65538),
		},
		{
			name: "distance code 30",
			build: func(w *bitWriter) {
				w.storedBlock(false, history)
				// distance 32769 + 7231 = 40000
				w.fixedBlock(true, []token{match(284, 0, 30, 7231)})
			},
			want: concat(history, history[60_000-40_000:][:227]),
		},
		{
			name: "distance code 31 max distance",
			build: func(w *bitWriter) {
				w.storedBlock(false, history)
				w.storedBlock(false, history[:5536])
				// distance 49153 + 16383 = 65536, the full window
				w.fixedBlock(true, []token{match(285, 97, 31, 16383)})
			},
			want: concat(history, history[:5536], history[:100]),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w bitWriter
			tt.build(&w)
			got, err := io.ReadAll(NewReader(bytes.NewReader(w.bytes())))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("got %d bytes, want %d bytes (prefix %q vs %q)", len(got), len(tt.want), head(got), head(tt.want))
			}
		})
	}
}

func head(b []byte) []byte {
	return b[:min(len(b), 16)]
}

func TestDict(t *testing.T) {
	var w bitWriter
	w.fixedBlock(true, []token{match(257, 0, 4, 1)})
	got, err := io.ReadAll(NewReaderDict(bytes.NewReader(w.bytes()), []byte("0123456789")))
	if err != nil {
		t.Fatal(err)
	}
	if want := "456"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Plain DEFLATE streams are valid Deflate64 as long as they never use length
// code 285, which compress/flate only emits for matches of length 258.
func TestCompatibleWithFlate(t *testing.T) {
	var noLongMatches []byte
	phrases := [][]byte{
		bytes.Repeat([]byte("alpha "), 20),
		bytes.Repeat([]byte("beta "), 25),
		[]byte("gamma delta epsilon zeta eta theta iota kappa lambda mu"),
	}
	for i := range 2000 {
		noLongMatches = append(noLongMatches, randomBytes(byte(i), 4)...)
		noLongMatches = append(noLongMatches, phrases[i%len(phrases)]...)
	}

	tests := []struct {
		name  string
		level int
		input []byte
	}{
		{"NoCompression", flate.NoCompression, randomInput},
		{"HuffmanOnly", flate.HuffmanOnly, textInput},
		{"BestSpeed", flate.BestSpeed, noLongMatches},
		{"DefaultCompression", flate.DefaultCompression, noLongMatches},
		{"BestCompression", flate.BestCompression, noLongMatches},
		{"BestCompression random", flate.BestCompression, randomInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			fw, err := flate.NewWriter(&buf, tt.level)
			if err != nil {
				t.Fatal(err)
			}
			fw.Write(tt.input)
			fw.Close()
			got, err := io.ReadAll(NewReader(&buf))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.input) {
				t.Errorf("output mismatch: got %d bytes, want %d", len(got), len(tt.input))
			}
		})
	}
}

func TestCorrupt(t *testing.T) {
	var corruptErr flate.CorruptInputError
	tests := []struct {
		name    string
		build   func(w *bitWriter)
		wantErr func(error) bool
	}{
		{
			name:    "empty input",
			build:   func(w *bitWriter) {},
			wantErr: isUnexpectedEOF,
		},
		{
			name:    "reserved block type",
			build:   func(w *bitWriter) { w.writeBits(1, 1); w.writeBits(3, 2) },
			wantErr: func(err error) bool { return errors.As(err, &corruptErr) },
		},
		{
			name: "stored length mismatch",
			build: func(w *bitWriter) {
				w.writeBits(1, 3)
				w.bytes()
				w.out = append(w.out, 5, 0, 0, 0)
			},
			wantErr: func(err error) bool { return errors.As(err, &corruptErr) },
		},
		{
			name: "truncated stored block",
			build: func(w *bitWriter) {
				w.storedBlock(true, []byte("hello"))
				w.out = w.out[:len(w.out)-2]
			},
			wantErr: isUnexpectedEOF,
		},
		{
			name:    "distance beyond history",
			build:   func(w *bitWriter) { w.fixedBlock(true, append(lit("a"), match(257, 0, 1, 0))) },
			wantErr: func(err error) bool { return errors.As(err, &corruptErr) },
		},
		{
			name: "invalid literal/length symbol 286",
			build: func(w *bitWriter) {
				w.writeBits(1, 1)
				w.writeBits(1, 2)
				w.fixedLitLen(286)
			},
			wantErr: func(err error) bool { return errors.As(err, &corruptErr) },
		},
		{
			name: "too many literal/length codes",
			build: func(w *bitWriter) {
				w.writeBits(1, 1)
				w.writeBits(2, 2)
				w.writeBits(30, 5)
				w.writeBits(0, 16)
			},
			wantErr: func(err error) bool { return errors.As(err, &corruptErr) },
		},
		{
			name: "oversubscribed code length code",
			build: func(w *bitWriter) {
				w.writeBits(1, 1)
				w.writeBits(2, 2)
				w.writeBits(0, 5)
				w.writeBits(0, 5)
				w.writeBits(0, 4)
				for range 4 {
					w.writeBits(1, 3)
				}
				w.writeBits(0, 16)
			},
			wantErr: func(err error) bool { return errors.As(err, &corruptErr) },
		},
		{
			name: "missing end of stream",
			build: func(w *bitWriter) {
				w.fixedBlock(false, lit("more?"))
			},
			wantErr: isUnexpectedEOF,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w bitWriter
			tt.build(&w)
			r := NewReader(bytes.NewReader(w.bytes()))
			_, err := io.ReadAll(r)
			if !tt.wantErr(err) {
				t.Errorf("unexpected error: %v", err)
			}
			if closeErr := r.Close(); closeErr != err {
				t.Errorf("Close() = %v, want %v", closeErr, err)
			}
		})
	}
}

func isUnexpectedEOF(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF)
}

func FuzzReader(f *testing.F) {
	for _, name := range []string{"text.zip", "run.zip", "dist30.zip"} {
		zr, err := zip.OpenReader(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		for _, file := range zr.File {
			r, _ := file.OpenRaw()
			raw, _ := io.ReadAll(r)
			f.Add(raw)
		}
		zr.Close()
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		io.Copy(io.Discard, io.LimitReader(NewReader(bytes.NewReader(data)), 1<<24))
	})
}
