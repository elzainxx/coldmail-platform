package contractcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// ProblemKind is what is wrong with the recorded fixtures of a package.
type ProblemKind string

const (
	// NoContracts: the list of contracts is empty (a gate whose subject is empty is red).
	NoContracts ProblemKind = "NoContracts"
	// BadContract: a contract that cannot be used (no route, no module, a status that is not HTTP, a fixture name that is not a plain file name, a zero Shape that checks
	// nothing, a Shape that cannot be read).
	BadContract ProblemKind = "BadContract"
	// DuplicateContract: two contracts for one module, route and case.
	DuplicateContract ProblemKind = "DuplicateContract"
	// DuplicateFixture: two contracts name one file (a file must be named by exactly one contract).
	DuplicateFixture ProblemKind = "DuplicateFixture"
	// Dangling: a contract names a file that does not exist.
	Dangling ProblemKind = "Dangling"
	// Stray: a file (or directory) in the fixture directory that no contract names: a fixture the adapter no longer uses.
	Stray ProblemKind = "Stray"
	// Unreadable: the fixture directory cannot be listed.
	Unreadable ProblemKind = "Unreadable"
	// NotJSON: the fixture is not one JSON document (see Detail for the ParseKind).
	NotJSON ProblemKind = "NotJSON"
	// ShapeRefused: the fixture does not decode with its contract's Shape: the adapter itself would refuse it.
	ShapeRefused ProblemKind = "ShapeRefused"
	// Secret: the fixture holds something secret-shaped (the repository is public).
	Secret ProblemKind = "Secret"
	// VocabKey: a Vocab key that is not a path of the documented grammar, or that names a position the fixture does not have (a typo would make the vocabulary check vacuous).
	VocabKey ProblemKind = "VocabKey"
	// VocabList: a closed vocabulary that is empty, or lists a value twice.
	VocabList ProblemKind = "VocabList"
	// VocabOutside: the fixture holds a vocabulary value that its own contract's Vocab does not list (the adapter's mapping would refuse its own fixture).
	VocabOutside ProblemKind = "VocabOutside"
)

// Problem is one thing wrong with the fixtures. It never carries a value of a fixture except a vocabulary value (printable) .
type Problem struct {
	Module  modclient.Module
	Route   string
	Case    string
	Fixture string
	Kind    ProblemKind
	Detail  string
}

func (p Problem) String() string {
	who := ""
	if p.Route != "" {
		who = fmt.Sprintf("%s %s", p.Module, p.Route)
		if p.Case != "" {
			who += " case=" + p.Case
		}
		who += " "
	}
	f := ""
	if p.Fixture != "" {
		f = "file=" + p.Fixture + " "
	}
	return fmt.Sprintf("%s%skind=%s: %s", who, f, p.Kind, p.Detail)
}

type contractKey struct {
	module modclient.Module
	route  string
	cas    string
}

// CheckFixtures is the no-rot check on the recorded files of one adapter package: dir is the fixture directory inside fsys (for example "testdata/contract"). It reports, and
// never stops at the first: no contract at all; a contract that cannot be used; two contracts for one route and case; two contracts naming one file; a contract naming a file
// that does not exist; a file in the directory that no contract names; a fixture that is not JSON, that its own Shape refuses, or that holds something secret-shaped; a
// Vocab key that is not a path or names a position the fixture does not have, an empty or repeating vocabulary, and a fixture value outside its own Vocab. An empty list
// means every contract has a usable fixture and every fixture a contract (it does not mean the fixtures match the real module: that is Compare, in the harness).
func CheckFixtures(contracts []modclient.RouteContract, fsys fs.FS, dir string) []Problem {
	if len(contracts) == 0 {
		return []Problem{{Kind: NoContracts, Detail: "found 0 contracts: the instrument matched nothing"}}
	}
	if fsys == nil {
		return []Problem{{Kind: Unreadable, Detail: "no file system was given"}}
	}
	var ps []Problem
	named := map[string]bool{}
	keys := map[contractKey]bool{}
	for _, rc := range contracts {
		p := func(k ProblemKind, format string, a ...any) {
			ps = append(ps, Problem{Module: rc.Module, Route: rc.Route, Case: rc.Case, Fixture: rc.Fixture, Kind: k, Detail: fmt.Sprintf(format, a...)})
		}
		ck := contractKey{rc.Module, rc.Route, rc.Case}
		if keys[ck] {
			p(DuplicateContract, "another contract has the same module, route and case")
		}
		keys[ck] = true
		if rc.Fixture != "" && named[rc.Fixture] {
			p(DuplicateFixture, "another contract names this file")
		}
		named[rc.Fixture] = true
		ps = append(ps, checkContract(rc, fsys, dir)...)
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		ps = append(ps, Problem{Kind: Unreadable, Detail: "the fixture directory cannot be listed: " + errWord(err)})
		return ps
	}
	for _, e := range entries {
		if !named[e.Name()] {
			what := "no contract names this file: a fixture the adapter no longer uses"
			if e.IsDir() {
				what = "a directory in the fixture directory (fixtures are plain files named by a contract)"
			}
			ps = append(ps, Problem{Fixture: e.Name(), Kind: Stray, Detail: what})
		}
	}
	return ps
}

func errWord(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	case errors.Is(err, fs.ErrInvalid):
		return "invalid path"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return "read error"
}

func checkContract(rc modclient.RouteContract, fsys fs.FS, dir string) []Problem {
	var ps []Problem
	p := func(k ProblemKind, format string, a ...any) {
		ps = append(ps, Problem{Module: rc.Module, Route: rc.Route, Case: rc.Case, Fixture: rc.Fixture, Kind: k, Detail: fmt.Sprintf(format, a...)})
	}
	ok := true
	if rc.Module == "" || rc.Route == "" {
		p(BadContract, "the module or the route is empty")
		ok = false
	}
	if rc.Status < 100 || rc.Status > 599 {
		p(BadContract, "the status is not an HTTP status")
		ok = false
	}
	if rc.Fixture == "" || strings.ContainsAny(rc.Fixture, `/\`) || !fs.ValidPath(rc.Fixture) || rc.Fixture == "." {
		p(BadContract, "the fixture name is not a plain file name")
		return ps
	}
	sv, err := viewOf(rc.Shape)
	if err != nil {
		p(BadContract, "the Shape cannot be read: %v", err)
		ok = false
	} else if sv.kind == skUnchecked {
		p(BadContract, "the Shape is the zero Shape, which checks nothing: the contract pins no shape")
		ok = false
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, rc.Fixture))
	if err != nil {
		p(Dangling, "the contract names a file that cannot be read (%s)", errWord(err))
		return ps
	}
	v, err := parse(data)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			p(NotJSON, "the fixture %s", pe.Kind)
		} else {
			p(NotJSON, "the fixture cannot be read")
		}
		return ps
	}
	if ok {
		if derr := modclient.Decode(&modclient.Response{Status: rc.Status, Body: data}, nil, rc.Shape); derr != nil {
			var me *modclient.Error
			if errors.As(derr, &me) {
				p(ShapeRefused, "the adapter's own Shape refuses the fixture at %q: %s", me.Key, me.Message)
			} else {
				p(ShapeRefused, "the adapter's own Shape refuses the fixture")
			}
		}
	}
	finds, _ := FindSecrets(data) // the document parsed above, so there is no error to see
	for _, f := range finds {
		p(Secret, "%s", f)
	}
	ps = append(ps, checkVocab(rc, v, sv)...)
	return ps
}

func checkVocab(rc modclient.RouteContract, v value, sv *sview) []Problem {
	var ps []Problem
	p := func(k ProblemKind, format string, a ...any) {
		ps = append(ps, Problem{Module: rc.Module, Route: rc.Route, Case: rc.Case, Fixture: rc.Fixture, Kind: k, Detail: fmt.Sprintf(format, a...)})
	}
	root := build(v, sv)
	for _, name := range sortedKeys(rc.Vocab) {
		segs, err := parsePath(name)
		if err != nil || len(segs) == 0 {
			p(VocabKey, "the Vocab key %q is not a path of the documented grammar (for example items[].state)", name)
			continue
		}
		if _, st := resolve(root, segs); st == missing {
			p(VocabKey, "the Vocab key %q names no position in the fixture (a typo makes the vocabulary check vacuous)", name)
		}
		list := rc.Vocab[name]
		if len(list) == 0 {
			p(VocabList, "the vocabulary %q is empty", name)
		}
		known := map[string]bool{}
		for _, s := range list {
			if known[s] {
				p(VocabList, "the vocabulary %q lists %s twice", name, printable(s))
			}
			known[s] = true
		}
		var vals []string
		collectStrings(v, segs, &vals)
		reported := map[string]bool{}
		for _, s := range vals {
			if !known[s] && !reported[s] {
				reported[s] = true
				p(VocabOutside, "the fixture holds the value %s at %q, which the contract's Vocab does not list", printable(s), name)
			}
		}
	}
	return ps
}
