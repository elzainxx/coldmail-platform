package modclient

import (
	"errors"
	"strings"
	"testing"
)

func respOf(body string) *Response {
	return &Response{Status: 200, Body: []byte(body), RequestID: m2ID, Attempts: 1, module: M2, route: "m2.report"}
}

// The shapes below are the ones an adapter would write for module 2's two report forms (research m2-api-and-worker 2.3 and 2.4): the single report OMITS a key until it is set and
// always carries `reason` and `error_class` (null when there is nothing to say); the campaign report's items carry every key, null when unset.
var (
	singleReport = Object(
		Req("job_id", Str), Req("state", Str), Req("attempt_count", Int),
		Req("reason", Str.OrNull()), Req("error_class", Str.OrNull()),
		Opt("provider_message_id", Str), Opt("completed_at", Str),
	)
	reportItem = Object(
		Req("job_item_id", Str), Req("state", Str), Req("provider_message_id", Str.OrNull()), Req("completed_at", Str.OrNull()),
		Req("attempt_count", Int), Req("reason", Str.OrNull()), Req("error_class", Str.OrNull()),
	)
	campaignReport = Object(
		Req("campaign_id", Str), Req("dispatch_paused", Bool),
		Req("totals", Object(Req("total", Int), Req("sent", Int), Req("bounced", Int), Req("failed", Int), Req("in_flight", Int), Req("pending", Int), Req("waiting_for_capacity", Int), Req("cancelled", Int))),
		Req("progress", Object(Req("terminal", Int), Req("fraction_complete", Num), Req("is_complete", Bool))),
		Req("items", Object(Req("results", ArrayOf(reportItem)), Req("next_cursor", Str.OrNull()), Req("page_size", Int))),
	)
)

const (
	singleOK   = `{"job_id":"j-1","state":"sent","attempt_count":0,"reason":null,"error_class":null,"provider_message_id":"<id@m>","completed_at":"2026-11-17T10:00:00.123456Z"}`
	campaignOK = `{"campaign_id":"c-1","dispatch_paused":false,
 "totals":{"total":3,"sent":1,"bounced":1,"failed":0,"in_flight":0,"pending":1,"waiting_for_capacity":0,"cancelled":0},
 "progress":{"terminal":2,"fraction_complete":0.6666666666666666,"is_complete":false},
 "items":{"results":[
  {"job_item_id":"i-1","state":"sent","provider_message_id":"<id@m>","completed_at":"2026-11-17T10:00:00Z","attempt_count":0,"reason":null,"error_class":null},
  {"job_item_id":"i-2","state":"bounced","provider_message_id":null,"completed_at":"2026-11-17T10:00:03Z","attempt_count":0,"reason":"bounce_permanent","error_class":"bounce_permanent"}],
  "next_cursor":null,"page_size":100}}`
)

func TestShape_ABrokenDocumentIsAContractErrorNamingTheRouteAndTheKey(t *testing.T) {
	rows := []struct {
		name  string
		shape Shape
		body  string
		key   string // "" = the document conforms (the paired control)
		why   string // a phrase of the message
	}{
		{"a conforming single report", singleReport, singleOK, "", ""},
		{"a conforming single report with a key nobody knows", singleReport, strings.Replace(singleOK, `"state"`, `"new_field":{"a":[1,2]},"state"`, 1), "", ""},
		{"a conforming campaign report", campaignReport, campaignOK, "", ""},
		{"a required key missing", singleReport, `{"job_id":"j-1","attempt_count":0,"reason":null,"error_class":null}`, "state", "is missing"},
		{"a required key of a string type holding a number", singleReport, `{"job_id":"j-1","state":7,"attempt_count":0,"reason":null,"error_class":null}`, "state", "is a number, want string"},
		{"an integer sent as a string", singleReport, `{"job_id":"j-1","state":"sent","attempt_count":"0","reason":null,"error_class":null}`, "attempt_count", "is a string, want integer"},
		{"an integer sent as a fraction", singleReport, `{"job_id":"j-1","state":"sent","attempt_count":1.5,"reason":null,"error_class":null}`, "attempt_count", "not an integer"},
		{"an integer sent as an exponent", singleReport, `{"job_id":"j-1","state":"sent","attempt_count":1e0,"reason":null,"error_class":null}`, "attempt_count", "not an integer"},
		{"an integer too large for 64 bits", singleReport, `{"job_id":"j-1","state":"sent","attempt_count":99999999999999999999,"reason":null,"error_class":null}`, "attempt_count", "not an integer"},
		{"a key that must be present but is omitted (reason)", singleReport, `{"job_id":"j-1","state":"sent","attempt_count":0,"error_class":null}`, "reason", "is missing"},
		{"null where null is not allowed", singleReport, `{"job_id":null,"state":"sent","attempt_count":0,"reason":null,"error_class":null}`, "job_id", "is null, want string"},
		{"an optional key that is null where it may only be omitted", singleReport, `{"job_id":"j","state":"sent","attempt_count":0,"reason":null,"error_class":null,"provider_message_id":null}`, "provider_message_id", "is null"},
		{"a nested key missing", campaignReport, strings.Replace(campaignOK, `"sent":1,`, ``, 1), "totals.sent", "is missing"},
		{"a nested key of the wrong type", campaignReport, strings.Replace(campaignOK, `"is_complete":false`, `"is_complete":"no"`, 1), "progress.is_complete", "is a string, want boolean"},
		{"an array element missing a key (named by index)", campaignReport, strings.Replace(campaignOK, `{"job_item_id":"i-2","state":"bounced",`, `{"job_item_id":"i-2",`, 1), "items.results[1].state", "is missing"},
		{"an array element of the wrong type", campaignReport, strings.Replace(campaignOK, `"attempt_count":0,"reason":"bounce_permanent"`, `"attempt_count":false,"reason":"bounce_permanent"`, 1), "items.results[1].attempt_count", "is a boolean, want integer"},
		{"an array where an array of objects is wanted holds a string", campaignReport, strings.Replace(campaignOK, `"results":[`, `"results":["x",`, 1), "items.results[0]", "is a string, want object"},
		{"an object where an array is wanted", campaignReport, `{"campaign_id":"c","dispatch_paused":false,"totals":{"total":0,"sent":0,"bounced":0,"failed":0,"in_flight":0,"pending":0,"waiting_for_capacity":0,"cancelled":0},"progress":{"terminal":0,"fraction_complete":0,"is_complete":false},"items":{"results":{},"next_cursor":null,"page_size":100}}`, "items.results", "is a object, want array"},
		{"a next_cursor that is a number", campaignReport, strings.Replace(campaignOK, `"next_cursor":null`, `"next_cursor":5`, 1), "items.next_cursor", "is a number, want string"},
		{"a top-level array where an object is wanted", singleReport, `[{"job_id":"j-1"}]`, "(body)", "is a array, want object"},
		{"a top-level string", singleReport, `"x"`, "(body)", "is a string, want object"},
		{"a top-level null", singleReport, `null`, "(body)", "is null, want object"},
		{"a map whose value has the wrong type", Object(Req("checks", MapOf(Bool))), `{"checks":{"db":true,"singleton_lock":"yes"}}`, "checks.singleton_lock", "is a string, want boolean"},
		{"a map key outside the safe alphabet is not echoed", Object(Req("checks", MapOf(Bool))), `{"checks":{"evil key é; DROP":1}}`, "checks.evil?key?????DROP", "want boolean"},
		{"Any is present but not null", Object(Req("x", Any)), `{"x":null}`, "x", "is null"},
		{"Num refuses a string", Object(Req("x", Num)), `{"x":"0.5"}`, "x", "is a string, want number"},
		{"a top-level array shape refuses an object", ArrayOf(Str), `{}`, "(body)", "is a object, want array"},
		{"a top-level array element of the wrong type", ArrayOf(Str), `["a",2]`, "[1]", "is a number, want string"},
	}
	if len(rows) < 25 {
		t.Fatal("the table shrank")
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var sink any
			err := Decode(respOf(row.body), &sink, row.shape)
			if row.key == "" { // the paired control
				if err != nil || sink == nil {
					t.Fatalf("a conforming document was refused: %v", err)
				}
				return
			}
			if sink != nil {
				t.Fatalf("the target was written on an error: %v", sink)
			}
			e := wantKind(t, err, Contract)
			if e.Key != row.key || e.Route != "m2.report" || e.Module != M2 || e.RequestID != m2ID || e.Status != 200 {
				t.Fatalf("error = %+v", *e)
			}
			if !strings.Contains(e.Message, row.why) || !strings.Contains(err.Error(), "m2.report") || !strings.Contains(err.Error(), row.key) {
				t.Fatalf("message %q / text %q: want the route, the key %q and %q", e.Message, err.Error(), row.key, row.why)
			}
			if strings.ContainsAny(e.Message, "é;") || strings.Contains(e.Message, "evil key") {
				t.Fatalf("a key the module chose reached the message raw: %s", e.Message)
			}
		})
	}

	t.Run("several violations are all named, the first is the Key, a flood is summarised", func(t *testing.T) {
		shape := Object(Req("a", Str), Req("b", Int), Req("c", Bool), Req("d", Str), Req("e", Str), Req("f", Str), Req("g", Str), Req("h", Str))
		err := Decode(respOf(`{}`), nil, shape)
		e := wantKind(t, err, Contract)
		if e.Key != "a" || !strings.Contains(e.Message, `"a" is missing`) || !strings.Contains(e.Message, `"e" is missing`) || !strings.Contains(e.Message, "and 3 more") {
			t.Fatalf("%+v", *e)
		}
	})

	t.Run("the zero Shape checks nothing but the document", func(t *testing.T) {
		if err := Decode(respOf(`{"anything":[1,{"x":null}]}`), nil, Shape{}); err != nil {
			t.Fatal(err)
		}
		if err := Decode(respOf(`[1,2,3]`), nil, Shape{}); err != nil {
			t.Fatal(err)
		}
		wantKind(t, Decode(respOf(`{"a":`), nil, Shape{}), Contract)
	})
}

func TestShape_OmittedUntilSetAndNullWhenUnsetAreDifferentConventions(t *testing.T) {
	// module 2's single report omits provider_message_id until it is set; the campaign item carries it as null. The two shapes accept their own and refuse the other's.
	omitted := `{"job_id":"j","state":"queued","attempt_count":0,"reason":"waiting_for_capacity","error_class":null}`
	nulled := `{"job_item_id":"i","state":"queued","provider_message_id":null,"completed_at":null,"attempt_count":0,"reason":"waiting_for_capacity","error_class":null}`
	if err := Decode(respOf(omitted), nil, singleReport); err != nil {
		t.Fatalf("the single report's own convention was refused: %v", err)
	}
	if err := Decode(respOf(nulled), nil, reportItem); err != nil {
		t.Fatalf("the campaign item's own convention was refused: %v", err)
	}
	// the single report's shape does not accept `null` for an omit-until-set key, and the item's shape does not accept an omitted key
	if e := wantKind(t, Decode(respOf(strings.Replace(omitted, `"error_class":null`, `"error_class":null,"completed_at":null`, 1)), nil, singleReport), Contract); e.Key != "completed_at" {
		t.Fatalf("key %q", e.Key)
	}
	if e := wantKind(t, Decode(respOf(`{"job_item_id":"i","state":"queued","attempt_count":0,"reason":null,"error_class":null}`), nil, reportItem), Contract); e.Key != "provider_message_id" {
		t.Fatalf("key %q", e.Key)
	}
}

func TestDecode_LeavesTheTargetUntouchedOnEveryError(t *testing.T) {
	type single struct {
		JobID    string `json:"job_id"`
		State    string `json:"state"`
		Attempts int    `json:"attempt_count"`
	}
	sentinel := single{"SENTINEL", "SENTINEL", -1}
	docs := []struct {
		name  string
		body  string
		shape Shape
	}{
		{"a missing key", `{"job_id":"j-1","attempt_count":0}`, singleReport},
		{"a wrong type", `{"job_id":"j-1","state":"sent","attempt_count":"0","reason":null,"error_class":null}`, singleReport},
		{"a value that does not fit the Go type (no shape: only the unmarshal can tell)", `{"job_id":"j-1","state":"sent","attempt_count":"zero"}`, Shape{}},
		{"a truncated document", `{"job_id":"j-1","state":"sent","att`, singleReport},
		{"trailing data", `{"job_id":"j-1","state":"sent","attempt_count":0,"reason":null,"error_class":null} x`, singleReport},
		{"not JSON", `<html>`, singleReport},
		{"an empty body", ``, singleReport},
		{"whitespace", "  \n", singleReport},
	}
	for _, d := range docs {
		t.Run(d.name, func(t *testing.T) {
			v := sentinel
			err := Decode(respOf(d.body), &v, d.shape)
			wantKind(t, err, Contract)
			if v != sentinel {
				t.Fatalf("the target was modified on an error: %+v", v)
			}
		})
	}
	// the paired control: a conforming document does overwrite the target, and ignores the keys the type does not name
	v := sentinel
	if err := Decode(respOf(singleOK), &v, singleReport); err != nil || v != (single{"j-1", "sent", 0}) {
		t.Fatalf("control failed: %+v, %v", v, err)
	}
}

func TestDecode_RefusesWhatIsNotADecodableTarget(t *testing.T) {
	type single struct {
		JobID string `json:"job_id"`
	}
	good := respOf(`{"job_id":"j"}`)
	if e := wantKind(t, Decode(nil, &single{}, Shape{}), Invalid); e.Message == "" {
		t.Fatal("no message")
	}
	for name, target := range map[string]any{"a non-pointer": single{}, "a nil pointer": (*single)(nil)} {
		if e := wantKind(t, Decode(good, target, Shape{}), Invalid); e.Route != "m2.report" {
			t.Fatalf("%s: %+v", name, *e)
		}
	}
	// A nil target means "check, do not copy"; the control is a real pointer.
	if err := Decode(good, nil, Object(Req("job_id", Str))); err != nil {
		t.Fatal(err)
	}
	var s single
	if err := Decode(good, &s, Object(Req("job_id", Str))); err != nil || s.JobID != "j" {
		t.Fatalf("%+v, %v", s, err)
	}
	var target any
	if err := Decode(good, &target, Shape{}); err != nil || target == nil {
		t.Fatalf("decoding into an interface: %+v, %v", target, err)
	}
	if errors.Is(Decode(nil, nil, Shape{}), ErrContract) {
		t.Fatal("a nil response is not a contract violation of the module")
	}
}
