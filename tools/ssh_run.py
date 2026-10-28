"""SSH 远程执行辅助（paramiko）。

安全设计：
- 不经 shell：本地无 subprocess，命令只通过 paramiko exec_command 传给远端 sshd；
- 凭据仅从环境变量读取：KVM_SSH_HOST / KVM_SSH_PORT / KVM_SSH_USER / KVM_SSH_PASS / KVM_SSH_KEY；
- 上传模式校验：本地文件路径按词法拒绝 ".."，且必须位于当前工作目录之下；
  远端路径必须以 / 开头的绝对路径；
- 密钥优先（KVM_SSH_KEY 存在则用密钥），否则回退密码。

用法：
  python tools/ssh_run.py "<remote shell command>"
  python tools/ssh_run.py --put <local-file> </abs/remote-path>
"""
import os
import sys

import paramiko

DOTDOT = ".."


def _reject_dotdot(p: str) -> bool:
    return any(seg == DOTDOT for seg in p.replace("\\", "/").split("/"))


def main() -> int:
    args = sys.argv[1:]
    if not args:
        print("usage: ssh_run.py <cmd> | --put <local> <remote>")
        return 2

    host = os.environ.get("KVM_SSH_HOST", "")
    port = int(os.environ.get("KVM_SSH_PORT", "22"))
    user = os.environ.get("KVM_SSH_USER", "")
    password = os.environ.get("KVM_SSH_PASS", "")
    key = os.environ.get("KVM_SSH_KEY", "")

    if not host or not user:
        print("KVM_SSH_HOST / KVM_SSH_USER required", file=sys.stderr)
        return 2

    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    if key and os.path.isfile(key):
        client.connect(host, port=port, username=user, key_filename=key, timeout=30)
    else:
        client.connect(host, port=port, username=user, password=password, timeout=30)

    try:
        if args[0] == "--put":
            if len(args) != 3:
                print("--put needs <local> <remote>", file=sys.stderr)
                return 2
            local, remote = args[1], args[2]
            if _reject_dotdot(local):
                print("local path traversal rejected", file=sys.stderr)
                return 2
            cwd = os.path.abspath(os.getcwd())
            local_abs = os.path.abspath(local)
            if not local_abs.startswith(cwd + os.sep):
                print("local path outside workspace rejected", file=sys.stderr)
                return 2
            if not os.path.isfile(local_abs):
                print("local file not found", file=sys.stderr)
                return 2
            if not remote.startswith("/"):
                print("remote path must be absolute", file=sys.stderr)
                return 2
            sftp = client.open_sftp()
            try:
                sftp.put(local_abs, remote)
            finally:
                sftp.close()
            print(f"UPLOADED {os.path.getsize(local_abs)} {remote}")
            return 0

        cmd = " ".join(args)
        if not cmd.strip():
            print("empty command", file=sys.stderr)
            return 2
        _, stdout, stderr = client.exec_command(cmd, timeout=900)
        out = stdout.read().decode("utf-8", "replace")
        err = stderr.read().decode("utf-8", "replace")
        code = stdout.channel.recv_exit_status()
        sys.stdout.write(out)
        if err:
            sys.stdout.write(err)
        return code
    finally:
        client.close()


if __name__ == "__main__":
    sys.exit(main())
