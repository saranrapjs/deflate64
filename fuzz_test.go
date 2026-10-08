package deflate64

import (
	"bytes"
	"io"
	"testing"
)

type codeTable struct {
	base  []int
	extra []uint
}

// The test encoder derives its code tables from the grouping rule in
// RFC 1951 §3.2.5 (plus the Deflate64 changes) instead of sharing the
// decoder's tables, so a typo in either one shows up as a mismatch.
var (
	testLengthCodes = deriveCodes(28, 8, 4, 3, 16)
	testDistCodes   = deriveCodes(32, 4, 2, 1, 0)
)

// deriveCodes builds n codes where the first `direct` codes have no extra
// bits and each following group of groupSize codes has one more. Length code
// 285 doesn't follow the rule, so it is appended when length285Extra > 0.
func deriveCodes(n, direct, groupSize, firstBase int, length285Extra uint) codeTable {
	var t codeTable
	base := firstBase
	for i := range n {
		var extra uint
		if i >= direct {
			extra = uint((i-direct)/groupSize + 1)
		}
		t.base = append(t.base, base)
		t.extra = append(t.extra, extra)
		base += 1 << extra
	}
	if length285Extra > 0 {
		t.base = append(t.base, 3)
		t.extra = append(t.extra, length285Extra)
	}
	return t
}

func (t codeTable) encode(v, numCodes int) (code int, extra uint64) {
	for i := numCodes - 1; i >= 0; i-- {
		if t.base[i] <= v {
			return i, uint64(v - t.base[i])
		}
	}
	panic("value below smallest base")
}

func encodeMatch(length, dist int, useCode285 bool) token {
	tok := token{lenCode: 285, lenExtra: uint64(length - 3)}
	if !useCode285 && length <= 257 {
		code, extra := testLengthCodes.encode(length, 28)
		tok.lenCode, tok.lenExtra = 257+code, extra
	}
	tok.distCode, tok.distExtra = testDistCodes.encode(dist, 32)
	return tok
}

type programReader struct {
	data []byte
}

func (p *programReader) done() bool {
	return len(p.data) == 0
}

func (p *programReader) byte() byte {
	if len(p.data) == 0 {
		return 0
	}
	b := p.data[0]
	p.data = p.data[1:]
	return b
}

func (p *programReader) uint16() int {
	return int(p.byte()) | int(p.byte())<<8
}

func (p *programReader) bytes(n int) []byte {
	n = min(n, len(p.data))
	b := p.data[:n]
	p.data = p.data[n:]
	return b
}

const maxFuzzOutput = 1 << 20

// buildStream interprets program as a sequence of operations, returning the
// Deflate64 stream that encodes them and the output it must decode to.
func buildStream(program []byte) (stream, want []byte) {
	p := programReader{program}
	var w bitWriter
	var toks []token
	dynamic := false
	var dynamicShape []byte
	endBlock := func(final bool) {
		if dynamic {
			w.dynamicBlock(final, toks, dynamicShape)
		} else {
			w.fixedBlock(final, toks)
		}
		toks = nil
	}

	for !p.done() && len(want) < maxFuzzOutput {
		switch p.byte() % 5 {
		case 0:
			for range p.byte()%16 + 1 {
				c := p.byte()
				toks = append(toks, token{lit: c})
				want = append(want, c)
			}
		case 1:
			length := 3 + p.uint16()
			dist := 1 + p.uint16()
			useCode285 := p.byte()&1 == 1
			length = min(length, maxFuzzOutput-len(want))
			if len(want) == 0 || length < 3 {
				continue
			}
			dist = 1 + (dist-1)%min(len(want), 1<<16)
			toks = append(toks, encodeMatch(length, dist, useCode285))
			for range length {
				want = append(want, want[len(want)-dist])
			}
		case 2:
			endBlock(false)
			data := p.bytes(p.uint16() % 1024)
			w.storedBlock(false, data)
			want = append(want, data...)
		case 3:
			endBlock(false)
			dynamic = false
		case 4:
			endBlock(false)
			dynamic = true
			dynamicShape = p.bytes(int(p.byte()))
		}
	}
	endBlock(true)
	return w.bytes(), want
}

var testCodeLengthOrder = []int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// dynamicBlock writes toks using Huffman codes whose lengths are steered by
// shape bytes, which also choose extra unused symbols and whether code
// lengths are run-length encoded.
func (w *bitWriter) dynamicBlock(final bool, toks []token, shape []byte) {
	s := programReader{shape}

	litUsed := make([]bool, 286)
	distUsed := make([]bool, 32)
	litUsed[256] = true
	numDistUsed := 0
	for _, tok := range toks {
		if tok.lenCode == 0 {
			litUsed[tok.lit] = true
			continue
		}
		litUsed[tok.lenCode] = true
		if !distUsed[tok.distCode] {
			distUsed[tok.distCode] = true
			numDistUsed++
		}
	}

	litLens := completeCodeLengths(litUsed, 15, &s)
	var distLens []uint8
	switch {
	case numDistUsed == 0 && s.byte()&1 == 0:
		// RFC 1951 §3.2.7: a single zero-length distance code means
		// the block has no matches.
		distLens = []uint8{0}
	case numDistUsed == 1 && s.byte()&1 == 0:
		// A lone distance code is the one incomplete code RFC 1951 allows.
		distLens = make([]uint8, 32)
		for sym, used := range distUsed {
			if used {
				distLens[sym] = 1
			}
		}
	default:
		distLens = completeCodeLengths(distUsed, 15, &s)
	}
	litLens = trimZeros(litLens, 257)
	distLens = trimZeros(distLens, 1)

	type clSymbol struct {
		sym   int
		extra uint64
		nbits uint
	}
	var clSyms []clSymbol
	allLens := append(append([]uint8{}, litLens...), distLens...)
	useRuns := s.byte()&1 == 0
	for i := 0; i < len(allLens); {
		l := allLens[i]
		run := 1
		for i+run < len(allLens) && allLens[i+run] == l {
			run++
		}
		switch {
		case useRuns && l == 0 && run >= 11:
			n := min(run, 138)
			clSyms = append(clSyms, clSymbol{18, uint64(n - 11), 7})
			i += n
		case useRuns && l == 0 && run >= 3:
			clSyms = append(clSyms, clSymbol{17, uint64(run - 3), 3})
			i += run
		case useRuns && i > 0 && allLens[i-1] == l && run >= 3:
			n := min(run, 6)
			clSyms = append(clSyms, clSymbol{16, uint64(n - 3), 2})
			i += n
		default:
			clSyms = append(clSyms, clSymbol{int(l), 0, 0})
			i++
		}
	}

	clUsed := make([]bool, 19)
	for _, cs := range clSyms {
		clUsed[cs.sym] = true
	}
	clLens := completeCodeLengths(clUsed, 7, &s)
	numCL := 4
	for i, sym := range testCodeLengthOrder {
		if clLens[sym] != 0 {
			numCL = max(numCL, i+1)
		}
	}

	w.writeBits(boolBit(final), 1)
	w.writeBits(2, 2)
	w.writeBits(uint64(len(litLens)-257), 5)
	w.writeBits(uint64(len(distLens)-1), 5)
	w.writeBits(uint64(numCL-4), 4)
	for _, sym := range testCodeLengthOrder[:numCL] {
		w.writeBits(uint64(clLens[sym]), 3)
	}
	clCodes := canonicalCodes(clLens)
	for _, cs := range clSyms {
		w.writeCode(clCodes[cs.sym], uint(clLens[cs.sym]))
		w.writeBits(cs.extra, cs.nbits)
	}

	litCodes, distCodes := canonicalCodes(litLens), canonicalCodes(distLens)
	w.writeTokens(toks,
		func(sym int) { w.writeCode(litCodes[sym], uint(litLens[sym])) },
		func(sym int) { w.writeCode(distCodes[sym], uint(distLens[sym])) })
}

// completeCodeLengths gives every used symbol, plus a few extra symbols
// picked by s, a code length of at most maxLen such that the lengths form a
// complete prefix code (the Kraft sum is exactly 1).
func completeCodeLengths(used []bool, maxLen int, s *programReader) []uint8 {
	lens := make([]uint8, len(used))
	var syms []int
	addSymbol := func(sym int) {
		if lens[sym] == 0 {
			lens[sym] = 1 + s.byte()%uint8(maxLen)
			syms = append(syms, sym)
		}
	}
	for sym, u := range used {
		if u {
			addSymbol(sym)
		}
	}
	for range s.byte() % 8 {
		addSymbol(s.uint16() % len(used))
	}
	for sym := 0; len(syms) < 2; sym++ {
		addSymbol(sym)
	}

	full := 1 << maxLen
	kraft := 0
	for _, sym := range syms {
		kraft += full >> lens[sym]
	}
	pick := func(better func(a, b uint8) bool) int {
		best := syms[0]
		for _, sym := range syms[1:] {
			if better(lens[sym], lens[best]) {
				best = sym
			}
		}
		return best
	}
	for kraft > full {
		sym := pick(func(a, b uint8) bool { return a < b })
		kraft -= full >> (lens[sym] + 1)
		lens[sym]++
	}
	// Every Kraft term is a multiple of the longest code's term, so the
	// remaining slack is too, and shortening the longest code never
	// overshoots.
	for kraft < full {
		sym := pick(func(a, b uint8) bool { return a > b })
		kraft += full >> lens[sym]
		lens[sym]--
	}
	return lens
}

func canonicalCodes(lens []uint8) []uint64 {
	var count [16]uint64
	for _, l := range lens {
		if l > 0 {
			count[l]++
		}
	}
	var next [16]uint64
	for l := 1; l < 16; l++ {
		next[l] = (next[l-1] + count[l-1]) << 1
	}
	codes := make([]uint64, len(lens))
	for sym, l := range lens {
		if l > 0 {
			codes[sym] = next[l]
			next[l]++
		}
	}
	return codes
}

func trimZeros(lens []uint8, minLen int) []uint8 {
	n := len(lens)
	for n > minLen && lens[n-1] == 0 {
		n--
	}
	return lens[:n]
}

func FuzzTokens(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("\x00\x05hello\x01\x00\x00\x01\x00\x00"))
	f.Add([]byte("\x00\x00x\x01\xff\xff\x00\x00\x01"))
	f.Add([]byte("\x04\x00\x00\x05hello\x01\x00\x00\x01\x00\x00"))
	f.Add([]byte("\x04\x06\x03\x01\x09\x02\x05\x01\x00\x0fdynamic huffman\x01\x10\x00\x03\x00\x01\x03\x00\x01\x01\x00"))
	f.Add([]byte("\x02\xff\x03" + string(randomBytes(5, 1023)) + "\x01\xff\x00\xff\xbf\x00\x03\x01\x00\x01\xff\xff\x01"))
	f.Fuzz(func(t *testing.T, program []byte) {
		stream, want := buildStream(program)
		got, err := io.ReadAll(NewReader(bytes.NewReader(stream)))
		if err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if !bytes.Equal(got, want) {
			at := 0
			for at < min(len(got), len(want)) && got[at] == want[at] {
				at++
			}
			t.Fatalf("got %d bytes, want %d; first difference at offset %d", len(got), len(want), at)
		}
	})
}
