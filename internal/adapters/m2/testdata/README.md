# Module 2 recorded shapes

Every file under `contract/` is a response body of module 2 (`coldmail-send-managed` at the pin in `tools/modules.lock`), one file per route and per response whose shape differs. They are
hand-written from the module's code (the Phase 3 research `Docs/history/p3-research/m2-api-and-worker.md` and the handlers it cites), to be replaced by a recording from the real module in the
harness (Phase 3 B6). Ids and times are obviously synthetic and there is no secret in any of them.

`m2.Contracts()` names each file once (`Route`, `Case`, `Fixture`, `Status`, the `Shape` the adapter decodes it with and the `Vocab` it maps from). The tests serve each file from an `httptest`
server and decode it through the adapter's real code, refuse a file no contract names and a contract whose file is missing, and refuse mutated copies (a required key removed, a key's type
changed, a state outside the vocabulary).
