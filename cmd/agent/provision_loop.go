// 建机任务循环：Agent 周期拉取本节点 pending 任务 → qemu-img/virsh 建机 → 回报结果。
// 控制面状态机：creating → running（成功）| error（失败，含原因，容量已回滚）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kvm-cloud/platform/internal/agentlib"
)

// deriveHTTPAddr 从 CONTROL_ADDR（gRPC host:port）推导控制面 HTTP 地址。
func deriveHTTPAddr(grpcAddr, httpPort string) string {
	host := grpcAddr
	if i := strings.LastIndex(grpcAddr, ":"); i > 0 {
		host = grpcAddr[:i]
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + httpPort
}

// provisionTask 对应控制面 GET /api/v1/agent/tasks/pending 的任务条目。
type provisionTask struct {
	VMID    string `json:"vm_id"`
	Name    string `json:"name"`
	VCPUs   int32  `json:"vcpus"`
	MemMB   int64  `json:"mem_mb"`
	DiskGB  int64  `json:"disk_gb"`
	MinKbps int64  `json:"min_kbps"`
	MaxKbps int64  `json:"max_kbps"`
}

type provisionClient struct {
	http    *http.Client
	baseURL string
	nodeID  string
}

// runProvisionLoop 建机主循环；任务串行执行，避免并发 qemu-img/virsh 争用。
func runProvisionLoop(ctx context.Context, log *slog.Logger, pc *provisionClient, prov *agentlib.Provisioner, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tasks, err := pc.fetchPending(ctx)
			if err != nil {
				log.Warn("fetch pending tasks", "err", err)
				continue
			}
			for _, tk := range tasks {
				pc.handleTask(ctx, log, prov, tk)
			}
		}
	}
}

func (pc *provisionClient) fetchPending(ctx context.Context) ([]provisionTask, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		pc.baseURL+"/api/v1/agent/tasks/pending?node_id="+url.QueryEscape(pc.nodeID), nil)
	if err != nil {
		return nil, err
	}
	return pc.doTasks(req)
}

func (pc *provisionClient) doTasks(req *http.Request) ([]provisionTask, error) {
	resp, err := pc.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out struct {
		Tasks []provisionTask `json:"tasks"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode pending tasks: %w", err)
	}
	return out.Tasks, nil
}

// handleTask 执行单个建机任务并回报。QoS 应用失败不改变 VM 状态（只记日志），
// 带宽契约由后续对账补偿；建机失败才回报 error。
func (pc *provisionClient) handleTask(ctx context.Context, log *slog.Logger, prov *agentlib.Provisioner, tk provisionTask) {
	spec := agentlib.ProvisionSpec{
		VMID: tk.VMID, Name: tk.Name,
		VCPUs: tk.VCPUs, MemMB: tk.MemMB, DiskGB: tk.DiskGB,
		MinKbps: tk.MinKbps, MaxKbps: tk.MaxKbps,
	}
	perr := prov.Provision(spec)
	if perr == nil {
		if qerr := prov.ApplyQoS(spec); qerr != nil {
			log.Warn("apply qos (vm stays running)", "vm_id", tk.VMID, "name", tk.Name, "err", qerr)
		}
		log.Info("vm provisioned", "vm_id", tk.VMID, "name", tk.Name,
			"vcpus", tk.VCPUs, "mem_mb", tk.MemMB, "disk_gb", tk.DiskGB,
			"min_kbps", tk.MinKbps, "max_kbps", tk.MaxKbps)
	} else {
		log.Warn("vm provision failed", "vm_id", tk.VMID, "name", tk.Name, "err", perr)
	}

	payload, _ := json.Marshal(map[string]any{
		"node_id": pc.nodeID,
		"vm_id":   tk.VMID,
		"ok":      perr == nil,
		"error":   errStr(perr),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pc.baseURL+"/api/v1/agent/tasks/report", bytes.NewReader(payload))
	if err != nil {
		log.Error("build report request", "vm_id", tk.VMID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := pc.http.Do(req)
	if err != nil {
		log.Error("report task result", "vm_id", tk.VMID, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		log.Error("report task result rejected", "vm_id", tk.VMID, "status", resp.StatusCode, "body", string(b))
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
