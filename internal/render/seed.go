package render

import (
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
)

// Part is the part of a message a template renders for. The zero value is PartHTML, the strictest escaping.
type Part int

const (
	// PartHTML is the HTML body: a merge value is HTML-escaped.
	PartHTML Part = iota
	// PartText is the plain-text body: a merge value is inserted as it is.
	PartText
	// PartSubject is the Subject header: a merge value is inserted as it is, then every C0 control character of the result becomes a space.
	PartSubject
)

// SpinSeed is the seed of one job item: the first 8 bytes, big-endian, of SHA-256(jobID + ":" + itemID). A preview sample and the message
// that is sent for the same item therefore spin alike. (Both ids are UUIDs, which hold no ":", so the pair is unambiguous.)
func SpinSeed(jobID, itemID string) uint64 {
	sum := sha256.Sum256([]byte(jobID + ":" + itemID))
	return binary.BigEndian.Uint64(sum[:8])
}

// subjectSalt is "subject" and a zero byte. It never changes.
const subjectSalt uint64 = 0x7375626a65637400

// PartSeed is the seed of the spin stream of one part. The HTML and the text body share one stream (PartText is PartHTML); the subject has
// its own: the first SplitMix64 output of the seed xor subjectSalt. Never change this: a stored seed must keep rendering the same subject.
func PartSeed(seed uint64, part Part) uint64 {
	if part == PartSubject {
		g := splitMix64{state: seed ^ subjectSalt}
		return g.next()
	}
	return seed
}

// splitMix64 is the generator that chooses spintax options: Vigna's SplitMix64 (public domain), written out here so that no library can
// ever change what a seed means. Its constants, and the way intn maps an output to an index, are pinned by test vectors.
type splitMix64 struct{ state uint64 }

func (g *splitMix64) next() uint64 {
	g.state += 0x9e3779b97f4a7c15
	z := g.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// intn returns an index in [0, n) for n >= 1: the high 64 bits of next()*n. One call draws exactly one output.
func (g *splitMix64) intn(n int) int {
	hi, _ := bits.Mul64(g.next(), uint64(n))
	return int(hi)
}
