package authn

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// cheapHash is a valid PHC argon2id hash with the smallest parameters VerifyHash accepts. The parameters live in
// each hash, so a store holding it behaves exactly like production, only fast enough to fuzz.
func cheapHash(t testing.TB, token string) string {
	t.Helper()
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key := argon2.IDKey([]byte(token), salt, 1, 8, 1, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=8,t=1,p=1$%s$%s", argon2.Version,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// FuzzStoreVerify: Store.Verify accepts exactly the token it issued. Any other string, including every one-byte
// change, truncation or extension of the real token, gets ErrInvalidToken and never a panic.
func FuzzStoreVerify(f *testing.F) {
	path := filepath.Join(f.TempDir(), "tokens.json")
	file, err := LoadFile(path) // a missing file is a fresh, empty version-1 store
	if err != nil {
		f.Fatal(err)
	}
	tok, rec, err := file.Create("fuzz", nil, RouteMCP, time.Unix(0, 0))
	if err != nil {
		f.Fatal(err)
	}
	file.Tokens[0].Hash = cheapHash(f, tok)
	if err := file.Save(path); err != nil {
		f.Fatal(err)
	}
	store := NewStore(path)
	if got, err := store.Verify(tok); err != nil || got.ID != rec.ID {
		f.Fatalf("the issued token must verify: %+v, %v", got, err)
	}

	for _, s := range []string{
		tok, tok[:len(tok)-1], tok + "A", tok[:prefixLen], tok[:prefixLen+16], "hms_", "", TokenPrefix + "x",
		"hmk_" + tok[len(TokenPrefix):], tok[:len(tok)-1] + "B",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, candidate string) {
		got, err := store.Verify(candidate)
		switch {
		case candidate == tok:
			if err != nil || got.ID != rec.ID {
				t.Fatalf("the issued token stopped verifying: %+v, %v", got, err)
			}
		case err == nil:
			t.Fatalf("Verify accepted %q, which is not the issued token", candidate)
		case !errors.Is(err, ErrInvalidToken):
			t.Fatalf("Verify(%q) = %v, want ErrInvalidToken", candidate, err)
		}
	})
}
