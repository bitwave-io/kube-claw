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
	// ListAudit returns the most recent audit rows, newest first (for the UI).
	ListAudit(limit int) ([]AuditRecord, error)

	// CreateRun inserts a new run.
	CreateRun(r Run) error
	// GetRun returns a run by id, or ErrNotFound.
	GetRun(id string) (Run, error)
	// ListRuns returns the most recent runs, newest first.
	ListRuns(limit int) ([]Run, error)
	// ListRunsByPhase returns runs in a given phase, oldest first (FIFO).
	ListRunsByPhase(phase string, limit int) ([]Run, error)
	// ListRunsBySession returns runs in a session (Slack thread), oldest first.
	ListRunsBySession(sessionID string, limit int) ([]Run, error)
	// ClaimNextPendingTurn atomically claims the oldest Pending run in a session
	// (marking it Running on the given pod) so a warm session pod can pick up
	// follow-up turns. Returns ok=false if none are pending.
	ClaimNextPendingTurn(sessionID, pod string) (Run, bool, error)
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
	// ListSecrets returns all secret metadata (never values), for the admin UI.
	ListSecrets() ([]Secret, error)
	// DeleteSecret removes a secret and its versions, granters, and grants.
	DeleteSecret(namespace, name string) error
	// AddSecretVersion stores a new encrypted version.
	AddSecretVersion(v SecretVersion) error
	// LatestSecretVersion returns the newest version of a secret.
	LatestSecretVersion(secretID string) (SecretVersion, error)

	// CreateIntakeToken stores a one-time secret-intake token (hash only). runID
	// optionally links the token to the run that requested provisioning, so the
	// agent can be auto-resumed when the value is submitted.
	CreateIntakeToken(tokenHash, secretID, runID, expiresAt string) error
	// ConsumeIntakeToken validates + single-use-consumes a token, returning its
	// secret id and the linked run id (if any). Returns ErrNotFound for unknown,
	// ErrTokenUsed for already consumed, ErrTokenExpired for expired.
	ConsumeIntakeToken(tokenHash string) (secretID, runID string, err error)

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
	// GetPendingRequest returns the Pending request for an agent+secret, or ErrNotFound.
	GetPendingRequest(ns, agent, secretID string) (SecretRequest, error)
	// SetSecretRequestStatus updates a request's status.
	SetSecretRequestStatus(id, status string) error
	// MarkRequestNotified records that the approval was posted to Slack.
	MarkRequestNotified(id string) error

	// SeenEvent records a connector event id and reports whether it was already
	// seen (dedupe). Returns true if this is a DUPLICATE (DESIGN.md §12).
	SeenEvent(source, eventID string) (bool, error)

	// --- base image registry ---

	// CreateBaseImage registers (or replaces) a named base image.
	CreateBaseImage(b BaseImage) error
	// GetBaseImage returns a base image by name, or ErrNotFound.
	GetBaseImage(name string) (BaseImage, error)
	// ListBaseImages returns all registered base images.
	ListBaseImages() ([]BaseImage, error)

	// --- agent prompts (editable system prompts) ---

	// SetPrompt creates or replaces an agent's system prompt.
	SetPrompt(p Prompt) error
	// GetPrompt returns an agent's prompt, or ErrNotFound.
	GetPrompt(ns, name string) (Prompt, error)
	// ListPrompts returns all stored prompts.
	ListPrompts() ([]Prompt, error)

	// --- dynamic channel configs (Slack onboarding) ---

	// SetChannelConfig creates or replaces a channel's bot behavior.
	SetChannelConfig(c ChannelConfig) error
	// GetChannelConfig returns a channel's config, or ErrNotFound.
	GetChannelConfig(channel string) (ChannelConfig, error)
	// ListChannelConfigs returns all configured channels.
	ListChannelConfigs() ([]ChannelConfig, error)

	// --- connectors (external message sources, DESIGN.md connector plane) ---

	// CreateConnector registers an external connector.
	CreateConnector(c Connector) error
	// GetConnector returns a connector by id, or ErrNotFound.
	GetConnector(id string) (Connector, error)
	// GetConnectorByKeyHash returns the connector owning an API key hash, or
	// ErrNotFound. Key lookup happens by hash so the key itself is never stored.
	GetConnectorByKeyHash(hash string) (Connector, error)
	// ListConnectors returns all connectors.
	ListConnectors() ([]Connector, error)
	// SetConnectorKeyHash replaces a connector's API key hash (rotation).
	SetConnectorKeyHash(id, hash string) error
	// DeleteConnector removes a connector.
	DeleteConnector(id string) error

	// --- git repos (agent-requestable repositories, gitrepo plane) ---

	// CreateGitRepo registers a repository (URL + credentials + granters).
	CreateGitRepo(g GitRepo) error
	// GetGitRepo returns a repo by namespace/name (incl. granters), or ErrNotFound.
	GetGitRepo(namespace, name string) (GitRepo, error)
	// GetGitRepoByID returns a repo by id (incl. granters), or ErrNotFound.
	GetGitRepoByID(id string) (GitRepo, error)
	// ListGitRepos returns all repo metadata (never credentials), for the admin UI.
	ListGitRepos() ([]GitRepo, error)
	// DeleteGitRepo removes a repo and its granters, grants, and requests.
	DeleteGitRepo(namespace, name string) error

	// CreateGitRepoGrant stores a durable git-repo grant.
	CreateGitRepoGrant(g GitRepoGrant) error
	// FindValidGitRepoGrant returns a non-revoked grant matching the binding
	// (agent + repo + digest + spec hash), or ErrNotFound. Access level is on the
	// returned grant; callers check it with gitrepo.Satisfies.
	FindValidGitRepoGrant(ns, agent, repoID, digest, specHash string) (GitRepoGrant, error)
	// RevokeGitRepoGrant marks a git-repo grant revoked.
	RevokeGitRepoGrant(id, reason string) error
	// ListGitRepoGrants returns git-repo grants for an agent.
	ListGitRepoGrants(ns, agent string) ([]GitRepoGrant, error)

	// CreateGitRepoRequest stores a pending git-repo access request.
	CreateGitRepoRequest(req GitRepoRequest) error
	// GetGitRepoRequest returns a request by id, or ErrNotFound.
	GetGitRepoRequest(id string) (GitRepoRequest, error)
	// GetPendingGitRepoRequest returns the Pending request for an agent+repo at
	// the given access level, or ErrNotFound (dedupe).
	GetPendingGitRepoRequest(ns, agent, repoID, access string) (GitRepoRequest, error)
	// ListGitRepoRequests returns requests with the given status (all if "").
	ListGitRepoRequests(status string) ([]GitRepoRequest, error)
	// SetGitRepoRequestStatus updates a request's status.
	SetGitRepoRequestStatus(id, status string) error

	// --- install-wide settings (key/value, DESIGN.md §24.6) ---

	// SetSetting creates or replaces a setting.
	SetSetting(key, value string) error
	// GetSetting returns a setting's value, or ErrNotFound.
	GetSetting(key string) (string, error)
	// SetSettingIfUnset stores a setting only when the key has no value yet
	// (first-claim-wins). Returns true if this call set it.
	SetSettingIfUnset(key, value string) (bool, error)

	// --- artifacts (shareable documents, e.g. design docs) ---

	// CreateArtifact stores a published document (immutable once written).
	CreateArtifact(a Artifact) error
	// GetArtifact returns an artifact by id, or ErrNotFound.
	GetArtifact(id string) (Artifact, error)
	// CreateArtifactToken stores a time-bound share token (hash only). Unlike
	// intake tokens, share tokens are multi-read until they expire.
	CreateArtifactToken(tokenHash, artifactID, expiresAt string) error
	// ResolveArtifactToken returns the artifact for a live token hash. Returns
	// ErrNotFound for unknown, ErrTokenExpired for expired OR revoked (no oracle
	// distinguishing the two), plus the token's expiry for display.
	ResolveArtifactToken(tokenHash string) (Artifact, string, error)
	// ArtifactIDByTokenHash returns the artifact behind a share-token hash even
	// when the token is expired or revoked — an old link is the one durable
	// handle a rebuilt agent session still has, so reshare accepts it. Returns
	// ErrNotFound for an unknown hash.
	ArtifactIDByTokenHash(tokenHash string) (string, error)
	// ListArtifacts returns metadata (Content left empty) for a session's
	// published documents, oldest first. For session-less (CLI) runs pass
	// sessionID=="" and scoping falls back to the single runID.
	ListArtifacts(sessionID, runID string) ([]Artifact, error)
	// RevokeArtifactTokens revokes all live tokens for an artifact (reshare).
	RevokeArtifactTokens(artifactID string) error

	// --- models (the LLM registry: UI-managed providers/endpoints/keys) ---

	// UpsertModel creates or replaces a named model configuration.
	UpsertModel(m Model) error
	// GetModel returns a model by name, or ErrNotFound.
	GetModel(name string) (Model, error)
	// ListModels returns all configured models, default first then by name.
	ListModels() ([]Model, error)
	// DeleteModel removes a model (and any session overrides pointing at it).
	DeleteModel(name string) error
	// SetDefaultModel marks one model as the install default (clears others
	// atomically). ErrNotFound if the model doesn't exist.
	SetDefaultModel(name string) error
	// GetDefaultModel returns the default model, or ErrNotFound when none set.
	GetDefaultModel() (Model, error)
	// SetSessionModel pins a session (Slack thread) to a named model.
	SetSessionModel(sessionID, modelName, setBy string) error
	// GetSessionModel returns the session's pinned model name, or ErrNotFound.
	GetSessionModel(sessionID string) (string, error)

	// --- schedules (cron-triggered agent invocations) ---

	// SetSchedule creates or replaces a schedule.
	SetSchedule(s Schedule) error
	// GetSchedule returns a schedule by id, or ErrNotFound.
	GetSchedule(id string) (Schedule, error)
	// ListSchedules returns all schedules.
	ListSchedules() ([]Schedule, error)
	// DeleteSchedule removes a schedule.
	DeleteSchedule(id string) error
}

// Well-known settings keys (the settings table is a KV; these are the keys the
// control plane itself uses — the self-update plane, DESIGN.md §24).
const (
	// SettingUpgradeAdmin is the Slack user id asked to approve upgrades.
	// Claimed at onboarding (first-claim-wins) or set via `claw settings set`.
	SettingUpgradeAdmin = "upgrade_admin_slack_user"
	// SettingSkippedVersion is a release version the admin chose to skip;
	// it is never re-prompted.
	SettingSkippedVersion = "upgrade_skipped_version"
	// SettingNotifiedVersion is the last availableVersion announced in Slack,
	// so detection doesn't re-DM on every poll.
	SettingNotifiedVersion = "upgrade_notified_version"
	// SettingMgmtChannel is the Slack channel where releases and upgrade
	// lifecycle events (available/applied/rolled back) are announced.
	SettingMgmtChannel = "management_channel"
	// SettingRemindAfter is an RFC3339 time before which the upgrade prompt is
	// suppressed ("Remind me later").
	SettingRemindAfter = "upgrade_remind_after"
)

// Schedule is a cron-triggered agent invocation: at each cron occurrence the
// scheduler creates a run for the agent with Prompt as input and posts the answer
// to the Slack channel. DB-backed (like prompts/channel configs), not a CRD.
type Schedule struct {
	ID             string `json:"id"`
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	Cron           string `json:"cron"`    // standard 5-field cron, e.g. "0 9 * * *"
	Prompt         string `json:"prompt"`  // the input given to the agent each run
	Channel        string `json:"channel"` // Slack channel id to post the answer to
	Enabled        bool   `json:"enabled"`
	LastRunAt      string `json:"lastRunAt,omitempty"`
	NextRunAt      string `json:"nextRunAt,omitempty"`
	CreatedAt      string `json:"createdAt"`
}

// Connector is a registered external message source (a SaaS integration, a
// gateway service, a web-chat backend). It POSTs inbound messages to its ingest
// URL, authenticated by API key, and receives run outputs at CallbackURL,
// signed with SigningSecret. The key is stored only as a SHA-256 hash; the
// signing secret is returned once at creation and held for outbound signing.
type Connector struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	CallbackURL    string `json:"callbackUrl"`
	APIKeyHash     string `json:"-"`
	SigningSecret  string `json:"-"`
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	Disabled       bool   `json:"disabled"`
	CreatedAt      string `json:"createdAt"`
}

// GitRepo is a registered git repository an agent can request access to (the
// gitrepo plane). It stores its own credentials — a read credential and/or a
// write credential (deploy keys, PATs) — which are returned only to an agent
// holding a matching grant, and are never serialized to JSON. Granters are the
// principals who may approve access requests (PAM, mirroring Secret.Granters).
type GitRepo struct {
	ID              string   `json:"id"`
	Namespace       string   `json:"namespace"`
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	Description     string   `json:"description,omitempty"` // usage context for the agent (never a credential)
	ReadCredential  string   `json:"-"`                     // materialized for read grants (empty = none registered)
	WriteCredential string   `json:"-"`                     // materialized for write grants (empty = none registered)
	Granters        []string `json:"granters,omitempty"`    // who may approve access (DESIGN.md §8)
	CreatedAt       string   `json:"createdAt"`
}

// HasReadCredential / HasWriteCredential report whether a credential is
// registered for the given access level.
func (g GitRepo) HasReadCredential() bool  { return g.ReadCredential != "" }
func (g GitRepo) HasWriteCredential() bool { return g.WriteCredential != "" }

// GitRepoGrant is a durable authorization to access a repository at a given
// access level. Like a secret Grant, it has no expiry — valid until revoked or
// until the image digest / spec hash it binds to changes (DESIGN.md §8, §14).
type GitRepoGrant struct {
	ID             string `json:"id"`
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	ImageDigest    string `json:"imageDigest"`
	AgentSpecHash  string `json:"agentSpecHash"`
	GitRepoID      string `json:"gitRepoId"`
	Access         string `json:"access"` // read|write (write implies read)
	ApprovedBy     string `json:"approvedBy"`
	ApprovedAt     string `json:"approvedAt"`
	Reason         string `json:"reason,omitempty"`
	RevokedAt      string `json:"revokedAt,omitempty"`
	RevokedReason  string `json:"revokedReason,omitempty"`
}

// GitRepoRequest is a pending git-repo access approval (mirrors SecretRequest).
type GitRepoRequest struct {
	ID             string `json:"id"`
	Status         string `json:"status"` // Pending|Approved|Denied
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	RunID          string `json:"runId,omitempty"`
	GitRepoID      string `json:"gitRepoId"`
	RepoName       string `json:"repoName,omitempty"`
	Access         string `json:"access"` // requested level: read|write
	ImageDigest    string `json:"imageDigest"`
	Context        string `json:"context,omitempty"`     // the agent's justification ("why") for the approver
	RequestedBy    string `json:"requestedBy,omitempty"` // Slack user the run is for ("who")
	CreatedAt      string `json:"createdAt"`
	NotifiedAt     string `json:"notifiedAt,omitempty"`
}

// ChannelConfig is per-Slack-channel bot behavior, set via the onboarding flow
// when the bot is added to a channel. It is a dynamic routing rule (alongside
// the static Helm routes).
type ChannelConfig struct {
	Channel         string
	AgentNamespace  string
	AgentName       string
	MentionRequired bool // true = only @mentions trigger; false = active participant
	ThreadOnly      bool // true = reply only in threads; false = may reply in-channel
	UpdatedAt       string
}

// Prompt is an editable system prompt for an agent (DESIGN.md §agent-loop). It
// seeds from the Agent CRD's model.systemPrompt and is editable via API/UI; the
// run engine resolves the current prompt at launch.
type Prompt struct {
	AgentNamespace string
	AgentName      string
	Content        string
	UpdatedAt      string
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

// AuditRecord is a stored audit row read back for display (the value-free,
// hash-chained log). Detail is the raw JSON detail string.
type AuditRecord struct {
	TS       string `json:"ts"`
	Type     string `json:"type"`
	RunID    string `json:"runId,omitempty"`
	GrantID  string `json:"grantId,omitempty"`
	SecretID string `json:"secretId,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Detail   string `json:"detail,omitempty"`
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

// Artifact is a document an agent published for sharing outside Slack (e.g. a
// design doc handed to another tool). Content is immutable once published —
// a revised doc is a new artifact. Access goes through time-bound share tokens
// (artifact_tokens); the artifact itself never expires, so a spent link can be
// reshared without regenerating the document.
type Artifact struct {
	ID        string `json:"id"`
	RunID     string `json:"runId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Title     string `json:"title"`
	Content   string `json:"content"` // markdown; never secret material
	CreatedAt string `json:"createdAt"`
}

// Output is a single result a run produced (DESIGN.md §22 status.outputs).
type Output struct {
	Kind      string `json:"kind"`    // e.g. "text", "slackMessage"
	Content   string `json:"content"` // never secret material
	CreatedAt string `json:"createdAt"`
}

// Secret is secret metadata (the value lives in SecretVersion, encrypted).
type Secret struct {
	ID          string   `json:"id"`
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"` // usage context for the agent (never the value)
	Granters    []string `json:"granters,omitempty"`    // PAM: who may approve (DESIGN.md §8)
	CreatedAt   string   `json:"createdAt"`
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
	Context        string `json:"context,omitempty"`     // the agent's justification ("why") for the approver
	RequestedBy    string `json:"requestedBy,omitempty"` // Slack user the run is for ("who")
	CreatedAt      string `json:"createdAt"`
	NotifiedAt     string `json:"notifiedAt,omitempty"` // when the approval was posted to Slack (empty = not yet)
}

// BaseImage is a registered, reusable agent runtime image (DESIGN.md §23). The
// description tells operators/agents WHEN to use this base (e.g. "has gcloud +
// bq, for GCP cost/billing agents").
type BaseImage struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"createdAt"`
}

// Model is one UI-managed LLM configuration the agent loop can run on:
// Anthropic, OpenAI, or any OpenAI-compatible endpoint (vLLM, Ollama,
// OpenRouter, …) via BaseURL. The API key is AEAD-encrypted at rest (the
// models service owns encrypt/decrypt); it is never rendered back to the UI.
type Model struct {
	Name     string `json:"name"`     // unique handle, e.g. "opus", "gpt5", "local-llama"
	Provider string `json:"provider"` // "anthropic" | "openai" (OpenAI-compatible wire format)
	ModelID  string `json:"modelId"`  // provider model id, e.g. claude-opus-4-8, gpt-5.2
	// BaseURL overrides the provider's default endpoint (self-hosted /
	// gateway). "" = provider default.
	BaseURL string `json:"baseUrl,omitempty"`
	// APIKeyCiphertext is the AEAD-encrypted API key; empty for keyless
	// self-hosted endpoints. Never serialized to JSON.
	APIKeyCiphertext []byte `json:"-"`
	Notes            string `json:"notes,omitempty"`
	IsDefault        bool   `json:"isDefault"`
	UpdatedAt        string `json:"updatedAt"`
}

// ModelProviders are the accepted Model.Provider values.
var ModelProviders = []string{"anthropic", "openai"}

// NowRFC3339 is the canonical timestamp format used for stored rows.
func NowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
