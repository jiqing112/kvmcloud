"""恢复 shell 脚本 git 可执行位 + 提交推送（subprocess 列表参数，shell=False）。"""
import subprocess


def git(*args: str) -> str:
    r = subprocess.run(["git", *args], shell=False, check=True, capture_output=True, text=True)
    return r.stdout


def main() -> None:
    for pattern in ("install.sh", "uninstall.sh"):
        git("update-index", "--chmod=+x", pattern)
        print("index +x:", pattern)
    git("add", "tools", "internal", "cmd")
    git("commit", "-m", "fix(agent): vps collection fallback + capacity refill on re-register\n\n"
        "- disk collect: fall back to / when /var/lib/kvmcloud missing\n"
        "- nic bandwidth: auto-detect non-lo iface, default 1Gbps when speed unreadable\n"
        "- control register: refill non-zero physical capacity/ratios on re-register\n"
        "- restore exec bit for install.sh/uninstall.sh")
    print(git("log", "--oneline", "-2"))
    print(git("ls-files", "-s", "install.sh", "uninstall.sh"))
    git("push", "origin", "main")
    print("PUSHED")


if __name__ == "__main__":
    main()
