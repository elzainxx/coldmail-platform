package contractcheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

const (
	beginPrefix = "==== CONTRACT-FIXTURE BEGIN "
	endMarker   = "==== CONTRACT-FIXTURE END ===="
	// MaxLog is the largest log ParseRecorded reads.
	MaxLog = 256 << 20
)

var (
	reHeader   = regexp.MustCompile(`^==== CONTRACT-FIXTURE BEGIN module=(\S+) route=(.*?) case=(.*?) file=(\S+) status=([0-9]+) ====$`)
	reFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	// A line of a downloaded workflow log: optional `job<TAB>step<TAB>`, then a timestamp and a space. Anchored at the start so that a timestamp inside the body is never
	// taken for a prefix (a body line starts with white space or a quote, and has no tab).
	reLogPrefix = regexp.MustCompile(`^(?:[^\t]*\t[^\t]*\t)?[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?Z `)
	// The decoration `go test` puts before a t.Logf line: four spaces, file:line and a space.
	reTestPrefix = regexp.MustCompile(`^ {4}[A-Za-z0-9_./\\-]+\.go:[0-9]+: `)
)

func headerFieldOK(s string, allowEmpty bool) bool {
	if s == "" {
		return allowEmpty
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func blockHeader(rc modclient.RouteContract, status int) (string, error) {
	bad := func(why string) (string, error) { return "", fmt.Errorf("%w: %s", ErrBadHeader, why) }
	switch {
	case !headerFieldOK(string(rc.Module), false) || strings.Contains(string(rc.Module), " "):
		return bad("the module is empty, holds a space or a control character")
	case !headerFieldOK(rc.Route, false) || strings.Contains(rc.Route, " case="):
		return bad("the route is empty, holds a control character or the text ' case='")
	case !headerFieldOK(rc.Case, true) || strings.Contains(rc.Case, " file="):
		return bad("the case holds a control character or the text ' file='")
	case !reFileName.MatchString(rc.Fixture) || strings.Contains(rc.Fixture, ".."):
		return bad("the fixture name is not a plain file name")
	case status < 100 || status > 599:
		return bad("the status is not an HTTP status")
	}
	return fmt.Sprintf("%smodule=%s route=%s case=%s file=%s status=%d ====", beginPrefix, rc.Module, rc.Route, rc.Case, rc.Fixture, status), nil
}

// Record writes one delimited block to w (the harness log):
//
//	==== CONTRACT-FIXTURE BEGIN module=<m> route=<route> case=<case> file=<fixture> status=<n> ====
//	<the normalised body>
//	==== CONTRACT-FIXTURE END ====
//
// The body is normalised first (Normalise). Record refuses, and writes nothing, when the contract cannot be written into a header unambiguously (ErrBadHeader), when the
// body cannot be read (a *ParseError) and when what is left after normalisation still holds something secret-shaped (ErrSecretShaped; the error names classes and paths,
// never a value).
func Record(w io.Writer, rc modclient.RouteContract, status int, body []byte) error {
	head, err := blockHeader(rc, status)
	if err != nil {
		return err
	}
	norm, err := Normalise(body)
	if err != nil {
		return err
	}
	findings, err := FindSecrets(norm)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		parts := make([]string, len(findings))
		for i, f := range findings {
			parts[i] = f.String()
		}
		return fmt.Errorf("%w: %s", ErrSecretShaped, strings.Join(parts, "; "))
	}
	var blk bytes.Buffer
	blk.WriteString(head + "\n")
	blk.Write(norm)
	blk.WriteString(endMarker + "\n")
	_, err = w.Write(blk.Bytes())
	return err
}

// Recorded is one block read back out of a log.
type Recorded struct {
	Module modclient.Module
	Route  string
	Case   string
	File   string
	Status int
	// Body is exactly what Record printed: the normalised body, ending with one newline.
	Body []byte
}

func stripPrefix(line string) string {
	if loc := reLogPrefix.FindStringIndex(line); loc != nil {
		line = line[loc[1]:]
	}
	if loc := reTestPrefix.FindStringIndex(line); loc != nil {
		line = line[loc[1]:]
	}
	return line
}

// ParseRecorded extracts the blocks Record printed from a log. Lines may be bare, or carry a workflow-log timestamp prefix (`2026-11-17T21:32:01.0030894Z `), the job and step
// prefix of a downloaded log (`harness<TAB>The harness<TAB>` before the timestamp), and the decoration `go test` puts before a t.Logf line (`    file.go:123: `); each is
// stripped from the start of a line, only when it is there. A blank line inside a block is skipped (a body never has one). It refuses a log with no block (ErrNoBlocks), a
// block that is not closed, is nested or has an END without a BEGIN or a header it cannot read (ErrMalformedLog), a body that is not one JSON document (a *ParseError), a
// body that is secret-shaped (ErrSecretShaped), and two blocks for one file with different bodies (ErrConflict); two identical blocks for one file are one. The blocks are
// returned in the order of first appearance.
func ParseRecorded(log []byte) ([]Recorded, error) {
	if len(log) > MaxLog {
		return nil, fmt.Errorf("%w: the log is larger than %d bytes", ErrMalformedLog, MaxLog)
	}
	text := strings.TrimPrefix(string(log), "\xef\xbb\xbf") // a byte-order mark at the start of a downloaded log
	var (
		out     []Recorded
		cur     *Recorded
		body    []string
		byFile  = map[string]int{}
		lineNum int
	)
	malformed := func(why string) error { return fmt.Errorf("%w: line %d: %s", ErrMalformedLog, lineNum, why) }
	for _, raw := range strings.Split(text, "\n") {
		lineNum++
		line := stripPrefix(strings.TrimRight(raw, "\r"))
		marker := strings.TrimRight(line, " ")
		switch {
		case strings.HasPrefix(marker, beginPrefix):
			if cur != nil {
				return nil, malformed("a block begins inside another block")
			}
			m := reHeader.FindStringSubmatch(marker)
			if m == nil {
				return nil, malformed("the block header cannot be read")
			}
			status, _ := strconv.Atoi(m[5])
			if !reFileName.MatchString(m[4]) || strings.Contains(m[4], "..") || status < 100 || status > 599 {
				return nil, malformed("the block header names an unusable file or status")
			}
			cur = &Recorded{Module: modclient.Module(m[1]), Route: m[2], Case: m[3], File: m[4], Status: status}
			body = body[:0]
		case marker == endMarker:
			if cur == nil {
				return nil, malformed("a block ends that never began")
			}
			cur.Body = []byte(strings.Join(body, "\n") + "\n")
			if _, err := parse(cur.Body); err != nil {
				return nil, fmt.Errorf("block for file %s: %w", cur.File, err)
			}
			findings, _ := FindSecrets(cur.Body)
			if len(findings) > 0 {
				return nil, fmt.Errorf("%w: block for file %s: %s", ErrSecretShaped, cur.File, findings[0])
			}
			if i, dup := byFile[cur.File]; dup {
				if !bytes.Equal(out[i].Body, cur.Body) || out[i].Module != cur.Module || out[i].Route != cur.Route || out[i].Case != cur.Case || out[i].Status != cur.Status {
					return nil, fmt.Errorf("%w: file %s is recorded twice with different content", ErrConflict, cur.File)
				}
			} else {
				byFile[cur.File] = len(out)
				out = append(out, *cur)
			}
			cur = nil
		case cur != nil:
			if line != "" {
				body = append(body, line)
			}
		}
	}
	if cur != nil {
		return nil, malformed("the block for file " + cur.File + " is never closed")
	}
	if len(out) == 0 {
		return nil, ErrNoBlocks
	}
	return out, nil
}

// WriteRecorded writes each block to dir/<file> and reports the module and route of each to out ("written", "unchanged" or "overwritten"). A file that exists with
// different content is not touched unless force is set: every block is checked first and nothing is written when any would be refused (ErrConflict).
func WriteRecorded(dir string, recs []Recorded, force bool, out io.Writer) error {
	if len(recs) == 0 {
		return ErrNoBlocks
	}
	type plan struct {
		path   string
		status string
	}
	plans := make([]plan, len(recs))
	for i, r := range recs {
		if !reFileName.MatchString(r.File) || strings.Contains(r.File, "..") || filepath.Base(r.File) != r.File {
			return fmt.Errorf("%w: %q is not a plain file name", ErrBadHeader, r.File)
		}
		p := filepath.Join(dir, r.File)
		plans[i] = plan{path: p, status: "written"}
		old, err := os.ReadFile(p)
		switch {
		case err == nil && bytes.Equal(old, r.Body):
			plans[i].status = "unchanged"
		case err == nil && !force:
			return fmt.Errorf("%w: %s exists with different content (use force to overwrite)", ErrConflict, r.File)
		case err == nil:
			plans[i].status = "overwritten"
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("cannot read %s: %w", r.File, err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, r := range recs {
		if plans[i].status != "unchanged" {
			if err := os.WriteFile(plans[i].path, r.Body, 0o644); err != nil {
				return err
			}
		}
		c := ""
		if r.Case != "" {
			c = " case=" + r.Case
		}
		if _, err := fmt.Fprintf(out, "%s %s %s%s -> %s (%s)\n", plans[i].status, r.Module, r.Route, c, r.File, "status "+strconv.Itoa(r.Status)); err != nil {
			return err
		}
	}
	return nil
}
