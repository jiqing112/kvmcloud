// Package updater 系统本体自动更新（控制面/Agent 通用）：
//
//	定时检测 → 校验下载 → 原子替换 → systemd 重启 → 失败自动回滚。
//
// 安全模型（与 install.sh 下载通道一致）：
//   - 检测/下载仅允许固定 https 主机族（gitee.com / github.com / githubusercontent.com），
//     重定向目标同样受白名单约束，拒绝一切其它主机；
//   - tag/构件名均由固定模板生成，不接受网络数据中的路径或命令；
//   - 二进制必须通过 checksums.txt 的 SHA-256 校验；tar 提取带 zip-slip 防护与
//     解压总量上限；下载体积/时限双重封顶；
//   - 替换前保留 .bak；重启后 is-active 探测失败自动回滚旧版并再次重启；
//   - dev 版本（本地构建）不参与自动更新；禁止降级；同 tag 连续失败 3 次熔断。
package updater

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/kvm-cloud/platform/internal/agentlib"
)

// ---------------- 发布通道常量（与 install.sh / .goreleaser.yml 对齐） ----------------

const (
	giteeOwner  = "yuntianojo"
	giteeRepo   = "kvmcloud"
	githubOwner = "ojoti"
	githubRepo  = "kvmcloud"

	// 资源上限：下载 ≤512MiB、解压总量 ≤512MiB、单请求 ≤15s、下载 ≤10m。
	maxDownloadBytes  = int64(512) << 20
	maxExtractedBytes = int64(512) << 20
	apiTimeout        = 15 * time.Second
	downloadTimeout   = 10 * time.Minute
	restartTimeout    = 30 * time.Second
	restartPollEvery  = 500 * time.Millisecond
	restartPollMax    = 20 // is-active 探测次数（≈10s）
	maxTagAttempts    = 3  // 同 tag 连续失败熔断
)

// Mode 更新模式：off 关闭；check 仅检测告警；auto 后台自动更新。
type Mode string

const (
	ModeOff   Mode = "off"
	ModeCheck Mode = "check"
	ModeAuto  Mode = "auto"
)

// Config 更新器配置（FromEnv 生成；测试可直接构造）。
type Config struct {
	Component      string        // control | agent（构件名 kvmcloud-{Component}）
	CurrentVersion string        // ldflags 注入；"dev" 不参与自动更新
	Mode           Mode          // off | check | auto
	Channel        string        // auto | gitee | github
	Interval       time.Duration // 检测间隔（≥10m）
	Jitter         time.Duration // 首次检测前随机延迟（防雷群）
	RestartUnit    string        // systemd 单元名
	HTTPTimeout    time.Duration // 单请求超时（覆盖 API 用）
}

// Updater 自更新执行器。
type Updater struct {
	cfg      Config
	log      *slog.Logger
	http     *http.Client
	failures map[string]int // tag → 连续失败次数
}

// New 构造（http.Client 自带重定向主机白名单）。
func New(cfg Config, log *slog.Logger) *Updater {
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = apiTimeout
	}
	return &Updater{
		cfg:      cfg,
		log:      log,
		failures: map[string]int{},
		http:     &http.Client{CheckRedirect: redirectGuard},
	}
}

// FromEnv 从环境变量构造；KC_AUTO_UPDATE=off（或非 linux 平台）返回 (nil, nil)。
//
//	KC_AUTO_UPDATE=auto|check|off   模式，默认 auto
//	KC_UPDATE_CHANNEL=auto|gitee|github  通道，默认 auto（gitee 优先，失败回退 github）
//	KC_UPDATE_INTERVAL=6h           检测间隔，默认 6h，下限 10m
//	KC_UPDATE_JITTER=30m            首检随机延迟，默认 30m
func FromEnv(component, currentVersion, unit string, log *slog.Logger) (*Updater, error) {
	mode := Mode(envOr("KC_AUTO_UPDATE", "auto"))
	if runtime.GOOS != "linux" {
		log.Info("auto update disabled (non-linux host)", "mode", mode)
		return nil, nil
	}
	switch mode {
	case ModeOff:
		return nil, nil
	case ModeCheck, ModeAuto:
	default:
		log.Warn("invalid KC_AUTO_UPDATE, updater disabled", "value", mode)
		return nil, nil
	}
	channel := envOr("KC_UPDATE_CHANNEL", "auto")
	switch channel {
	case "auto", "gitee", "github":
	default:
		log.Warn("invalid KC_UPDATE_CHANNEL, updater disabled", "value", channel)
		return nil, nil
	}
	interval := envDur("KC_UPDATE_INTERVAL", 6*time.Hour)
	if interval < 10*time.Minute {
		interval = 10 * time.Minute
	}
	return New(Config{
		Component:      component,
		CurrentVersion: currentVersion,
		Mode:           mode,
		Channel:        channel,
		Interval:       interval,
		Jitter:         envDur("KC_UPDATE_JITTER", 30*time.Minute),
		RestartUnit:    unit,
	}, log), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// Run 更新循环：先随机延迟（防雷群），后按 Interval 周期检测；阻塞直到 ctx 结束。
func (u *Updater) Run(ctx context.Context) {
	first := time.NewTimer(u.delay())
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	u.checkAndUpdate(ctx)
	tick := time.NewTicker(u.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			u.checkAndUpdate(ctx)
		}
	}
}

// delay 首检延迟：Interval 的 1/4 与 Jitter 取小者内随机，避免全员同时打 API。
func (u *Updater) delay() time.Duration {
	upper := u.cfg.Jitter
	if cap := u.cfg.Interval / 4; cap < upper {
		upper = cap
	}
	if upper <= 0 {
		return 0
	}
	return rand.N(upper)
}

// checkAndUpdate 单轮：检测 → （auto 模式）校验下载 → 替换重启。
func (u *Updater) checkAndUpdate(ctx context.Context) {
	current, ok := ParseVersion(u.cfg.CurrentVersion)
	if !ok {
		// dev/非法版本：本地构建不自动更新，仅记录一次。
		u.log.Warn("auto update unavailable for non-semver build", "version", u.cfg.CurrentVersion)
		return
	}
	tag, err := u.latestTag(ctx)
	if err != nil {
		u.log.Warn("version check failed", "err", err)
		return
	}
	latest, ok := ParseVersion(tag)
	if !ok {
		u.log.Warn("upstream tag invalid", "tag", tag)
		return
	}
	if Compare(latest, current) <= 0 {
		return // 已是最新（禁止降级）
	}
	if u.cfg.Mode == ModeCheck {
		u.log.Info("new version available", "current", u.cfg.CurrentVersion, "latest", tag)
		return
	}
	if u.failures[tag] >= maxTagAttempts {
		u.log.Warn("update suspended after repeated failures", "tag", tag)
		return
	}
	if err = u.performUpdate(ctx, tag); err != nil {
		u.failures[tag]++
		u.log.Error("self update failed", "tag", tag, "attempt", u.failures[tag], "err", err)
		return
	}
	u.log.Info("self update applied", "from", u.cfg.CurrentVersion, "to", tag)
}

// ---------------- 版本检测（双通道，auto = gitee 优先回退 github） ----------------

type releaseInfo struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

func (u *Updater) latestTag(ctx context.Context) (string, error) {
	channels := []string{u.cfg.Channel}
	if u.cfg.Channel == "auto" {
		channels = []string{"gitee", "github"}
	}
	var lastErr error
	for _, ch := range channels {
		if ch == "auto" { // 显式 auto 不会出现在这里；防御
			continue
		}
		info, err := u.fetchRelease(ctx, ch)
		if err != nil {
			lastErr = err
			u.log.Debug("channel probe failed", "channel", ch, "err", err)
			continue
		}
		if info.Draft || info.Prerelease {
			return "", fmt.Errorf("channel %s latest is draft/prerelease", ch)
		}
		return info.TagName, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no channel available")
	}
	return "", lastErr
}

func (u *Updater) fetchRelease(ctx context.Context, channel string) (*releaseInfo, error) {
	apiURL, err := latestAPIURL(channel)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("latest api %s: %s", channel, resp.Status)
	}
	var info releaseInfo
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return nil, err
	}
	if !tagRe.MatchString(info.TagName) {
		return nil, fmt.Errorf("channel %s tag invalid: %q", channel, info.TagName)
	}
	return &info, nil
}

func latestAPIURL(channel string) (string, error) {
	switch channel {
	case "gitee":
		return fmt.Sprintf("https://gitee.com/api/v5/repos/%s/%s/releases/latest", giteeOwner, giteeRepo), nil
	case "github":
		return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo), nil
	default:
		return "", fmt.Errorf("unknown channel %q", channel)
	}
}

// ---------------- 下载 / 校验 / 提取 ---------------------------------------

func archiveName(component string) string {
	return fmt.Sprintf("kvmcloud-%s_%s_%s.tar.gz", component, runtime.GOOS, runtime.GOARCH)
}

// archiveURL / checksumURL 固定模板：tag 已过 tagRe，文件名由本端常量生成。
func archiveURL(channel, tag, file string) (string, error) {
	switch channel {
	case "gitee":
		return fmt.Sprintf("https://gitee.com/%s/%s/releases/download/%s/%s", giteeOwner, giteeRepo, tag, file), nil
	case "github":
		return fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", githubOwner, githubRepo, tag, file), nil
	default:
		return "", fmt.Errorf("unknown channel %q", channel)
	}
}

// performUpdate 全流程：checksums → 归档下载 → SHA-256 校验 → 提取 → 原子替换
// → systemd 重启 → 健康探测，失败自动回滚。
func (u *Updater) performUpdate(ctx context.Context, tag string) error {
	// 归档落盘目录必须与当前二进制同文件系统（保证 rename 原子性）。
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return err
	}
	binDir := filepath.Dir(execPath)
	binName := "kvmcloud-" + u.cfg.Component

	channel, err := u.resolveChannel(ctx)
	if err != nil {
		return err
	}
	// 1) checksums.txt（与归档同 tag 同通道）。
	sums, err := u.downloadChecksums(ctx, channel, tag)
	if err != nil {
		return err
	}
	name := archiveName(u.cfg.Component)
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("checksums.txt missing entry for %s", name)
	}
	// 2) 归档下载 + SHA-256 校验。
	archivePath, err := u.downloadFile(ctx, channel, tag, name, binDir)
	if err != nil {
		return err
	}
	defer os.Remove(archivePath)
	got, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch for %s: got %s want %s", name, got, want)
	}
	// 3) 提取（zip-slip 防护 + 总量上限）到同目录临时工作区。
	workDir, err := os.MkdirTemp(binDir, "kvmcloud-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	if err = extractBinary(archivePath, workDir, binName); err != nil {
		return err
	}
	newBin := filepath.Join(workDir, binName)
	if err = os.Chmod(newBin, 0o755); err != nil {
		return err
	}
	// 4) 原子替换 + systemd 重启 + 失败回滚（.bak 保留上一版）。
	if err = swapAndRestart(ctx, execPath, newBin, u.cfg.RestartUnit); err != nil {
		return err
	}
	return nil
}

// resolveChannel auto 模式下按 gitee→github 探测出可用通道。
func (u *Updater) resolveChannel(ctx context.Context) (string, error) {
	if u.cfg.Channel != "auto" {
		return u.cfg.Channel, nil
	}
	for _, ch := range []string{"gitee", "github"} {
		if _, err := u.fetchRelease(ctx, ch); err == nil {
			return ch, nil
		}
	}
	return "", errors.New("no release channel reachable")
}

func (u *Updater) downloadChecksums(ctx context.Context, channel, tag string) (map[string]string, error) {
	sumsURL, err := archiveURL(channel, tag, "checksums.txt")
	if err != nil {
		return nil, err
	}
	raw, err := u.downloadBytes(ctx, sumsURL, 1<<20)
	if err != nil {
		return nil, err
	}
	return parseChecksums(string(raw))
}

// downloadBytes 拉取小型文本（体积硬上限）。
func (u *Updater) downloadBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", safeURL(rawURL), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes: %s", limit, safeURL(rawURL))
	}
	return data, nil
}

// downloadFile 拉取归档到 binDir（同文件系统），返回落盘路径；体积/时限硬上限。
func (u *Updater) downloadFile(ctx context.Context, channel, tag, name, binDir string) (string, error) {
	rawURL, err := archiveURL(channel, tag, name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", safeURL(rawURL), resp.Status)
	}
	dst := filepath.Join(binDir, name+".updating")
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && n > maxDownloadBytes {
		err = fmt.Errorf("archive exceeds %d bytes", maxDownloadBytes)
	}
	if err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// ---------------- 替换 / 重启 / 回滚 ---------------------------------------

// swapAndRestart 原子替换二进制并重启 systemd 单元；健康探测失败自动回滚。
// 未运行于 systemd（is-active 失败）时只替换并提示手工重启。
func swapAndRestart(ctx context.Context, execPath, newBin, unit string) error {
	backup := execPath + ".bak"
	_ = os.Remove(backup)
	if err := os.Rename(execPath, backup); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(newBin, execPath); err != nil {
		_ = os.Rename(backup, execPath) // 还原
		return fmt.Errorf("install new binary: %w", err)
	}
	managed, err := unitIsActive(ctx, unit)
	if err != nil || !managed {
		// 非 systemd 环境（手工运行/开发机）：替换完成，由调用方自行重启。
		return nil
	}
	if err = restartUnit(ctx, unit); err != nil {
		return rollback(ctx, execPath, backup, unit, err)
	}
	if err = waitActive(ctx, unit); err != nil {
		return rollback(ctx, execPath, backup, unit, err)
	}
	_ = os.Remove(backup) // 成功后保留删除；.bak 只在失败回滚时使用
	return nil
}

// rollback 恢复旧版二进制并再次重启；回滚失败仅能报警（不二次自愈）。
func rollback(ctx context.Context, execPath, backup, unit string, cause error) error {
	if rbErr := os.Rename(backup, execPath); rbErr != nil {
		return fmt.Errorf("update failed (%v) AND rollback failed (%w)", cause, rbErr)
	}
	if err := restartUnit(ctx, unit); err != nil {
		return fmt.Errorf("update failed (%v), binary rolled back, restart failed: %w", cause, err)
	}
	if err := waitActive(ctx, unit); err != nil {
		return fmt.Errorf("update failed (%v), binary rolled back, unit still unhealthy: %w", cause, err)
	}
	return fmt.Errorf("update failed, rolled back to previous version: %w", cause)
}

var unitRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._-]{0,127}\.service$`)

// restartUnit / unitIsActive / waitActive 均经 SystemCtl 白名单执行器。
func restartUnit(ctx context.Context, unit string) error {
	if !unitRe.MatchString(unit) {
		return fmt.Errorf("unit name invalid: %q", unit)
	}
	ctx, cancel := context.WithTimeout(ctx, restartTimeout)
	defer cancel()
	out, err := agentlib.SystemCtl(ctx, "restart", unit)
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %w: %s", unit, err, out)
	}
	return nil
}

func unitIsActive(ctx context.Context, unit string) (bool, error) {
	if !unitRe.MatchString(unit) {
		return false, fmt.Errorf("unit name invalid: %q", unit)
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	out, err := agentlib.SystemCtl(ctx, "is-active", unit)
	state := strings.TrimSpace(out)
	if err != nil {
		// is-active 对 inactive/failed 返回非零退出码：区分"未纳管"与"真错误"。
		if state == "inactive" || state == "unknown" || state == "failed" {
			return false, nil
		}
		return false, err
	}
	return state == "active", nil
}

func waitActive(ctx context.Context, unit string) error {
	for i := 0; i < restartPollMax; i++ {
		active, err := unitIsActive(ctx, unit)
		if err == nil && active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(restartPollEvery):
		}
	}
	return fmt.Errorf("unit %s not active within %.0fs", unit,
		restartPollEvery.Seconds()*restartPollMax)
}

// ---------------- 通用工具 -------------------------------------------------

// redirectGuard 重定向守卫：最多 5 跳，每跳目标主机必须在白名单内。
func redirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if !allowedHost(req.URL.Hostname()) {
		return fmt.Errorf("redirect host not allowed: %q", req.URL.Hostname())
	}
	return nil
}

// allowedHost 固定 https 主机白名单（与 install.sh 双通道一致）。
func allowedHost(host string) bool {
	host = strings.ToLower(host)
	switch {
	case host == "gitee.com" || strings.HasSuffix(host, ".gitee.com"):
		return true
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return true // 含 api.github.com
	case strings.HasSuffix(host, ".githubusercontent.com"):
		return true // raw / objects（GitHub release 下载重定向目标）
	default:
		return false
	}
}

// safeURL 错误信息中只保留主机与路径，不带 query。
func safeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "(unparsed)"
	}
	return u.Hostname() + u.Path
}

var (
	tagRe       = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	checksumLine = regexp.MustCompile(`^([0-9a-fA-F]{64})[ \t]+\*?(.+)$`)
)

// parseChecksums 解析 sha256sum 格式校验和清单（hex 大小写不敏感）。
func parseChecksums(text string) (map[string]string, error) {
	sums := make(map[string]string, 8)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := checksumLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("checksums.txt line invalid: %q", line)
		}
		sums[m[2]] = strings.ToLower(m[1])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, errors.New("checksums.txt empty")
	}
	return sums, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinary 从 tar.gz 提取指定二进制：仅接受常规文件、拒绝符号链接、
// 目标路径必须落在 dest 内（zip-slip 防护）、解压总量封顶。
func extractBinary(archivePath, dest, binName string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	var total int64
	found := false
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // 目录/符号链接/硬链接一律跳过
		}
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("archive entry escapes destination: %q", hdr.Name)
		}
		target := filepath.Join(dest, name)
		if !strings.HasPrefix(target, dest+string(filepath.Separator)) {
			return fmt.Errorf("archive entry escapes destination: %q", hdr.Name)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(tr, maxExtractedBytes-total+1))
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		total += n
		if total > maxExtractedBytes {
			return errors.New("archive expands beyond limit")
		}
		if filepath.Base(name) == binName {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("binary %q not found in archive", binName)
	}
	return nil
}
