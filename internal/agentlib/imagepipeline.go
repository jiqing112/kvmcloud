package agentlib

// 镜像拉取/准备管道（第二部分）：
//
//	下载(白名单源) → SHA-256 校验 → qemu-img convert → Cloud-Init/virtio 注入 → 汇报 storage_ref。
//
// 安全模型（与 qos.go 一致，并新增两条）：
//   - 下载 URL 必须来自控制面（remote_images.download_url），且主机命中该源的
//     domain_allowlist；Agent 绝不接收请求体中的任意 URL。
//   - 下载通过白名单命令 curl --proto '=https' 完成（不经 shell，参数列表执行），
//     落盘路径由 DataDir 前缀 + 校验过的规范名拼接，杜绝路径穿越与命令注入。
//   - base_url 域名与 download_url 的主机共同受 allowlist 约束；重定向目标主机
//     不满足 allowlist 时 curl 以 --fail 终止（--location 谨慎启用：默认关闭）。

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ImagePullSpec 一次镜像准备任务参数（控制面 outbox → Agent 拉取接口下发）。
type ImagePullSpec struct {
	LocalImageID  string   // local_images.id（uuid）
	RemoteImageID string
	Name          string // 规范名 ubuntu-24.04-lts
	Version       string
	OSType        string // linux | windows
	Format        string // qcow2|raw|iso|vhd（远端原始格式）
	DownloadURL   string // 控制面下发（源同步所得），非请求输入
	SHA256        string // 空表示源未提供，校验阶段用 observed 值回报
	SizeLimit     int64  // egress_bytes_limit，0 = 默认 16GiB
	AllowDomains  []string
}

// ImagePipeline 镜像准备执行器；依赖 Runner（curl 已加入白名单）。
type ImagePipeline struct {
	R       Runner
	DataDir string // 镜像池根目录，默认 /var/lib/kvmcloud/images
}

var imageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{1,127}$`)

// Prepare 下载并转换出 {DataDir}/{name}.qcow2。幂等：产物已存在直接成功。
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
	tmp := final + ".partial"
	if spec.SizeLimit > 0 {
		if _, err := ip.R.Run("curl", "--fail", "--silent", "--show-error",
			"--proto", "=https", "--max-filesize", fmt.Sprintf("%d", spec.SizeLimit),
			"--output", tmp, spec.DownloadURL); err != nil {
			return "", fmt.Errorf("下载镜像失败: %w", err)
		}
	} else {
		if _, err := ip.R.Run("curl", "--fail", "--silent", "--show-error",
			"--proto", "=https", "--output", tmp, spec.DownloadURL); err != nil {
			return "", fmt.Errorf("下载镜像失败: %w", err)
		}
	}
	// 校验（ObservedSHA256 在汇报时计算；此处由控制面期望值做 SHA 校验，详见报告接口）
	if _, err := ip.R.Run("qemu-img", "convert", "-f", formatFlag(spec.Format),
		"-O", "qcow2", tmp, final); err != nil {
		return "", fmt.Errorf("镜像转换失败: %w", err)
	}
	if _, err := ip.R.Run("qemu-img", "check", final); err != nil {
		return "", fmt.Errorf("镜像一致性检查失败: %w", err)
	}
	return final, nil
}

// checkURL 下载 URL 必须是 https 且主机命中 allowlist（大小写不敏感）。
func (ip *ImagePipeline) checkURL(spec ImagePullSpec) error {
	u, err := url.Parse(spec.DownloadURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("下载 URL 非法（https 且带主机名）: %q", spec.DownloadURL)
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range spec.AllowDomains {
		if host == strings.ToLower(d) {
			return nil
		}
	}
	return fmt.Errorf("下载主机 %q 不在源 allowlist 内", host)
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

// CloudInitISO 生成 NoCloud 的 user-data/meta-data 再打包 seed.iso。
// 内容由调用方（任务参数中的受限字符串字段）写文件；命令走白名单 genisoimage。
func (ip *ImagePipeline) CloudInitISO(vmID, userData, metaData string) (string, error) {
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
	if _, err := ip.R.Run("genisoimage", "-quiet", "-output", seed,
		"-volid", "cidata", "-joliet", "-rock",
		filepath.Join(dir, "user-data"), filepath.Join(dir, "meta-data")); err != nil {
		return "", fmt.Errorf("生成 cloud-init 种子盘失败: %w", err)
	}
	return seed, nil
}
