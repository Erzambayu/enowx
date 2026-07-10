package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/enowdev/enowx/core/syncbus"
	"github.com/enowdev/enowx/store"
)

type accountStore struct{ db *sql.DB }

func (s *accountStore) List(ctx context.Context, provider string) ([]store.Account, error) {
	query := `SELECT id, provider, label, secret, creds, status, disabled, created_at
		 FROM accounts WHERE (? = '' OR provider = ?) ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, provider, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Account
	for rows.Next() {
		var a store.Account
		var creds string
		if err := rows.Scan(&a.ID, &a.Provider, &a.Label, &a.Secret, &creds, &a.Status, &a.Disabled, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Creds = decodeCreds(creds)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *accountStore) Add(ctx context.Context, a store.Account) (int64, error) {
	creds := encodeCreds(a.Creds)
	// De-dupe by a STABLE identity, not the raw creds blob. For OAuth providers
	// the tokens (access/refresh/expiry) change on every re-auth, so matching the
	// whole creds string lets the same account clone endlessly. The identity is
	// the account email if present, else the secret (API-key providers). When an
	// account with that identity already exists for the provider, UPDATE it in
	// place (refresh creds/secret, reactivate) instead of inserting a duplicate.
	if id := s.findByIdentity(ctx, a); id != 0 {
		_, err := s.db.ExecContext(ctx,
			`UPDATE accounts SET label = ?, secret = ?, creds = ?, status = 'active', disabled = 0 WHERE id = ?`,
			a.Label, a.Secret, creds, id)
		if err != nil {
			return 0, err
		}
		syncbus.Dirty("account")
		return id, nil
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (provider, label, secret, creds, status) VALUES (?, ?, ?, ?, ?)`,
		a.Provider, a.Label, a.Secret, creds, nz(a.Status, "active"))
	if err != nil {
		return 0, err
	}
	syncbus.Dirty("account")
	return res.LastInsertId()
}

// findByIdentity returns the id of an existing account for the same provider that
// represents the same real account — matched by email (if the creds carry one),
// otherwise by a non-empty secret. Returns 0 when there's no stable identity to
// match on (so such accounts are always inserted).
func (s *accountStore) findByIdentity(ctx context.Context, a store.Account) int64 {
	email := a.Creds["email"]
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, secret, creds FROM accounts WHERE provider = ?`, a.Provider)
	if err != nil {
		return 0
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var secret, creds string
		if err := rows.Scan(&id, &secret, &creds); err != nil {
			continue
		}
		if email != "" {
			if decodeCreds(creds)["email"] == email {
				return id
			}
			continue
		}
		// No email identity → fall back to a non-empty secret match (API keys).
		if a.Secret != "" && secret == a.Secret {
			return id
		}
	}
	return 0
}

func (s *accountStore) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *accountStore) SetLabel(ctx context.Context, id int64, label string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET label = ? WHERE id = ?`, label, id)
	if err == nil {
		syncbus.Dirty("account")
	}
	return err
}

func (s *accountStore) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET disabled = ? WHERE id = ?`, disabled, id)
	return err
}

func (s *accountStore) UpdateCreds(ctx context.Context, id int64, creds map[string]string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET creds = ? WHERE id = ?`, encodeCreds(creds), id)
	if err == nil {
		syncbus.Dirty("account")
	}
	return err
}

func (s *accountStore) Delete(ctx context.Context, id int64) error {
	// warmup_logs has no FK to accounts, so removing the account leaves its
	// warmup history orphaned (unbounded DB bloat). Delete both together.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM warmup_logs WHERE account_id = ?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	syncbus.Dirty("account")
	return nil
}

func encodeCreds(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeCreds(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

func nz(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
