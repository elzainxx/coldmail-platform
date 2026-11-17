# Recorded shapes for module 3's adapter

`contract/` holds one response body per route and case that the adapter relies on: the shared four routes and describe (the same names `internal/adapters/sendapi` uses), module 3's
feasibility quote, and the account routes (list, get, create, the test button, bulk add, events). Each file is named by exactly one entry of `m3.Contracts()` and every entry names an existing
file (a test holds both).

These bodies are hand-written from the module's code (`Docs/history/p3-research/m3-api.md`, checked against the module at the pinned commit), to be replaced by a recording from the real
module in the harness (Phase 3 B6). Ids and times are obviously synthetic, addresses are in the documentation ranges, and there is no secret in any file. `accounts.bulk.ok.json` carries one
rejected row whose `code` is a raw internal reason with error text, on purpose: it is what the module can send, and what the adapter must never pass on.

The describe body is trimmed (eight of the eighteen extension routes, one schema stub) because the adapter reads only what `modclient.Describe` reads.
