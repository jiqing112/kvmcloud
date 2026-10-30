// kvmcloud-agent 计算节点 Agent：
//  1. 启动时用 JOIN_TOKEN 向主控注册（幂等）；
//  2. 周期心跳上报资源快照，回执水位；
//  3. 周期拉取建机任务 → qemu-img/virsh 建机 + TC QoS → 回报结果；
//  4. gRPC QoS 反向下发（ApplyPolicy/UpdateCeil）为后续演进项。
// 配置仅从环境变量读取；凭据不落盘（systemd EnvironmentFile 权限 0600）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	cloudv1 "github.com/kvm-cloud/platform/internal/pb/cloudv1"
	"github.com/kvm-cloud/platform/internal/agentlib"
	"github.com/kvm-cloud/platform/internal/updater"
)

var (
	version = "dev"
	commit  = "none"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	switch os.Getenv(key) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Printf("kvmcloud-agent %s (commit %s)\n", version, commit)
		return
	}

	controlAddr := os.Getenv("CONTROL_ADDR")
	joinToken := os.Getenv("JOIN_TOKEN")
	nodeName := envOr("NODE_NAME", hostname())
	if controlAddr == "" || joinToken == "" {
		log.Error("CONTROL_ADDR and JOIN_TOKEN are required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := grpc.NewClient(controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Error("dial control plane", "addr", controlAddr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()
	nodeClient := cloudv1.NewNodeServiceClient(conn)

	runner := agentlib.ExecRunner{}
	collector := &agentlib.Collector{R: runner}

	// 物理带宽：env 覆盖 > 网卡协商速率 > 1Gbps 缺省（启动时定，注册与心跳一致）
	nicKbps := agentlib.ResolveNICBandwidthKbps(envOr("AGENT_NIC", ""), envInt64("AGENT_NIC_BANDWIDTH_KBPS", 0))
	log.Info("nic bandwidth resolved", "kbps", nicKbps)

	// --- 注册（幂等，重试到成功） ---
	nodeID := ""
	for {
		snap, err := collector.Collect()
		if err != nil {
			log.Error("collect host info", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		resp, err := nodeClient.Register(ctx, &cloudv1.RegisterRequest{
			Token:                    joinToken,
			NodeName:                 nodeName,
			Hostname:                 hostname(),
			PhysicalVcpus:            snap.PhysicalVCPUs,
			PhysicalMemMb:            snap.PhysicalMemMB,
			PhysicalDiskGb:           snap.PhysicalDiskGB,
			PhysicalNicBandwidthKbps: nicKbps,
			CpuRatio:                 envFloat("CPU_RATIO", 4.0),
			MemRatio:                 envFloat("MEM_RATIO", 1.5),
			DiskRatio:                envFloat("DISK_RATIO", 2.0),
			BwRatio:                  envFloat("BW_RATIO", 1.0),
		})
		if err != nil {
			log.Warn("register retry", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		nodeID = resp.GetNodeId()
		log.Info("registered", "node_id", nodeID, "created", resp.GetCreated(), "control", controlAddr)
		break
	}

	// --- QoS 下发服务（Agent 侧 gRPC server 由控制面反向调用在后续版本演进；
	//     当前版本 Agent 轮询控制面 HTTP 获取 pending 变更，保持简单可靠） ---

	// --- 建机任务循环：拉取本节点 pending → qemu-img/virsh 建机 → 回报结果 ---
	controlHTTP := envOr("CONTROL_HTTP_ADDR", deriveHTTPAddr(controlAddr, envOr("CONTROL_HTTP_PORT", "8080")))
	prov := &agentlib.Provisioner{
		R:         runner,
		DataDir:   envOr("AGENT_DATA_DIR", "/var/lib/kvmcloud"),
		BaseImage: os.Getenv("AGENT_BASE_IMAGE"),
		Bridge:    envOr("AGENT_BRIDGE", "virbr0"),
	}
	log.Info("provisioner ready", "control_http", controlHTTP, "base_image", prov.BaseImage, "bridge", prov.Bridge)
	provInterval := 10 * time.Second
	if v := os.Getenv("PROVISION_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			provInterval = d
		}
	}
	pc := &provisionClient{http: &http.Client{Timeout: 120 * time.Second}, baseURL: controlHTTP, nodeID: nodeID}
	go runProvisionLoop(ctx, log, pc, prov, provInterval)

	// --- 本体自动更新：定时检测新版本，auto 模式后台自更新（失败自动回滚） ---
	selfUpdate, err := updater.FromEnv("agent", version, "cloud-agent.service", log)
	if err != nil {
		log.Warn("auto update disabled", "err", err)
	} else if selfUpdate != nil {
		go selfUpdate.Run(ctx)
		log.Info("auto updater enabled", "mode", "on")
	}

	// --- 心跳循环 ---
	heartbeatInterval := 15 * time.Second
	if v := os.Getenv("HEARTBEAT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			heartbeatInterval = d
		}
	}
	balloonEnabled := envBool("MEM_BALLOON_ENABLED", false)
	ksmEnabled := envBool("KSM_ENABLED", false)

	if ksmEnabled {
		if err := agentlib.SetKSM(true); err != nil {
			log.Warn("enable ksm failed", "err", err)
		}
	}

	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("agent stopped")
			return
		case <-tick.C:
			snap, err := collector.Collect()
			if err != nil {
				log.Error("collect", "err", err)
				continue
			}
			resp, err := nodeClient.Heartbeat(ctx, &cloudv1.HeartbeatRequest{
				NodeId: nodeID,
				Token:  joinToken,
				Snapshot: &cloudv1.ResourceSnapshot{
					PhysicalVcpus:            snap.PhysicalVCPUs,
					PhysicalMemMb:            snap.PhysicalMemMB,
					PhysicalDiskGb:           snap.PhysicalDiskGB,
					PhysicalNicBandwidthKbps: nicKbps,
					VcpuUsedPercent:          snap.VCPUUsedPercent,
					MemUsedMb:                snap.MemUsedMB,
					BalloonReclaimableMb:     snap.BalloonReclaimableMB,
					DiskUsedGb:               snap.DiskUsedGB,
					ThinPoolUsedGb:           snap.ThinPoolUsedGB,
				},
			})
			if err != nil {
				log.Warn("heartbeat", "err", err)
				continue
			}
			log.Debug("heartbeat ok",
				"cpu_wm", resp.GetCpuWatermark(), "mem_wm", resp.GetMemWatermark(),
				"disk_wm", resp.GetDiskWatermark(), "bw_wm", resp.GetBwWatermark())

			// 内存气球：水位高时回收（仅 MEM_BALLOON_ENABLED=true 且装了 virsh 时）
			if balloonEnabled && resp.GetMemWatermark() > 0.85 {
				target := snap.PhysicalMemMB - snap.BalloonReclaimableMB/2
				if err := collector.SetBalloonTarget(nodeName, target); err != nil {
					log.Debug("balloon skip", "err", err)
				}
			}
		}
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
