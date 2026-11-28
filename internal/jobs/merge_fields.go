package jobs

import (
	"encoding/json"
	"sort"

	"github.com/coldmail-platform/platform/internal/render"
)

// THE MERGE FIELD NAMES OF A JOB (Phase 6: Job.MergeFields, `merge_fields` in the job JSON). The names, never the values, of the merge data a job's rows carry: what the authoring screen offers the author
// to write as {{name}}, and what a chained send inherits from the verification list it was made from. They are computed ONCE, when the draft is created, and frozen with the job (platform.jobs.frozen_settings,
// the top-level key `merge_fields`, beside `settings` and `content`), so they are NOT touched by the content retention: the purge clears the VALUES (job_rows.fields) and the template, and the guard
// (platform_0021) lets frozen_settings become only the old document with its template cleared, which keeps this key and every other byte of it.
//
//   - a CSV or JSON draft: the union of the keys of its rows' `fields`;
//   - a chained send: the source job's `merge_fields`, or, when the source has none (a job made before this existed, or one with no merge data), the union of the keys of the source rows read at creation.
//
// WHICH KEYS COUNT. Only a key a template could name: the grammar of internal/render (render.ValidFieldName: a letter or an underscore, then letters, digits and underscores, at most 64 characters), and not
// one of the two names the platform supplies itself (render.IsReserved: `email` and `unsubscribe_url`, which a merge column of the same name never overrides). A column called `First name` is merge data
// and no template can ask for it, so it is not listed. At most MaxMergeFieldNames are kept, sorted, unique.

// MaxMergeFieldNames is the most names a job lists. The union of the keys of the rows of a JSON list can be longer (rows need not share keys): the first MaxMergeFieldNames in byte order are kept.
const MaxMergeFieldNames = 200

// usableMergeName says whether a key of a row's merge data is a name a template could use.
func usableMergeName(k string) bool { return render.ValidFieldName(k) && !render.IsReserved(k) }

// nameSet collects names.
type nameSet map[string]struct{}

func (n nameSet) addKeys(fields map[string]string) {
	for k := range fields {
		if usableMergeName(k) {
			n[k] = struct{}{}
		}
	}
}

// sorted is the names in byte order, at most MaxMergeFieldNames, never nil.
func (n nameSet) sorted() []string {
	out := make([]string, 0, len(n))
	for k := range n {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > MaxMergeFieldNames {
		out = out[:MaxMergeFieldNames]
	}
	return out
}

// mergeFieldsOfRows is the names of a list a user supplied: the union of the keys of every row's fields.
func mergeFieldsOfRows(rows []RowIn) []string {
	n := nameSet{}
	for _, r := range rows {
		n.addKeys(r.Fields)
	}
	return n.sorted()
}

// mergeFieldsOfSource is the names a chained send inherits: the source job's own list when it has one, otherwise the union of the keys of the source rows' merge data (each a JSON object as
// platform.job_rows.fields holds it). A value that is not an object has no keys.
func mergeFieldsOfSource(listed []string, res []sourceResult) []string {
	if len(listed) > 0 {
		return cleanMergeFields(listed)
	}
	n := nameSet{}
	for _, r := range res {
		if r.Fields == "" || r.Fields == "{}" {
			continue
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.Fields), &keys); err != nil {
			continue
		}
		for k := range keys {
			if usableMergeName(k) {
				n[k] = struct{}{}
			}
		}
	}
	return n.sorted()
}

// cleanMergeFields makes a list read back from a frozen document what the store would have written: only usable names, unique, sorted, at most MaxMergeFieldNames (a document this store did not write is
// not trusted to hold a clean list).
func cleanMergeFields(names []string) []string {
	n := nameSet{}
	for _, k := range names {
		if usableMergeName(k) {
			n[k] = struct{}{}
		}
	}
	return n.sorted()
}
