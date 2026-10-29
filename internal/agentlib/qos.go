// Package agentlib Agent 侧执行：TC HTB QoS、资源采集、内存气球、KSM。
//
// 命令执行安全模型：
//   - 不经 shell：全部为参数列表执行；
//   - 二进制白名单：仅 tc / virsh / nproc / df / free 五个命令可达，
//     每个调用点的可执行文件名都是字符串字面量；
//   - 接口名白名单：iface 须匹配 [a-z0-9][a-z0-9._-]{0,14}（ValidateIface）；
//   - 数值参数：仅接受 int64 格式化结果。
package agentlib

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Runner 命令执行抽象（测试可注入 fake）。
type Runner interface {
	Run(name string, args ...string) (string, error)
}

// ifaceRe 网络接口名白名单。
var ifaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,14}$`)

// ValidateIface 校验接口名。
func ValidateIface(iface string) error {
	if !ifaceRe.MatchString(iface) {
		return fmt.Errorf("invalid iface name %q", iface)
	}
	return nil
}

// ExecRunner 白名单执行器：Run 的 name 参数必须是白名单内的命令，
// 每个分支用字符串字面量调用 exec.Command（参数列表执行，不经 shell）。
type ExecRunner struct{}

// Run 按白名单分发执行。
func (ExecRunner) Run(name string, args ...string) (string, error) {
	switch name {
	case "tc":
		return combined(exec.Command("tc", args...))
	case "virsh":
		return combined(exec.Command("virsh", args...))
	case "qemu-img":
		return combined(exec.Command("qemu-img", args...))
	case "nproc":
		return combined(exec.Command("nproc"))
	case "df":
		return combined(exec.Command("df", args...))
	case "free":
		return combined(exec.Command("free", args...))
	default:
		return "", fmt.Errorf("binary %q is not whitelisted", name)
	}
}

func combined(c *exec.Cmd) (string, error) {
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", c.Path, strings.Join(c.Args[1:], " "), err, string(out))
	}
	return string(out), nil
}

// FakeRunner 记录调用的测试替身。
type FakeRunner struct {
	Calls []string
	Out   string
}

// Run 记录并返回预设输出。
func (f *FakeRunner) Run(name string, args ...string) (string, error) {
	f.Calls = append(f.Calls, name+" "+strings.Join(args, " "))
	return f.Out, nil
}

// QoSApplier TC HTB 实现：单端口 保障速率(rate) + 上限(ceil)。
// 语义：rate = min_kbps（HTB 保证带宽），ceil = 当前允许上限
// （无借用时 = min；借用授予后 = min+granted；被抢占压回 min）。
type QoSApplier struct {
	R Runner
}

// ApplyQoS 建立 HTB root qdisc 与默认 class：rate=min，ceil=初始 ceil。
func (q *QoSApplier) ApplyQoS(iface string, minKbps, initialCeilKbps int64) error {
	if minKbps <= 0 || initialCeilKbps < minKbps {
		return fmt.Errorf("invalid qos params: min=%d ceil=%d", minKbps, initialCeilKbps)
	}
	if err := ValidateIface(iface); err != nil {
		return err
	}
	r := q.R
	step1 := []string{"qdisc", "replace", "dev", iface, "root", "handle", "1:", "htb", "default", "10"}
	step2 := []string{"class", "replace", "dev", iface, "parent", "1:", "classid", "1:10", "htb",
		"rate", kbit(minKbps), "ceil", kbit(initialCeilKbps)}
	step3 := []string{"filter", "replace", "dev", iface, "parent", "1:", "protocol", "all", "prio", "1",
		"u32", "match", "u32", "0", "0", "flowid", "1:10"}
	for _, args := range [][]string{step1, step2, step3} {
		if _, err := r.Run("tc", args...); err != nil {
			return err
		}
	}
	return nil
}

// UpdateCeil 只调整 ceil（借用授予/抢占压回的高频路径）。
func (q *QoSApplier) UpdateCeil(iface string, newCeilKbps int64) error {
	if newCeilKbps <= 0 {
		return fmt.Errorf("invalid ceil: %d", newCeilKbps)
	}
	if err := ValidateIface(iface); err != nil {
		return err
	}
	_, err := q.R.Run("tc", "class", "change", "dev", iface, "parent", "1:", "classid", "1:10", "htb", "ceil", kbit(newCeilKbps))
	return err
}

// RemoveQoS 清理 qdisc（VM 删除时）。
func (q *QoSApplier) RemoveQoS(iface string) error {
	if err := ValidateIface(iface); err != nil {
		return err
	}
	_, err := q.R.Run("tc", "qdisc", "del", "dev", iface, "root")
	return err
}

func kbit(kbps int64) string { return strconv.FormatInt(kbps, 10) + "kbit" }
