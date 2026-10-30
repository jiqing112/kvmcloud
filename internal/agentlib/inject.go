package agentlib

// Cloud-Init（ISO/NoCloud）与 Windows virtio 驱动注入骨架（模块 13 管道第 4 步）。
//
// 注入物只允许两种来源，绝不接受任务请求中的任意 ISO 路径/URL：
//   - CloudInitISO 生成的 seed.iso（user-data/meta-data 为任务参数中的受限字段）；
//   - 平台预置的 virtio-win.iso（DataDir/drivers 下，Agent 启动时按 SHA 清单校验）。
//
// 挂载经 virsh attach-disk 参数列表执行（白名单），域名为受限字符集，
// ISO 路径必须位于 DataDir 内且后缀 .iso——杜绝路径穿越。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// domainNameRe libvirt 域名白名单（与控制面 vmName 同字符集）。
var domainNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// AttachISO 以只读 CD-ROM 挂载 ISO 到域（virsh attach-disk，参数列表执行）。
// domain 必须匹配域名字符集；isoPath 必须位于 DataDir 内且后缀 .iso（防路径穿越）。
func (ip *ImagePipeline) AttachISO(domain, isoPath string) error {
	if !domainNameRe.MatchString(domain) {
		return fmt.Errorf("域名非法 %q", domain)
	}
	root := filepath.Clean(ip.DataDir)
	clean := filepath.Clean(isoPath)
	if !strings.HasPrefix(clean, root+string(filepath.Separator)) || filepath.Ext(clean) != ".iso" {
		return fmt.Errorf("ISO 路径越界（必须在 %s 内且后缀 .iso）: %q", ip.DataDir, isoPath)
	}
	_, err := ip.R.Run("virsh", "attach-disk", domain, clean, "sdb", "--type", "cdrom",
		"--mode", "readonly", "--config")
	return err
}

// InjectCloudInit 生成 NoCloud seed.iso 并挂载（qemu-img/gemisoimage 白名单路径）。
// 失败回退：删除 seed 目录（内含 user-data/meta-data/seed.iso），不留半成品。
func (ip *ImagePipeline) InjectCloudInit(domain, userData, metaData string) (string, error) {
	seed, err := ip.CloudInitISO(domain, userData, metaData)
	if err != nil {
		return "", err
	}
	if err = ip.AttachISO(domain, seed); err != nil {
		_ = os.RemoveAll(filepath.Dir(seed)) // 回退清理
		return "", fmt.Errorf("挂载 seed.iso 失败: %w", err)
	}
	return seed, nil
}

// InjectVirtioDrivers 将平台预置的 virtio-win.iso 挂载到 Windows 域。
// ISO 为部署期预置（路径固定），Agent 不从任务参数接收任何路径。
func (ip *ImagePipeline) InjectVirtioDrivers(domain string) (string, error) {
	iso := filepath.Join(ip.DataDir, "drivers", "virtio-win.iso")
	if _, err := os.Stat(iso); err != nil {
		return "", fmt.Errorf("virtio 驱动 ISO 未预置: %w", err)
	}
	if err := ip.AttachISO(domain, iso); err != nil {
		return "", fmt.Errorf("挂载 virtio 驱动失败: %w", err)
	}
	return iso, nil
}
