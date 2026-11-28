package jobs

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// rowBatch is how many rows one INSERT statement carries. The statement takes its columns as arrays (nine parameters whatever the batch), so this bounds only the size of one message and of
// one statement's work, and lets a duplicate name a row of an earlier batch.
const rowBatch = 5000

// CreateDraft makes a job in status draft with its rows, in ONE transaction: the job, its `created` event (actor: the user) and every row, classified once.
//
// The request. Module is m1, m2 or m3 and active in platform.modules. A send job (m2, m3) carries Content, valid (validateContent) and, in a store with a template renderer (WithPreparer), written in the
// template language (a syntax error is a *TemplateError, an ErrBadRequest that names the part, the line and the column); a verify job (m1) carries none. The list is Rows, between
// 1 and the store's limit (ErrBadRequest for none, ErrTooManyRows for more); each row's merge data is size-capped (64 keys, 4 KiB). For a send chained to a verify job (SourceJobID) the list
// is NOT supplied (Rows must be empty): the source must be the user's own succeeded m1 job, and the rows are its module results (origin 'module'), one row per result in the source's order,
// pending when the verdict is selected (valid and risky always, unknown when Settings.UnknownChoice is include, invalid never) and excluded (reason: the verdict) when not. A source that is
// not the user's, or does not exist, is the same ErrBadRequest.
//
// The classification (what job_rows.state, reason, duplicate_of and email_norm hold): norm.Validate refuses the address -> malformed, reason the norm word (never the address; too_long and
// invalid_text for text that cannot be stored); a second row whose normalised address equals an earlier PENDING row's -> duplicate, duplicate_of that row's id; everything else pending.
// Suppression is NOT applied here: the user's suppression list changes between a draft and its confirm, so it is counted by Preview and applied by Confirm.
func (s *Store) CreateDraft(ctx context.Context, userID string, req CreateReq) (Job, error) {
	if err := checkUser(userID); err != nil {
		return Job{}, opErr("create", err)
	}
	chained := req.SourceJobID != ""
	if err := s.validateCreate(req, chained); err != nil {
		return Job{}, opErr("create", err)
	}
	settings, err := normaliseSettings(req.Settings, chained)
	if err != nil {
		return Job{}, opErr("create", err)
	}
	var content *Content
	if req.Module != ModuleVerify {
		if content, err = validateContent(req.Content); err != nil {
			return Job{}, opErr("create", err)
		}
		// the templates are compiled HERE, before anything is written: a mistake of syntax is a 400 that names the part, the line, the column and the message (template.go). This is the one place a send
		// job gets its content (no other path replaces it: a draft's content is frozen), and any path that comes to replace it must call checkTemplates too.
		if err := s.checkTemplates(content); err != nil {
			return Job{}, opErr("create", err)
		}
	}
	// the frozen settings are made INSIDE the transaction below (freezeVendorChoice): a module 1 job reads the user's vendor opt-out there, once, and freezes it with the other choices

	// the supplied list is classified before the transaction opens: it is pure work and holds nothing
	var rows []classified
	if !chained {
		if rows, err = classifyRows(req.Rows, newUUID); err != nil {
			return Job{}, opErr("create", err)
		}
	}

	var out Job
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT active FROM platform.modules WHERE id = $1`, req.Module).Scan(&active); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return badRequest("module %q does not exist", req.Module)
			}
			return s.fail(err)
		}
		if !active {
			return badRequest("module %q is not active", req.Module)
		}
		// the names of the merge data, once, for ever (merge_fields.go): the union of the keys of the supplied rows, or, for a chained send, the source job's own list (its rows' keys when it has none)
		mergeFields := mergeFieldsOfRows(req.Rows)
		if chained {
			res, listed, err := s.chainSource(ctx, tx, userID, req.SourceJobID)
			if err != nil {
				return err
			}
			rows = classifyChain(res, settings.UnknownChoice, newUUID)
			mergeFields = mergeFieldsOfSource(listed, res)
		}

		// Phase 5 S6: the one frozen choice that is not the caller's. A module 1 job reads the user's vendor opt-out NOW, once (never trusting the request), and freezes it; any other module never reads it
		frozen, _, err := freezeVendorChoice(ctx, tx, userID, req.Module, settings, content, mergeFields)
		if err != nil {
			return s.fail(err)
		}
		var jobID string
		if err := tx.QueryRow(ctx, `INSERT INTO platform.jobs (user_id, module_id, source_job_id, frozen_settings)
			VALUES ($1::uuid, $2, NULLIF($3, '')::uuid, $4::jsonb) RETURNING id::text`, userID, req.Module, req.SourceJobID, string(frozen)).Scan(&jobID); err != nil {
			return s.fail(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.job_events (job_id, kind, actor_kind, acting_user_id, reason)
			VALUES ($1::uuid, 'created', 'user', $2::uuid, 'draft created')`, jobID, userID); err != nil {
			return s.fail(err)
		}
		for start := 0; start < len(rows); start += rowBatch {
			if err := s.insertRows(ctx, tx, jobID, userID, rows[start:min(start+rowBatch, len(rows))]); err != nil {
				return err
			}
		}
		j, err := s.getJob(ctx, tx, userID, jobID, false)
		if err != nil {
			return err
		}
		out = j
		return nil
	})
	if err != nil {
		s.log.Info("draft refused", "reason", refusalWord(err))
		return Job{}, opErr("create", err)
	}
	s.log.Info("draft created", "job", out.ID, "module", out.Module, "chained", out.SourceJobID != "")
	return out, nil
}

// validateCreate checks everything about a request that needs no database.
func (s *Store) validateCreate(req CreateReq, chained bool) error {
	switch req.Module {
	case ModuleVerify, ModuleSendManaged, ModuleSendBYO:
	default:
		return badRequest("module must be m1, m2 or m3")
	}
	if req.Module == ModuleVerify && req.Content != nil {
		return badRequest("a verify job carries no content")
	}
	if chained {
		if !isUUID(req.SourceJobID) {
			return badRequest("the source job does not exist")
		}
		if req.Module == ModuleVerify {
			return badRequest("only a send job (m2 or m3) can be chained to a verify job")
		}
		if len(req.Rows) != 0 {
			return badRequest("a chained send takes its rows from the source job: none may be supplied")
		}
		return nil
	}
	switch n := len(req.Rows); {
	case n == 0:
		return badRequest("a job needs at least one row")
	case n > s.maxRows:
		return &TooManyRowsError{Limit: s.maxRows}
	}
	return nil
}

// chainSource reads the module results of the user's succeeded verify job, in the order of its rows, each with the merge data of its source row, and checks that it may be chained: the user's own (a job of
// another user and a job that does not exist are one answer), m1, succeeded, with at least one module result and not more than the store's limit. It also returns the names of merge data the source job
// itself listed (merge_fields.go; nil for a job made before they were kept).
//
// THE MERGE DATA comes in the SAME query as the results, through a join on the source job's own rows (job_rows.id = job_results.item_ref, both scoped by the source job and the user): one statement, no
// round trip per row, the same order as before. A result whose row is gone (it cannot be: the result's foreign key names it) or whose merge data the retention cleared reads {}.
func (s *Store) chainSource(ctx context.Context, tx pgx.Tx, userID, sourceID string) ([]sourceResult, []string, error) {
	var srcModule, srcStatus string
	var listedDoc []byte // frozen_settings -> merge_fields: NULL for a job that has none
	err := tx.QueryRow(ctx, `SELECT module_id, status, frozen_settings -> 'merge_fields' FROM platform.jobs WHERE id = $1::uuid AND user_id = $2::uuid`, sourceID, userID).Scan(&srcModule, &srcStatus, &listedDoc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, badRequest("the source job does not exist")
	}
	if err != nil {
		return nil, nil, s.fail(err)
	}
	if srcModule != ModuleVerify {
		return nil, nil, badRequest("the source job is not a verify job")
	}
	if srcStatus != StatusSucceeded {
		return nil, nil, badRequest("the source job has not succeeded (it is %s)", srcStatus)
	}
	var listed []string
	if len(listedDoc) > 0 {
		if err := json.Unmarshal(listedDoc, &listed); err != nil {
			listed = nil // a list this store did not write is not trusted: the keys of the rows are read instead
		}
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform.job_results WHERE job_id = $1::uuid AND user_id = $2::uuid AND origin = 'module'`, sourceID, userID).Scan(&n); err != nil {
		return nil, nil, s.fail(err)
	}
	if n == 0 {
		return nil, nil, badRequest("the source job has no results from the module to chain")
	}
	if n > s.maxRows {
		return nil, nil, &TooManyRowsError{Limit: s.maxRows}
	}
	rows, err := tx.Query(ctx, `SELECT r.item_ref::text, r.email, r.module_state, COALESCE(w.fields, '{}'::jsonb)::text
		FROM platform.job_results r
		LEFT JOIN platform.job_rows w ON w.job_id = r.job_id AND w.user_id = r.user_id AND w.id = r.item_ref
		WHERE r.job_id = $1::uuid AND r.user_id = $2::uuid AND r.origin = 'module' ORDER BY r.ordinal, r.item_ref`, sourceID, userID)
	if err != nil {
		return nil, nil, s.fail(err)
	}
	defer rows.Close()
	out := make([]sourceResult, 0, n)
	for rows.Next() {
		var r sourceResult
		if err := rows.Scan(&r.ItemRef, &r.Email, &r.Verdict, &r.Fields); err != nil {
			return nil, nil, s.fail(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, s.fail(err)
	}
	return out, listed, nil
}

// insertRows writes one batch of classified rows with one INSERT ... SELECT over arrays (the table's BEFORE INSERT trigger runs per row, which is fine at the size limit). NULLs travel as the
// empty string and become NULL with NULLIF: a normalised address, a reason, a duplicate's target and a source item are never empty when present.
func (s *Store) insertRows(ctx context.Context, tx pgx.Tx, jobID, userID string, batch []classified) error {
	n := len(batch)
	ids, raws, norms, states, reasons, dups, fields, srcs := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	ordinals := make([]int32, n)
	for i, c := range batch {
		ids[i], ordinals[i], raws[i], norms[i], states[i], reasons[i], dups[i], fields[i], srcs[i] = c.ID, int32(c.Ordinal), c.Raw, c.Norm, c.State, c.Reason, c.DuplicateOf, c.Fields, c.SourceItem
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform.job_rows (id, job_id, user_id, ordinal, email_raw, email_norm, state, reason, duplicate_of, fields, source_item)
		SELECT u.id::uuid, $1::uuid, $2::uuid, u.ordinal, u.raw, NULLIF(u.norm, ''), u.state, NULLIF(u.reason, ''), NULLIF(u.dup, '')::uuid, u.fields::jsonb, NULLIF(u.src, '')::uuid
		FROM unnest($3::text[], $4::int[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[]) AS u(id, ordinal, raw, norm, state, reason, dup, fields, src)`,
		jobID, userID, ids, ordinals, raws, norms, states, reasons, dups, fields, srcs); err != nil {
		return s.fail(err)
	}
	return nil
}
