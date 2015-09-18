// Copyright 2011 The Snappy-Go Authors. All rights reserved.
// Modified for deflate by Klaus Post (c) 2015.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

type tokens struct {
	tokens []token
}

// We limit how far copy back-references can go, the same as the C++ code.
const maxOffset = 1 << 15

// emitLiteral writes a literal chunk and returns the number of bytes written.
func emitLiteral(dst *tokens, lit []byte) {
	ol := len(dst.tokens)
	dst.tokens = dst.tokens[0 : ol+len(lit)]
	t := dst.tokens[ol:]
	for i, v := range lit {
		t[i] = token(v)
	}
}

// emitCopy writes a copy chunk and returns the number of bytes written.
func emitCopy(dst *tokens, offset, length int) {
	dst.tokens = append(dst.tokens, matchToken(uint32(length-3), uint32(offset-minOffsetSize)))
}

// snappyEncode uses Snappy-like compression, but stores as Huffman
// blocks.
func snappyEncode(dst *tokens, src []byte) {
	// Return early if src is short.
	if len(src) <= 4 {
		if len(src) != 0 {
			emitLiteral(dst, src)
		}
		return
	}

	// Initialize the hash table. Its size ranges from 1<<8 to 1<<14 inclusive.
	const maxTableSize = 1 << 14
	shift, tableSize := uint(32-8), 1<<8
	for tableSize < maxTableSize && tableSize < len(src) {
		shift--
		tableSize *= 2
	}
	var table [maxTableSize]int

	// Iterate over the source bytes.
	var (
		s   int // The iterator position.
		t   int // The last position with the same hash as s.
		lit int // The start position of any pending literal bytes.
	)
	for s+3 < len(src) {
		// Update the hash table.
		b0, b1, b2, b3 := src[s], src[s+1], src[s+2], src[s+3]
		h := uint32(b0) | uint32(b1)<<8 | uint32(b2)<<16 | uint32(b3)<<24
		p := &table[(h*0x1e35a7bd)>>shift]
		// We need to to store values in [-1, inf) in table. To save
		// some initialization time, (re)use the table's zero value
		// and shift the values against this zero: add 1 on writes,
		// subtract 1 on reads.
		t, *p = *p-1, s+1
		// If t is invalid or src[s:s+4] differs from src[t:t+4], accumulate a literal byte.
		if t < 0 || s-t >= maxOffset || b0 != src[t] || b1 != src[t+1] || b2 != src[t+2] || b3 != src[t+3] {
			s++
			continue
		}
		// Otherwise, we have a match. First, emit any pending literal bytes.
		if lit != s {
			emitLiteral(dst, src[lit:s])
		}
		// Extend the match to be as long as possible.
		s0 := s
		s1 := s + maxMatchLength
		if s1 > len(src) {
			s1 = len(src)
		}
		s, t = s+4, t+4
		for s < s1 && src[s] == src[t] {
			s++
			t++
		}
		// Emit the copied bytes.
		// inlined: emitCopy(dst, s-t, s-s0)
		dst.tokens = append(dst.tokens, matchToken(uint32(s-s0-3), uint32(s-t-minOffsetSize)))

		lit = s
	}

	// Emit any final pending literal bytes and return.
	if lit != len(src) {
		emitLiteral(dst, src[lit:])
	}
}
