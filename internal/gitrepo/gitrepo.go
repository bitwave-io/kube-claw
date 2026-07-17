// Package gitrepo is the git-repo access plane: a repository is registered
// (URL + its own read/write credentials + who may grant access), and an agent
// requests access to it by name at a given level (read | write). The request
// goes to the repo's granters for approval and becomes a durable grant bound to
// the agent's current image digest + spec hash + access level — exactly like a
// secret grant (internal/secrets), but the credential material lives on the
// GitRepo itself rather than in the secret store.
//
// A read grant hands back the read credential; a write grant hands back the
// write credential. Write implies read, so a write grant also satisfies a read
// materialization. A read grant literally cannot push — it never sees the write
// credential.
package gitrepo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Access levels. Write implies read (see Satisfies).
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// ValidAccess reports whether s is a known access level.
func ValidAccess(s string) bool { return s == AccessRead || s == AccessWrite }

// Satisfies reports whether a grant at level `held` covers a request for level
// `want` — write implies read.
func Satisfies(held, want string) bool {
	if held == AccessWrite {
		return true
	}
	return held == want
}

// NewGitRepoID mints a git-repo id.
func NewGitRepoID() string { return "repo-" + randHex(8) }

// NewID mints an id with the given prefix (e.g. "grepo-grant", "grepo-req").
func NewID(prefix string) string { return prefix + "-" + randHex(8) }

// ValidRepoName allows only safe characters so a name can't be used for path
// traversal when interpolated into a materialized file path (mirrors
// validSecretName in the secret plane).
func ValidRepoName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return name != "." && name != ".." && !strings.Contains(name, "..")
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("gitrepo: rand: %v", err))
	}
	return hex.EncodeToString(b)
}
