//go:build harness

package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- wait ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// until is what `wait` waits for.
type until struct {
	kind string          // terminal, running or any
	set  map[string]bool // for any: the statuses
}

// parseUntil reads --until: `terminal`, `running`, or `any:<status>[,<status>]`.
func parseUntil(spec string) (until, error) {
	switch {
	case spec == "terminal" || spec == "running":
		return until{kind: spec}, nil
	case strings.HasPrefix(spec, "any:"):
		u := until{kind: "any", set: map[string]bool{}}
		for _, s := range strings.Split(strings.TrimPrefix(spec, "any:"), ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				return until{}, failf(exitUsage, "--until any: names an empty status")
			}
			u.set[s] = true
		}
		return u, nil
	}
	return until{}, failf(exitUsage, "--until is terminal, running or any:<status>[,<status>] (got %q)", spec)
}

// pastRunning says whether a job has gone beyond the status running (so a wait for running that has not seen it never will).
func pastRunning(status string) bool {
	switch status {
	case "cancelling", "completing", "succeeded", "failed", "cancelled", "expired":
		return true
	}
	return false
}

func (u until) met(status string) bool {
	switch u.kind {
	case "terminal":
		return terminal(status)
	case "running":
		return status == "running"
	}
	return u.set[status]
}

// hopeless says whether the wait can no longer be met because the job went somewhere it cannot come back from.
func (u until) hopeless(status string) (string, bool) {
	switch u.kind {
	case "running":
		if pastRunning(status) {
			return fmt.Sprintf("the job is already %s and was never seen running: it ran faster than the poll (use a longer list), or it was started and ended before the wait began", status), true
		}
	case "any":
		if terminal(status) && !u.set[status] {
			return fmt.Sprintf("the job ended %s, which is none of the statuses waited for", status), true
		}
	}
	return "", false
}

func cmdWait(args []string) error {
	fs := flags("wait")
	var job, spec string
	var timeout, pollMS, minResults int
	var allStamped bool
	fs.StringVar(&job, "job", "", "the job")
	fs.StringVar(&spec, "until", "terminal", "terminal, running or any:<status>[,<status>]")
	fs.IntVar(&minResults, "min-results", 0, "also wait until the engine has recorded at least this many results of module 1's answers")
	fs.BoolVar(&allStamped, "all-stamped", false, "also wait until every pending row has been handed to the module (submitted_at set): a pause or a cancel then lands on a job the module holds whole, never on a job whose engine is still between two batches")
	fs.IntVar(&timeout, "timeout", 300, "seconds to wait")
	fs.IntVar(&pollMS, "poll-ms", 200, "milliseconds between two reads of the job")
	if err := parse(fs, args); err != nil {
		return err
	}
	if job == "" {
		return failf(exitUsage, "--job is required")
	}
	if timeout < 1 || pollMS < 20 {
		return failf(exitUsage, "--timeout is at least 1 second and --poll-ms at least 20")
	}
	want, err := parseUntil(spec)
	if err != nil {
		return err
	}
	ctx, cancel := ctxFor(time.Duration(timeout)*time.Second + time.Minute)
	defer cancel()
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	start := time.Now()
	deadline := start.Add(time.Duration(timeout) * time.Second)
	last := ""
	for {
		h, err := d.head(ctx, job)
		if err != nil {
			return err
		}
		last = h.Status
		if want.met(last) {
			enough := minResults <= 0
			if !enough {
				var n int
				if err := d.c.QueryRow(ctx, `SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND origin = 'module'`, job).Scan(&n); err != nil {
					return d.scrub(err)
				}
				enough = n >= minResults
			}
			if enough && allStamped {
				var unstamped int
				if err := d.c.QueryRow(ctx, `SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending' AND submitted_at IS NULL`, job).Scan(&unstamped); err != nil {
					return d.scrub(err)
				}
				enough = unstamped == 0
			}
			if enough {
				break
			}
		}
		if why, bad := want.hopeless(last); bad {
			evs, _ := d.events(ctx, job)
			printEvents(evs)
			return failf(exitFail, "waiting for %s: %s", spec, why)
		}
		if time.Now().After(deadline) {
			evs, _ := d.events(ctx, job)
			printEvents(evs)
			return failf(exitFail, "waited %d s for %s and the job is still %s", timeout, spec, last)
		}
		time.Sleep(time.Duration(pollMS) * time.Millisecond)
	}
	evs, err := d.events(ctx, job)
	if err != nil {
		return err
	}
	printEvents(evs)
	fact("waited", "job", job, "until", spec, "status", last, "seconds", time.Since(start).Round(100*time.Millisecond).Seconds())
	return nil
}

// ---- state -----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

// A condition is `name<op>value` over the fields of the state line; the operators are =, !=, >, >=, < and <=. Numbers compare as numbers, anything else (a status, the module's reference) only with = and !=.
type condition struct{ name, op, value string }

var ops = []string{">=", "<=", "!=", "=", ">", "<"}

func parseConditions(spec string) ([]condition, error) {
	var out []condition
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexAny(part, "=!<>")
		if i <= 0 {
			return nil, failf(exitUsage, "the condition %q has no name before its operator", part)
		}
		var found string
		for _, op := range ops {
			if strings.HasPrefix(part[i:], op) {
				found = op
				break
			}
		}
		if found == "" {
			return nil, failf(exitUsage, "the condition %q has no operator (=, !=, >, >=, < or <=)", part)
		}
		out = append(out, condition{name: strings.TrimSpace(part[:i]), op: found, value: strings.TrimSpace(part[i+len(found):])})
	}
	return out, nil
}

// holds judges one condition against the state; an unknown name is an error, never a pass.
func (c condition) holds(s snapshot) (bool, string, error) {
	got, ok := s.get(c.name)
	if !ok {
		return false, "", failf(exitUsage, "the state has no field %q", c.name)
	}
	want := c.value
	if strings.HasPrefix(want, "@") { // @pending: the value of another field of the same state
		v, ok := s.get(want[1:])
		if !ok {
			return false, got, failf(exitUsage, "the state has no field %q to compare %s with", want[1:], c.name)
		}
		want = v
	}
	a, aerr := strconv.ParseInt(got, 10, 64)
	b, berr := strconv.ParseInt(want, 10, 64)
	if aerr == nil && berr == nil {
		var r bool
		switch c.op {
		case "=":
			r = a == b
		case "!=":
			r = a != b
		case ">":
			r = a > b
		case ">=":
			r = a >= b
		case "<":
			r = a < b
		case "<=":
			r = a <= b
		}
		return r, got, nil
	}
	switch c.op {
	case "=":
		return got == want, got, nil
	case "!=":
		return got != want, got, nil
	}
	return false, got, failf(exitUsage, "the condition %s%s%s compares text with an ordering operator", c.name, c.op, c.value)
}

// atNote says what an @field reference was worth, for the message of a condition that does not hold.
func atNote(c condition, s snapshot) string {
	if !strings.HasPrefix(c.value, "@") {
		return ""
	}
	v, _ := s.get(c.value[1:])
	return ", " + c.value[1:] + " is " + v
}

func cmdState(args []string) error {
	fs := flags("state")
	var job, expect string
	var report bool
	fs.StringVar(&job, "job", "", "the job")
	fs.StringVar(&expect, "expect", "", "conditions that must hold, comma separated: name=value, name>n, ...")
	fs.BoolVar(&report, "report", false, "a send job: also read the module's own campaign report (the module must answer)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if job == "" {
		return failf(exitUsage, "--job is required")
	}
	conds, err := parseConditions(expect)
	if err != nil {
		return err
	}
	ctx, cancel := ctxFor(2 * time.Minute)
	defer cancel()
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	s, err := d.snapshotOf(ctx, job)
	if err != nil {
		return err
	}
	switch {
	case s.Head.Module != "m1":
		if s, err = d.sendSnapshot(ctx, s, report); err != nil { // send.go: the module's own tables, the sink and (with --report) the module's report
			return err
		}
	case report:
		return failf(exitUsage, "--report reads a send job's campaign report; job %s is a module 1 job", job)
	}
	pairs := []any{"job", job}
	for _, p := range s.Vals {
		pairs = append(pairs, p.Name, p.Value)
	}
	fact("state", pairs...)
	var unmet []string
	for _, c := range conds {
		ok, got, err := c.holds(s)
		if err != nil {
			return err
		}
		if !ok {
			unmet = append(unmet, fmt.Sprintf("%s%s%s (it is %s%s)", c.name, c.op, c.value, got, atNote(c, s)))
		}
	}
	if len(unmet) > 0 {
		return failf(exitFail, "the state of job %s does not hold: %s", job, strings.Join(unmet, "; "))
	}
	if len(conds) > 0 {
		fact("state_holds", "job", job, "conditions", len(conds))
	}
	return nil
}

// ---- leader ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func cmdLeader(args []string) error {
	fs := flags("leader")
	if err := parse(fs, args); err != nil {
		return err
	}
	ctx, cancel := ctxFor(time.Minute)
	defer cancel()
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	names, n, err := d.holders(ctx)
	if err != nil {
		return err
	}
	fact("leader", "holders", joinNames(names), "locks", n)
	return nil
}
