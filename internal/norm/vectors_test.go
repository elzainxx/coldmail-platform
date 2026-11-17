package norm

// The shared vector file and the machinery that reads it. testdata/norm-vectors.json is the platform's copy of the file module 1 also runs through its own Normalize
// (coldmail-verify, control-plane/internal/hash/testdata/norm-vectors.json): its expectations were produced by module 1's real function, never by this package, and the
// harness test tests/harness/norm_agreement_test.go compares the two copies byte for byte. Nothing in this file calls Normalize to decide what is right.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"unicode/utf8"
)

const vectorFilePath = "testdata/norm-vectors.json"

// vector is one entry of the file. Input and InputB64 are pointers so that "absent" and "empty" differ.
type vector struct {
	Name       string  `json:"name"`
	Input      *string `json:"input"`
	InputB64   *string `json:"input_b64"`
	Malformed  bool    `json:"malformed"`
	Normalized *string `json:"normalized"`
	Group      *string `json:"group"`
	Why        string  `json:"why"`
}

type vectorFile struct {
	Format      string   `json:"format"`
	Scheme      string   `json:"scheme"`
	GeneratedBy string   `json:"generated_by"`
	Fields      string   `json:"fields"`
	Vectors     []vector `json:"vectors"`
}

// raw returns the bytes of the input, whichever of the two fields carries them.
func (v vector) raw() (string, error) {
	switch {
	case v.Input != nil && v.InputB64 != nil:
		return "", fmt.Errorf("vector %q has both input and input_b64", v.Name)
	case v.Input != nil:
		return *v.Input, nil
	case v.InputB64 != nil:
		b, err := base64.StdEncoding.DecodeString(*v.InputB64)
		if err != nil {
			return "", fmt.Errorf("vector %q: input_b64 is not base64: %v", v.Name, err)
		}
		return string(b), nil
	}
	return "", fmt.Errorf("vector %q has neither input nor input_b64", v.Name)
}

func (v vector) group() string {
	if v.Group == nil {
		return ""
	}
	return *v.Group
}

func (v vector) normalized() string {
	if v.Normalized == nil {
		return ""
	}
	return *v.Normalized
}

// readVectorFile returns the decoded file and its exact bytes. The decoder refuses an unknown field, so a typo in the file (or a field nobody reads) is an error, not a silent skip.
func readVectorFile(t testing.TB) (vectorFile, []byte) {
	t.Helper()
	b, err := os.ReadFile(vectorFilePath)
	if err != nil {
		t.Fatalf("reading the shared vector file: %v", err)
	}
	var f vectorFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("%s does not decode (an unknown or mistyped field is an error): %v", vectorFilePath, err)
	}
	if len(f.Vectors) == 0 {
		t.Fatalf("%s holds no vectors: the instrument matched nothing", vectorFilePath)
	}
	return f, b
}

func loadVectors(t testing.TB) []vector {
	t.Helper()
	f, _ := readVectorFile(t)
	return f.Vectors
}

// normFunc is the shape of Normalize, so the checkers can be run on a deliberately broken copy.
type normFunc func(string) (string, error)

// checkVector is the whole verdict for one vector: malformed iff the file says so, and the normalised string byte for byte. It returns "" when fn agrees with the file.
func checkVector(fn normFunc, v vector) string {
	in, err := v.raw()
	if err != nil {
		return err.Error()
	}
	got, gotErr := fn(in)
	switch {
	case v.Malformed && gotErr == nil:
		return fmt.Sprintf("%s: the file says MALFORMED, the function accepted it and returned %q", v.Name, got)
	case !v.Malformed && gotErr != nil:
		return fmt.Sprintf("%s: the file says the address is well formed (%q), the function refused it: %v", v.Name, v.normalized(), gotErr)
	case !v.Malformed && got != v.normalized():
		return fmt.Sprintf("%s: the function returned %q, the file says %q", v.Name, got, v.normalized())
	case v.Malformed && got != "":
		return fmt.Sprintf("%s: a refusal must return the empty string, got %q", v.Name, got)
	}
	return ""
}

// checkVectors runs every vector and returns every disagreement, in file order.
func checkVectors(fn normFunc, vs []vector) []string {
	var problems []string
	for _, v := range vs {
		if p := checkVector(fn, v); p != "" {
			problems = append(problems, p)
		}
	}
	return problems
}

// checkGroups returns every way the group claims of the file fail under fn: members of one group that do not give one string, and two groups that give the same string. A vector
// that fn refuses (or that is malformed in the file) has no group and is skipped; a vector with a group that fn refuses is a problem of its own.
func checkGroups(fn normFunc, vs []vector) []string {
	var problems []string
	byGroup := map[string]string{} // group -> the string its first member gave
	firstOf := map[string]string{} // group -> its first member's name
	byString := map[string]string{}
	for _, v := range vs {
		g := v.group()
		if g == "" {
			continue
		}
		in, err := v.raw()
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		got, gotErr := fn(in)
		if gotErr != nil {
			problems = append(problems, fmt.Sprintf("%s (group %q): refused by the function", v.Name, g))
			continue
		}
		if prev, ok := byGroup[g]; ok && prev != got {
			problems = append(problems, fmt.Sprintf("group %q: %s gives %q but %s gave %q", g, v.Name, got, firstOf[g], prev))
		} else if !ok {
			byGroup[g], firstOf[g] = got, v.Name
		}
		if other, ok := byString[got]; ok && other != g {
			problems = append(problems, fmt.Sprintf("groups %q and %q both give %q (%s)", other, g, got, v.Name))
		} else if !ok {
			byString[got] = g
		}
	}
	return problems
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
