// Package grpcserver 控制面 gRPC 服务：节点注册、心跳、QoS 下发。
package grpcserver

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	cloudv1 "github.com/kvm-cloud/platform/internal/pb/cloudv1"
	"github.com/kvm-cloud/platform/internal/auth"
	"github.com/kvm-cloud/platform/internal/model"
	"github.com/kvm-cloud/platform/internal/monitor"
	"github.com/kvm-cloud/platform/internal/store"
)

// Server 实现 cloudv1.NodeService 与 QoSService。
type Server struct {
	cloudv1.UnimplementedNodeServiceServer
	cloudv1.UnimplementedQoSServiceServer

	st       *store.Store
	mon      *monitor.Monitor
	joinAuth *auth.Validator
	log      *slog.Logger
}

// New 构造。
func New(st *store.Store, mon *monitor.Monitor, joinAuth *auth.Validator, log *slog.Logger) *Server {
	return &Server{st: st, mon: mon, joinAuth: joinAuth, log: log}
}

// Listen 在 addr 上启动 gRPC（阻塞）。
func (s *Server) Listen(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer()
	cloudv1.RegisterNodeServiceServer(gs, s)
	cloudv1.RegisterQoSServiceServer(gs, s)
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	s.log.Info("grpc listening", "addr", addr)
	return gs.Serve(lis)
}

// Register 节点注册：token 校验；同 node_name 幂等返回已有 ID；注册后节点置 online。
func (s *Server) Register(ctx context.Context, req *cloudv1.RegisterRequest) (*cloudv1.RegisterResponse, error) {
	if err := s.joinAuth.Validate(req.Token); err != nil {
		s.log.Warn("register denied", "node", req.GetNodeName(), "err", err)
		return nil, status.Error(codes.PermissionDenied, "invalid join token")
	}
	hostname := req.GetHostname()
	if hostname == "" {
		hostname = req.GetNodeName()
	}
	ip := req.GetIp()
	if ip == "" {
		if p, ok := peer.FromContext(ctx); ok {
			if addr, ok := p.Addr.(*net.TCPAddr); ok {
				ip = addr.IP.String()
			}
		}
	}

	n := &model.Node{
		Hostname:            req.GetNodeName(),
		Status:              model.NodeStatusOnline,
		PhysicalVCPUs:       req.GetPhysicalVcpus(),
		PhysicalMemMB:       req.GetPhysicalMemMb(),
		PhysicalDiskGB:      req.GetPhysicalDiskGb(),
		PhysicalNICBwKbps:   req.GetPhysicalNicBandwidthKbps(),
		CPUOvercommitRatio:  req.GetCpuRatio(),
		MemOvercommitRatio:  req.GetMemRatio(),
		DiskOvercommitRatio: req.GetDiskRatio(),
		BWOvercommitRatio:   req.GetBwRatio(),
	}
	if n.CPUOvercommitRatio < 1 {
		n.CPUOvercommitRatio = 4.0
	}
	if n.MemOvercommitRatio < 1 {
		n.MemOvercommitRatio = 1.5
	}
	if n.DiskOvercommitRatio < 1 {
		n.DiskOvercommitRatio = 2.0
	}
	if n.BWOvercommitRatio < 1 {
		n.BWOvercommitRatio = 1.0
	}

	// 幂等：先查同名节点
	if existing, err := s.st.GetNodeByName(ctx, n.Hostname); err == nil {
		n.ID = existing.ID
		n.CreatedAt = existing.CreatedAt
		// 物理容量与超开比以最新上报为准（重装/扩容场景）
		if err := s.st.CreateNode(ctx, n); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return s.response(ctx, n, false), nil
	} else if err != store.ErrNotFound {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.st.CreateNode(ctx, n); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = ip // 预留：后续入库 agent 元数据
	s.touchSeen(n.ID)
	s.log.Info("node registered", "node", n.Hostname, "id", n.ID)
	return s.response(ctx, n, true), nil
}

func (s *Server) response(ctx context.Context, n *model.Node, created bool) *cloudv1.RegisterResponse {
	return &cloudv1.RegisterResponse{NodeId: n.ID, Created: created, ControlHttpAddr: ""}
}

// Heartbeat 心跳：上报快照、刷新节点状态、评估水位（触发 overcommit_alerts）。
func (s *Server) Heartbeat(ctx context.Context, req *cloudv1.HeartbeatRequest) (*cloudv1.HeartbeatResponse, error) {
	n, err := s.st.GetNode(ctx, req.GetNodeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "node not registered")
	}
	s.touchSeen(n.ID)
	if n.Status != model.NodeStatusOnline {
		if err := s.st.SetNodeStatus(ctx, n.ID, model.NodeStatusOnline); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	var w monitor.Watermarks
	if snap := req.GetSnapshot(); snap != nil {
		if snap.GetPhysicalVcpus() > 0 {
			n.PhysicalVCPUs = snap.GetPhysicalVcpus()
		}
		if snap.GetPhysicalMemMb() > 0 {
			n.PhysicalMemMB = snap.GetPhysicalMemMb()
		}
		if snap.GetPhysicalDiskGb() > 0 {
			n.PhysicalDiskGB = snap.GetPhysicalDiskGb()
		}
		if snap.GetPhysicalNicBandwidthKbps() > 0 {
			n.PhysicalNICBwKbps = snap.GetPhysicalNicBandwidthKbps()
		}
	}
	w = s.mon.EvaluateNode(ctx, n)
	return &cloudv1.HeartbeatResponse{
		Ok:            true,
		CpuWatermark:  w.CPU,
		MemWatermark:  w.Mem,
		DiskWatermark: w.Disk,
		BwWatermark:   w.BW,
	}, nil
}

// Watchdog 节点离线看门狗：超过 staleAfter 无心跳则置 offline。
func (s *Server) Watchdog(ctx context.Context, staleAfter time.Duration, interval time.Duration) {
	// 当前心跳状态存内存；生产可扩展至 Redis。注册/心跳会刷新 lastSeen。
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for id, ts := range s.lastSeen() {
				if time.Since(ts) > staleAfter {
					if err := s.st.SetNodeStatus(ctx, id, model.NodeStatusOffline); err != nil {
						s.log.Error("watchdog mark offline", "node", id, "err", err)
					} else {
						s.log.Warn("node marked offline (heartbeat stale)", "node", id)
					}
				}
			}
		}
	}
}

var (
	seenMu sync.Mutex
	seen   = map[string]time.Time{}
)

func (s *Server) lastSeen() map[string]time.Time {
	seenMu.Lock()
	defer seenMu.Unlock()
	out := make(map[string]time.Time, len(seen))
	for k, v := range seen {
		out[k] = v
	}
	return out
}

func (s *Server) touchSeen(nodeID string) {
	seenMu.Lock()
	defer seenMu.Unlock()
	seen[nodeID] = time.Now()
}
