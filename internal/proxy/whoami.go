package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"

	"github.com/dehydr8/cproxy/internal/version"
)

// WhoamiPath lets a local client confirm that a listener is cproxy and holds
// its client token before handing that token to claude. The request carries
// only the token's SHA-256 and a nonce; the answer proves knowledge of the
// token with HMAC-SHA256(token, nonce). Something else squatting on the port
// learns nothing usable, and a cproxy started with a different configuration
// answers without a proof.
const WhoamiPath = "/_cproxy/whoami"

// WhoamiResponse is the body of GET WhoamiPath?client=<sha256 hex>&nonce=….
type WhoamiResponse struct {
	Cproxy string `json:"cproxy"` // version; always set
	PID    int    `json:"pid"`
	Proof  string `json:"proof,omitempty"` // absent when the token is unknown
}

// TokenHash is the client identifier sent to WhoamiPath.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// WhoamiProof is the proof a cproxy holding token returns for nonce.
func WhoamiProof(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(nonce))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) serveWhoami(w http.ResponseWriter, r *http.Request, st *State) {
	q := r.URL.Query()
	out := WhoamiResponse{Cproxy: version.String(), PID: os.Getpid()}
	if nonce := q.Get("nonce"); nonce != "" {
		if tok := st.tokenByHash(q.Get("client")); tok != "" {
			out.Proof = WhoamiProof(tok, nonce)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// tokenByHash returns the configured client token whose SHA-256 is hash.
func (st *State) tokenByHash(hash string) string {
	if len(hash) != sha256.Size*2 {
		return ""
	}
	match := func(tok string) bool {
		return subtle.ConstantTimeCompare([]byte(TokenHash(tok)), []byte(hash)) == 1
	}
	if st.SessionToken != "" && match(st.SessionToken) {
		return st.SessionToken
	}
	if st.Runtime != nil {
		for tok := range st.Runtime.ClientAuth {
			if match(tok) {
				return tok
			}
		}
	}
	return ""
}
