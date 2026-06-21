// Package store is the controller's persistence boundary.
//
// The v0 default implementation is SQLite (internal/store/sqlite); Postgres or
// Spanner can implement the same interface, which is also the HA path
// (DESIGN.md §7).
//
// INVARIANT: every secret-state mutation writes its audit row in the SAME
// transaction as the change. Callers mutate secret state only through Tx
// repository methods, so "forgot to audit" cannot compile. The audit log is
// hash-chained (tamper-evident), not merely insert-only.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by read methods when the row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrTokenUsed / ErrTokenExpired are returned by ConsumeIntakeToken.
var (
	ErrTokenUsed    = errors.New("store: intake token already used")
	ErrTokenExpired = errors.New("store: intake token expired")
)

// Store is the persistence interface backing the controller.
type Store interface {
	// Tx runs fn inside a single (serializable) transaction. fn returning a
	// non-nil error rolls back; nil commits.
	Tx(ctx context.Context, fn func(Tx) error) error

	// Migrate applies pending schema migrations idempotently.
	Migrate(ctx context.Context) error

	// Close releases the underlying handle.
	Close() error
}

// Tx is the transactional repository surface. Typed methods are added alongside
// their features. Secret/grant/request methods land in Phases 3-4.
type Tx interface {
	// AppendAudit writes a hash-chained, tamper-evident audit row.
	AppendAudit(ev AuditEvent) error

	// CreateRun inserts a new run.
	CreateRun(r Run) error
	// GetRun returns a run by id, or ErrNotFound.
	GetRun(id string) (Run, error)
	// ListRuns returns the most recent runs, newest first.
	ListRuns(limit int) ([]Run, error)
	// ListRunsByPhase returns runs in a given phase, oldest first (FIFO).
	ListRunsByPhase(phase string, limit int) ([]Run, error)
	// MarkRunRunning sets phase=Running, assigned pod, and started_at.
	MarkRunRunning(id, pod string) error
	// MarkRunBlocked sets phase=Blocked (awaiting secret approval).
	MarkRunBlocked(id string) error
	// MarkRunSucceeded sets phase=Succeeded and completed_at.
	MarkRunSucceeded(id string) error
	// MarkRunFailed sets phase=Failed and completed_at.
	MarkRunFailed(id string) error

	// AppendOutput records an output produced by a run.
	AppendOutput(runID string, out Output) error
	// ListOutputs returns a run's outputs, oldest first.
	ListOutputs(runID string) ([]Output, error)

	// --- secrets (Phase 3) ---

	// CreateSecret inserts secret metadata + granters.
	CreateSecret(s Secret) error
	// GetSecret returns secret metadata (incl. granters) by namespace/name.
	GetSecret(namespace, name string) (Secret, error)
	// AddSecretVersion stores a new encrypted version.
	AddSecretVersion(v SecretVersion) error
	// LatestSecretVersion returns the newest version of a secret.
	LatestSecretVersion(secretID string) (SecretVersion, error)

	// CreateIntakeToken stores a one-time secret-intake token (hash only).
	CreateIntakeToken(tokenHash, secretID, expiresAt string) error
	// ConsumeIntakeToken validates + single-use-consumes a token, returning its
	// secret id. Returns ErrNotFound for unknown, ErrTokenUsed for already
	// consumed, ErrTokenExpired for expired.
	ConsumeIntakeToken(tokenHash string) (secretID string, err error)

	// --- grants & requests (Phase 4) ---

	// CreateGrant stores a durable grant.
	CreateGrant(g Grant) error
	// FindValidGrant returns a non-revoked grant matching the full binding
	// (agent + secret + digest + spec hash + delivery hash), or ErrNotFound.
	FindValidGrant(ns, agent, secretID, digest, specHash, deliveryHash string) (Grant, error)
	// RevokeGrant marks a grant revoked.
	RevokeGrant(id, reason string) error
	// ListGrants returns grants for an agent.
	ListGrants(ns, agent string) ([]Grant, error)

	// CreateSecretRequest stores a pending approval request.
	CreateSecretRequest(req SecretRequest) error
	// GetSecretRequest returns a request by id, or ErrNotFound.
	GetSecretRequest(id string) (SecretRequest, error)
	// ListSecretRequests returns requests with the given status (all if "").
	ListSecretRequests(status string) ([]SecretRequest, error)
	// PendingRequestExists reports whether a Pending request already exists for
	// this agent+secret (dedupe).
	PendingRequestExists(ns, agent, secretID string) (bool, error)
	// SetSecretRequestStatus updates a request's status.
	SetSecretRequestStatus(id, status string) error

	// SeenEvent records a connector event id and reports whether it was already
	// seen (dedupe). Returns true if this is a DUPLICATE (DESIGN.md §12).
	SeenEvent(source, eventID string) (bool, error)
}

// AuditEvent is one append-only audit record (DESIGN.md §21).
type AuditEvent struct {
	Type     string         // e.g. "secret.created", "agentrun.created"
	RunID    string         // optional
	GrantID  string         // optional
	SecretID string         // optional
	Actor    string         // optional
	Detail   map[string]any // optional structured detail (never secret values)
}

// Run is the unit of work and audit visibility (DESIGN.md §22). Source/Input are
// opaque JSON strings owned by the caller.
type Run struct {
	ID             string `json:"id"`
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	SessionID      string `json:"sessionId,omitempty"`
	Phase          string `json:"phase"` // Pending|Blocked|Waking|Running|Succeeded|Failed
	Source         string `json:"source,omitempty"`
	Input          string `json:"input,omitempty"`
	AssignedPod    string `json:"assignedPod,omitempty"`
	PodUID         string `json:"podUid,omitempty"`
	CreatedAt      string `json:"createdAt"`
	StartedAt      string `json:"startedAt,omitempty"`
	CompletedAt    string `json:"completedAt,omitempty"`
}

// Output is a single result a run produced (DESIGN.md §22 status.outputs).
type Output struct {
	Kind      string `json:"kind"`    // e.g. "text", "slackMessage"
	Content   string `json:"content"` // never secret material
	CreatedAt string `json:"createdAt"`
}

// Secret is secret metadata (the value lives in SecretVersion, encrypted).
type Secret struct {
	ID        string   `json:"id"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Type      string   `json:"type,omitempty"`
	Granters  []string `json:"granters,omitempty"` // PAM: who may approve (DESIGN.md §8)
	CreatedAt string   `json:"createdAt"`
}

// SecretVersion is one immutable, encrypted version of a secret's value.
type SecretVersion struct {
	ID         string
	SecretID   string
	Ciphertext []byte // Tink-encrypted; never logged
	Checksum   string // sha256 of plaintext, for integrity checks
	CreatedAt  string
	CreatedBy  string
}

// Grant is a durable authorization (DESIGN.md §8, §14). No expiry/lease — it is
// valid until revoked or until the image digest / spec hash / delivery hash it
// binds to changes.
type Grant struct {
	ID             string `json:"id"`
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	ImageDigest    string `json:"imageDigest"`
	AgentSpecHash  string `json:"agentSpecHash"`
	DeliveryHash   string `json:"deliveryHash"`
	SecretID       string `json:"secretId"`
	ApprovedBy     string `json:"approvedBy"`
	ApprovedAt     string `json:"approvedAt"`
	Reason         string `json:"reason,omitempty"`
	RevokedAt      string `json:"revokedAt,omitempty"`
	RevokedReason  string `json:"revokedReason,omitempty"`
}

// SecretRequest is a pending approval (DESIGN.md §16).
type SecretRequest struct {
	ID             string `json:"id"`
	Status         string `json:"status"` // Pending|Approved|Denied
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	RunID          string `json:"runId,omitempty"`
	SecretID       string `json:"secretId"`
	SecretName     string `json:"secretName,omitempty"`
	ImageDigest    string `json:"imageDigest"`
	Context        string `json:"context,omitempty"`
	CreatedAt      string `json:"createdAt"`
}

// NowRFC3339 is the canonical timestamp format used for stored rows.
func NowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
