package coreapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var (
	testTenant  = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	testActor   = uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	testCluster = uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	testNode    = uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	testImage   = uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	testNetwork = uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	testVM      = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	testNow     = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
)

type sqlStep struct {
	query    bool
	contains string
	rows     [][]driver.Value
	affected int64
	err      error
	check    func([]driver.NamedValue)
}

// Scripted driver checks statement order and that ALL SQL runs inside one tx.
// It intentionally does not claim to emulate PostgreSQL concurrency or RLS.
type sqlScript struct {
	t                       *testing.T
	steps                   []sqlStep
	active                  bool
	begins, commits, aborts int
	commitErr               error
}

type scriptConnector struct{ s *sqlScript }
type scriptDriver struct{ s *sqlScript }
type scriptConn struct{ s *sqlScript }
type scriptTx struct{ s *sqlScript }
type scriptRows struct {
	values [][]driver.Value
	count  int
}

func (c scriptConnector) Connect(context.Context) (driver.Conn, error) { return scriptConn{c.s}, nil }
func (c scriptConnector) Driver() driver.Driver                        { return scriptDriver{c.s} }
func (d scriptDriver) Open(string) (driver.Conn, error)                { return scriptConn{d.s}, nil }
func (c scriptConn) Prepare(string) (driver.Stmt, error)               { return nil, errors.New("not supported") }
func (c scriptConn) Close() error                                      { return nil }
func (c scriptConn) Begin() (driver.Tx, error) {
	c.s.active = true
	c.s.begins++
	return scriptTx{c.s}, nil
}
func (c scriptConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}
func (t scriptTx) Commit() error {
	t.s.active = false
	t.s.commits++
	return t.s.commitErr
}
func (t scriptTx) Rollback() error {
	t.s.active = false
	t.s.aborts++
	return nil
}
func (s *sqlScript) next(sqlText string, args []driver.NamedValue, isQuery bool) (sqlStep, error) {
	s.t.Helper()
	if !s.active || len(s.steps) == 0 {
		s.t.Errorf("unexpected SQL outside the expected transaction: %s", sqlText)
		return sqlStep{}, errors.New("unexpected SQL")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if step.query != isQuery || !strings.Contains(sqlText, step.contains) {
		s.t.Errorf("expected query=%v %q, got query=%v %q", step.query, step.contains, isQuery, sqlText)
		return sqlStep{}, errors.New("unexpected statement")
	}
	if strings.Contains(sqlText, "web-01") || strings.Contains(sqlText, "test-secret") {
		s.t.Errorf("unbound user input in SQL: %s", sqlText)
	}
	if step.check != nil {
		step.check(args)
	}
	return step, step.err
}
func (c scriptConn) ExecContext(_ context.Context, text string, args []driver.NamedValue) (driver.Result, error) {
	step, err := c.s.next(text, args, false)
	return driver.RowsAffected(step.affected), err
}
func (c scriptConn) QueryContext(_ context.Context, text string, args []driver.NamedValue) (driver.Rows, error) {
	step, err := c.s.next(text, args, true)
	count := 1
	if len(step.rows) > 0 {
		count = len(step.rows[0])
	}
	return &scriptRows{values: step.rows, count: count}, err
}
func (r *scriptRows) Columns() []string {
	names := make([]string, r.count)
	for i := range names {
		names[i] = "column"
	}
	return names
}
func (r *scriptRows) Close() error { return nil }
func (r *scriptRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

type memoryRedis struct {
	mu    sync.Mutex
	items map[string]string
	gets  int
	sets  int
	evals int
	err   error
}

func (r *memoryRedis) Get(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gets++
	if r.err != nil {
		return "", r.err
	}
	value, exists := r.items[key]
	if !exists {
		return "", ErrCacheMiss
	}
	return value, nil
}
func (r *memoryRedis) SetNX(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets++
	if r.err != nil {
		return false, r.err
	}
	if ttl != joinTTL {
		return false, errors.New("unexpected TTL")
	}
	if _, exists := r.items[key]; exists {
		return false, nil
	}
	r.items[key] = value
	return true, nil
}
func (r *memoryRedis) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evals++
	if r.err != nil {
		return nil, r.err
	}
	if script != consumeJoinScript || len(keys) != 1 || len(args) != 3 {
		return nil, errors.New("unexpected Lua script")
	}
	raw, exists := r.items[keys[0]]
	if !exists {
		return nil, ErrCacheMiss
	}
	var claims JoinClaims
	if err := json.Unmarshal([]byte(raw), &claims); err != nil {
		return nil, err
	}
	if claims.NodeID.String() != args[0] || claims.ClusterID.String() != args[1] ||
		claims.ExpectedSPKISHA256 != "" && claims.ExpectedSPKISHA256 != args[2] {
		return nil, ErrCacheMiss
	}
	delete(r.items, keys[0])
	return raw, nil
}

func testAPI(t *testing.T, steps []sqlStep, scopes ...string) (*API, *sqlScript, *memoryRedis) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	script := &sqlScript{t: t, steps: steps}
	db := sql.OpenDB(scriptConnector{script})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		db.Close()
		if len(script.steps) != 0 {
			t.Errorf("%d unconsumed SQL steps", len(script.steps))
		}
	})
	cache := &memoryRedis{items: make(map[string]string)}
	granted := make(map[string]bool)
	for _, scope := range scopes {
		granted[scope] = true
	}
	api, err := New(db, db, cache, func(context.Context, *http.Request) (Principal, error) {
		return Principal{ID: testActor, TenantID: testTenant, Scopes: granted}, nil
	}, "https://control.example/releases/pinned/install.sh", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	api.Now = func() time.Time { return testNow }
	return api, script, cache
}

func requestAPI(a *API, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	recorder := httptest.NewRecorder()
	a.Router().ServeHTTP(recorder, request)
	return recorder
}

func vmRequest() CreateVMRequest {
	return CreateVMRequest{Name: "web-01", ImageID: testImage, NetworkID: testNetwork,
		VCPUs: 2, MemoryMiB: 2048, RootDiskGiB: 40,
		Bandwidth: BandwidthSpec{MinKbps: 1000, MaxKbps: 10000, Priority: 50}}
}

func vmBody() string {
	body, _ := json.Marshal(vmRequest())
	return string(body)
}

func preflight() []sqlStep {
	return []sqlStep{
		{contains: "set_config", affected: 1},
		{query: true, contains: "kvmcloud_core.tenants", rows: [][]driver.Value{{testTenant.String()}}},
		{contains: "INSERT INTO kvmcloud_core.idempotency_keys", affected: 1},
		{query: true, contains: "kvmcloud_core.networks", rows: [][]driver.Value{{testCluster.String()}}},
		{query: true, contains: "kvmcloud_core.images", rows: [][]driver.Value{{testImage.String()}}},
	}
}

func creationSteps() []sqlStep {
	steps := append(preflight(),
		sqlStep{contains: "UPDATE kvmcloud_core.tenant_quotas", affected: 1},
		sqlStep{query: true, contains: "FOR UPDATE SKIP LOCKED", rows: [][]driver.Value{{testNode.String()}}},
		sqlStep{contains: "UPDATE kvmcloud_core.nodes", affected: 1})
	for _, relation := range []string{"vms", "vm_nics", "bandwidth_policies",
		"resource_reservations", "volumes", "tasks", "outbox_events", "audit_logs"} {
		steps = append(steps, sqlStep{contains: "INSERT INTO kvmcloud_core." + relation, affected: 1})
	}
	return append(steps, sqlStep{contains: "UPDATE kvmcloud_core.idempotency_keys", affected: 1})
}

func TestCreateVMSingleTransaction(t *testing.T) {
	a, script, cache := testAPI(t, creationSteps(), "vms:write")
	response := requestAPI(a, http.MethodPost, "/api/v1/vms", vmBody(), "create-vm-test-001")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if script.begins != 1 || script.commits != 1 || script.aborts != 0 || cache.sets != 0 {
		t.Fatalf("not one atomic SQL transaction: %+v", script)
	}
	if !strings.HasPrefix(response.Header().Get("Location"), "/api/v1/tasks/") {
		t.Fatal("missing task Location")
	}
}

func TestCreateVMQuotaFailureRollsBack(t *testing.T) {
	steps := append(preflight(), sqlStep{contains: "UPDATE kvmcloud_core.tenant_quotas", affected: 0})
	a, script, _ := testAPI(t, steps, "vms:write")
	response := requestAPI(a, http.MethodPost, "/api/v1/vms", vmBody(), "create-vm-test-002")
	if response.Code != http.StatusConflict || script.commits != 0 || script.aborts != 1 {
		t.Fatalf("status=%d commits=%d rollbacks=%d", response.Code, script.commits, script.aborts)
	}
}

func TestCreateVMMetadataFailureRollsBackReservation(t *testing.T) {
	steps := creationSteps()[:9]
	steps[8].err = errors.New("private SQL failure test-secret")
	a, script, _ := testAPI(t, steps, "vms:write")
	response := requestAPI(a, http.MethodPost, "/api/v1/vms", vmBody(), "create-vm-test-003")
	if response.Code != http.StatusServiceUnavailable || script.commits != 0 || script.aborts != 1 ||
		strings.Contains(response.Body.String(), "test-secret") {
		t.Fatalf("metadata failure leaked or did not roll back: %s", response.Body.String())
	}
}

func TestCreateVMCommitUnknownDoesNotCompensate(t *testing.T) {
	a, script, cache := testAPI(t, creationSteps(), "vms:write")
	script.commitErr = errors.New("lost commit acknowledgement")
	response := requestAPI(a, http.MethodPost, "/api/v1/vms", vmBody(), "create-vm-test-004")
	if response.Code != http.StatusServiceUnavailable || script.commits != 1 || cache.sets != 0 {
		t.Fatal("ambiguous COMMIT must be retried with the same key, without external effects")
	}
}

func TestIdempotencyReplaysBeforeReservation(t *testing.T) {
	_, hash, _ := digestJSON(vmRequest())
	responseBody := `{"vm_id":"` + testVM.String() + `","task_id":"` + testNode.String() + `","state":"pending"}`
	steps := []sqlStep{
		{contains: "set_config", affected: 1},
		{query: true, contains: "kvmcloud_core.tenants", rows: [][]driver.Value{{testTenant.String()}}},
		{contains: "INSERT INTO kvmcloud_core.idempotency_keys", affected: 0},
		{query: true, contains: "FOR UPDATE", rows: [][]driver.Value{
			{hash, int64(202), []byte(responseBody), "/api/v1/tasks/" + testNode.String(), testNow.Add(time.Hour)},
		}},
	}
	a, script, _ := testAPI(t, steps, "vms:write")
	response := requestAPI(a, http.MethodPost, "/api/v1/vms", vmBody(), "create-vm-test-005")
	if response.Code != http.StatusAccepted || response.Body.String() != responseBody ||
		response.Header().Get("Idempotency-Replayed") != "true" || script.commits != 1 {
		t.Fatalf("incorrect idempotent response: %s", response.Body.String())
	}
}

func TestCreateVMRejectsCredentialsAndTrailingJSON(t *testing.T) {
	for _, extra := range []string{`,"password":"secret"}`, `,"ssh_credentials":{}}`, `,"command":"id"}`} {
		a, script, _ := testAPI(t, nil, "vms:write")
		response := requestAPI(a, http.MethodPost, "/api/v1/vms",
			strings.TrimSuffix(vmBody(), "}")+extra, "create-vm-test-006")
		if response.Code != http.StatusBadRequest || script.begins != 0 {
			t.Fatal("credential/command field accepted")
		}
	}
	a, _, _ := testAPI(t, nil, "vms:write")
	if requestAPI(a, http.MethodPost, "/api/v1/vms", vmBody()+" {}", "create-vm-test-007").Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
}

func TestScopesFailClosed(t *testing.T) {
	a, script, _ := testAPI(t, nil)
	response := requestAPI(a, http.MethodPost, "/api/v1/vms", vmBody(), "create-vm-test-008")
	if response.Code != http.StatusForbidden || script.begins != 0 {
		t.Fatal("missing scope did not fail closed")
	}
}

func TestHighPriorityRequiresEntitlement(t *testing.T) {
	a, script, _ := testAPI(t, nil, "vms:write")
	request := vmRequest()
	request.Bandwidth.Priority = 100
	body, _ := json.Marshal(request)
	response := requestAPI(a, http.MethodPost, "/api/v1/vms", string(body), "create-vm-test-009")
	if response.Code != http.StatusForbidden || script.begins != 0 {
		t.Fatal("high-priority scheduling granted without entitlement")
	}
}

func TestInstallTokenHasNoPlaintextPersistence(t *testing.T) {
	steps := []sqlStep{
		{query: true, contains: "kvmcloud_core.clusters", rows: [][]driver.Value{{testCluster.String()}}},
		{contains: "INSERT INTO kvmcloud_core.node_join_issuances", affected: 1, check: func(args []driver.NamedValue) {
			if digest, ok := args[4].Value.([]byte); !ok || len(digest) != 32 {
				t.Fatal("token must be persisted only as a SHA-256 digest")
			}
		}},
		{contains: "INSERT INTO kvmcloud_core.audit_logs", affected: 1},
	}
	a, script, cache := testAPI(t, steps, "nodes:enroll")
	response := requestAPI(a, http.MethodPost, "/api/v1/nodes/install-token",
		`{"cluster_id":"`+testCluster.String()+`"}`, "")
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" ||
		script.commits != 1 || cache.sets != 1 || len(cache.items) != 1 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		InstallCommand string `json:"install_command"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	pieces := strings.Split(result.InstallCommand, "--token '")
	if len(pieces) != 2 {
		t.Fatal("installer token missing")
	}
	token := strings.SplitN(pieces[1], "'", 2)[0]
	if !joinTokenFormat.MatchString(token) || !strings.Contains(result.InstallCommand, "sha256sum --check") {
		t.Fatal("weak token or unverified installer")
	}
	for key, value := range cache.items {
		if key != tokenKey(token) || strings.Contains(value, token) {
			t.Fatal("Redis persisted plaintext token")
		}
	}
}

func TestJoinTokenConcurrentSingleUseAndBinding(t *testing.T) {
	a, _, cache := testAPI(t, nil)
	token := strings.Repeat("A", 43)
	spki := strings.Repeat("b", 64)
	claims := JoinClaims{IssuanceID: uuid.New(), NodeID: testNode, ClusterID: testCluster,
		ExpectedSPKISHA256: spki, ExpiresAt: testNow.Add(time.Minute)}
	raw, _ := json.Marshal(claims)
	cache.items[tokenKey(token)] = string(raw)
	if _, err := a.ConsumeJoinToken(context.Background(), token, testVM, testCluster, spki); !errors.Is(err, ErrJoinRejected) {
		t.Fatal("wrong binding consumed token")
	}
	var wg sync.WaitGroup
	var successes int
	var mu sync.Mutex
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.ConsumeJoinToken(context.Background(), token, testNode, testCluster, spki); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("expected exactly one successful consume, got %d", successes)
	}
}

func metricSteps(owned bool) []sqlStep {
	steps := []sqlStep{{contains: "set_config", affected: 1}, {query: true, contains: "kvmcloud_core.vms"}}
	if owned {
		steps[1].rows = [][]driver.Value{{testVM.String()}}
	}
	return steps
}

func TestMetricsAuthorizationPrecedesCache(t *testing.T) {
	a, script, cache := testAPI(t, metricSteps(false), "vms:read")
	response := requestAPI(a, http.MethodGet, "/api/v1/vms/"+testVM.String()+"/bandwidth-metrics", "", "")
	if response.Code != http.StatusNotFound || cache.gets != 0 || script.aborts != 1 {
		t.Fatal("unauthorized resource read from cache")
	}
}

func TestMetricsCacheMissIsNotZeroTraffic(t *testing.T) {
	a, _, cache := testAPI(t, metricSteps(true), "vms:read")
	response := requestAPI(a, http.MethodGet, "/api/v1/vms/"+testVM.String()+"/bandwidth-metrics", "", "")
	if response.Code != http.StatusServiceUnavailable || cache.gets != 1 {
		t.Fatalf("cache miss became fake data: %s", response.Body.String())
	}
}

func TestMetricsStaleness(t *testing.T) {
	a, script, cache := testAPI(t, metricSteps(true), "vms:read")
	collected := testNow.Add(-time.Minute)
	metrics := BandwidthMetrics{CollectedAt: collected, Samples: []BandwidthSample{
		{At: collected, IngressKbps: 1, EgressKbps: 2, EgressBytes: "9007199254740993"},
	}}
	raw, _ := json.Marshal(metrics)
	cache.items[metricsKey(testTenant, testVM)] = string(raw)
	response := requestAPI(a, http.MethodGet, "/api/v1/vms/"+testVM.String()+"/bandwidth-metrics", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"stale":true`) ||
		!strings.Contains(response.Body.String(), `"9007199254740993"`) || script.commits != 1 {
		t.Fatalf("incorrect cached metrics: %s", response.Body.String())
	}
}

func TestInstallerConfigurationRejectsInjection(t *testing.T) {
	a, _, _ := testAPI(t, nil)
	for _, url := range []string{
		"http://control.example/install.sh",
		"https://control.example/install.sh?cmd=anything",
		"https://control.example/install.sh';touch /tmp/bad;'",
		"https://user:password@control.example/install.sh",
	} {
		if _, err := New(a.DB, a.AdminDB, a.Redis, a.Authenticate, url, strings.Repeat("a", 64)); err == nil {
			t.Fatalf("unsafe installer URL accepted: %s", url)
		}
	}
}
