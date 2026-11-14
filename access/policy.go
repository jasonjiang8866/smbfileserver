// Package access composes service authentication and logical-share authorization
// around a read-only backend. It uses only the standard library. Paths are checked
// lexically; server-side share/ACL/link policy is the filesystem security boundary.
package access

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"strings"
)

// Reader owns SMB transport and resource limits; Policy adds no connections.
type Reader interface {
	Open(context.Context, string, string) (fs.File, error)
	ReadFile(context.Context, string, string) ([]byte, error)
}

// Config is snapshotted by New. TokenSHA256 holds hex SHA-256 digests, never raw
// credentials. Two digests per service permit overlap during rolling rotation.
type Config struct {
	Shares   []Share   `json:"shares"`
	Services []Service `json:"services"`
}
type Share struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Service struct {
	ID          string   `json:"id"`
	TokenSHA256 []string `json:"token_sha256"`
	Shares      []string `json:"shares"`
}
type credential struct {
	digest  [32]byte
	service int
}
type principal struct {
	owner  *Policy
	shares map[string]bool
}
type identityKey struct{}

// Policy is immutable and safe for concurrent use. Construct a replacement to
// change identities/grants; in-flight requests retain their original snapshot.
type Policy struct {
	reader      Reader
	shares      map[string]string
	identities  []principal
	credentials []credential
}

func New(config Config, reader Reader) (*Policy, error) {
	if reader == nil {
		return nil, errors.New("access: reader is required")
	}
	if len(config.Shares) == 0 || len(config.Shares) > 128 || len(config.Services) == 0 || len(config.Services) > 128 {
		return nil, errors.New("access: configure 1 to 128 shares and services")
	}
	p := &Policy{reader: reader, shares: make(map[string]string)}
	for _, share := range config.Shares {
		if !validID(share.ID) || !validShare(share.Name) {
			return nil, errors.New("access: invalid share mapping")
		}
		if _, ok := p.shares[share.ID]; ok {
			return nil, errors.New("access: duplicate share ID")
		}
		p.shares[share.ID] = share.Name
	}
	ids := make(map[string]bool)
	digests := make(map[[32]byte]bool)
	for index, service := range config.Services {
		if !validID(service.ID) || ids[service.ID] {
			return nil, errors.New("access: invalid or duplicate service ID")
		}
		ids[service.ID] = true
		if len(service.TokenSHA256) < 1 || len(service.TokenSHA256) > 2 || len(service.Shares) < 1 || len(service.Shares) > 128 {
			return nil, errors.New("access: each service requires 1 or 2 token digests and explicit share grants")
		}
		identity := principal{owner: p, shares: make(map[string]bool)}
		for _, id := range service.Shares {
			if _, ok := p.shares[id]; !ok {
				return nil, errors.New("access: unknown share grant")
			}
			if identity.shares[id] {
				return nil, errors.New("access: duplicate share grant")
			}
			identity.shares[id] = true
		}
		p.identities = append(p.identities, identity)
		for _, value := range service.TokenSHA256 {
			digest, err := hex.DecodeString(value)
			if err != nil || len(digest) != sha256.Size {
				return nil, errors.New("access: token digest must be SHA-256 hex")
			}
			var fixed [32]byte
			copy(fixed[:], digest)
			if digests[fixed] {
				return nil, errors.New("access: token digests must be unique")
			}
			digests[fixed] = true
			p.credentials = append(p.credentials, credential{fixed, index})
		}
	}
	return p, nil
}

// Wrap authenticates every request before routing or parsing a batch. It accepts
// exactly one Authorization header and no query/cookie/forwarded identity tokens.
// Use HTTPS or a trusted TLS-terminating proxy; the package does not provide TLS.
func (p *Policy) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("Content-Security-Policy", "sandbox")
		values := r.Header.Values("Authorization")
		var token string
		if len(values) == 1 {
			scheme, value, ok := strings.Cut(values[0], " ")
			if ok && strings.EqualFold(scheme, "Bearer") && validToken(value) {
				token = value
			}
		}
		match := -1
		digest := sha256.Sum256([]byte(token))
		for _, c := range p.credentials {
			// Hash first to compare equal-length values; scan every configured digest.
			if subtle.ConstantTimeCompare(digest[:], c.digest[:]) == 1 {
				match = c.service
			}
		}
		if token == "" || match < 0 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="smbfileserver"`)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusUnauthorized)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(`{"error":"authentication required"}`))
			}
			return
		}
		ctx := context.WithValue(r.Context(), identityKey{}, p.identities[match])
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (p *Policy) resolve(ctx context.Context, id, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	identity, ok := ctx.Value(identityKey{}).(principal)
	if !ok || identity.owner != p || !identity.shares[id] {
		return "", fs.ErrPermission
	}
	if !validPath(name) {
		return "", fs.ErrInvalid
	}
	return p.shares[id], nil
}
func (p *Policy) Open(ctx context.Context, id, name string) (fs.File, error) {
	share, err := p.resolve(ctx, id, name)
	if err != nil {
		return nil, err
	}
	return p.reader.Open(ctx, share, name)
}
func (p *Policy) ReadFile(ctx context.Context, id, name string) ([]byte, error) {
	share, err := p.resolve(ctx, id, name)
	if err != nil {
		return nil, err
	}
	return p.reader.ReadFile(ctx, share, name)
}
func validID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func validToken(s string) bool {
	if len(s) < 32 || len(s) > 512 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~+/=", r)) {
			return false
		}
	}
	return true
}
