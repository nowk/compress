// Copyright 2011 The Snappy-Go Authors. All rights reserved.
// Modified for deflate by Klaus Post (c) 2015.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

// emitLiteral writes a literal chunk and returns the number of bytes written.
func emitLiteral(dst *tokens, lit []byte) {
	ol := int(dst.n)
	for i, v := range lit {
		dst.tokens[(i+ol)&maxStoreBlockSize] = token(v)
	}
	dst.n += uint16(len(lit))
}

// emitCopy writes a copy chunk and returns the number of bytes written.
func emitCopy(dst *tokens, offset, length int) {
	dst.tokens[dst.n] = matchToken(uint32(length-3), uint32(offset-minOffsetSize))
	dst.n++
}

type snappyEnc interface {
	Encode(dst *tokens, src []byte)
	Reset()
}

func newSnappy(level int) snappyEnc {
	switch level {
	case 1:
		return &snappyL1{}
	case 2:
		return &snappyL2{snappyGen: snappyGen{cur: 1, prev: make([]byte, 0, maxStoreBlockSize)}}
	case 3:
		return &snappyL3{snappyGen: snappyGen{cur: 1, prev: make([]byte, 0, maxStoreBlockSize)}}
	default:
		panic("invalid level specified")
	}
}

const (
	tableBits       = 14             // Bits used in the table
	tableSize       = 1 << tableBits // Size of the table
	tableMask       = tableSize - 1  // Mask for table indices. Redundant, but can eliminate bounds checks.
	tableShift      = 32 - tableBits // Right-shift to get the tableBits most significant bits of a uint32.
	baseMatchOffset = 1              // The smallest match offset
	baseMatchLength = 3              // The smallest match length per the RFC section 3.2.5
	maxMatchOffset  = 1 << 15        // The largest match offset
)

func load32(b []byte, i int) uint32 {
	b = b[i : i+4 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func load64(b []byte, i int) uint64 {
	b = b[i : i+8 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func hash(u uint32) uint32 {
	return (u * 0x1e35a7bd) >> tableShift
}

// snappyL1 encapsulates level 1 compression
type snappyL1 struct{}

func (e *snappyL1) Reset() {}

func (e *snappyL1) Encode(dst *tokens, src []byte) {
	const (
		inputMargin            = 16 - 1
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)

	// This check isn't in the Snappy implementation, but there, the caller
	// instead of the callee handles this case.
	if len(src) < minNonLiteralBlockSize {
		// We do not fill the token table.
		// This will be picked up by caller.
		dst.n = uint16(len(src))
		return
	}

	// Initialize the hash table.
	//
	// The table element type is uint16, as s < sLimit and sLimit < len(src)
	// and len(src) <= maxStoreBlockSize and maxStoreBlockSize == 65535.
	var table [tableSize]uint16

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := len(src) - inputMargin

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := 0

	// The encoded form must start with a literal, as there are no previous
	// bytes to copy, so we start looking for hash matches at s == 1.
	s := 1
	nextHash := hash(load32(src, s))

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		skip := 32

		nextS := s
		candidate := 0
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				goto emitRemainder
			}
			candidate = int(table[nextHash&tableMask])
			table[nextHash&tableMask] = uint16(s)
			nextHash = hash(load32(src, nextS))
			// TODO: < should be <=, and add a test for that.
			if s-candidate < maxMatchOffset && load32(src, s) == load32(src, candidate) {
				break
			}
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.
		emitLiteral(dst, src[nextEmit:s])

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.
			base := s

			// Extend the 4-byte match as long as possible.
			//
			// This is an inlined version of Snappy's:
			//	s = extendMatch(src, candidate+4, s+4)
			s += 4
			s1 := base + maxMatchLength
			if s1 > len(src) {
				s1 = len(src)
			}
			for i := candidate + 4; s < s1 && src[i] == src[s]; i, s = i+1, s+1 {
			}

			// matchToken is flate's equivalent of Snappy's emitCopy.
			dst.tokens[dst.n] = matchToken(uint32(s-base-baseMatchLength), uint32(base-candidate-baseMatchOffset))
			dst.n++
			nextEmit = s
			if s >= sLimit {
				goto emitRemainder
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-1 and at s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load64(src, s-1)
			prevHash := hash(uint32(x >> 0))
			table[prevHash&tableMask] = uint16(s - 1)
			currHash := hash(uint32(x >> 8))
			candidate = int(table[currHash&tableMask])
			table[currHash&tableMask] = uint16(s)
			// TODO: >= should be >, and add a test for that.
			if s-candidate >= maxMatchOffset || uint32(x>>8) != load32(src, candidate) {
				nextHash = hash(uint32(x >> 16))
				s++
				break
			}
		}
	}

emitRemainder:
	if nextEmit < len(src) {
		emitLiteral(dst, src[nextEmit:])
	}
}

type tableEntry struct {
	val    uint32
	offset int32
}

func load3232(b []byte, i int32) uint32 {
	b = b[i : i+4 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func load6432(b []byte, i int32) uint64 {
	b = b[i : i+8 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// snappyGen maintains the table for matches,
// and the previous byte block for level 2.
// This is the generic implementation.
type snappyGen struct {
	prev []byte
	cur  int32
}

// snappyGen maintains the table for matches,
// and the previous byte block for level 2.
// This is the generic implementation.
type snappyL2 struct {
	snappyGen
	table [tableSize]tableEntry
}

// EncodeL2 uses a similar algorithm to level 1, but is capable
// of matching across blocks giving better compression at a small slowdown.
func (e *snappyL2) Encode(dst *tokens, src []byte) {
	const (
		inputMargin            = 16 - 1
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)

	// Ensure that e.cur doesn't wrap, mainly an issue on 32 bits.
	if e.cur > 1<<30 {
		e.cur = 1
	}

	// This check isn't in the Snappy implementation, but there, the caller
	// instead of the callee handles this case.
	if len(src) < minNonLiteralBlockSize {
		// We do not fill the token table.
		// This will be picked up by caller.
		dst.n = uint16(len(src))
		e.cur += maxStoreBlockSize
		e.prev = e.prev[:0]
		return
	}

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := int32(len(src) - inputMargin)

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := int32(0)

	// The encoded form must start with a literal, as there are no previous
	// bytes to copy, so we start looking for hash matches at s == 1.
	s := int32(1)
	cv := load3232(src, s)
	nextHash := hash(cv)
	prevLen := int32(len(e.prev))

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		skip := int32(32)

		nextS := s
		var candidate tableEntry
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				goto emitRemainder
			}
			candidate = e.table[nextHash&tableMask]
			now := load3232(src, nextS)
			e.table[nextHash&tableMask] = tableEntry{offset: s + e.cur, val: cv}
			nextHash = hash(now)

			offset := s - (candidate.offset - e.cur)
			if offset >= maxMatchOffset ||
				cv != candidate.val ||
				offset-s >= prevLen {
				// Out of range or not matched.
				cv = now
				continue
			}
			break
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.
		emitLiteral(dst, src[nextEmit:s])

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.

			// Extend the 4-byte match as long as possible.
			//
			s += 4
			t := candidate.offset - e.cur + 4
			l := e.matchlen(s, t, src)

			// matchToken is flate's equivalent of Snappy's emitCopy. (length,offset)
			dst.tokens[dst.n] = matchToken(uint32(l+4-baseMatchLength), uint32(s-t-baseMatchOffset))
			dst.n++
			s += l
			nextEmit = s
			if s >= sLimit {
				goto emitRemainder
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-1 and at s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load6432(src, s-1)
			prevHash := hash(uint32(x))
			e.table[prevHash&tableMask] = tableEntry{offset: e.cur + s - 1, val: uint32(x)}
			x >>= 8
			currHash := hash(uint32(x))
			candidate = e.table[currHash&tableMask]
			e.table[currHash&tableMask] = tableEntry{offset: e.cur + s, val: uint32(x)}

			offset := s - (candidate.offset - e.cur)
			if offset >= maxMatchOffset ||
				uint32(x) != candidate.val ||
				offset-s >= prevLen {
				cv = uint32(x >> 8)
				nextHash = hash(cv)
				s++
				break
			}
		}
	}

emitRemainder:
	if int(nextEmit) < len(src) {
		emitLiteral(dst, src[nextEmit:])
	}
	e.cur += int32(len(src))
	e.prev = e.prev[:len(src)]
	copy(e.prev, src)
}

type tableEntryPrev struct {
	Cur  tableEntry
	Prev tableEntry
}

// snappyL3
type snappyL3 struct {
	snappyGen
	table [tableSize]tableEntryPrev
}

// Encode uses a similar algorithm to level 2, will check up to two candidates.
func (e *snappyL3) Encode(dst *tokens, src []byte) {
	const (
		inputMargin            = 16 - 1
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)

	// Ensure that e.cur doesn't wrap, mainly an issue on 32 bits.
	if e.cur > 1<<30 {
		e.cur = 1
	}

	// This check isn't in the Snappy implementation, but there, the caller
	// instead of the callee handles this case.
	if len(src) < minNonLiteralBlockSize {
		// We do not fill the token table.
		// This will be picked up by caller.
		dst.n = uint16(len(src))
		e.cur += maxStoreBlockSize
		e.prev = e.prev[:0]
		return
	}

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := int32(len(src) - inputMargin)

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := int32(0)

	// The encoded form must start with a literal, as there are no previous
	// bytes to copy, so we start looking for hash matches at s == 1.
	s := int32(1)
	cv := load3232(src, s)
	nextHash := hash(cv)
	var prevLen = int32(len(e.prev))

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		skip := int32(32)

		nextS := s
		var candidate tableEntry
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				goto emitRemainder
			}
			candidates := e.table[nextHash&tableMask]
			now := load3232(src, nextS)
			e.table[nextHash&tableMask] = tableEntryPrev{Prev: candidates.Cur, Cur: tableEntry{offset: s + e.cur, val: cv}}
			nextHash = hash(now)

			// Check both candidates
			candidate = candidates.Cur
			if cv == candidate.val {
				offset := s - (candidate.offset - e.cur)
				if offset < maxMatchOffset &&
					offset-s < prevLen {
					break
				}
			} else {
				// We only check if value mismatches.
				// Offset will always be invalid in other cases.
				candidate = candidates.Prev
				if cv == candidate.val {
					offset := s - (candidate.offset - e.cur)
					if offset < maxMatchOffset &&
						offset-s < prevLen {
						break
					}
				}
			}
			cv = now
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.
		emitLiteral(dst, src[nextEmit:s])

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.

			// Extend the 4-byte match as long as possible.
			//
			s += 4
			t := candidate.offset - e.cur + 4
			l := e.matchlen(s, t, src)

			// matchToken is flate's equivalent of Snappy's emitCopy. (length,offset)
			dst.tokens[dst.n] = matchTokend(uint32(l+4-baseMatchLength), uint32(s-t-baseMatchOffset))
			dst.n++
			s += l
			nextEmit = s
			if s >= sLimit {
				goto emitRemainder
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-2, s-1 and at s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load6432(src, s-2)
			prevHash := hash(uint32(x))

			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 2, val: uint32(x)},
			}
			x >>= 8
			prevHash = hash(uint32(x))

			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 1, val: uint32(x)},
			}
			x >>= 8
			currHash := hash(uint32(x))
			candidates := e.table[currHash&tableMask]
			cv = uint32(x)
			e.table[currHash&tableMask] = tableEntryPrev{
				Prev: candidates.Cur,
				Cur:  tableEntry{offset: s + e.cur, val: cv},
			}

			// Check both candidates
			candidate = candidates.Cur
			if cv == candidate.val {
				offset := s - (candidate.offset - e.cur)
				if offset < maxMatchOffset &&
					offset-s < prevLen {
					continue
				}
			} else {
				// We only check if value mismatches.
				// Offset will always be invalid in other cases.
				candidate = candidates.Prev
				if cv == candidate.val {
					offset := s - (candidate.offset - e.cur)
					if offset < maxMatchOffset &&
						offset-s < prevLen {
						continue
					}
				}
			}
			cv = uint32(x >> 8)
			nextHash = hash(cv)
			s++
			break
		}
	}

emitRemainder:
	if int(nextEmit) < len(src) {
		emitLiteral(dst, src[nextEmit:])
	}
	e.cur += int32(len(src))
	e.prev = e.prev[:len(src)]
	copy(e.prev, src)
}

func (e *snappyGen) matchlen(s, t int32, src []byte) int32 {
	s0 := s
	s1 := s + maxMatchLength - 4
	if s1 > int32(len(src)) {
		s1 = int32(len(src))
	}

	// If we are inside the current block
	if t >= 0 {
		// Extend the match to be as long as possible.
		for s < s1 && src[s] == src[t] {
			s, t = s+1, t+1
		}
		return s - s0
	}

	// We found a match in the previous block.
	tp := int32(len(e.prev)) + t
	prevLen := int32(len(e.prev))

	// Extend the match to be as long as possible.
	for s < s1 && src[s] == e.prev[tp] {
		s, tp = s+1, tp+1
		if tp == prevLen {
			// continue in current buffer
			t = 0
			for s < s1 && src[s] == src[t] {
				s, t = s+1, t+1
			}
			return s - s0
		}
	}
	return s - s0
}

// Reset the encoding table.
func (e *snappyGen) Reset() {
	e.prev = e.prev[:0]
	e.cur += maxMatchOffset + 1
}
