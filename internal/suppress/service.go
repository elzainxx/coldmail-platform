package suppress

import (
	"context"
	"errors"
	"io"
)

// DB is the connection a Store works over: *pgxpool.Pool (a login as platform_app) satisfies it.
type DB interface {
	Execer
	Querier
	Lister
}

// Store is the user-facing surface of the suppression list: what the HTTP API (internal/api suppressions.go) is given. Every method takes the user id of the AUTHENTICATED PRINCIPAL and nothing else, and
// every statement is scoped by it (the rule of CLAUDE.md: the store layer, where all writers pass, enforces tenancy). It writes only through the functions of store.go: Add, AddMany and Remove, which
// validate the address first. It is safe for concurrent use.
type Store struct {
	db DB
}

// ErrNoDB: the store was built with no database.
var ErrNoDB = errors.New("suppress: no database")

// NewStore builds the store over db.
func NewStore(db DB) (*Store, error) {
	if db == nil {
		return nil, ErrNoDB
	}
	return &Store{db: db}, nil
}

// List is List over the store's connection.
func (s *Store) List(ctx context.Context, userID, query, cursor string, limit int) ([]Entry, string, error) {
	return List(ctx, s.db, userID, query, cursor, limit)
}

// Add suppresses email for the user as a manual entry and returns the entry as it stands: created is true when this call inserted it, false when the user already had the address (the first row stands, its
// source and its time included, whatever they were). An address the platform's rule refuses is an *InvalidAddressError, decided before any statement.
func (s *Store) Add(ctx context.Context, userID, email string) (entry Entry, created bool, err error) {
	// the row that answers is read back after the write; a removal that lands between the two (the same user, in another request) leaves nothing to read, and the write is then made once more
	for attempt := 0; attempt < 2; attempt++ {
		res, err := Add(ctx, s.db, userID, email, SourceManual)
		if err != nil {
			return Entry{}, false, err
		}
		e, ok, err := Get(ctx, s.db, userID, res.Email)
		if err != nil {
			return Entry{}, false, err
		}
		if ok {
			return e, res.Created, nil
		}
	}
	return Entry{}, false, opErr("add", ErrConflict)
}

// Remove is Remove over the store's connection: whether the user's own entry for email was removed.
func (s *Store) Remove(ctx context.Context, userID, email string) (bool, error) {
	return Remove(ctx, s.db, userID, email)
}

// Import is ImportCSV over the store's connection.
func (s *Store) Import(ctx context.Context, userID string, body io.Reader, maxRows int) (ImportResult, error) {
	return ImportCSV(ctx, s.db, userID, body, maxRows)
}
