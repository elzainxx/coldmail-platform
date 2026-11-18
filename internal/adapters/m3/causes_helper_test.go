package m3_test

import "github.com/coldmail-platform/platform/internal/adapters/sendapi"

// causeWords is the closed set of causes of a run refusal as the words of describe's vocabulary run_conflict_cause.
func causeWords() []string {
	var out []string
	for _, c := range sendapi.RunCauses() {
		out = append(out, string(c))
	}
	return out
}
