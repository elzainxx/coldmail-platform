package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

// `merge_fields` (Phase 6, additive): the names, never the values, of the merge data a job's rows carry. It is always an array, so a client never tests for null, and it is the store's list unchanged.

func TestViews_MergeFieldsIsAlwaysAnArrayOfTheStoresNamesAndNeverAValue(t *testing.T) {
	for name, c := range map[string]struct {
		in   []string
		want []string
	}{
		"a job with names":                 {[]string{"city", "first_name"}, []string{"city", "first_name"}},
		"a job with none (nil)":            {nil, []string{}},
		"a job made before they were kept": {[]string{}, []string{}},
	} {
		j := fullJob()
		j.MergeFields = c.in
		raw, err := json.Marshal(viewJob(j, true))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		arr, ok := m["merge_fields"].([]any)
		if !ok {
			t.Errorf("%s: merge_fields is %v, want an array", name, m["merge_fields"])
			continue
		}
		got := make([]string, len(arr))
		for i, a := range arr {
			got[i], _ = a.(string)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: merge_fields = %v, want %v", name, got, c.want)
		}
	}
	// the list too: a job in a list carries the names (they are not the campaign, which the list leaves out)
	j := fullJob()
	j.MergeFields = []string{"city"}
	raw, _ := json.Marshal(viewJob(j, false))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if got, _ := m["merge_fields"].([]any); len(got) != 1 {
		t.Errorf("a job in a list shows merge_fields %v", m["merge_fields"])
	}
	// and the view never gains a copy of the job's data by sharing the store's slice
	src := []string{"a", "b"}
	j.MergeFields = src
	v := viewJob(j, true)
	v.MergeFields[0] = "changed"
	if src[0] != "a" {
		t.Error("the view shares the store's slice")
	}
}
