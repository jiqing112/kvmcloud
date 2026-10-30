// Package agentlib SystemCtl：systemd 控制专用白名单执行器。
//
// 与 ExecRunner 同一安全模型：二进制名固定字面量、参数列表执行不经 shell；
// action 必须命中白名单、单元名过正则校验；经 timeout(1) 包裹实现进程级硬超时
//（防 restart 卡死拖住更新流程）。
package agentlib

import (
	"context"
	"fmt"
	"regexp"
)

// unitNameRe systemd 单元名白名单（.service 结尾）。
var unitNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._-]{0,127}\.service$`)

// systemctlActions 允许的动作白名单（自更新仅需 is-active/restart）。
var systemctlActions = map[string]bool{
	"is-active": true,
	"restart":   true,
	"start":     true,
	"stop":      true,
}

// SystemCtl 执行 systemctl 白名单动作；ctx 已取消/超时则拒绝启动。
// timeout(1) 30s + kill-after 5s：重启卡死的兜底，防更新流程悬挂。
func SystemCtl(ctx context.Context, action, unit string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !systemctlActions[action] {
		return "", fmt.Errorf("systemctl action %q is not whitelisted", action)
	}
	if !unitNameRe.MatchString(unit) {
		return "", fmt.Errorf("unit name invalid: %q", unit)
	}
	return ExecRunner{}.Run("timeout", "--kill-after=5", "30", "systemctl", action, unit)
}
