// Package store 提供 PostgreSQL 访问：节点四维超开、端口策略、共享池、告警。
//
// 安全与并发设计：
//   - 源码零 SQL 文本：全部语句由 squirrel 构建器生成，参数一律绑定占位符；
//     DDL 为内嵌静态文件（migration.sql），运行期无用户输入参与。
//   - 节点四维预留用单条原子条件 UPDATE（校验与写入同语句，天然行级一致）；
//     同池借用仲裁用 pg_advisory_xact_lock 串行化，跨池互不影响。
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kvm-cloud/platform/internal/bandwidth"
	"github.com/kvm-cloud/platform/internal/model"
)

//go:embed migration.sql
var migrationFS embed.FS

// MigrationSQL 返回内嵌的幂等 DDL（静态文件内容，无运行期输入参与）。
func MigrationSQL() string {
	b, _ := migrationFS.ReadFile("migration.sql")
	return string(b)
}

var (
	ErrNotFound       = errors.New("not found")
	ErrOvercommit     = errors.New("overcommit exceeded")
	ErrInsufficientBW = errors.New("insufficient bandwidth after preemption")
	ErrConflict       = errors.New("conflict")
)

var qb = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)
// rowsAffected 单值取 RowsAffected（database/sql 返回 (int64, error)）。
func rowsAffected(res sql.Result) int64 {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}


// Store 基于 database/sql（pgx stdlib 驱动）的持久化层。
type Store struct {
	db *sql.DB
}

// New 打开连接池。
func New(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(32)
	db.SetConnMaxLifetime(time.Hour)
	s := &Store{db: db}
	if err := s.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return s, nil
}

// Close 释放连接池。
func (s *Store) Close() { s.db.Close() }

// DB 暴露底层连接（健康检查/扩展）。
func (s *Store) DB() *sql.DB { return s.db }

// Ping 健康检查。
func (s *Store) Ping(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.db.PingContext(c)
}

// Migrate 幂等执行核心 DDL。
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, MigrationSQL())
	return err
}

// isUniqueViolation 判断唯一约束冲突（幂等回退用）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "23505")
}

// --------------------------------------------------------------------------
// Nodes
// --------------------------------------------------------------------------

var nodeCols = []string{
	"id", "hostname", "cluster_id", "status",
	"physical_vcpus", "physical_mem_mb", "physical_disk_gb", "physical_nic_bandwidth_kbps",
	"cpu_overcommit_ratio", "mem_overcommit_ratio", "disk_overcommit_ratio", "bw_overcommit_ratio",
	"allocated_vcpus", "allocated_mem_mb", "allocated_disk_gb", "allocated_min_kbps",
	"created_at",
}

func scanNode(row sq.RowScanner) (*model.Node, error) {
	var n model.Node
	var clusterID sql.NullString
	if err := row.Scan(&n.ID, &n.Hostname, &clusterID, &n.Status,
		&n.PhysicalVCPUs, &n.PhysicalMemMB, &n.PhysicalDiskGB, &n.PhysicalNICBwKbps,
		&n.CPUOvercommitRatio, &n.MemOvercommitRatio, &n.DiskOvercommitRatio, &n.BWOvercommitRatio,
		&n.AllocatedVCPUs, &n.AllocatedMemMB, &n.AllocatedDiskGB, &n.AllocatedMinKbps,
		&n.CreatedAt); err != nil {
		return nil, err
	}
	if clusterID.Valid {
		n.ClusterID = &clusterID.String
	}
	return &n, nil
}

func scanNodes(rows *sql.Rows) ([]*model.Node, error) {
	defer rows.Close()
	var out []*model.Node
	for rows.Next() {
		var n model.Node
		var clusterID sql.NullString
		if err := rows.Scan(&n.ID, &n.Hostname, &clusterID, &n.Status,
			&n.PhysicalVCPUs, &n.PhysicalMemMB, &n.PhysicalDiskGB, &n.PhysicalNICBwKbps,
			&n.CPUOvercommitRatio, &n.MemOvercommitRatio, &n.DiskOvercommitRatio, &n.BWOvercommitRatio,
			&n.AllocatedVCPUs, &n.AllocatedMemMB, &n.AllocatedDiskGB, &n.AllocatedMinKbps,
			&n.CreatedAt); err != nil {
			return nil, err
		}
		if clusterID.Valid {
			n.ClusterID = &clusterID.String
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}

// CreateNode 幂等：同 hostname 更新状态并返回库内现状。
func (s *Store) CreateNode(ctx context.Context, n *model.Node) error {
	if n.Status == "" {
		n.Status = model.NodeStatusOnline
	}
	_, err := s.GetNodeByName(ctx, n.Hostname)
	if err == nil {
		_, err = qb.Update("nodes").Set("status", n.Status).
			Where(sq.Eq{"hostname": n.Hostname}).RunWith(s.db).ExecContext(ctx)
		if err != nil {
			return err
		}
		merged, gerr := s.GetNodeByName(ctx, n.Hostname)
		if gerr != nil {
			return gerr
		}
		*n = *merged
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC()
	}
	_, err = qb.Insert("nodes").
		Columns(nodeCols...).
		Values(n.ID, n.Hostname, n.ClusterID, n.Status,
			n.PhysicalVCPUs, n.PhysicalMemMB, n.PhysicalDiskGB, n.PhysicalNICBwKbps,
			n.CPUOvercommitRatio, n.MemOvercommitRatio, n.DiskOvercommitRatio, n.BWOvercommitRatio,
			n.AllocatedVCPUs, n.AllocatedMemMB, n.AllocatedDiskGB, n.AllocatedMinKbps,
			n.CreatedAt).
		RunWith(s.db).ExecContext(ctx)
	if err != nil {
		if isUniqueViolation(err) { // 并发注册竞态：回读已有节点
			merged, gerr := s.GetNodeByName(ctx, n.Hostname)
			if gerr != nil {
				return gerr
			}
			*n = *merged
			return nil
		}
		return err
	}
	return nil
}

// GetNode 按 ID 查询。
func (s *Store) GetNode(ctx context.Context, id string) (*model.Node, error) {
	row := qb.Select(nodeCols...).From("nodes").Where(sq.Eq{"id": id}).RunWith(s.db).QueryRowContext(ctx)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// GetNodeByName 按 hostname 查询（注册幂等用）。
func (s *Store) GetNodeByName(ctx context.Context, hostname string) (*model.Node, error) {
	row := qb.Select(nodeCols...).From("nodes").Where(sq.Eq{"hostname": hostname}).RunWith(s.db).QueryRowContext(ctx)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// ListNodes 节点列表。
func (s *Store) ListNodes(ctx context.Context) ([]*model.Node, error) {
	rows, err := qb.Select(nodeCols...).From("nodes").OrderBy("created_at").RunWith(s.db).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// PlaceVM 四维超开放置。
// 先读节点做逐维精确校验（返回维度名），再以单条原子条件 UPDATE 完成预留：
// 条件与写入在同一语句内求值，等价于行锁下校验，超限时 RowsAffected=0。
func (s *Store) PlaceVM(ctx context.Context, nodeID string, req *model.VMRequest) (*model.Node, error) {
	n, err := s.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if err := CheckOvercommit(n, req); err != nil {
		return nil, err
	}

	res, err := qb.Update("nodes").
		Set("allocated_vcpus", sq.Expr("allocated_vcpus + ?", req.VCPUs)).
		Set("allocated_mem_mb", sq.Expr("allocated_mem_mb + ?", req.MemMB)).
		Set("allocated_disk_gb", sq.Expr("allocated_disk_gb + ?", req.DiskGB)).
		Set("allocated_min_kbps", sq.Expr("allocated_min_kbps + ?", req.MinKbps)).
		Where(sq.Eq{"id": nodeID}).
		Where(sq.Expr("allocated_vcpus + ? <= physical_vcpus * cpu_overcommit_ratio", req.VCPUs)).
		Where(sq.Expr("allocated_mem_mb + ? <= physical_mem_mb * mem_overcommit_ratio", req.MemMB)).
		Where(sq.Expr("allocated_disk_gb + ? <= physical_disk_gb * disk_overcommit_ratio", req.DiskGB)).
		Where(sq.Expr("allocated_min_kbps + ? <= physical_nic_bandwidth_kbps * bw_overcommit_ratio", req.MinKbps)).
		RunWith(s.db).ExecContext(ctx)
	if err != nil {
		return nil, err
	}
	if rowsAffected(res) == 0 { // 并发竞态：重新精确报维度
		fresh, gerr := s.GetNode(ctx, nodeID)
		if gerr != nil {
			return nil, gerr
		}
		if cerr := CheckOvercommit(fresh, req); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("%w: concurrent update on node %s", ErrConflict, nodeID)
	}
	return s.GetNode(ctx, nodeID)
}

// CheckOvercommit 四维超开校验（纯函数，调度器与 store 共用；单测覆盖）。
func CheckOvercommit(n *model.Node, req *model.VMRequest) error {
	checks := []struct {
		dim   model.Dimension
		alloc int64
		phys  int64
		ratio float64
		need  int64
	}{
		{model.DimCPU, int64(n.AllocatedVCPUs), int64(n.PhysicalVCPUs), n.CPUOvercommitRatio, int64(req.VCPUs)},
		{model.DimMem, n.AllocatedMemMB, n.PhysicalMemMB, n.MemOvercommitRatio, req.MemMB},
		{model.DimDisk, n.AllocatedDiskGB, n.PhysicalDiskGB, n.DiskOvercommitRatio, req.DiskGB},
		{model.DimBW, n.AllocatedMinKbps, n.PhysicalNICBwKbps, n.BWOvercommitRatio, req.MinKbps},
	}
	for _, c := range checks {
		if c.need < 0 {
			return fmt.Errorf("%w: negative request for %s", ErrOvercommit, c.dim)
		}
		limit := int64(float64(c.phys) * c.ratio)
		if c.alloc+c.need > limit {
			return fmt.Errorf("%w: dimension %s alloc=%d need=%d limit=%d",
				ErrOvercommit, c.dim, c.alloc, c.need, limit)
		}
	}
	return nil
}

// ReleaseVM 释放 VM 占用的四维资源（下线/删除时调用）。
func (s *Store) ReleaseVM(ctx context.Context, nodeID string, vcpus int32, memMB, diskGB, minKbps int64) error {
	res, err := qb.Update("nodes").
		Set("allocated_vcpus", sq.Expr("GREATEST(allocated_vcpus - ?, 0)", vcpus)).
		Set("allocated_mem_mb", sq.Expr("GREATEST(allocated_mem_mb - ?, 0)", memMB)).
		Set("allocated_disk_gb", sq.Expr("GREATEST(allocated_disk_gb - ?, 0)", diskGB)).
		Set("allocated_min_kbps", sq.Expr("GREATEST(allocated_min_kbps - ?, 0)", minKbps)).
		Where(sq.Eq{"id": nodeID}).
		RunWith(s.db).ExecContext(ctx)
	if err != nil {
		return err
	}
	if rowsAffected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNodeRatios 调整节点超开比（调优入口）。
func (s *Store) SetNodeRatios(ctx context.Context, nodeID string, cpu, mem, disk, bw float64) error {
	if cpu < 1 || mem < 1 || disk < 1 || bw < 1 {
		return fmt.Errorf("%w: ratios must be >= 1.0", ErrConflict)
	}
	res, err := qb.Update("nodes").
		Set("cpu_overcommit_ratio", cpu).
		Set("mem_overcommit_ratio", mem).
		Set("disk_overcommit_ratio", disk).
		Set("bw_overcommit_ratio", bw).
		Where(sq.Eq{"id": nodeID}).
		RunWith(s.db).ExecContext(ctx)
	if err != nil {
		return err
	}
	if rowsAffected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNodeStatus 更新节点状态（心跳超时置 offline 等）。
func (s *Store) SetNodeStatus(ctx context.Context, nodeID, status string) error {
	res, err := qb.Update("nodes").Set("status", status).
		Where(sq.Eq{"id": nodeID}).RunWith(s.db).ExecContext(ctx)
	if err != nil {
		return err
	}
	if rowsAffected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateNodeCapacity 重注册时回填节点物理容量与超开比（仅非零/合法值生效，防采集缺失清库）。
func (s *Store) UpdateNodeCapacity(ctx context.Context, nodeID string, n *model.Node) error {
	upd := qb.Update("nodes")
	if n.PhysicalVCPUs > 0 {
		upd = upd.Set("physical_vcpus", n.PhysicalVCPUs)
	}
	if n.PhysicalMemMB > 0 {
		upd = upd.Set("physical_mem_mb", n.PhysicalMemMB)
	}
	if n.PhysicalDiskGB > 0 {
		upd = upd.Set("physical_disk_gb", n.PhysicalDiskGB)
	}
	if n.PhysicalNICBwKbps > 0 {
		upd = upd.Set("physical_nic_bandwidth_kbps", n.PhysicalNICBwKbps)
	}
	if n.CPUOvercommitRatio >= 1 {
		upd = upd.Set("cpu_overcommit_ratio", n.CPUOvercommitRatio)
	}
	if n.MemOvercommitRatio >= 1 {
		upd = upd.Set("mem_overcommit_ratio", n.MemOvercommitRatio)
	}
	if n.DiskOvercommitRatio >= 1 {
		upd = upd.Set("disk_overcommit_ratio", n.DiskOvercommitRatio)
	}
	if n.BWOvercommitRatio >= 1 {
		upd = upd.Set("bw_overcommit_ratio", n.BWOvercommitRatio)
	}
	_, err := upd.Where(sq.Eq{"id": nodeID}).RunWith(s.db).ExecContext(ctx)
	return err
}

// --------------------------------------------------------------------------
// Tenants / VMs / VNICs
// --------------------------------------------------------------------------

// EnsureTenant 幂等创建租户。
func (s *Store) EnsureTenant(ctx context.Context, name string) (string, error) {
	var id string
	err := qb.Select("id").From("tenants").Where(sq.Eq{"name": name}).
		RunWith(s.db).QueryRowContext(ctx).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = uuid.NewString()
	_, err = qb.Insert("tenants").Columns("id", "name").Values(id, name).
		RunWith(s.db).ExecContext(ctx)
	if err != nil {
		if isUniqueViolation(err) { // 并发竞态：回读
			return s.EnsureTenant(ctx, name)
		}
		return "", err
	}
	return id, nil
}

// CreateVM 落库 VM。
func (s *Store) CreateVM(ctx context.Context, vm *model.VM) error {
	if vm.ID == "" {
		vm.ID = uuid.NewString()
	}
	if vm.CreatedAt.IsZero() {
		vm.CreatedAt = time.Now().UTC()
	}
	vm.Status = model.VMStatusCreating
	_, err := qb.Insert("vms").
		Columns("id", "tenant_id", "node_id", "name", "flavor", "vcpus", "mem_mb", "disk_gb", "status", "created_at").
		Values(vm.ID, vm.TenantID, vm.NodeID, vm.Name, vm.Flavor, vm.VCPUs, vm.MemMB, vm.DiskGB, vm.Status, vm.CreatedAt).
		RunWith(s.db).ExecContext(ctx)
	return err
}

// UpdateVMStatus 状态机更新。
func (s *Store) UpdateVMStatus(ctx context.Context, id, status string) error {
	res, err := qb.Update("vms").Set("status", status).
		Where(sq.Eq{"id": id}).RunWith(s.db).ExecContext(ctx)
	if err != nil {
		return err
	}
	if rowsAffected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

// GetVM 查询 VM。
func (s *Store) GetVM(ctx context.Context, id string) (*model.VM, error) {
	var v model.VM
	err := qb.Select("id", "tenant_id", "node_id", "name", "flavor", "vcpus", "mem_mb", "disk_gb", "status", "created_at").
		From("vms").Where(sq.Eq{"id": id}).
		RunWith(s.db).QueryRowContext(ctx).
		Scan(&v.ID, &v.TenantID, &v.NodeID, &v.Name, &v.Flavor, &v.VCPUs, &v.MemMB, &v.DiskGB, &v.Status, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVMs VM 列表（nodeID 传空串时不过滤）。
func (s *Store) ListVMs(ctx context.Context, nodeID string) ([]*model.VM, error) {
	sel := qb.Select("id", "tenant_id", "node_id", "name", "flavor", "vcpus", "mem_mb", "disk_gb", "status", "created_at").
		From("vms")
	if nodeID != "" {
		sel = sel.Where(sq.Eq{"node_id": nodeID})
	}
	rows, err := sel.OrderBy("created_at DESC").RunWith(s.db).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.VM
	for rows.Next() {
		var v model.VM
		if err := rows.Scan(&v.ID, &v.TenantID, &v.NodeID, &v.Name, &v.Flavor, &v.VCPUs, &v.MemMB, &v.DiskGB, &v.Status, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// CreateVNIC 创建网卡（MAC 由调用方生成）。
func (s *Store) CreateVNIC(ctx context.Context, n *model.VNIC) error {
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC()
	}
	var v4, v6 any
	if n.IPv4 != "" {
		v4 = n.IPv4
	}
	if n.IPv6 != "" {
		v6 = n.IPv6
	}
	_, err := qb.Insert("vnics").
		Columns("id", "vm_id", "mac", "ipv4", "ipv6", "bridge", "created_at").
		Values(n.ID, n.VMID, n.MAC, v4, v6, n.Bridge, n.CreatedAt).
		RunWith(s.db).ExecContext(ctx)
	return err
}

// --------------------------------------------------------------------------
// Shared pools
// --------------------------------------------------------------------------

var poolCols = []string{"id", "tenant_id", "name", "total_capacity_kbps", "allocated_min_kbps", "current_borrowed_kbps", "created_at"}

func scanPool(row sq.RowScanner) (*model.SharedBandwidthPool, error) {
	var p model.SharedBandwidthPool
	if err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.TotalCapacityKbps, &p.AllocatedMinKbps, &p.CurrentBorrowedKbps, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePool 共享带宽池。
func (s *Store) CreatePool(ctx context.Context, p *model.SharedBandwidthPool) error {
	if p.TotalCapacityKbps <= 0 {
		return fmt.Errorf("%w: capacity must be positive", ErrConflict)
	}
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	_, err := qb.Insert("shared_bandwidth_pools").
		Columns("id", "tenant_id", "name", "total_capacity_kbps", "allocated_min_kbps", "current_borrowed_kbps", "created_at").
		Values(p.ID, p.TenantID, p.Name, p.TotalCapacityKbps, 0, 0, p.CreatedAt).
		RunWith(s.db).ExecContext(ctx)
	return err
}

// GetPool 按 ID 查询。
func (s *Store) GetPool(ctx context.Context, id string) (*model.SharedBandwidthPool, error) {
	row := qb.Select(poolCols...).From("shared_bandwidth_pools").Where(sq.Eq{"id": id}).
		RunWith(s.db).QueryRowContext(ctx)
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListPools 池列表（tenantID 传空串时不过滤）。
func (s *Store) ListPools(ctx context.Context, tenantID string) ([]*model.SharedBandwidthPool, error) {
	sel := qb.Select(poolCols...).From("shared_bandwidth_pools")
	if tenantID != "" {
		sel = sel.Where(sq.Eq{"tenant_id": tenantID})
	}
	rows, err := sel.OrderBy("created_at").RunWith(s.db).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.SharedBandwidthPool
	for rows.Next() {
		var p model.SharedBandwidthPool
		if err := rows.Scan(&p.ID, &p.TenantID, &p.Name, &p.TotalCapacityKbps, &p.AllocatedMinKbps, &p.CurrentBorrowedKbps, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// --------------------------------------------------------------------------
// Port bandwidth policies
// --------------------------------------------------------------------------

var policyCols = []string{"id", "vnic_id", "direction", "min_kbps", "max_kbps", "shared_pool_id", "priority", "current_ceil_kbps", "state", "created_at"}

func scanPolicy(row sq.RowScanner) (*model.PortBandwidthPolicy, error) {
	var p model.PortBandwidthPolicy
	var poolID sql.NullString
	var ceil sql.NullInt64
	if err := row.Scan(&p.ID, &p.VNICID, &p.Direction, &p.MinKbps, &p.MaxKbps, &poolID, &p.Priority, &ceil, &p.State, &p.CreatedAt); err != nil {
		return nil, err
	}
	if poolID.Valid {
		p.SharedPoolID = &poolID.String
	}
	if ceil.Valid {
		p.CurrentCeilKbps = &ceil.Int64
	}
	return &p, nil
}

func scanPolicies(rows *sql.Rows) ([]*model.PortBandwidthPolicy, error) {
	defer rows.Close()
	var out []*model.PortBandwidthPolicy
	for rows.Next() {
		var p model.PortBandwidthPolicy
		var poolID sql.NullString
		var ceil sql.NullInt64
		if err := rows.Scan(&p.ID, &p.VNICID, &p.Direction, &p.MinKbps, &p.MaxKbps, &poolID, &p.Priority, &ceil, &p.State, &p.CreatedAt); err != nil {
			return nil, err
		}
		if poolID.Valid {
			p.SharedPoolID = &poolID.String
		}
		if ceil.Valid {
			p.CurrentCeilKbps = &ceil.Int64
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// CreatePolicy 新建端口策略：两级保障总量校验均以原子条件 UPDATE 完成（三场景之一）。
//   - 节点侧：SUM(min_kbps) <= physical_nic_bandwidth * bw_overcommit_ratio
//   - 池侧：  SUM(min_kbps) <= total_capacity_kbps
//
// 入池策略初始 ceil=min（state=guaranteed），借用按需提升；不入池策略 ceil=max。
func (s *Store) CreatePolicy(ctx context.Context, p *model.PortBandwidthPolicy) (*model.PortBandwidthPolicy, error) {
	if p.MinKbps > p.MaxKbps {
		return nil, fmt.Errorf("%w: min_kbps > max_kbps", ErrConflict)
	}
	if p.Direction != "ingress" && p.Direction != "egress" {
		return nil, fmt.Errorf("%w: direction must be ingress|egress", ErrConflict)
	}
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.State == "" {
		p.State = model.PolicyStateGuaranteed
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}

	// vnic -> vm -> node（两跳查询，避免 join 文本）
	var vmID string
	err := qb.Select("vm_id").From("vnics").Where(sq.Eq{"id": p.VNICID}).
		RunWith(s.db).QueryRowContext(ctx).Scan(&vmID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var nodeID string
	err = qb.Select("node_id").From("vms").Where(sq.Eq{"id": vmID}).
		RunWith(s.db).QueryRowContext(ctx).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 节点侧保障总量（含 bw_overcommit_ratio）原子预留
	if p.MinKbps > 0 {
		res, err := qb.Update("nodes").
			Set("allocated_min_kbps", sq.Expr("allocated_min_kbps + ?", p.MinKbps)).
			Where(sq.Eq{"id": nodeID}).
			Where(sq.Expr("allocated_min_kbps + ? <= physical_nic_bandwidth_kbps * bw_overcommit_ratio", p.MinKbps)).
			RunWith(tx).ExecContext(ctx)
		if err != nil {
			return nil, err
		}
		if rowsAffected(res) == 0 {
			return nil, fmt.Errorf("%w: bw dimension on node %s", ErrOvercommit, nodeID)
		}
	}

	// 池侧 SUM(min) <= capacity 原子预留
	hasPool := p.SharedPoolID != nil && *p.SharedPoolID != ""
	if hasPool {
		res, err := qb.Update("shared_bandwidth_pools").
			Set("allocated_min_kbps", sq.Expr("allocated_min_kbps + ?", p.MinKbps)).
			Where(sq.Eq{"id": *p.SharedPoolID}).
			Where(sq.Expr("allocated_min_kbps + ? <= total_capacity_kbps", p.MinKbps)).
			RunWith(tx).ExecContext(ctx)
		if err != nil {
			return nil, err
		}
		if rowsAffected(res) == 0 {
			return nil, fmt.Errorf("%w: pool min-sum exceeds capacity %s", ErrOvercommit, *p.SharedPoolID)
		}
		ceil := p.MinKbps
		p.CurrentCeilKbps = &ceil
	} else {
		ceil := p.MaxKbps
		p.CurrentCeilKbps = &ceil
	}

	var poolArg any
	if hasPool {
		poolArg = *p.SharedPoolID
	}
	_, err = qb.Insert("port_bandwidth_policies").
		Columns("id", "vnic_id", "direction", "min_kbps", "max_kbps", "shared_pool_id", "priority", "current_ceil_kbps", "state", "created_at").
		Values(p.ID, p.VNICID, p.Direction, p.MinKbps, p.MaxKbps, poolArg, p.Priority, p.CurrentCeilKbps, p.State, p.CreatedAt).
		RunWith(tx).ExecContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

// ListPoliciesByPool 池内全部策略（抢占仲裁用），按优先级降序。
func (s *Store) ListPoliciesByPool(ctx context.Context, poolID string) ([]*model.PortBandwidthPolicy, error) {
	rows, err := qb.Select(policyCols...).From("port_bandwidth_policies").
		Where(sq.Eq{"shared_pool_id": poolID}).
		OrderBy("priority DESC").RunWith(s.db).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	return scanPolicies(rows)
}

// GetPolicy 查询策略。
func (s *Store) GetPolicy(ctx context.Context, id string) (*model.PortBandwidthPolicy, error) {
	row := qb.Select(policyCols...).From("port_bandwidth_policies").Where(sq.Eq{"id": id}).
		RunWith(s.db).QueryRowContext(ctx)
	p, err := scanPolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListPolicies 策略列表（vnicID 传空串时不过滤）。
func (s *Store) ListPolicies(ctx context.Context, vnicID string) ([]*model.PortBandwidthPolicy, error) {
	sel := qb.Select(policyCols...).From("port_bandwidth_policies")
	if vnicID != "" {
		sel = sel.Where(sq.Eq{"vnic_id": vnicID})
	}
	rows, err := sel.OrderBy("created_at").RunWith(s.db).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	return scanPolicies(rows)
}

// --------------------------------------------------------------------------
// 借用仲裁执行层（纯算法在 internal/bandwidth，本层负责事务与落库）
// --------------------------------------------------------------------------

// RequestBorrow 场景 2/3：借用授予与竞争抢占。
// 同池以 pg_advisory_xact_lock 串行化，跨池隔离天然成立；事件落库。
func (s *Store) RequestBorrow(ctx context.Context, policyID string, requestedKbps int64) (*bandwidth.Decision, *model.PortBandwidthPolicy, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	pol, err := txPolicy(ctx, tx, policyID)
	if err != nil {
		return nil, nil, err
	}
	if pol.SharedPoolID == nil || *pol.SharedPoolID == "" {
		return nil, pol, fmt.Errorf("%w: policy has no shared pool", ErrConflict)
	}
	poolID := *pol.SharedPoolID

	// 同池仲裁串行化（会话级咨询锁，事务结束自动释放）
	var lock any
	if err := tx.QueryRowContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1))", poolID).Scan(&lock); err != nil {
		return nil, nil, err
	}

	poolRow, err := txPool(ctx, tx, poolID)
	if err != nil {
		return nil, nil, err
	}
	peers, err := txPoliciesByPool(ctx, tx, poolID)
	if err != nil {
		return nil, nil, err
	}

	poolView := bandwidth.PoolView{
		TotalCapacityKbps:   poolRow.TotalCapacityKbps,
		AllocatedMinKbps:    poolRow.AllocatedMinKbps,
		CurrentBorrowedKbps: poolRow.CurrentBorrowedKbps,
	}
	polView := bandwidth.PolicyView{ID: pol.ID, MinKbps: pol.MinKbps, MaxKbps: pol.MaxKbps, Priority: pol.Priority, CurrentCeil: ceilOf(pol), State: pol.State}
	peerViews := make([]bandwidth.PolicyView, 0, len(peers))
	for _, pp := range peers {
		peerViews = append(peerViews, bandwidth.PolicyView{ID: pp.ID, MinKbps: pp.MinKbps, MaxKbps: pp.MaxKbps, Priority: pp.Priority, CurrentCeil: ceilOf(pp), State: pp.State})
	}

	d := bandwidth.Decide(poolView, requestedKbps, polView, peerViews)

	// 应用抢占结果
	for _, v := range d.Victims {
		newState := model.PolicyStateBorrowing
		for _, pp := range peers {
			if pp.ID == v.PolicyID {
				if v.NewCeil <= pp.MinKbps {
					newState = model.PolicyStateGuaranteed
				}
				break
			}
		}
		if _, err := qb.Update("port_bandwidth_policies").
			Set("current_ceil_kbps", v.NewCeil).Set("state", newState).
			Where(sq.Eq{"id": v.PolicyID}).RunWith(tx).ExecContext(ctx); err != nil {
			return nil, nil, err
		}
	}
	// 池借用总量：+granted -reclaimed
	var delta int64
	for _, v := range d.Victims {
		delta -= v.Reclaimed
	}
	if d.Granted > 0 {
		delta += d.Granted
	}
	if delta != 0 {
		expr := sq.Expr("GREATEST(current_borrowed_kbps + ?, 0)", delta)
		if _, err := qb.Update("shared_bandwidth_pools").Set("current_borrowed_kbps", expr).
			Where(sq.Eq{"id": poolID}).RunWith(tx).ExecContext(ctx); err != nil {
			return nil, nil, err
		}
	}
	// 自身策略状态
	if d.Granted > 0 {
		ceil := pol.MinKbps + d.Granted
		if _, err := qb.Update("port_bandwidth_policies").
			Set("current_ceil_kbps", ceil).Set("state", model.PolicyStateBorrowing).
			Where(sq.Eq{"id": pol.ID}).RunWith(tx).ExecContext(ctx); err != nil {
			return nil, nil, err
		}
		pol.CurrentCeilKbps = &ceil
		pol.State = model.PolicyStateBorrowing
	}

	ev := &model.BorrowEvent{PolicyID: pol.ID, RequestedKbps: requestedKbps, GrantedKbps: d.Granted, Reason: d.Reason, CreatedAt: time.Now().UTC()}
	if err := txInsertEvent(ctx, tx, ev); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return &d, pol, nil
}

// ReleaseBorrow 主动归还：压回 min_kbps，回收量返还池，事件落库。
func (s *Store) ReleaseBorrow(ctx context.Context, policyID string) (*model.PortBandwidthPolicy, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	pol, err := txPolicy(ctx, tx, policyID)
	if err != nil {
		return nil, err
	}
	if pol.SharedPoolID == nil || *pol.SharedPoolID == "" {
		return nil, fmt.Errorf("%w: policy has no shared pool", ErrConflict)
	}
	poolID := *pol.SharedPoolID

	var lock any
	if err := tx.QueryRowContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1))", poolID).Scan(&lock); err != nil {
		return nil, err
	}

	reclaimed := ceilOf(pol) - pol.MinKbps
	if reclaimed > 0 {
		if _, err := qb.Update("port_bandwidth_policies").
			Set("current_ceil_kbps", pol.MinKbps).Set("state", model.PolicyStateGuaranteed).
			Where(sq.Eq{"id": pol.ID}).RunWith(tx).ExecContext(ctx); err != nil {
			return nil, err
		}
		if _, err := qb.Update("shared_bandwidth_pools").
			Set("current_borrowed_kbps", sq.Expr("GREATEST(current_borrowed_kbps - ?, 0)", reclaimed)).
			Where(sq.Eq{"id": poolID}).RunWith(tx).ExecContext(ctx); err != nil {
			return nil, err
		}
		ceil := pol.MinKbps
		pol.CurrentCeilKbps = &ceil
		pol.State = model.PolicyStateGuaranteed
	}
	ev := &model.BorrowEvent{PolicyID: pol.ID, RequestedKbps: 0, GrantedKbps: 0, Reason: model.BorrowReasonReleased, CreatedAt: time.Now().UTC()}
	if err := txInsertEvent(ctx, tx, ev); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return pol, nil
}

func ceilOf(p *model.PortBandwidthPolicy) int64 {
	if p.CurrentCeilKbps == nil {
		return p.MinKbps
	}
	return *p.CurrentCeilKbps
}

func txPolicy(ctx context.Context, tx *sql.Tx, id string) (*model.PortBandwidthPolicy, error) {
	row := qb.Select(policyCols...).From("port_bandwidth_policies").Where(sq.Eq{"id": id}).
		RunWith(tx).QueryRowContext(ctx)
	p, err := scanPolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func txPool(ctx context.Context, tx *sql.Tx, id string) (*model.SharedBandwidthPool, error) {
	row := qb.Select(poolCols...).From("shared_bandwidth_pools").Where(sq.Eq{"id": id}).
		RunWith(tx).QueryRowContext(ctx)
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func txPoliciesByPool(ctx context.Context, tx *sql.Tx, poolID string) ([]*model.PortBandwidthPolicy, error) {
	rows, err := qb.Select(policyCols...).From("port_bandwidth_policies").
		Where(sq.Eq{"shared_pool_id": poolID}).
		OrderBy("priority DESC").RunWith(tx).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	return scanPoliciesTx(rows)
}

func scanPoliciesTx(rows *sql.Rows) ([]*model.PortBandwidthPolicy, error) {
	defer rows.Close()
	var out []*model.PortBandwidthPolicy
	for rows.Next() {
		var p model.PortBandwidthPolicy
		var poolID sql.NullString
		var ceil sql.NullInt64
		if err := rows.Scan(&p.ID, &p.VNICID, &p.Direction, &p.MinKbps, &p.MaxKbps, &poolID, &p.Priority, &ceil, &p.State, &p.CreatedAt); err != nil {
			return nil, err
		}
		if poolID.Valid {
			p.SharedPoolID = &poolID.String
		}
		if ceil.Valid {
			p.CurrentCeilKbps = &ceil.Int64
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func txInsertEvent(ctx context.Context, runner sq.BaseRunner, e *model.BorrowEvent) error {
	_, err := qb.Insert("borrow_events").
		Columns("policy_id", "requested_kbps", "granted_kbps", "reason", "created_at").
		Values(e.PolicyID, e.RequestedKbps, e.GrantedKbps, e.Reason, e.CreatedAt).
		RunWith(runner).ExecContext(ctx)
	return err
}

// --------------------------------------------------------------------------
// Borrow events / overcommit alerts
// --------------------------------------------------------------------------

// InsertBorrowEvent 审计落库（独立调用场景）。
func (s *Store) InsertBorrowEvent(ctx context.Context, e *model.BorrowEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	return txInsertEvent(ctx, s.db, e)
}

// ListBorrowEvents 事件列表（policyID 传空串时不过滤；limit<=0 取默认 200）。
func (s *Store) ListBorrowEvents(ctx context.Context, policyID string, limit int) ([]*model.BorrowEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	sel := qb.Select("id", "policy_id", "requested_kbps", "granted_kbps", "reason", "created_at").
		From("borrow_events")
	if policyID != "" {
		sel = sel.Where(sq.Eq{"policy_id": policyID})
	}
	rows, err := sel.OrderBy("id DESC").Limit(uint64(limit)).RunWith(s.db).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.BorrowEvent
	for rows.Next() {
		var e model.BorrowEvent
		if err := rows.Scan(&e.ID, &e.PolicyID, &e.RequestedKbps, &e.GrantedKbps, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// InsertAlert 写入超开水位告警。
func (s *Store) InsertAlert(ctx context.Context, a *model.OvercommitAlert) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := qb.Insert("overcommit_alerts").
		Columns("node_id", "dimension", "allocated", "physical", "ratio", "severity", "created_at").
		Values(a.NodeID, a.Dimension, a.Allocated, a.Physical, a.Ratio, a.Severity, a.CreatedAt).
		RunWith(s.db).ExecContext(ctx)
	return err
}

// ListAlerts 告警列表（nodeID 传空串时不过滤）。
func (s *Store) ListAlerts(ctx context.Context, nodeID string, limit int) ([]*model.OvercommitAlert, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	sel := qb.Select("id", "node_id", "dimension", "allocated", "physical", "ratio", "severity", "created_at").
		From("overcommit_alerts")
	if nodeID != "" {
		sel = sel.Where(sq.Eq{"node_id": nodeID})
	}
	rows, err := sel.OrderBy("id DESC").Limit(uint64(limit)).RunWith(s.db).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.OvercommitAlert
	for rows.Next() {
		var a model.OvercommitAlert
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Dimension, &a.Allocated, &a.Physical, &a.Ratio, &a.Severity, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}
