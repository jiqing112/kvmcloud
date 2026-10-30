package coreapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const joinTTL = 10 * time.Minute

type InstallTokenRequest struct {
	ClusterID          uuid.UUID `json:"cluster_id"`
	ExpectedSPKISHA256 string    `json:"expected_spki_sha256"`
}

type JoinClaims struct {
	IssuanceID         uuid.UUID `json:"issuance_id"`
	NodeID             uuid.UUID `json:"node_id"`
	ClusterID          uuid.UUID `json:"cluster_id"`
	ExpectedSPKISHA256 string    `json:"expected_spki_sha256"`
	ExpiresAt          time.Time `json:"expires_at"`
}

var fingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)
var joinTokenFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var ErrJoinRejected = errors.New("join credential rejected")

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "kvmcloud:join:v1:" + hex.EncodeToString(sum[:])
}

// GenerateInstallToken never accepts host passwords, SSH keys or remote commands.
// It returns a command string for a human to run; no process is started here.
func (a *API) GenerateInstallToken(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("Referrer-Policy", "no-referrer")
	p, ok := principal(c, "nodes:enroll", false)
	if !ok {
		return
	}
	if c.GetHeader("Idempotency-Key") != "" {
		fail(c, http.StatusBadRequest, "secret_replay_not_supported")
		return
	}
	var request InstallTokenRequest
	if !strictJSON(c, &request) {
		return
	}
	if request.ClusterID == uuid.Nil ||
		request.ExpectedSPKISHA256 != "" && !fingerprint.MatchString(request.ExpectedSPKISHA256) {
		fail(c, http.StatusUnprocessableEntity, "invalid_enrollment")
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		fail(c, http.StatusServiceUnavailable, "entropy_unavailable")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	claims := JoinClaims{
		IssuanceID: uuid.New(), NodeID: uuid.New(), ClusterID: request.ClusterID,
		ExpectedSPKISHA256: request.ExpectedSPKISHA256, ExpiresAt: a.Now().UTC().Add(joinTTL),
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	tx, err := a.AdminDB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		databaseError(c, err)
		return
	}
	defer tx.Rollback()
	var clusterID string
	err = row(ctx, tx, query.Select("id").From("kvmcloud_core.clusters").
		Where(sq.Eq{"id": request.ClusterID.String()}), &clusterID)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "cluster_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	sum := sha256.Sum256([]byte(token))
	var expected any
	if request.ExpectedSPKISHA256 != "" {
		expected = request.ExpectedSPKISHA256
	}
	_, err = exec(ctx, tx, query.Insert("kvmcloud_core.node_join_issuances").
		Columns("id", "cluster_id", "node_id", "issued_by", "token_sha256", "expected_spki_sha256", "expires_at").
		Values(claims.IssuanceID.String(), clusterID, claims.NodeID.String(),
			p.ID.String(), sum[:], expected, claims.ExpiresAt))
	if err == nil {
		err = audit(ctx, tx, c, p, nil, "nodes.install-token", "node", claims.NodeID)
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	// Persist issuance/audit before exposing a usable token. A crash/cache
	// failure can leave an unusable issuance, never an unaudited credential.
	stored, err := a.Redis.SetNX(ctx, tokenKey(token), string(encoded), joinTTL)
	if err != nil || !stored {
		fail(c, http.StatusServiceUnavailable, "enrollment_unavailable")
		return
	}
	// Config URL/digest and base64url token are all restricted alphabets.
	// Verify the installer BEFORE executing it, in a fresh private directory.
	command := `d="$(mktemp -d)" && curl --fail --silent --show-error --proto '=https' --tlsv1.2 '` +
		a.InstallerURL + `' -o "$d/install.sh" && printf '%s  %s\n' '` + a.InstallerSHA256 +
		`' "$d/install.sh" | sha256sum --check --status && sudo bash "$d/install.sh" --token '` + token +
		`' --node-id '` + claims.NodeID.String() + `' --cluster-id '` + claims.ClusterID.String() + `'`
	c.JSON(http.StatusCreated, gin.H{
		"issuance_id": claims.IssuanceID, "node_id": claims.NodeID,
		"cluster_id": claims.ClusterID, "expires_at": claims.ExpiresAt,
		"install_command": command,
	})
}

// Do not implement consume as GET then DEL: two control-plane replicas race.
// Claim binding and consumption occur in one Redis operation.
const consumeJoinScript = `
local raw = redis.call('GET', KEYS[1])
if not raw then return false end
local claims = cjson.decode(raw)
if claims.node_id ~= ARGV[1] or claims.cluster_id ~= ARGV[2] then return false end
if claims.expected_spki_sha256 ~= '' and claims.expected_spki_sha256 ~= ARGV[3] then return false end
redis.call('DEL', KEYS[1])
return raw
`

// ConsumeJoinToken is called ONLY by the gRPC bootstrap service after TLS
// server authentication, CSR signature validation and key proof-of-possession.
// The service must ALSO atomically claim node_join_issuances.consumed_at and
// persist the node/SPKI before certificate issuance (see the design document).
func (a *API) ConsumeJoinToken(ctx context.Context, token string, nodeID, clusterID uuid.UUID, spkiSHA256 string) (*JoinClaims, error) {
	if !joinTokenFormat.MatchString(token) || nodeID == uuid.Nil || clusterID == uuid.Nil ||
		!fingerprint.MatchString(spkiSHA256) {
		return nil, ErrJoinRejected
	}
	result, err := a.Redis.Eval(ctx, consumeJoinScript, []string{tokenKey(token)},
		nodeID.String(), clusterID.String(), spkiSHA256)
	if errors.Is(err, ErrCacheMiss) {
		return nil, ErrJoinRejected
	}
	if err != nil {
		return nil, err
	}
	raw, ok := result.(string)
	if !ok || raw == "" {
		return nil, ErrJoinRejected
	}
	var claims JoinClaims
	if json.Unmarshal([]byte(raw), &claims) != nil || claims.IssuanceID == uuid.Nil ||
		claims.NodeID != nodeID || claims.ClusterID != clusterID ||
		!claims.ExpiresAt.After(a.Now()) {
		return nil, ErrJoinRejected
	}
	return &claims, nil
}
