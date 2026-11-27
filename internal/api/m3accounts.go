package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// THE MODULE 3 ACCOUNT ROUTES (Phase 6 A4; Docs/jobs-api.md, "Module 3 accounts"): the user's own SMTP sender accounts at module 3 (send-byo): list them, add one, run the test button, add many, remove one.
//
// WHO THE ACCOUNTS ARE THE USER'S. The client of module 3 is built FOR THE AUTHENTICATED USER ONLY (M3Accounts.For(principal's id): the adapter puts that user in the body or the query of every call and refuses an
// answer that names another), no path, query or body names a user (a body that tries is an unknown member: 400), and an account id that is not this user's, is gone or never was is `404 not_found`, the same answer
// for the three, because module 3 answers them alike.
//
// NO MODULE TEXT. A response holds words the API chose and the user's own values, never a sentence module 3 wrote: an account is a view of fields (the From-gate verdict is one of `unverified`, `aligned`,
// `unaligned_warned`, `blocked`, `indeterminate`; the provider's own reply and the module's rendered From-gate sentence are not passed on), a refusal is one of the closed codes of errors_extra.go with at most
// words in its details. THE SMTP PASSWORD (`smtp_secret`) is accepted in the body of an add and of a bulk add and goes to the adapter, whose type prints it as [redacted]: it is in no response, no log line and no error.
//
// WHO MAY ACT. A user who has no module 3 entitlement, or whom the platform has observed suspended there, may list and remove accounts (their own data) but not add, test or bulk-add (each is a real
// authentication attempt against a mail provider, which only an entitled user may cause): `403 no_entitlement` and `403 suspended`, decided by the platform's own tables (M3Gate), as the job store decides a confirm.
// Module 3 may refuse for its own reasons, and each is a code.

// M3Account is what the API needs of module 3's account client for one user. *m3.Client satisfies it; the unit tests use a fake.
type M3Account interface {
	ListAccounts(ctx context.Context, cursor string) (m3.AccountPage, error)
	CreateAccount(ctx context.Context, n m3.NewAccount) (m3.Account, error)
	TestAccount(ctx context.Context, accountID string, acknowledge bool) (m3.TestResult, error)
	BulkAddAccounts(ctx context.Context, accounts []m3.NewAccount) (m3.BulkResult, error)
	DeleteAccount(ctx context.Context, accountID string) error
}

var _ M3Account = (*m3.Client)(nil)

// M3Accounts builds the client of one user. The API asks it for exactly one user per request: the principal's. *m3.Adapter does not satisfy it directly (its For returns a concrete type): NewM3Accounts wraps it.
type M3Accounts interface {
	For(userID string) (M3Account, error)
}

type adapterAccounts struct{ ad *m3.Adapter }

func (a adapterAccounts) For(userID string) (M3Account, error) {
	c, err := a.ad.For(userID)
	if err != nil {
		return nil, err // never a typed nil in the interface
	}
	return c, nil
}

// NewM3Accounts is the production M3Accounts over module 3's adapter; nil (a nil INTERFACE) when the adapter is nil, which is module 3 not configured in this process.
func NewM3Accounts(ad *m3.Adapter) M3Accounts {
	if ad == nil {
		return nil
	}
	return adapterAccounts{ad: ad}
}

// M3Gate decides whether a user may cause a real action at module 3 (add, test, bulk add): jobs.ErrNoEntitlement when the user has no enabled, unexpired entitlement and jobs.ErrSuspended when the platform has observed
// module 3 holding the user suspended; nil otherwise. nil in Deps means no gate (the unit tests of the routes that are not about it).
type M3Gate interface {
	Check(ctx context.Context, userID string) error
}

// M3GateDB is what the production gate reads through: *pgxpool.Pool (a login as platform_app, which holds SELECT on both tables).
type M3GateDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type m3Gate struct{ db M3GateDB }

// NewM3Gate is the production gate over the platform's tables; nil when db is nil.
func NewM3Gate(db M3GateDB) M3Gate {
	if db == nil {
		return nil
	}
	return m3Gate{db: db}
}

// Check is the rule of the job store's confirm (jobs checkEntitlement), the database's clock included: an entitlement that is enabled and not past its date, and no observation of 'suspended'.
func (g m3Gate) Check(ctx context.Context, userID string) error {
	var enabled, current bool
	err := g.db.QueryRow(ctx, `SELECT enabled, enabled_until IS NULL OR enabled_until > clock_timestamp() FROM platform.m3_entitlements WHERE user_id = $1::uuid`, userID).Scan(&enabled, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ErrNoEntitlement
	}
	if err != nil {
		return err
	}
	if !enabled || !current {
		return jobs.ErrNoEntitlement
	}
	var observed string
	err = g.db.QueryRow(ctx, `SELECT observed FROM platform.m3_sync WHERE user_id = $1::uuid`, userID).Scan(&observed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing was ever observed
	}
	if err != nil {
		return err
	}
	if observed == "suspended" {
		return jobs.ErrSuspended
	}
	return nil
}

const (
	m3AccountsPath = "/v1/m3/accounts"
	// maxBulkBody is the cap of the body of a bulk add (JSON or CSV): module 3 takes at most m3.MaxBulkRows rows, so a body over this is not a batch.
	maxBulkBody int64 = 1 << 20
	// maxAccountText bounds every text member of an account (the module bounds its own columns; this keeps a body from carrying more than any of them could hold).
	maxAccountText = 1024
)

// ---- the views -------------------------------------------------------------------------------------------------------------

// m3AccountView is an account as the user sees it. has_credential is all there is of the secret.
type m3AccountView struct {
	ID               string  `json:"id"`
	SMTPHost         string  `json:"smtp_host"`
	SMTPPort         int     `json:"smtp_port"`
	SMTPUsername     string  `json:"smtp_username"`
	FromAddress      string  `json:"from_address"`
	FromName         *string `json:"from_name"`
	DailyLimit       int     `json:"daily_limit"`
	HasCredential    bool    `json:"has_credential"`
	Status           string  `json:"status"`
	PauseReason      *string `json:"pause_reason"`
	QuarantineReason *string `json:"quarantine_reason"`
	Verification     string  `json:"verification"`
	VerifyCode       *string `json:"verify_code"`
	FromGate         string  `json:"from_gate"`
	CreatedAt        string  `json:"created_at"`
}

// fromGateWord is the From-gate verdict as one of the five words, whatever the adapter carried: a state outside them (which the adapter refuses before it gets here) is told as indeterminate.
func fromGateWord(s m3.AlignmentState) string {
	switch s {
	case m3.AlignmentUnverified, m3.AlignmentAligned, m3.AlignmentWarned, m3.AlignmentBlocked, m3.AlignmentIndeterminate:
		return string(s)
	}
	return string(m3.AlignmentIndeterminate)
}

// verificationWord is the credential check of an account as one of unverified, verified, failed (a check that could not run is `unverified` in a listing).
func verificationWord(s m3.VerificationState) string {
	switch s {
	case m3.VerificationVerified, m3.VerificationFailed:
		return string(s)
	}
	return string(m3.VerificationUnverified)
}

// statusWord is what an account is doing: disabled, quarantined, paused or active, in that order of precedence.
func statusWord(a m3.Account) string {
	switch {
	case a.Disabled:
		return "disabled"
	case a.Quarantined:
		return "quarantined"
	case a.Paused:
		return "paused"
	}
	return "active"
}

func viewM3Account(a m3.Account) m3AccountView {
	return m3AccountView{
		ID: a.ID, SMTPHost: a.SMTPHost, SMTPPort: a.SMTPPort, SMTPUsername: a.SMTPUsername, FromAddress: a.FromAddress, FromName: a.FromName, DailyLimit: a.DailyLimit,
		HasCredential: a.HasCredential, Status: statusWord(a), PauseReason: strPtr(a.PauseReason), QuarantineReason: strPtr(a.QuarantineReason),
		Verification: verificationWord(a.Verification.State), VerifyCode: strPtr(string(a.Verification.Code)), FromGate: fromGateWord(a.FromAlignment.State), CreatedAt: stamp(a.CreatedAt),
	}
}

// testStage is where a test of an account got to, in one word: done (the credential was verified), connect (the connection could not be made or was lost), host (module 3 refused the SMTP host as a
// destination), tls, login (the credentials were refused), protocol, or unavailable (the check could not run).
func testStage(v m3.TestVerification) string {
	if v.State == m3.VerificationVerified {
		return "done"
	}
	switch v.Code {
	case m3.VerifyConnectFailed, m3.VerifyTimeout, m3.VerifyConnectionLost:
		return "connect"
	case m3.VerifyHostNotAllowed:
		return "host"
	case m3.VerifyTLSUnavailable, m3.VerifyTLSFailed:
		return "tls"
	case m3.VerifyAuthFailed:
		return "login"
	case m3.VerifyProtocolError:
		return "protocol"
	}
	return "unavailable"
}

// bulkCode is the code of a row of a bulk add: created, created_unverified (the account exists and its credential is not verified), or why the row was not created, in the words of the error codes where
// there is one.
func bulkCode(r m3.BulkRow) string {
	if r.Created {
		if r.Verification == m3.VerificationVerified {
			return "created"
		}
		return "created_unverified"
	}
	if r.HostForbidden() {
		return codeHostForbidden
	}
	switch r.Reason {
	case m3.RejectValidation, m3.RejectUnprocessable:
		return codeValidationFailed
	case m3.RejectAccountCap:
		return "account_limit_reached"
	case m3.RejectDuplicate:
		return codeAccountDuplicate
	case m3.RejectAlignmentWarning:
		return codeFromGateAckRequired
	case m3.RejectConflict:
		return codeAccountUnavailable
	case m3.RejectNotFound:
		return codeNotFound
	}
	return codeInternal
}

// ---- the request ------------------------------------------------------------------------------------------------------------

// m3AccountBody is an account to add: the adapter's new-account fields and acknowledge_from_gate. The members are pointers so that a member that is absent is told from one that is empty or zero.
type m3AccountBody struct {
	SMTPHost            *string `json:"smtp_host"`
	SMTPPort            *int    `json:"smtp_port"`
	SMTPUsername        *string `json:"smtp_username"`
	SMTPSecret          *string `json:"smtp_secret"`
	FromAddress         *string `json:"from_address"`
	FromName            *string `json:"from_name"`
	DailyLimit          *int    `json:"daily_limit"`
	AcknowledgeFromGate bool    `json:"acknowledge_from_gate"`
}

func cleanText(s string) bool {
	return len(s) <= maxAccountText && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// newAccount turns a body into the adapter's account and the list of what is wrong with it ("field:required", "field:malformed"): the four members every account needs must be present and not empty, and every text
// member must be bounded text. Numbers are the module's to judge (its own out_of_range comes back as a validation_failed with its fields).
func (b m3AccountBody) newAccount() (m3.NewAccount, []string) {
	var bad []string
	need := func(name string, v *string) string {
		switch {
		case v == nil || strings.TrimSpace(*v) == "":
			bad = append(bad, name+":required")
			return ""
		case !cleanText(*v):
			bad = append(bad, name+":malformed")
			return ""
		}
		return *v
	}
	n := m3.NewAccount{AcknowledgeAlignmentWarning: b.AcknowledgeFromGate}
	n.SMTPHost = need("smtp_host", b.SMTPHost)
	n.SMTPUsername = need("smtp_username", b.SMTPUsername)
	n.FromAddress = need("from_address", b.FromAddress)
	if secret := need("smtp_secret", b.SMTPSecret); secret != "" {
		n.SMTPSecret = config.Secret(secret)
	}
	if b.FromName != nil {
		if !cleanText(*b.FromName) {
			bad = append(bad, "from_name:malformed")
		} else if strings.TrimSpace(*b.FromName) != "" {
			name := *b.FromName
			n.FromName = &name
		}
	}
	if b.SMTPPort != nil {
		n.SMTPPort = *b.SMTPPort
	}
	if b.DailyLimit != nil {
		n.DailyLimit = *b.DailyLimit
	}
	return n, bad
}

// refuseAccount answers an account that is not valid: 400 validation_failed with the fields, as words, and the line of a batch when there is one.
func (a *API) refuseAccount(w http.ResponseWriter, r *http.Request, line int, fields []string) {
	reason := "the account is not valid"
	if line > 0 {
		httpapi.WriteError(w, r, http.StatusBadRequest, codeValidationFailed, messages[codeValidationFailed], "reason", "a row of the batch is not valid", "line", line, "fields", fields)
		return
	}
	httpapi.WriteError(w, r, http.StatusBadRequest, codeValidationFailed, messages[codeValidationFailed], "reason", reason, "fields", fields)
}

// accountID is the {id} of the path, or false after the 404 an account that cannot exist gets (the same answer as one that does not).
func (a *API) accountID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !isJobID(id) {
		a.fail(w, r, jobs.ErrNotFound)
		return "", false
	}
	return id, true
}

// m3Client is the client of module 3 for the AUTHENTICATED user. Module 3 not configured in this process is `503 module_unavailable`.
func (a *API) m3Client(w http.ResponseWriter, r *http.Request, p principal) (M3Account, bool) {
	if a.m3 == nil {
		a.fail(w, r, jobs.ErrModuleUnavailable)
		return nil, false
	}
	c, err := a.m3.For(p.UserID)
	if err != nil {
		a.fail(w, r, err) // the principal is always a customer, so this is the server's own mistake: a 500 whose cause is logged
		return nil, false
	}
	return c, true
}

// m3Entitled runs the gate, when there is one, for an action that is a real attempt against a mail provider.
func (a *API) m3Entitled(w http.ResponseWriter, r *http.Request, p principal) bool {
	if a.m3gate == nil {
		return true
	}
	if err := a.m3gate.Check(r.Context(), p.UserID); err != nil {
		a.fail(w, r, err)
		return false
	}
	return true
}

// ---- the routes ------------------------------------------------------------------------------------------------------------

// cursorShape is what a cursor of module 3's list looks like (an opaque token of a URL-safe alphabet); a cursor outside it is not one this API gave.
var cursorShape = regexp.MustCompile(`^[A-Za-z0-9_=.+/-]+$`)

func (a *API) listM3Accounts(w http.ResponseWriter, r *http.Request, p principal) {
	q, ok := a.query(w, r, "cursor")
	if !ok {
		return
	}
	cursor := q.Get("cursor")
	if cursor != "" && (len(cursor) > 1024 || !cursorShape.MatchString(cursor)) {
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "the cursor is not one this API gave")
		return
	}
	c, ok := a.m3Client(w, r, p)
	if !ok {
		return
	}
	page, err := c.ListAccounts(r.Context(), cursor)
	if err != nil {
		a.fail(w, r, m3Err(err, false))
		return
	}
	out := struct {
		Accounts   []m3AccountView `json:"accounts"`
		NextCursor *string         `json:"next_cursor"`
	}{Accounts: make([]m3AccountView, 0, len(page.Items)), NextCursor: strPtr(page.NextCursor)}
	for _, acc := range page.Items {
		out.Accounts = append(out.Accounts, viewM3Account(acc))
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

func (a *API) addM3Account(w http.ResponseWriter, r *http.Request, p principal) {
	if _, ok := a.query(w, r); !ok {
		return
	}
	var body m3AccountBody
	if !a.readJSON(w, r, maxSmallBody, smallBodyWindow, &body) {
		return
	}
	n, bad := body.newAccount()
	if len(bad) > 0 {
		a.refuseAccount(w, r, 0, bad)
		return
	}
	c, ok := a.m3Client(w, r, p)
	if !ok || !a.m3Entitled(w, r, p) {
		return
	}
	acc, err := c.CreateAccount(r.Context(), n)
	if err != nil {
		a.fail(w, r, m3Err(err, true))
		return
	}
	a.writeJSON(w, r, http.StatusCreated, struct {
		Account m3AccountView `json:"account"`
	}{viewM3Account(acc)})
}

// testBody is the optional body of the test button.
type testBody struct {
	Acknowledge bool `json:"acknowledge"`
}

func (a *API) testM3Account(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.accountID(w, r)
	if !ok {
		return
	}
	if _, ok := a.query(w, r); !ok {
		return
	}
	// the body is optional: no body at all is a test without the acknowledgement; a body is one object with the one member
	if a.tooBig(w, r, maxSmallBody) {
		return
	}
	done := extendReadDeadline(w, smallBodyWindow)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSmallBody))
	done()
	if err != nil {
		a.badBody(w, r, err)
		return
	}
	var body testBody
	if len(bytes.TrimSpace(raw)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if !a.readJSON(w, r, maxSmallBody, smallBodyWindow, &body) {
			return
		}
	}
	c, ok := a.m3Client(w, r, p)
	if !ok || !a.m3Entitled(w, r, p) {
		return
	}
	res, err := c.TestAccount(r.Context(), id, body.Acknowledge)
	if err != nil {
		a.fail(w, r, m3Err(err, true))
		return
	}
	type result struct {
		OK       bool   `json:"ok"`
		Stage    string `json:"stage"`
		FromGate string `json:"from_gate"`
	}
	a.writeJSON(w, r, http.StatusOK, struct {
		Result result `json:"result"`
	}{result{OK: res.Verification.State == m3.VerificationVerified, Stage: testStage(res.Verification), FromGate: fromGateWord(res.Alignment.State)}})
}

func (a *API) removeM3Account(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := a.accountID(w, r)
	if !ok || !a.drain(w, r) {
		return
	}
	if _, ok := a.query(w, r); !ok {
		return
	}
	c, ok := a.m3Client(w, r, p)
	if !ok {
		return
	}
	if err := c.DeleteAccount(r.Context(), id); err != nil {
		a.fail(w, r, m3Err(err, true))
		return
	}
	a.writeJSON(w, r, http.StatusOK, struct {
		Removed bool `json:"removed"`
	}{true})
}

// bulkBody is the JSON of a bulk add: the accounts, each as an account to add.
type bulkBody struct {
	Accounts []m3AccountBody `json:"accounts"`
}

// bulkColumns are the columns of a CSV bulk add, by header name (any letter case, surrounding space ignored): the same members as the JSON of one account.
var bulkColumns = map[string]bool{"smtp_host": true, "smtp_port": true, "smtp_username": true, "smtp_secret": true, "from_address": true, "from_name": true, "daily_limit": true, "acknowledge_from_gate": true}

func (a *API) bulkM3Accounts(w http.ResponseWriter, r *http.Request, p principal) {
	if _, ok := a.query(w, r); !ok {
		return
	}
	var rows []m3AccountBody
	var lines []int // the line (CSV) or position (JSON) of each row, 1-based
	switch mediaType(r) {
	case "application/json":
		var body bulkBody
		if !a.readJSON(w, r, maxBulkBody, smallBodyWindow, &body) {
			return
		}
		rows = body.Accounts
		for i := range rows {
			lines = append(lines, i+1)
		}
	case "text/csv":
		var ok bool
		if rows, lines, ok = a.readBulkCSV(w, r); !ok {
			return
		}
	default:
		a.refuseRequest(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMedia, "send the accounts as application/json or text/csv")
		return
	}
	switch {
	case len(rows) == 0:
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "the batch has no accounts")
		return
	case len(rows) > m3.MaxBulkRows:
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, "the batch has more than "+strconv.Itoa(m3.MaxBulkRows)+" accounts; split it")
		return
	}
	accounts := make([]m3.NewAccount, len(rows))
	for i, row := range rows {
		n, bad := row.newAccount()
		if len(bad) > 0 {
			a.refuseAccount(w, r, lines[i], bad)
			return
		}
		accounts[i] = n
	}
	c, ok := a.m3Client(w, r, p)
	if !ok || !a.m3Entitled(w, r, p) {
		return
	}
	res, err := c.BulkAddAccounts(r.Context(), accounts)
	if err != nil {
		a.fail(w, r, m3Err(err, true))
		return
	}
	type rowView struct {
		Line int    `json:"line"`
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	out := struct {
		Results []rowView `json:"results"`
	}{Results: make([]rowView, 0, len(res.Rows))}
	for _, row := range res.Rows {
		line := row.Row
		if row.Row >= 1 && row.Row <= len(lines) {
			line = lines[row.Row-1]
		}
		out.Results = append(out.Results, rowView{Line: line, OK: row.Created, Code: bulkCode(row)})
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

// readBulkCSV reads a CSV bulk add: a header naming columns (smtp_host, smtp_username, smtp_secret and from_address are required) and one account per row. It answers the failure itself, never naming a cell.
func (a *API) readBulkCSV(w http.ResponseWriter, r *http.Request) ([]m3AccountBody, []int, bool) {
	if a.tooBig(w, r, maxBulkBody) {
		return nil, nil, false
	}
	done := extendReadDeadline(w, smallBodyWindow)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBulkBody))
	done()
	if err != nil {
		a.badBody(w, r, err)
		return nil, nil, false
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	refuse := func(reason string) ([]m3AccountBody, []int, bool) {
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, reason)
		return nil, nil, false
	}
	cr := csv.NewReader(bytes.NewReader(raw))
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return refuse("the CSV has no header row")
	}
	if err != nil {
		return refuse("the header row is not valid CSV")
	}
	cr.FieldsPerRecord = len(header)
	col := make([]string, len(header))
	seen := map[string]bool{}
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		switch {
		case !bulkColumns[name]:
			return refuse("column " + strconv.Itoa(i+1) + " of the header is not one the API knows")
		case seen[name]:
			return refuse("the header names a column twice")
		}
		seen[name] = true
		col[i] = name
	}
	var rows []m3AccountBody
	var lines []int
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, lines, true
		}
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			return refuse("line " + strconv.Itoa(pe.StartLine) + " is not valid CSV or has a different number of columns than the header")
		}
		if err != nil {
			return refuse("the body could not be read")
		}
		if len(rows) >= m3.MaxBulkRows {
			return refuse("the batch has more than " + strconv.Itoa(m3.MaxBulkRows) + " accounts; split it")
		}
		line, _ := cr.FieldPos(0)
		var b m3AccountBody
		for i, v := range rec {
			v := v
			switch col[i] {
			case "smtp_host":
				b.SMTPHost = &v
			case "smtp_username":
				b.SMTPUsername = &v
			case "smtp_secret":
				b.SMTPSecret = &v
			case "from_address":
				b.FromAddress = &v
			case "from_name":
				b.FromName = &v
			case "smtp_port", "daily_limit":
				if strings.TrimSpace(v) == "" {
					continue
				}
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					return refuse("line " + strconv.Itoa(line) + " has a " + col[i] + " that is not a whole number")
				}
				if col[i] == "smtp_port" {
					b.SMTPPort = &n
				} else {
					b.DailyLimit = &n
				}
			case "acknowledge_from_gate":
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "", "false", "0", "no":
				case "true", "1", "yes":
					b.AcknowledgeFromGate = true
				default:
					return refuse("line " + strconv.Itoa(line) + " has an acknowledge_from_gate that is not true or false")
				}
			}
		}
		rows = append(rows, b)
		lines = append(lines, line)
	}
}
