package sendapitest

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// CheckVocabAt is the adapter side's own check of RouteContract.Vocab against RouteContract.VocabAt and the recorded body, written independently of internal/contractcheck (it reads the
// path grammar a second time, on purpose: the drift gate reads the same paths against the real module, and a vocabulary placed at a path that the recorded body does not hold would make
// that check silently inert). For every contract it requires: every Vocab name has a VocabAt path and every VocabAt name a Vocab entry; the path is well formed (a.b is a key, x[] is
// every element of an array, x{} every value of a map); the recorded body holds at least one STRING at the path (a vocabulary that constrains nothing in its own fixture is not placed
// anywhere real); every string it holds there is in the set; and the set is not empty and lists no value twice. It returns the problems, each naming the contract and the vocabulary, and a
// problem for an empty subject (no contract carries a vocabulary).
func CheckVocabAt(contracts []modclient.RouteContract, fsys fs.FS, dir string) []string {
	var problems []string
	carried := 0
	for _, c := range contracts {
		who := func(name, format string, a ...any) {
			problems = append(problems, fmt.Sprintf("%s / %s: vocabulary %q: %s", c.Route, c.Case, name, fmt.Sprintf(format, a...)))
		}
		for _, name := range sortedNames(c.VocabAt) {
			if _, ok := c.Vocab[name]; !ok {
				who(name, "VocabAt names it and Vocab has no such entry")
			}
		}
		var doc any
		loaded := false
		for _, name := range sortedNames(c.Vocab) {
			carried++
			set := c.Vocab[name]
			at, ok := c.VocabAt[name]
			if !ok {
				who(name, "Vocab lists it and VocabAt gives it no path (the live drift gate would read the name as a path of the body)")
				continue
			}
			segs, err := parseVocabPath(at)
			if err != nil {
				who(name, "the path %q is not of the grammar: %v", at, err)
				continue
			}
			if len(set) == 0 {
				who(name, "the set is empty")
			}
			known := map[string]bool{}
			for _, v := range set {
				if known[v] {
					who(name, "the set lists %q twice", v)
				}
				known[v] = true
			}
			if !loaded {
				raw, err := fs.ReadFile(fsys, path.Join(dir, c.Fixture))
				if err != nil {
					who(name, "the fixture %s cannot be read", c.Fixture)
					continue
				}
				if err := json.Unmarshal(raw, &doc); err != nil {
					who(name, "the fixture %s is not JSON", c.Fixture)
					continue
				}
				loaded = true
			}
			var found []string
			collect(doc, segs, &found)
			if len(found) == 0 {
				who(name, "the fixture %s holds no string at %q: the vocabulary constrains nothing in its own fixture", c.Fixture, at)
				continue
			}
			reported := map[string]bool{}
			for _, v := range found {
				if !known[v] && !reported[v] {
					reported[v] = true
					who(name, "the fixture %s holds %q at %q, which the set does not list", c.Fixture, v, at)
				}
			}
		}
	}
	if carried == 0 {
		problems = append(problems, "no contract carries a vocabulary: the subject of the check is empty")
	}
	return problems
}

type vseg struct {
	kind byte // 'k' a key, 'e' every array element, 'm' every map value
	name string
}

// parseVocabPath reads the grammar: names of letters, digits, '_' and '-' joined by dots, each followed by any number of [] or {}; a path may start with [] or {}.
func parseVocabPath(p string) ([]vseg, error) {
	var out []vseg
	i, needName := 0, false
	for i < len(p) {
		switch {
		case strings.HasPrefix(p[i:], "[]"), strings.HasPrefix(p[i:], "{}"):
			if needName {
				return nil, fmt.Errorf("a name must follow the dot")
			}
			out = append(out, vseg{kind: map[byte]byte{'[': 'e', '{': 'm'}[p[i]]})
			i += 2
		case p[i] == '.':
			if len(out) == 0 || needName {
				return nil, fmt.Errorf("a dot needs a name before it")
			}
			needName = true
			i++
		default:
			j := i
			for j < len(p) && (p[j] >= 'a' && p[j] <= 'z' || p[j] >= 'A' && p[j] <= 'Z' || p[j] >= '0' && p[j] <= '9' || p[j] == '_' || p[j] == '-') {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("unexpected character at offset %d", i)
			}
			if len(out) > 0 && !needName {
				return nil, fmt.Errorf("a name must be joined to its parent with a dot")
			}
			out = append(out, vseg{kind: 'k', name: p[i:j]})
			needName = false
			i = j
		}
	}
	if needName {
		return nil, fmt.Errorf("the path ends with a dot")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the path is empty")
	}
	return out, nil
}

// collect gathers the strings at a path of a decoded body (a value of another type at the path is not a vocabulary value).
func collect(v any, segs []vseg, out *[]string) {
	if len(segs) == 0 {
		if s, ok := v.(string); ok {
			*out = append(*out, s)
		}
		return
	}
	switch segs[0].kind {
	case 'k':
		if m, ok := v.(map[string]any); ok {
			if e, ok := m[segs[0].name]; ok {
				collect(e, segs[1:], out)
			}
		}
	case 'e':
		if a, ok := v.([]any); ok {
			for _, e := range a {
				collect(e, segs[1:], out)
			}
		}
	case 'm':
		if m, ok := v.(map[string]any); ok {
			for _, e := range m {
				collect(e, segs[1:], out)
			}
		}
	}
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
