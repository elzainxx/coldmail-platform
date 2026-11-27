package render

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// The vectors below were computed OUTSIDE this package: the SplitMix64 and subject-seed values with a Python implementation of Vigna's
// algorithm (the first output of seed 0, 0xe220a8397b1dcdaf, is the published reference value), the SHA-256 values with sha256sum and
// openssl. They are what "a given seed gives a given choice sequence forever" means: a change to a constant, to the way an output becomes an
// index, or to the way a seed is derived turns one of these red.

var splitMixVectors = []struct {
	seed uint64
	want [5]uint64
}{
	{0, [5]uint64{0xe220a8397b1dcdaf, 0x6e789e6aa1b965f4, 0x06c45d188009454f, 0xf88bb8a8724c81ec, 0x1b39896a51a8749b}},
	{1234567, [5]uint64{0x599ed017fb08fc85, 0x2c73f08458540fa5, 0x883ebce5a3f27c77, 0x3fbef740e9177b3f, 0xe3b8346708cb5ecd}},
	{^uint64(0), [5]uint64{0xe4d971771b652c20, 0xe99ff867dbf682c9, 0x382ff84cb27281e9, 0x6d1db36ccba982d2, 0xb4a0472e578069ae}},
	{42, [5]uint64{0xbdd732262feb6e95, 0x28efe333b266f103, 0x47526757130f9f52, 0x581ce1ff0e4ae394, 0x09bc585a244823f2}},
}

// checkSplitMix compares a generator (as a function from seed to its next-function) with the vectors.
func checkSplitMix(newGen func(seed uint64) func() uint64) []uint64 {
	var bad []uint64
	for _, v := range splitMixVectors {
		next := newGen(v.seed)
		for _, w := range v.want {
			if next() != w {
				bad = append(bad, v.seed)
				break
			}
		}
	}
	return bad
}

func TestGeneratorVectors(t *testing.T) {
	bad := checkSplitMix(func(seed uint64) func() uint64 {
		g := splitMix64{state: seed}
		return g.next
	})
	if len(bad) != 0 {
		t.Errorf("SplitMix64 output differs from the reference for seeds %v", bad)
	}
}

// TestGeneratorVectorsControl: the same check, run on generators with one constant changed, fails for every seed.
func TestGeneratorVectorsControl(t *testing.T) {
	variant := func(gamma, m1, m2 uint64, s1, s2, s3 uint) func(uint64) func() uint64 {
		return func(seed uint64) func() uint64 {
			state := seed
			return func() uint64 {
				state += gamma
				z := state
				z = (z ^ (z >> s1)) * m1
				z = (z ^ (z >> s2)) * m2
				return z ^ (z >> s3)
			}
		}
	}
	const g, m1, m2 = 0x9e3779b97f4a7c15, 0xbf58476d1ce4e5b9, 0x94d049bb133111eb
	if bad := checkSplitMix(variant(g, m1, m2, 30, 27, 31)); len(bad) != 0 {
		t.Fatalf("the reference variant disagrees with its own vectors for %v: the control is broken", bad)
	}
	for name, fn := range map[string]func(uint64) func() uint64{
		"gamma":             variant(g+2, m1, m2, 30, 27, 31),
		"first multiplier":  variant(g, m1+2, m2, 30, 27, 31),
		"second multiplier": variant(g, m1, m2+2, 30, 27, 31),
		"first shift":       variant(g, m1, m2, 29, 27, 31),
		"second shift":      variant(g, m1, m2, 30, 26, 31),
		"final shift":       variant(g, m1, m2, 30, 27, 32),
	} {
		if bad := checkSplitMix(fn); len(bad) != len(splitMixVectors) {
			t.Errorf("a changed %s was caught for %d of %d seeds", name, len(bad), len(splitMixVectors))
		}
	}
}

func TestIntnVectors(t *testing.T) {
	// seed 42, draws taken with these group sizes in this order; index = high 64 bits of output * n
	sizes := []int{2, 3, 4, 64, 2, 3, 4, 64, 2, 3, 4, 64}
	want := []int{1, 0, 1, 22, 0, 2, 0, 51, 0, 1, 0, 31}
	g := splitMix64{state: 42}
	for i, n := range sizes {
		if got := g.intn(n); got != want[i] {
			t.Errorf("draw %d (n=%d) = %d, want %d", i, n, got, want[i])
		}
	}
	// the control: a modulo mapping gives other indexes for the same outputs, so the vector pins the mapping and not only the outputs
	g = splitMix64{state: 42}
	same := 0
	for i, n := range sizes {
		if int(g.next()%uint64(n)) == want[i] {
			same++
		}
	}
	if same == len(sizes) {
		t.Error("a modulo mapping agrees with the pinned indexes on every draw: the vector does not pin the mapping")
	}
	// every n from 1 to 64 stays in range
	g = splitMix64{state: 7}
	for n := 1; n <= 64; n++ {
		for i := 0; i < 200; i++ {
			if v := g.intn(n); v < 0 || v >= n {
				t.Fatalf("intn(%d) = %d", n, v)
			}
		}
	}
}

var spinSeedVectors = []struct {
	job, item string
	want      uint64
}{
	{"job-1", "item-1", 0xa3cadbd7ca9a87e1},
	{"", "", 0xe7ac0786668e0ff0},
	{"0b2e5d1c-1111-4222-8333-444455556666", "9a8b7c6d-aaaa-4bbb-8ccc-ddddeeeeffff", 0xd6fcfcc320e91e49},
	{"a:b", "c", 0xb0ee04f880c4ff42},
	{"a", "b:c", 0xb0ee04f880c4ff42}, // the documented consequence of the ruled derivation: ids hold no colon, so it never arises
}

func TestSpinSeedVectors(t *testing.T) {
	for _, v := range spinSeedVectors {
		if got := SpinSeed(v.job, v.item); got != v.want {
			t.Errorf("SpinSeed(%q, %q) = %#x, want %#x", v.job, v.item, got, v.want)
		}
		// and the definition itself, recomputed here: first 8 bytes, big-endian, of SHA-256(job ":" item)
		sum := sha256.Sum256([]byte(v.job + ":" + v.item))
		if got := binary.BigEndian.Uint64(sum[:8]); got != v.want {
			t.Errorf("the pinned vector for (%q, %q) is not the definition: %#x", v.job, v.item, got)
		}
	}
}

// TestSpinSeedControl: each way of getting the derivation wrong gives a value the vectors reject.
func TestSpinSeedControl(t *testing.T) {
	first8 := func(s string) uint64 {
		sum := sha256.Sum256([]byte(s))
		return binary.BigEndian.Uint64(sum[:8])
	}
	wrong := map[string]func(job, item string) uint64{
		"no separator": func(j, i string) uint64 { return first8(j + i) },
		"swapped ids":  func(j, i string) uint64 { return first8(i + ":" + j) },
		"little endian": func(j, i string) uint64 {
			s := sha256.Sum256([]byte(j + ":" + i))
			return binary.LittleEndian.Uint64(s[:8])
		},
		"last 8 bytes": func(j, i string) uint64 {
			s := sha256.Sum256([]byte(j + ":" + i))
			return binary.BigEndian.Uint64(s[24:])
		},
		"item id alone":     func(_, i string) uint64 { return first8(i) },
		"a different glue":  func(j, i string) uint64 { return first8(j + "/" + i) },
		"trimmed input":     func(j, i string) uint64 { return first8(strings.TrimSpace(j) + ":" + strings.TrimSpace(i) + "\n") },
		"the zero seed":     func(string, string) uint64 { return 0 },
		"the job id alone":  func(j, _ string) uint64 { return first8(j) },
		"sha256 of nothing": func(string, string) uint64 { return first8("") },
	}
	for name, fn := range wrong {
		caught := 0
		for _, v := range spinSeedVectors {
			if fn(v.job, v.item) != v.want {
				caught++
			}
		}
		if caught == 0 {
			t.Errorf("%s: no vector noticed", name)
		}
	}
	// different ids give different seeds
	if SpinSeed("j", "1") == SpinSeed("j", "2") || SpinSeed("j", "1") == SpinSeed("k", "1") {
		t.Error("two different (job, item) pairs share a seed")
	}
}

func TestPartSeed(t *testing.T) {
	subject := []struct{ seed, want uint64 }{
		{0, 0x5f6e9458fc7bd700},
		{1, 0x292d53e82d3d781d},
		{42, 0x3b62469a04011887},
		{^uint64(0), 0xd6e863f6d1ed6a87},
		{0xa3cadbd7ca9a87e1, 0x92e81c3f3c85bc90},
	}
	for _, v := range subject {
		if got := PartSeed(v.seed, PartSubject); got != v.want {
			t.Errorf("PartSeed(%#x, PartSubject) = %#x, want %#x", v.seed, got, v.want)
		}
		if got := PartSeed(v.seed, PartHTML); got != v.seed {
			t.Errorf("PartSeed(%#x, PartHTML) = %#x, want the seed itself", v.seed, got)
		}
		if got := PartSeed(v.seed, PartText); got != PartSeed(v.seed, PartHTML) {
			t.Errorf("PartSeed(%#x, PartText) = %#x, want the HTML seed", v.seed, got)
		}
		if PartSeed(v.seed, PartSubject) == v.seed {
			t.Errorf("the subject seed of %#x is the seed itself", v.seed)
		}
	}
	// the control: the subject stream is not a shifted copy of the HTML stream, so a subject group and an HTML group do not move together
	agree, total := 0, 3000
	for seed := uint64(0); seed < uint64(total); seed++ {
		h := splitMix64{state: PartSeed(seed, PartHTML)}
		s := splitMix64{state: PartSeed(seed, PartSubject)}
		if h.intn(3) == s.intn(3) {
			agree++
		}
	}
	if agree < total/3-150 || agree > total/3+150 { // independent streams agree a third of the time, give or take 5 standard deviations
		t.Errorf("html and subject draws agree on %d of %d seeds; independent streams agree on about %d", agree, total, total/3)
	}
}

func TestDeterminism(t *testing.T) {
	const src = "{Hi|Hello|Hey|Dear} {{first_name|friend}}, {we {saw|noticed|read} your {post|note|reply}|your {work|writing} stood out}. {{x}}"
	tpl := mustCompile(t, src)
	row := vals(map[string]string{"x": "<&>"})
	for _, part := range []Part{PartHTML, PartText, PartSubject} {
		for _, seed := range []uint64{0, 1, 42, SpinSeed("job-1", "item-1"), ^uint64(0)} {
			first := tpl.Render(seed, part, row)
			for i := 0; i < 1000; i++ {
				if got := tpl.Render(seed, part, row); got != first {
					t.Fatalf("part %d seed %d: repeat %d gave %q, the first gave %q", part, seed, i, got, first)
				}
			}
			// a freshly compiled template renders alike
			if got := mustCompile(t, src).Render(seed, part, row); got != first {
				t.Fatalf("part %d seed %d: a second Compile rendered %q, the first %q", part, seed, got, first)
			}
		}
	}
	// the control: the outputs are not the same for every seed
	distinct := map[string]bool{}
	for seed := uint64(0); seed < 200; seed++ {
		distinct[tpl.Render(seed, PartHTML, row)] = true
	}
	// the template has 4 x (9 + 2) = 44 possible renders; a spread over 200 seeds reaches nearly all of them, a constant reaches one
	if len(distinct) < 30 {
		t.Errorf("only %d distinct renders over 200 seeds: the determinism above would hold of a constant too", len(distinct))
	}
}

// chiSquare is the Pearson statistic of observed counts against a uniform expectation.
func chiSquare(counts []int, total int) float64 {
	exp := float64(total) / float64(len(counts))
	var x float64
	for _, c := range counts {
		d := float64(c) - exp
		x += d * d / exp
	}
	return x
}

// The bounds are the upper 0.001 critical values of the chi-square distribution for the degrees of freedom of each check: a fair generator
// exceeds one of them once in a thousand runs; the seeds are fixed, so the statistic is a fixed number and this test never flakes.
//
//	df  2: 13.82   (a group of 3 options)
//	df  3: 16.27   (4 options)
//	df  8: 26.12   (two groups of 3 options, joint: 9 cells)
//	df 63: 103.5   (64 options; Wilson-Hilferty approximation)
const (
	chi2Bound3  = 13.82
	chi2Bound4  = 16.27
	chi2Bound9  = 26.12
	chi2Bound64 = 103.5
)

func seedSample(n int) []uint64 {
	seeds := make([]uint64, 0, 2*n)
	for i := 0; i < n; i++ {
		seeds = append(seeds, uint64(i)) // consecutive integers: the worst case for a weak mixer
		seeds = append(seeds, SpinSeed("job-"+strconv.Itoa(i/50), "item-"+strconv.Itoa(i)))
	}
	return seeds
}

func TestSeedsTakeEveryOption(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		opts  []string
		bound float64
	}{
		{"3 options", "{a|b|c}", []string{"a", "b", "c"}, chi2Bound3},
		{"4 options", "{a|b|c|d}", []string{"a", "b", "c", "d"}, chi2Bound4},
	} {
		tpl := mustCompile(t, tc.src)
		for _, group := range []struct {
			name  string
			seeds []uint64
		}{
			{"2000 consecutive seeds", func() []uint64 {
				s := make([]uint64, 2000)
				for i := range s {
					s[i] = uint64(i)
				}
				return s
			}()},
			{"2000 job-item seeds", func() []uint64 {
				s := make([]uint64, 2000)
				for i := range s {
					s[i] = SpinSeed("job-1", fmt.Sprintf("item-%d", i))
				}
				return s
			}()},
		} {
			for _, part := range []Part{PartHTML, PartSubject} {
				idx := map[string]int{}
				for i, o := range tc.opts {
					idx[o] = i
				}
				counts := make([]int, len(tc.opts))
				for _, seed := range group.seeds {
					counts[idx[tpl.Render(seed, part, nil)]]++
				}
				for i, c := range counts {
					if c == 0 {
						t.Errorf("%s, %s, part %d: option %q was never taken", tc.name, group.name, part, tc.opts[i])
					}
				}
				if x := chiSquare(counts, len(group.seeds)); x > tc.bound {
					t.Errorf("%s, %s, part %d: chi-square %.2f exceeds the bound %.2f (counts %v)", tc.name, group.name, part, x, tc.bound, counts)
				} else {
					t.Logf("%s, %s, part %d: chi-square %.2f <= %.2f (counts %v)", tc.name, group.name, part, x, tc.bound, counts)
				}
			}
		}
	}
}

func TestSeedsTakeEveryOptionOf64AndJointly(t *testing.T) {
	opts := make([]string, 64)
	for i := range opts {
		opts[i] = "o" + strconv.Itoa(i) + ";"
	}
	tpl := mustCompile(t, "{"+strings.Join(opts, "|")+"}")
	counts := make([]int, 64)
	seeds := seedSample(1000) // 2000 seeds: consecutive and job-item
	for _, seed := range seeds {
		out := tpl.Render(seed, PartHTML, nil)
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(out, "o"), ";"))
		if err != nil {
			t.Fatalf("unexpected output %q", out)
		}
		counts[n]++
	}
	if x := chiSquare(counts, len(seeds)); x > chi2Bound64 {
		t.Errorf("64 options: chi-square %.2f exceeds the bound %.2f", x, chi2Bound64)
	} else {
		t.Logf("64 options over %d seeds: chi-square %.2f <= %.2f", len(seeds), x, chi2Bound64)
	}

	// two groups of three: the 9 combinations are as likely as each other, so the second draw does not depend on the first
	pair := mustCompile(t, "{0|1|2}{0|1|2}")
	joint := make([]int, 9)
	for _, seed := range seeds {
		out := pair.Render(seed, PartHTML, nil)
		joint[int(out[0]-'0')*3+int(out[1]-'0')]++
	}
	if x := chiSquare(joint, len(seeds)); x > chi2Bound9 {
		t.Errorf("two groups: joint chi-square %.2f exceeds the bound %.2f (counts %v)", x, chi2Bound9, joint)
	} else {
		t.Logf("two groups of 3 over %d seeds: joint chi-square %.2f <= %.2f", len(seeds), x, chi2Bound9)
	}
}

// TestChiSquareCatchesBadGenerators is the control of the two distribution tests above: the statistic and its bounds reject a constant
// choice, a choice that follows the seed's low bit only, and two draws that always agree.
func TestChiSquareCatchesBadGenerators(t *testing.T) {
	const n = 2000
	constant := make([]int, 3)
	constant[0] = n
	if x := chiSquare(constant, n); x <= chi2Bound3 {
		t.Errorf("a constant choice has chi-square %.2f, within the bound", x)
	}
	lowBit := make([]int, 3) // seed&1 only ever picks options 0 and 1
	for i := 0; i < n; i++ {
		lowBit[i&1]++
	}
	if x := chiSquare(lowBit, n); x <= chi2Bound3 {
		t.Errorf("a two-valued choice has chi-square %.2f, within the bound", x)
	}
	g := splitMix64{state: 5}
	same := make([]int, 9)
	for i := 0; i < n; i++ {
		v := g.intn(3)
		same[v*3+v]++ // the second draw always equals the first
	}
	if x := chiSquare(same, n); x <= chi2Bound9 {
		t.Errorf("two perfectly correlated draws have chi-square %.2f, within the bound", x)
	}
	// and a uniform generator is accepted by the same statistic (the bound is not simply unreachable)
	uniform := []int{667, 667, 666}
	if x := chiSquare(uniform, n); x > chi2Bound3 {
		t.Errorf("a perfectly even split has chi-square %.2f, over the bound", x)
	}
}
