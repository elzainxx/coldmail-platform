package config

import "strings"

// REMOVED VARIABLES. A dial that is removed is removed everywhere: the variable, its field, its parse, its documentation row, the compose files and the tests that set it. What stays is this list,
// and it exists for one reason: an operator who upgrades with the old variable still in the deployment's environment must be TOLD it no longer does anything, by name, at start-up, instead of
// the setting being silently accepted (a dial that looks live and is not is how a customer ends up reserved at a rate nobody chose). Load never reads a removed variable: it cannot fail the
// start-up (an old value that was once invalid is not a configuration error now) and it changes nothing in the Config; SetRemoved is what internal/app asks, once, to log one warning each.
//
// This file is the ONE place in the tree (besides the migration that once created the flag, the configuration reference's "Removed variables" table and the phase records) that may name a removed
// variable: TestRemovedVariables_NoTrackedFileNamesOneOutsideItsRecords fails when any other tracked file does.

// RemovedVar is one variable an earlier version read and no version reads any more.
type RemovedVar struct {
	// Name is the variable's name.
	Name string
	// Instead says, in one sentence, what replaced it (printed in the start-up warning and in the configuration reference).
	Instead string
}

// removedVars is the list, oldest removal first. Add an entry when a variable is removed; never delete one while a deployment may still carry it.
var removedVars = []RemovedVar{
	{
		Name:    "PLATFORM_M1_VENDOR_REACHABLE",
		Instead: "module 1 says whether its vendor fallback is reachable (GET /v1/describe, vendor_fallback.reachable) and the platform reads it at every module 1 preview and confirm; switch module 1's own TM_EXTERNAL_* dials, nothing on the platform",
	},
}

// RemovedVars returns a copy of the list of removed variables.
func RemovedVars() []RemovedVar { return append([]RemovedVar(nil), removedVars...) }

// SetRemoved returns the removed variables that getenv holds a non-blank value for, in the order of the list. It reads no value into anything and returns none: the caller learns that a name
// is set, never what it was set to (the value may be a secret that an operator pasted into the wrong line). A nil getenv means an empty environment.
func SetRemoved(getenv func(string) string) []RemovedVar {
	if getenv == nil {
		return nil
	}
	var out []RemovedVar
	for _, r := range removedVars {
		if v := getenv(r.Name); strings.TrimSpace(v) != "" {
			out = append(out, r)
		}
	}
	return out
}
