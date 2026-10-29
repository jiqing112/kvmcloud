package agentlib

// 建机执行：qcow2 系统盘准备 + libvirt 域定义/启动 + 启动后 QoS 应用。
//
// 依赖 virsh / qemu-img（ExecRunner 白名单），不经 shell。
// 幂等：域已定义则确保运行后直接成功（Agent 重试 / 控制面重复拉取均安全）。
// 失败语义：任何一步失败返回错误，由调用方回报控制面置 error 并释放四维预留；
// 重试 = 删除 VM 重新放置（容量账本在失败时已回滚）。

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProvisionSpec 一次建机任务的参数（对应控制面 pendingTask）。
type ProvisionSpec struct {
	VMID    string
	Name    string
	VCPUs   int32
	MemMB   int64
	DiskGB  int64
	MinKbps int64 // egress 保障带宽；0 = 无带宽策略
	MaxKbps int64 // egress 借用上限
}

// Provisioner 建机执行器。
type Provisioner struct {
	R         Runner
	DataDir   string // VM 数据根目录，默认 /var/lib/kvmcloud
	BaseImage string // qcow2 基础镜像路径（backing file）；空 = 建机必失败并给出可操作错误
	Bridge    string // 桥接网口，默认 virbr0（libvirt 默认 NAT 网络）
}

// MACFromVMID 由 VMID 确定性生成 52:54:00 前缀 MAC（重试/重建保持一致）。
func MACFromVMID(vmID string) string {
	sum := sha256.Sum256([]byte(vmID))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// DomainName libvirt 域名：kvmcloud-<vm_id>（全局唯一，避免重名冲突）。
func DomainName(vmID string) string { return "kvmcloud-" + vmID }

// Provision 建机（幂等）。成功返回时域处于 running 状态。
func (p *Provisioner) Provision(spec ProvisionSpec) error {
	dom := DomainName(spec.VMID)

	// 幂等：域已定义 → 确保运行即可
	if out, err := p.R.Run("virsh", "dominfo", dom); err == nil && out != "" {
		return p.ensureRunning(dom)
	}

	if p.BaseImage == "" {
		return fmt.Errorf("未配置基础镜像（agent.env 设置 AGENT_BASE_IMAGE=/path/to/base.qcow2），无法创建系统盘")
	}

	disk, err := p.prepareDisk(spec)
	if err != nil {
		return err
	}

	xmlPath, err := p.writeDomainXML(spec, disk)
	if err != nil {
		return err
	}
	if _, err := p.R.Run("virsh", "define", xmlPath); err != nil {
		return fmt.Errorf("定义域失败: %w", err)
	}
	return p.ensureRunning(dom)
}

// ensureRunning 确保域处于运行状态（shut off 时启动）。
func (p *Provisioner) ensureRunning(dom string) error {
	state, err := p.R.Run("virsh", "domstate", dom)
	if err != nil {
		return fmt.Errorf("查询域状态失败: %w", err)
	}
	low := strings.ToLower(state)
	if strings.Contains(low, "running") || strings.Contains(low, "idle") {
		return nil
	}
	if _, err := p.R.Run("virsh", "start", dom); err != nil {
		return fmt.Errorf("启动域失败: %w", err)
	}
	return nil
}

// prepareDisk 准备系统盘：qcow2（有基础镜像时以 backing file 方式创建，秒级完成）。
func (p *Provisioner) prepareDisk(spec ProvisionSpec) (string, error) {
	vmDir := filepath.Join(p.DataDir, "vms", spec.VMID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return "", fmt.Errorf("创建数据目录失败: %w", err)
	}
	disk := filepath.Join(vmDir, "disk.qcow2")
	if _, err := os.Stat(disk); err == nil {
		return disk, nil // 重试场景：盘已存在
	}
	// 数值参数由调用方（控制面 pendingTask）约束为正数；此处仅拼 int64 格式化结果
	args := []string{"create", "-f", "qcow2"}
	if p.BaseImage != "" {
		args = append(args, "-F", "qcow2", "-b", p.BaseImage)
	}
	args = append(args, disk, fmt.Sprintf("%dG", spec.DiskGB))
	if _, err := p.R.Run("qemu-img", args...); err != nil {
		return "", fmt.Errorf("创建系统盘失败: %w", err)
	}
	return disk, nil
}

// ApplyQoS 域启动后按 MAC 定位 tap 设备并应用 HTB（rate=min，ceil=max）。
// 无带宽策略（min=0）时跳过；QoS 失败不影响 VM 运行状态，由调用方记日志。
func (p *Provisioner) ApplyQoS(spec ProvisionSpec) error {
	if spec.MinKbps <= 0 || spec.MaxKbps < spec.MinKbps {
		return nil
	}
	out, err := p.R.Run("virsh", "domiflist", DomainName(spec.VMID))
	if err != nil {
		return fmt.Errorf("查询域网卡失败: %w", err)
	}
	mac := strings.ToLower(MACFromVMID(spec.VMID))
	tap := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && strings.ToLower(f[4]) == mac {
			tap = f[0]
			break
		}
	}
	if tap == "" {
		return fmt.Errorf("未找到 MAC %s 对应的 tap 设备", mac)
	}
	q := QoSApplier{R: p.R}
	if err := q.ApplyQoS(tap, spec.MinKbps, spec.MaxKbps); err != nil {
		return fmt.Errorf("应用 QoS 失败: %w", err)
	}
	return nil
}

// emulator 探测 qemu-system 模拟器路径与架构。
func (p *Provisioner) emulator() (path, arch string) {
	candidates := []string{
		"/usr/bin/qemu-system-x86_64",
		"/usr/bin/qemu-system-aarch64",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			if strings.HasSuffix(c, "aarch64") {
				return c, "aarch64"
			}
			return c, "x86_64"
		}
	}
	matches, _ := filepath.Glob("/usr/bin/qemu-system-*")
	if len(matches) > 0 {
		base := filepath.Base(matches[0])
		return matches[0], strings.TrimPrefix(base, "qemu-system-")
	}
	return "/usr/bin/qemu-system-x86_64", "x86_64" // 兜底：define 时由 libvirt 报错
}

// writeDomainXML 生成并落盘 libvirt 域 XML。
func (p *Provisioner) writeDomainXML(spec ProvisionSpec, disk string) (string, error) {
	emulator, arch := p.emulator()
	bridge := p.Bridge
	if bridge == "" {
		bridge = "virbr0"
	}
	xml := buildDomainXML(DomainName(spec.VMID), spec, disk, bridge, emulator, arch)
	xmlPath := filepath.Join(p.DataDir, "vms", spec.VMID, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte(xml), 0o644); err != nil {
		return "", fmt.Errorf("写域 XML 失败: %w", err)
	}
	return xmlPath, nil
}

// buildDomainXML 生成 libvirt 域 XML（参数均为内部受控值，不来自用户自由输入）。
func buildDomainXML(dom string, spec ProvisionSpec, disk, bridge, emulator, arch string) string {
	memKiB := spec.MemMB * 1024
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <title>%s</title>
  <memory unit='KiB'>%d</memory>
  <currentMemory unit='KiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>
  <os>
    <type arch='%s' machine='pc'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/>
    <apic/>
  </features>
  <clock offset='utc'/>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>restart</on_crash>
  <devices>
    <emulator>%s</emulator>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='writeback' discard='unmap'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <interface type='bridge'>
      <mac address='%s'/>
      <source bridge='%s'/>
      <model type='virtio'/>
    </interface>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>
    <memballoon model='virtio'/>
  </devices>
</domain>
`, dom, spec.Name, memKiB, memKiB, spec.VCPUs, arch, emulator, disk, MACFromVMID(spec.VMID), bridge)
}
