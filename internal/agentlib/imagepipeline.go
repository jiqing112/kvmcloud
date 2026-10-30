package agentlib

// 镜像拉取/准备管道（第二部分）：
//
//	DownloadImage → VerifyImage → ConvertImage →（建机时）InjectCloudInit / InjectVirtioDrivers → 汇报 storage_ref。
//
// 安全模型（与 qos.go 白名单一致）：
//   - 下载 URL 必须来自控制面（remote_images.download_url），且主机命中该源的
//     domain_allowlist；Agent 绝不接收请求体中的任意 URL；
//   - 仅 https；主机为 IP 字面量（环回/私有/保留地址）一律拒绝——防 SSRF 与内网探测；
//   - 下载经白名单 curl 参数列表执行（不经 shell）：--proto =https、--max-filesize
//     大小硬上限、--max-time 7200s 网络超时、--max-redirs 3 重定向次数限制；
//   - 落盘路径 = DataDir 前缀 + 校验过的规范名，杜绝路径穿越与命令注入；
//   - SHA-256 期望值来自源 CHECKSUM（VerifyImage）；observed 摘要经 HashFile
//     计算并回传控制面；
//   - 失败回退：cleanup 删除 .partial 与半成品，幂等（不存在视为成功）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ImagePullSpec 一次镜像准备任务参数（控制面 outbox → Agent 拉取接口下发）。
type ImagePullSpec struct {
	LocalImageID  string   // local_images.id（uuid）
	RemoteImageID string   // remote_images.id（uuid）
	Name          string   // 规范名 ubuntu-24.04-lts
	Version       string
	OSType        string // linux | windows
	Format        string // qcow2|raw|iso|vhd（远端原始格式）
	DownloadURL   string // 控制面下发（源同步所得），非请求输入
	SHA256        string // 空表示源未提供 CHECKSUM；仍计算 observed 值回传
	SizeLimit     int64  // egress_bytes_limit，0 = 默认 16GiB
	AllowDomains  []string
}

// ImagePipeline 镜像准备执行器；依赖 Runner（curl / qemu-img / genisoimage / timeout 均在白名单）。
type ImagePipeline struct {
	R       Runner
	DataDir string // 镜像池根目录，默认 /var/lib/kvmcloud/images
}

// 默认资源上限：单镜像 16GiB、下载 2h、转换 1h、genisoimage 5m。
const (
	defaultSizeLimit  = 16 << 30
	downloadTimeout   = 2 * time.Hour
	convertTimeout    = 1 * time.Hour
	injectTimeout     = 5 * time.Minute
	maxRedirects      = 3
	partialSuffix     = ".partial"
	defaultImagePerms = 0o644
)

var (
	imageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{1,127}$`)
	sha256HexRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// Prepare 编排：下载 → 校验 → 转换 → 一致性检查 → 返回最终 qcow2 路径。
// 幂等：产物已存在直接成功（Agent 重试/控制面重复下发均安全）。
func (ip *ImagePipeline) Prepare(spec ImagePullSpec) (string, error) {
	if !imageNameRe.MatchString(spec.Name) {
		return "", fmt.Errorf("镜像规范名非法 %q", spec.Name)
	}
	if err := ip.checkURL(spec); err != nil {
		return "", err
	}
	final := filepath.Join(ip.DataDir, spec.Name+".qcow2")
	if _, err := os.Stat(final); err == nil {
		return final, nil // 幂等重试
	}
	tmp := final + partialSuffix
	if err := ip.DownloadImage(spec, tmp); err != nil {
		cleanup(tmp)
		return "", fmt.Errorf("下载镜像失败: %w", err)
	}
	if err := VerifyImage(tmp, spec.SHA256, ip.limit(spec)); err != nil {
		cleanup(tmp)
		return "", err
	}
	if err := ip.ConvertImage(tmp, final, spec.Format); err != nil {
		cleanup(tmp, final)
		return "", fmt.Errorf("镜像转换失败: %w", err)
	}
	if _, err := ip.R.Run("qemu-img", "check", final); err != nil {
		cleanup(tmp, final)
		return "", fmt.Errorf("镜像一致性检查失败: %w", err)
	}
	return final, nil
}

// DownloadImage 白名单下载（curl 参数列表执行，不经 shell）：仅 https、
// --max-filesize 大小硬上限、--max-time 网络超时、--max-redirs 重定向次数限制、
// 输出到 DataDir 内固定 .partial 后缀。
func (ip *ImagePipeline) DownloadImage(spec ImagePullSpec, dst string) error {
	args := []string{"--fail", "--silent", "--show-error", "--proto", "=https",
		"--max-filesize", strconv.FormatInt(ip.limit(spec), 10),
		"--max-time", strconv.Itoa(int(downloadTimeout.Seconds())),
		"--max-redirs", strconv.Itoa(maxRedirects)}
	args = append(args, "--output", dst, spec.DownloadURL)
	_, err := ip.R.Run("curl", args...)
	return err
}

// VerifyImage SHA-256 校验：期望值来自源 CHECKSUM（非空时强校验，大小写不敏感）；
// 期望为空时跳过（observed 摘要由汇报阶段用 HashFile 计算并回传控制面）。
// limit 同时约束读取上限，防磁盘耗尽。
func VerifyImage(path, expect string, limit int64) error {
	if expect == "" {
		return nil
	}
	if !sha256HexRe.MatchString(expect) {
		return fmt.Errorf("期望 SHA256 格式非法")
	}
	observed, err := HashFile(path, limit)
	if err != nil {
		return err
	}
	if !strings.EqualFold(observed, expect) {
		return fmt.Errorf("SHA256 不匹配: got %s want %s", observed, expect)
	}
	return nil
}

// ConvertImage qemu-img convert 参数化调用：-f 由 formatFlag 枚举映射（qcow2/raw/
// iso→raw/vhd→vpc），-O 固定 qcow2；src/dst 由调用方以校验过的规范名构造；
// 经 timeout(1) 包裹实现进程级硬超时（--kill-after 强杀）。
func (ip *ImagePipeline) ConvertImage(src, dst, format string) error {
	_, err := ip.R.Run("timeout", "--kill-after=10", strconv.Itoa(int(convertTimeout.Seconds())),
		"qemu-img", "convert", "-f", formatFlag(format), "-O", "qcow2", src, dst)
	return err
}

// HashFile 流式 SHA-256（limit 字节硬上限；超过即拒绝）。
func HashFile(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if limit > 0 {
		n, err := io.Copy(h, io.LimitReader(f, limit+1))
		if err != nil {
			return "", err
		}
		if n > limit {
			return "", fmt.Errorf("镜像超过大小上限 %d 字节", limit)
		}
	} else if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkURL 下载 URL 校验：https + 带主机名 + 主机命中 allowlist + 拒绝 IP 字面量
//（环回/私有/保留地址天然被拒，防 SSRF 与内网探测）。
func (ip *ImagePipeline) checkURL(spec ImagePullSpec) error {
	u, err := url.Parse(spec.DownloadURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("下载 URL 非法（仅允许 https 且带主机名）: %q", spec.DownloadURL)
	}
	host := strings.ToLower(u.Hostname())
	if addr, err := netip.ParseAddr(host); err == nil {
		return fmt.Errorf("下载主机不允许为 IP 字面量（含环回/私有/保留地址）: %s", addr)
	}
	for _, d := range spec.AllowDomains {
		// allowlist 条目本身也必须是合法域名（拒绝把 IP 写进白名单）。
		if _, err := netip.ParseAddr(strings.ToLower(d)); err == nil {
			continue
		}
		if host == strings.ToLower(d) {
			return nil
		}
	}
	return fmt.Errorf("下载主机 %q 不在源 allowlist 内", host)
}

// limit 单镜像大小上限：spec 显式值优先，否则默认 16GiB；上限封顶 16GiB（防线炸）。
func (ip *ImagePipeline) limit(spec ImagePullSpec) int64 {
	if spec.SizeLimit > 0 && spec.SizeLimit < defaultSizeLimit {
		return spec.SizeLimit
	}
	return defaultSizeLimit
}

// cleanup 失败回退：删除半成品（幂等，不存在视为成功）。
func cleanup(paths ...string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// CloudInitISO 生成 NoCloud 的 user-data/meta-data 并打包 seed.iso（genisoimage 白名单，
// timeout 包裹）。内容为任务参数中的受限字符串字段；路径 = DataDir/seed/{vmID}/。
func (ip *ImagePipeline) CloudInitISO(vmID, userData, metaData string) (string, error) {
	if !imageNameRe.MatchString(vmID) {
		return "", fmt.Errorf("VM 标识非法 %q", vmID)
	}
	dir := filepath.Join(ip.DataDir, "seed", vmID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "user-data"), []byte(userData), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta-data"), []byte(metaData), 0o600); err != nil {
		return "", err
	}
	seed := filepath.Join(dir, "seed.iso")
	if _, err := ip.R.Run("timeout", "--kill-after=5", strconv.Itoa(int(injectTimeout.Seconds())),
		"genisoimage", "-quiet", "-output", seed, "-volid", "cidata", "-joliet", "-rock",
		filepath.Join(dir, "user-data"), filepath.Join(dir, "meta-data")); err != nil {
		return "", fmt.Errorf("生成 cloud-init 种子盘失败: %w", err)
	}
	return seed, nil
}

func formatFlag(f string) string {
	switch f {
	case "raw":
		return "raw"
	case "iso":
		return "raw" // ISO 按原始流转换（保持可执行内容）
	case "vhd":
		return "vpc"
	default:
		return "qcow2"
	}
}
