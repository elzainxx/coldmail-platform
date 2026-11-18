package contractcheck

import (
	"errors"
	"strconv"
)

// ParseKind is why a body could not be read as one JSON document. Every refusal of the parser is a *ParseError with exactly one of these, so a caller (and a test) asserts the
// reason and not only that something failed.
type ParseKind uint8

const (
	// ParseEmpty: nothing but white space.
	ParseEmpty ParseKind = iota + 1
	// ParseTooLarge: more than MaxBody bytes.
	ParseTooLarge
	// ParseInvalidUTF8: the bytes are not valid UTF-8 (the standard decoder would silently replace them, and a checker must not).
	ParseInvalidUTF8
	// ParseSyntax: not JSON, or truncated.
	ParseSyntax
	// ParseTooDeep: containers nested deeper than MaxDepth.
	ParseTooDeep
	// ParseDuplicateKey: an object names the same key twice (the decoder the adapters use keeps the last, so what the module meant cannot be known).
	ParseDuplicateKey
	// ParseTrailingData: a complete document followed by more.
	ParseTrailingData
)

func (k ParseKind) String() string {
	switch k {
	case ParseEmpty:
		return "is empty"
	case ParseTooLarge:
		return "is larger than the size limit"
	case ParseInvalidUTF8:
		return "is not valid UTF-8"
	case ParseSyntax:
		return "is not one complete JSON document"
	case ParseTooDeep:
		return "nests deeper than the depth limit"
	case ParseDuplicateKey:
		return "names a key twice"
	case ParseTrailingData:
		return "has data after the JSON document"
	}
	return "is unreadable (" + strconv.Itoa(int(k)) + ")"
}

// ParseError is the typed refusal of the parser. It carries the kind and, where one applies, the path of the position; never a byte of the body.
type ParseError struct {
	Kind ParseKind
	// Path is the position in the document grammar of this package ("" for the document itself).
	Path string
}

func (e *ParseError) Error() string {
	s := "contractcheck: the body " + e.Kind.String()
	if e.Path != "" {
		s += " (at " + e.Path + ")"
	}
	return s
}

// ParseKindOf returns the ParseKind of err, or 0 when err is not (and does not wrap) a *ParseError.
func ParseKindOf(err error) ParseKind {
	var pe *ParseError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return 0
}

// The sentinels the other operations wrap (errors.Is).
var (
	// ErrSecretShaped: a body holds something that looks like a credential or personal detail; the repository is public, so it is refused.
	ErrSecretShaped = errors.New("contractcheck: the body holds something secret-shaped")
	// ErrBadHeader: a contract's module, route, case, fixture name or status cannot be written into (or read back from) a recorded block unambiguously.
	ErrBadHeader = errors.New("contractcheck: the contract cannot be written into a recorded block")
	// ErrNoBlocks: a log with no recorded block at all (a gate whose subject is empty is red).
	ErrNoBlocks = errors.New("contractcheck: the log holds no recorded fixture block")
	// ErrMalformedLog: a block that is not closed, nested, orphaned or whose header cannot be read.
	ErrMalformedLog = errors.New("contractcheck: the log holds a malformed fixture block")
	// ErrConflict: two blocks for one file with different bodies, or a file on disk that would be overwritten with different content.
	ErrConflict = errors.New("contractcheck: a fixture file would receive two different bodies")
	// ErrTooManyAddresses: a body holds more distinct IPv4 addresses than the three documentation ranges have hosts (MaxDistinctAddresses), so Normalise cannot rewrite them.
	ErrTooManyAddresses = errors.New("contractcheck: the body holds more distinct IPv4 addresses than the documentation ranges can stand for")
	// ErrShapeUnreadable: the contract's Shape could not be walked (modclient changed the internals this package reads).
	ErrShapeUnreadable = errors.New("contractcheck: the Shape of a contract cannot be read")
)
