package deflate64

import (
	"bufio"
	"compress/flate"
	"io"
	"math/bits"
	"sync"
)

const (
	windowSize    = 1 << 16
	maxCodeLen    = 15
	numLitLen     = 286
	numDist       = 32
	numCodeLen    = 19
	endOfBlock    = 256
	firstLenCode  = 257
	maxTableIndex = numLitLen + numDist
)

var lengthBase = [...]uint16{
	3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31,
	35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 3,
}

var lengthExtra = [...]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
	3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 16,
}

var distBase = [...]uint32{
	1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193,
	257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577, 32769, 49153,
}

var distExtra = [...]uint8{
	0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
	7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13, 14, 14,
}

var codeLengthOrder = [numCodeLen]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// huffman is a single-level lookup table indexed by the next maxLen input
// bits. Each entry packs symbol<<4 | codeLength; a zero entry is an unused
// code.
type huffman struct {
	table  []uint16
	maxLen uint
}

func (h *huffman) init(lengths []uint8) bool {
	var count [maxCodeLen + 1]int
	var maxLen uint8
	for _, l := range lengths {
		count[l]++
		maxLen = max(maxLen, l)
	}
	count[0] = 0

	left := 1
	for l := 1; l <= maxCodeLen; l++ {
		left = left<<1 - count[l]
		if left < 0 {
			return false
		}
	}

	var next [maxCodeLen + 1]int
	code := 0
	for l := 1; l <= maxCodeLen; l++ {
		code = (code + count[l-1]) << 1
		next[l] = code
	}

	size := 1 << maxLen
	if cap(h.table) < size {
		h.table = make([]uint16, size)
	}
	h.table = h.table[:size]
	clear(h.table)
	h.maxLen = uint(maxLen)

	for sym, l := range lengths {
		if l == 0 {
			continue
		}
		reversed := int(bits.Reverse16(uint16(next[l])) >> (16 - l))
		next[l]++
		entry := uint16(sym<<4) | uint16(l)
		for i := reversed; i < size; i += 1 << l {
			h.table[i] = entry
		}
	}
	return true
}

var (
	fixedOnce   sync.Once
	fixedLitLen huffman
	fixedDist   huffman
)

func initFixed() {
	var lengths [288]uint8
	for i := range lengths {
		switch {
		case i < 144:
			lengths[i] = 8
		case i < 256:
			lengths[i] = 9
		case i < 280:
			lengths[i] = 7
		default:
			lengths[i] = 8
		}
	}
	fixedLitLen.init(lengths[:])

	var dist [numDist]uint8
	for i := range dist {
		dist[i] = 5
	}
	fixedDist.init(dist[:])
}

// window is the 64 KiB history buffer. Bytes in hist[rdPos:wrPos] have been
// decoded but not yet returned from Read.
type window struct {
	hist  []byte
	wrPos int
	rdPos int
	full  bool
}

func (w *window) reset(dict []byte) {
	if w.hist == nil {
		w.hist = make([]byte, windowSize)
	}
	if len(dict) > windowSize {
		dict = dict[len(dict)-windowSize:]
	}
	w.wrPos = copy(w.hist, dict)
	w.rdPos = w.wrPos
	w.full = false
	if w.wrPos == windowSize {
		w.wrPos, w.rdPos, w.full = 0, 0, true
	}
}

func (w *window) available() int {
	if w.full {
		return windowSize
	}
	return w.wrPos
}

func (w *window) spaceLeft() int {
	return windowSize - w.wrPos
}

func (w *window) writeByte(c byte) {
	w.hist[w.wrPos] = c
	w.wrPos++
}

// writeCopy copies length bytes from dist bytes back, stopping early if the
// end of the buffer is reached. It returns the number of bytes copied.
func (w *window) writeCopy(dist, length int) int {
	start := w.wrPos
	dst := start
	end := min(dst+length, windowSize)
	src := dst - dist
	if src < 0 {
		src += windowSize
		dst += copy(w.hist[dst:end], w.hist[src:])
		src = 0
	}
	// Overlapping copies double the copied run each iteration.
	for dst < end {
		dst += copy(w.hist[dst:end], w.hist[src:dst])
	}
	w.wrPos = dst
	return dst - start
}

func (w *window) flush() []byte {
	out := w.hist[w.rdPos:w.wrPos]
	w.rdPos = w.wrPos
	if w.wrPos == windowSize {
		w.wrPos, w.rdPos, w.full = 0, 0, true
	}
	return out
}

type byteReader interface {
	io.Reader
	io.ByteReader
}

type state int

const (
	stateBlockHeader state = iota
	stateStored
	stateHuffman
	stateDone
)

type decompressor struct {
	r       byteReader
	roffset int64

	bits  uint32
	nbits uint

	win    window
	toRead []byte
	err    error

	state      state
	final      bool
	storedLeft int
	copyLen    int
	copyDist   int

	litLen  *huffman
	dist    *huffman
	dynLit  huffman
	dynDist huffman
	codeLen huffman
	lengths [maxTableIndex]uint8
}

func (d *decompressor) Reset(r io.Reader, dict []byte) error {
	br, ok := r.(byteReader)
	if !ok {
		br = bufio.NewReader(r)
	}
	*d = decompressor{
		r:       br,
		win:     d.win,
		dynLit:  d.dynLit,
		dynDist: d.dynDist,
		codeLen: d.codeLen,
	}
	d.win.reset(dict)
	return nil
}

func (d *decompressor) Read(p []byte) (int, error) {
	for {
		if len(d.toRead) > 0 {
			n := copy(p, d.toRead)
			d.toRead = d.toRead[n:]
			return n, nil
		}
		if d.err != nil {
			return 0, d.err
		}
		d.err = d.step()
		d.toRead = d.win.flush()
	}
}

func (d *decompressor) Close() error {
	if d.err == io.EOF {
		return nil
	}
	return d.err
}

// step decodes until the window fills or the stream ends. A nil return means
// more output may follow.
func (d *decompressor) step() error {
	for {
		switch d.state {
		case stateBlockHeader:
			if d.final {
				d.state = stateDone
				continue
			}
			if err := d.readBlockHeader(); err != nil {
				return err
			}
		case stateStored:
			if d.storedLeft == 0 {
				d.state = stateBlockHeader
				continue
			}
			if d.win.spaceLeft() == 0 {
				return nil
			}
			if err := d.copyStored(); err != nil {
				return err
			}
		case stateHuffman:
			done, err := d.decodeHuffman()
			if err != nil || !done {
				return err
			}
			d.state = stateBlockHeader
		case stateDone:
			return io.EOF
		}
	}
}

func (d *decompressor) corrupt() error {
	return flate.CorruptInputError(d.roffset)
}

func (d *decompressor) moreBits() error {
	c, err := d.r.ReadByte()
	if err != nil {
		return noEOF(err)
	}
	d.roffset++
	d.bits |= uint32(c) << d.nbits
	d.nbits += 8
	return nil
}

func (d *decompressor) readBits(n uint) (uint32, error) {
	for d.nbits < n {
		if err := d.moreBits(); err != nil {
			return 0, err
		}
	}
	v := d.bits & (1<<n - 1)
	d.bits >>= n
	d.nbits -= n
	return v, nil
}

func (d *decompressor) readSymbol(h *huffman) (int, error) {
	hitEOF := false
	for d.nbits < h.maxLen {
		if err := d.moreBits(); err != nil {
			if err != io.ErrUnexpectedEOF {
				return 0, err
			}
			hitEOF = true
			break
		}
	}
	entry := h.table[d.bits&(1<<h.maxLen-1)]
	n := uint(entry & 0xf)
	switch {
	case n > d.nbits || (n == 0 && hitEOF):
		return 0, io.ErrUnexpectedEOF
	case n == 0:
		return 0, d.corrupt()
	}
	d.bits >>= n
	d.nbits -= n
	return int(entry >> 4), nil
}

func (d *decompressor) readBlockHeader() error {
	header, err := d.readBits(3)
	if err != nil {
		return err
	}
	d.final = header&1 == 1
	switch header >> 1 {
	case 0:
		return d.readStoredHeader()
	case 1:
		fixedOnce.Do(initFixed)
		d.litLen, d.dist = &fixedLitLen, &fixedDist
	case 2:
		if err := d.readDynamicHeader(); err != nil {
			return err
		}
		d.litLen, d.dist = &d.dynLit, &d.dynDist
	default:
		return d.corrupt()
	}
	d.state = stateHuffman
	return nil
}

func (d *decompressor) readStoredHeader() error {
	d.bits >>= d.nbits % 8
	d.nbits -= d.nbits % 8
	length, err := d.readBits(16)
	if err != nil {
		return err
	}
	inverse, err := d.readBits(16)
	if err != nil {
		return err
	}
	if uint16(length) != ^uint16(inverse) {
		return d.corrupt()
	}
	d.storedLeft = int(length)
	d.state = stateStored
	return nil
}

func (d *decompressor) copyStored() error {
	for d.nbits >= 8 && d.storedLeft > 0 && d.win.spaceLeft() > 0 {
		d.win.writeByte(byte(d.bits))
		d.bits >>= 8
		d.nbits -= 8
		d.storedLeft--
	}
	n := min(d.storedLeft, d.win.spaceLeft())
	read, err := io.ReadFull(d.r, d.win.hist[d.win.wrPos:d.win.wrPos+n])
	d.roffset += int64(read)
	d.win.wrPos += read
	d.storedLeft -= read
	return noEOF(err)
}

func (d *decompressor) readDynamicHeader() error {
	hlit, err := d.readBits(5)
	if err != nil {
		return err
	}
	hdist, err := d.readBits(5)
	if err != nil {
		return err
	}
	hclen, err := d.readBits(4)
	if err != nil {
		return err
	}
	nlit, ndist, nclen := int(hlit)+257, int(hdist)+1, int(hclen)+4
	if nlit > numLitLen {
		return d.corrupt()
	}

	var clen [numCodeLen]uint8
	for _, sym := range codeLengthOrder[:nclen] {
		v, err := d.readBits(3)
		if err != nil {
			return err
		}
		clen[sym] = uint8(v)
	}
	if !d.codeLen.init(clen[:]) {
		return d.corrupt()
	}

	lengths := d.lengths[:nlit+ndist]
	for i := 0; i < len(lengths); {
		sym, err := d.readSymbol(&d.codeLen)
		if err != nil {
			return err
		}
		if sym < 16 {
			lengths[i] = uint8(sym)
			i++
			continue
		}
		var repeat uint32
		var value uint8
		switch sym {
		case 16:
			if i == 0 {
				return d.corrupt()
			}
			value = lengths[i-1]
			repeat, err = d.readBits(2)
			repeat += 3
		case 17:
			repeat, err = d.readBits(3)
			repeat += 3
		case 18:
			repeat, err = d.readBits(7)
			repeat += 11
		}
		if err != nil {
			return err
		}
		if i+int(repeat) > len(lengths) {
			return d.corrupt()
		}
		for range repeat {
			lengths[i] = value
			i++
		}
	}

	if lengths[endOfBlock] == 0 || !d.dynLit.init(lengths[:nlit]) || !d.dynDist.init(lengths[nlit:]) {
		return d.corrupt()
	}
	return nil
}

// decodeHuffman decodes symbols until the end of the block (done == true) or
// until the window fills.
func (d *decompressor) decodeHuffman() (done bool, err error) {
	for {
		if d.copyLen > 0 {
			if d.win.spaceLeft() == 0 {
				return false, nil
			}
			d.copyLen -= d.win.writeCopy(d.copyDist, d.copyLen)
			continue
		}
		if d.win.spaceLeft() == 0 {
			return false, nil
		}

		sym, err := d.readSymbol(d.litLen)
		if err != nil {
			return false, err
		}
		switch {
		case sym < endOfBlock:
			d.win.writeByte(byte(sym))
			continue
		case sym == endOfBlock:
			return true, nil
		case sym >= numLitLen:
			return false, d.corrupt()
		}

		lenCode := sym - firstLenCode
		extra, err := d.readBits(uint(lengthExtra[lenCode]))
		if err != nil {
			return false, err
		}
		length := int(lengthBase[lenCode]) + int(extra)

		distCode, err := d.readSymbol(d.dist)
		if err != nil {
			return false, err
		}
		extra, err = d.readBits(uint(distExtra[distCode]))
		if err != nil {
			return false, err
		}
		dist := int(distBase[distCode]) + int(extra)
		if dist > d.win.available() {
			return false, d.corrupt()
		}
		d.copyLen, d.copyDist = length, dist
	}
}

func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
