#!/usr/bin/env bash
# kvmcloud 卸载脚本：等价 install.sh uninstall
#   ./uninstall.sh [agent|control|all]
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$DIR/install.sh" uninstall "${1:-all}"
