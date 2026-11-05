package login

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidFlow = errors.New("login expired or browser state did not match")
	ErrAccountLink = errors.New("sign in with the original provider, then explicitly link this provider from your account")
	ErrDisabled    = errors.New("account is disabled")
)

// User contains only browser account details, not an authorization grant.
type User struct {
	ID, Name, Email string
}

// Flow is a one-use browser-bound login transaction.
type Flow struct {
	Provider, Verifier, Nonce, ReturnPath, LinkUserID string
}

// Store makes callback replay and session rotation atomic across server instances.
type Store struct{ Pool *pgxpool.Pool }

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate login secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func digest(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func (s *Store) SaveFlow(ctx context.Context, state, browser string, f Flow) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO login_flows(state_hash,browser_hash,provider,verifier,nonce,return_path,link_user_id,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,now()+interval '10 minutes')`,
		digest(state), digest(browser), f.Provider, f.Verifier, f.Nonce, f.ReturnPath, f.LinkUserID)
	if err != nil {
		return fmt.Errorf("save login flow: %w", err)
	}
	return nil
}

func (s *Store) ConsumeFlow(ctx context.Context, state, browser, provider string) (Flow, error) {
	var f Flow
	err := s.Pool.QueryRow(ctx, `DELETE FROM login_flows WHERE state_hash=$1 AND browser_hash=$2 AND provider=$3 AND expires_at>now()
		RETURNING provider,verifier,nonce,return_path,coalesce(link_user_id::text,'')`, digest(state), digest(browser), provider).
		Scan(&f.Provider, &f.Verifier, &f.Nonce, &f.ReturnPath, &f.LinkUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Flow{}, ErrInvalidFlow
	}
	if err != nil {
		return Flow{}, fmt.Errorf("consume login flow: %w", err)
	}
	return f, nil
}

// Complete links only an explicitly authenticated user. A shared email address
// does not prove ownership of an existing Headmesh account.
func (s *Store) Complete(ctx context.Context, identity Identity, linkUser, previousSession string) (User, string, error) {
	if !identity.Verified || identity.Subject == "" || identity.Email == "" {
		return User{}, "", errors.New("verified provider identity is required")
	}
	token, err := randomSecret()
	if err != nil {
		return User{}, "", err
	}
	var user User
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT u.id::text,coalesce(u.name,''),u.email FROM login_identities i JOIN users u ON u.id=i.user_id
			WHERE i.provider=$1 AND i.subject=$2 AND u.disabled_at IS NULL`, identity.Provider, identity.Subject).
			Scan(&user.ID, &user.Name, &user.Email)
		if errors.Is(err, pgx.ErrNoRows) {
			if linkUser != "" {
				// Lock and revalidate the exact browser session used to request linking.
				err = tx.QueryRow(ctx, `SELECT u.id::text,coalesce(u.name,''),u.email FROM users u JOIN browser_sessions s ON s.user_id=u.id
					WHERE u.id=$1::uuid AND u.disabled_at IS NULL AND s.token_hash=$2 AND s.revoked_at IS NULL AND s.expires_at>now() FOR UPDATE OF s`,
					linkUser, digest(previousSession)).Scan(&user.ID, &user.Name, &user.Email)
			} else {
				err = tx.QueryRow(ctx, `INSERT INTO users(email,name,email_verified_at) VALUES ($1,$2,now()) RETURNING id::text,name,email`,
					identity.Email, identity.Name).Scan(&user.ID, &user.Name, &user.Email)
			}
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO login_identities(provider,subject,user_id) VALUES($1,$2,$3::uuid)`, identity.Provider, identity.Subject, user.ID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if linkUser != "" && user.ID != linkUser {
			return ErrAccountLink
		}
		if previousSession != "" {
			if _, err := tx.Exec(ctx, `UPDATE browser_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, digest(previousSession)); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO browser_sessions(token_hash,user_id,expires_at) VALUES($1,$2::uuid,now()+interval '12 hours')`, digest(token), user.ID)
		return err
	})
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return User{}, "", ErrAccountLink
		}
		return User{}, "", fmt.Errorf("complete login: %w", err)
	}
	return user, token, nil
}

func (s *Store) Session(ctx context.Context, token string) (User, error) {
	if len(token) != 43 {
		return User{}, ErrDisabled
	}
	var user User
	err := s.Pool.QueryRow(ctx, `SELECT u.id::text,coalesce(u.name,''),u.email FROM browser_sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now() AND u.disabled_at IS NULL`, digest(token)).
		Scan(&user.ID, &user.Name, &user.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrDisabled
	}
	if err != nil {
		return User{}, fmt.Errorf("load browser session: %w", err)
	}
	return user, nil
}

// LinkedProviders lists only the authenticated user's own identity bindings.
func (s *Store) LinkedProviders(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT provider FROM login_identities WHERE user_id=$1::uuid ORDER BY provider`, userID)
	if err != nil {
		return nil, fmt.Errorf("load linked identities: %w", err)
	}
	defer rows.Close()
	var providers []string
	for rows.Next() {
		var provider string
		if err := rows.Scan(&provider); err != nil {
			return nil, fmt.Errorf("read linked identity: %w", err)
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE browser_sessions SET revoked_at=$2 WHERE token_hash=$1 AND revoked_at IS NULL`, digest(token), time.Now())
	if err != nil {
		return fmt.Errorf("revoke browser session: %w", err)
	}
	return nil
}
