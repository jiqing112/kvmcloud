package agentlib

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Collector 资源采集：物理容量 + 实际使用 + 气球可回收量。
// Linux 依赖 /proc 与 /sys；采集失败不 panic，由调用方决定降级。
type Collector struct {
	R Runner
}

// HostSnapshot 一次采集的快照（对应 gRPC ResourceSnapshot）。
type HostSnapshot struct {
	PhysicalVCPUs     int32
	PhysicalMemMB     int64
	PhysicalDiskGB    int64
	PhysicalNICBwKbps int64

	VCPUUsedPercent     float64
	MemUsedMB           int64
	BalloonReclaimableMB int64
	DiskUsedGB          int64
	ThinPoolUsedGB      int64
}

// Collect 采集物理容量（不含带宽——带宽由安装期配置/接口速率决定）。
func (c *Collector) Collect() (*HostSnapshot, error) {
	s := &HostSnapshot{}
	nproc, err := c.R.Run("nproc")
	if err != nil {
		return nil, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(nproc))
	if err != nil {
		return nil, err
	}
	s.PhysicalVCPUs = int32(v)

	memTotal, memAvail, err := readMemInfo()
	if err != nil {
		return nil, err
	}
	s.PhysicalMemMB = memTotal / 1024
	s.MemUsedMB = (memTotal - memAvail) / 1024
	s.BalloonReclaimableMB = memAvail / 1024 // 可回收近似：当前可用页

	dfOut, err := c.R.Run("df", "-BG", "--output=size,used", "/var/lib/kvmcloud")
	if err == nil {
		// 解析第二行：size|used
		lines := strings.Split(strings.TrimSpace(dfOut), "\n")
		if len(lines) >= 2 {
			fields := strings.Fields(lines[1])
			if len(fields) == 2 {
				s.PhysicalDiskGB = parseGB(fields[0])
				s.DiskUsedGB = parseGB(fields[1])
			}
		}
	}
	return s, nil
}

func parseGB(s string) int64 {
	s = strings.TrimSuffix(s, "G")
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// readMemInfo 读取 /proc/meminfo（MemTotal/MemAvailable，kB）。
func readMemInfo() (totalKB, availKB int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB, _ = strconv.ParseInt(trimKb(line), 10, 64)
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB, _ = strconv.ParseInt(trimKb(line), 10, 64)
		}
	}
	if totalKB <= 0 {
		return 0, 0, fmt.Errorf("meminfo parse failed")
	}
	return totalKB, availKB, nil
}

func trimKb(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "0"
	}
	return fields[1]
}

// SetKSM 开关 KSM（/sys/kernel/mm/ksm/run）。需要 root。
func SetKSM(enable bool) error {
	v := "0"
	if enable {
		v = "1"
	}
	return os.WriteFile("/sys/kernel/mm/ksm/run", []byte(v), 0o644)
}

// SetBalloonTarget 通过 virsh 设置气球目标（MB）。MEM_BALLOON_ENABLED=false 时由调用方跳过。
func (c *Collector) SetBalloonTarget(domain string, targetMB int64) error {
	_, err := c.R.Run("virsh", "setmem", domain, fmt.Sprintf("%d", targetMB), "--live", "--config")
	return err
}

// IfaceSpeedKbps 读取网卡协商速率（/sys/class/net/<iface>/speed，Mbps→kbps）。
func IfaceSpeedKbps(iface string) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/speed", iface))
	if err != nil {
		return 0
	}
	mbps, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return mbps * 1000
}
