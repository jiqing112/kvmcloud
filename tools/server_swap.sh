#!/usr/bin/env bash
# 服务器侧：替换二进制 + 重启服务 + 回显节点状态（由 kvm_remote.py 触发执行）
set -e
install -m0755 /tmp/kvmcloud-control.new /usr/local/bin/kvmcloud-control
install -m0755 /tmp/kvmcloud-agent.new /usr/local/bin/kvmcloud-agent
systemctl restart cloud-control cloud-agent
sleep 6
echo "services: $(systemctl is-active cloud-control) $(systemctl is-active cloud-agent)"
curl -s http://127.0.0.1:8080/api/v1/nodes
echo
echo SWAP_OK
