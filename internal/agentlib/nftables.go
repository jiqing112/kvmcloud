package agentlib

// nftables NAT（模块 6）与分布式安全组（模块 9）Agent 侧执行器。
//
// 安全模型：
//   - 仅经 `nft -f <file>` 应用；规则文件由结构化字段生成，动态值全部先过
//     类型校验（协议枚举、端口 1..65535、netip 前缀解析、ValidateIface），
//     表/链名为字面量（kvmcloud_nat / kvmcloud_sec），租户输入不可能进入
//     命令行或规则文件的注释/字符串位置；
//   - 声明式全量收敛：每次 apply flush 表后重建，重复应用幂等；
//   - 与 TC 限速共存：NAT/安全组作用于 netfilter hook，TC HTB 作用于 qdisc，
//     两者分层独立，NAT 不破坏 Tap 设备上的限速（速率语义见 QoSApplier）；
//   - 临时规则文件落在 ConfDir（0600），应用后立即删除，失败不残留。

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// NftApplier nftables 执行器（依赖 Runner：nft 已在白名单）。
type NftApplier struct {
	R       Runner
	ConfDir string // 规则临时目录，默认 /var/lib/kvmcloud/nft
}

// natTable / secTable 固定表名（字面量，防注入）。
const (
	natTable = "kvmcloud_nat"
	secTable = "kvmcloud_sec"
	// maxNATMappings / maxSecRules 与控制面下发的声明式上限一致。
	maxNATMappings = 256
	maxSecRules    = 400
)

// PortForwarding 单条 NAT 映射（控制面声明式下发）。
type PortForwarding struct {
	Protocol     string // tcp | udp
	ExternalPort int
	InternalPort int
	InternalIPv4 string
}

// SecGroupRule 单条安全组规则（与 security_group_rules 列一一对应）。
type SecGroupRule struct {
	GroupID    string // security_groups.id（uuid）
	Direction  string // ingress | egress
	EtherType  int    // 4 | 6
	Protocol   string // any | tcp | udp | icmp | icmpv6
	RemoteCIDR string // 带掩码 CIDR（控制面 DDL 已约束 family=ethertype）
	PortFrom   *int
	PortTo     *int
	ICMPType   *int
	ICMPCode   *int
}

// ApplyNAT 应用完整 NAT 映射集（声明式收敛：flush 后重建）。
func (n *NftApplier) ApplyNAT(wanIface string, mappings []PortForwarding) error {
	if err := ValidateIface(wanIface); err != nil {
		return err
	}
	if len(mappings) > maxNATMappings {
		return fmt.Errorf("NAT 映射数超上限 %d", maxNATMappings)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table ip %s {}\n", natTable) // 不存在则创建
	fmt.Fprintf(&b, "flush table ip %s\n\n", natTable)
	fmt.Fprintf(&b, "table ip %s {\n", natTable)
	fmt.Fprintf(&b, "  chain prerouting {\n    type nat hook prerouting priority dstnat; policy accept;\n")
	for _, m := range mappings {
		line, err := natRule(m)
		if err != nil {
			return err
		}
		b.WriteString(line)
	}
	fmt.Fprintf(&b, "  }\n  chain postrouting {\n    type nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "    oifname %q masquerade\n", wanIface)
	fmt.Fprintf(&b, "  }\n}\n")
	return n.apply(b.String())
}

// natRule 渲染一条 dnat 规则：动态值全部过类型校验。
func natRule(m PortForwarding) (string, error) {
	if m.Protocol != "tcp" && m.Protocol != "udp" {
		return "", fmt.Errorf("NAT 协议枚举非法: %q", m.Protocol)
	}
	if m.ExternalPort < 1 || m.ExternalPort > 65535 ||
		m.InternalPort < 1 || m.InternalPort > 65535 {
		return "", fmt.Errorf("NAT 端口越界: %d/%d", m.ExternalPort, m.InternalPort)
	}
	ip, err := netip.ParseAddr(m.InternalIPv4)
	if err != nil || !ip.Is4() || ip.Zone() != "" {
		return "", fmt.Errorf("NAT 内网地址非法: %q", m.InternalIPv4)
	}
	return fmt.Sprintf("    %s dport %d dnat ip to %s:%d\n",
		m.Protocol, m.ExternalPort, ip.String(), m.InternalPort), nil
}

// ApplySecurityGroups 应用 VM 主 NIC 的安全组规则集（声明式收敛）。
// 默认策略：无规则 = 全放行（与安全组语义一致）；有规则 = 白名单 accept + 末尾 drop。
func (n *NftApplier) ApplySecurityGroups(tapIface string, rules []SecGroupRule) error {
	if err := ValidateIface(tapIface); err != nil {
		return err
	}
	if len(rules) > maxSecRules {
		return fmt.Errorf("安全组规则数超上限 %d", maxSecRules)
	}
	var ingress, egress []string
	for _, r := range rules {
		line, err := secRule(r)
		if err != nil {
			return err
		}
		switch r.Direction {
		case "ingress": // 流向 VM 的流量在 forward 链上匹配 oifname=tap
			ingress = append(ingress, fmt.Sprintf("      oifname %q %s accept\n", tapIface, line))
		case "egress": // VM 发出的流量匹配 iifname=tap
			egress = append(egress, fmt.Sprintf("      iifname %q %s accept\n", tapIface, line))
		default:
			return fmt.Errorf("安全组方向枚举非法: %q", r.Direction)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {}\n", secTable)
	fmt.Fprintf(&b, "flush table inet %s\n\n", secTable)
	fmt.Fprintf(&b, "table inet %s {\n", secTable)
	fmt.Fprintf(&b, "  chain forward {\n    type filter hook forward priority filter; policy accept;\n")
	fmt.Fprintf(&b, "      iifname %q ct state established,related accept\n", tapIface)
	for _, line := range ingress {
		b.WriteString(line)
	}
	for _, line := range egress {
		b.WriteString(line)
	}
	// 有规则的方向收敛为默认丢弃（白名单模式）；无规则方向保持放行。
	if len(ingress) > 0 {
		fmt.Fprintf(&b, "      oifname %q counter drop\n", tapIface)
	}
	if len(egress) > 0 {
		fmt.Fprintf(&b, "      iifname %q counter drop\n", tapIface)
	}
	fmt.Fprintf(&b, "  }\n}\n")
	return n.apply(b.String())
}

// secRule 渲染单条匹配条件（不含 accept/drop 动作）。
func secRule(r SecGroupRule) (string, error) {
	prefix, err := netip.ParsePrefix(r.RemoteCIDR)
	if err != nil {
		return "", fmt.Errorf("远端 CIDR 非法: %q", r.RemoteCIDR)
	}
	// family 与 ethertype 一致性（DDL CHECK 已约束，Agent 侧双保险）。
	if (prefix.Addr().Is4() && r.EtherType != 4) || (prefix.Addr().Is6() && r.EtherType != 6) {
		return "", fmt.Errorf("CIDR 族与 ethertype 不一致: %q ethertype=%d", r.RemoteCIDR, r.EtherType)
	}
	family := "ip"
	if r.EtherType == 6 {
		family = "ip6"
	}
	switch r.Protocol {
	case "tcp", "udp":
		if r.PortFrom == nil || r.PortTo == nil || *r.PortFrom < 1 || *r.PortTo > 65535 || *r.PortFrom > *r.PortTo {
			return "", fmt.Errorf("TCP/UDP 端口区间非法")
		}
		if *r.PortFrom == *r.PortTo {
			return fmt.Sprintf("%s %s dport %d", family, r.Protocol, *r.PortFrom), nil
		}
		return fmt.Sprintf("%s %s dport %d-%d", family, r.Protocol, *r.PortFrom, *r.PortTo), nil
	case "icmp", "icmpv6":
		if r.ICMPType == nil || *r.ICMPType < 0 || *r.ICMPType > 255 {
			return "", fmt.Errorf("ICMP type 非法")
		}
		if r.ICMPCode != nil && *r.ICMPCode >= 0 && *r.ICMPCode <= 255 {
			return fmt.Sprintf("%s %s type %d code %d", family, r.Protocol, *r.ICMPType, *r.ICMPCode), nil
		}
		return fmt.Sprintf("%s %s type %d", family, r.Protocol, *r.ICMPType), nil
	case "any":
		return fmt.Sprintf("%s saddr %s", family, prefix.String()), nil
	default:
		return "", fmt.Errorf("协议枚举非法: %q", r.Protocol)
	}
}

// apply 写临时规则文件（0600）后 nft -f 应用，应用完立即删除。
func (n *NftApplier) apply(content string) error {
	if err := os.MkdirAll(n.ConfDir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(n.ConfDir, "rules-*.nft")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path) // 幂等清理：应用成功/失败都不残留
	if _, err = f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return err
	}
	_, err = n.R.Run("nft", "-f", path)
	return err
}

// RemoveNAT / RemoveSecurityGroups：VM 删除时整表清理（幂等，表不存在亦成功）。
func (n *NftApplier) RemoveNAT() error {
	_, err := n.R.Run("nft", "delete", "table", "ip", natTable)
	return ignoreNotExist(err)
}

func (n *NftApplier) RemoveSecurityGroups() error {
	_, err := n.R.Run("nft", "delete", "table", "inet", secTable)
	return ignoreNotExist(err)
}

// ignoreNotExist nft 对不存在对象的删除报错；幂等语义下视为成功。
func ignoreNotExist(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "No such file or directory") ||
		strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}
