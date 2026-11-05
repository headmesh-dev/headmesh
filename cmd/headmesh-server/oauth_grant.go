package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/headmesh-dev/headmesh/api/oauth"
)

// oauthGrant is operator-only. Select a linked identity by its immutable
// provider subject, never by matching an untrusted sign-up email.
func oauthGrant(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("oauth-grant", flag.ContinueOnError)
	provider := flags.String("provider", "", "github or huggingface")
	subject := flags.String("subject", "", "immutable provider subject of the sandbox owner")
	resource := flags.String("resource", "", "exact HTTPS MCP resource URL")
	name := flags.String("name", "", "sandbox name")
	scopeList := flags.String("scopes", "sandbox:read", "comma-separated explicit sandbox scopes")
	revoke := flags.Bool("revoke", false, "revoke this owner's resource access")
	if err := flags.Parse(args); err != nil {
		return err
	}
	u, err := url.Parse(*resource)
	if !slices.Contains([]string{"github", "huggingface"}, *provider) || *subject == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(*resource) > 2048 {
		return errors.New("provider, immutable subject, and exact HTTPS resource are required")
	}
	scopes := strings.Split(*scopeList, ",")
	for _, scope := range scopes {
		if !slices.Contains(oauth.ResourceScopes, scope) {
			return errors.New("unsupported sandbox scope")
		}
	}
	if !*revoke && (*name == "" || !slices.Contains(scopes, "sandbox:read")) {
		return errors.New("a name and sandbox:read scope are required")
	}
	dsn := env("HEADMESH_DATABASE_OWNER_URL", "")
	if dsn == "" {
		return errors.New("HEADMESH_DATABASE_OWNER_URL is required for operator grant management")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return errors.New("operator database connection failed")
	}
	defer pool.Close()
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('headmesh.system','on',true)`); err != nil {
			return err
		}
		var user string
		if err := tx.QueryRow(ctx, `SELECT i.user_id::text FROM login_identities i JOIN users u ON u.id=i.user_id WHERE i.provider=$1 AND i.subject=$2 AND u.disabled_at IS NULL`, *provider, *subject).Scan(&user); err != nil {
			return errors.New("no enabled account has that linked provider identity")
		}
		if *revoke {
			result, err := tx.Exec(ctx, `UPDATE oauth_resource_grants SET revoked_at=now() WHERE user_id=$1::uuid AND resource=$2`, user, *resource)
			if err == nil && result.RowsAffected() != 1 {
				return errors.New("resource grant does not exist")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO oauth_resources(resource,name) VALUES($1,$2) ON CONFLICT(resource) DO UPDATE SET name=excluded.name`, *resource, *name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO oauth_resource_grants(user_id,resource,scopes) VALUES($1::uuid,$2,$3) ON CONFLICT(user_id,resource) DO UPDATE SET scopes=excluded.scopes,revoked_at=NULL`, user, *resource, scopes)
		return err
	})
	if err != nil {
		return fmt.Errorf("OAuth resource grant: %w", err)
	}
	if *revoke {
		fmt.Println("Sandbox OAuth access revoked")
	} else {
		fmt.Println("Explicit sandbox OAuth grant saved")
	}
	return nil
}
