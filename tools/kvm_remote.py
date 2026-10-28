"""kvmcloud 服务器部署远端执行辅助（paramiko）。

- 凭据仅从环境变量读取：KVM_SSH_HOST / KVM_SSH_PORT / KVM_SSH_USER / KVM_SSH_PASS；
- 本地无 subprocess、无 shell 解释：命令经 paramiko exec_command 直达远端 sshd；
- 上传模式：本地路径按词法拒绝 ".." 并限定工作目录内；远端路径必须绝对路径。

用法：
  python tools/kvm_remote.py "<remote command>"
  python tools/kvm_remote.py --put <local-file> </abs/remote-path>
"""
import os
import sys

import paramiko

DOTDOT = ".."


def main() -> int:
    args = sys.argv[1:]
    if not args:
        print("usage: kvm_remote.py <cmd> | --put <local> <remote>")
        return 2

    host = os.environ["KVM_SSH_HOST"]
    port = int(os.environ.get("KVM_SSH_PORT", "22"))
    user = os.environ["KVM_SSH_USER"]
    password = os.environ["KVM_SSH_PASS"]

    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(host, port=port, username=user, password=password, timeout=30)

    try:
        if args[0] == "--put":
            if len(args) != 3:
                print("--put needs <local> <remote>", file=sys.stderr)
                return 2
            local, remote = args[1], args[2]
            if any(seg == DOTDOT for seg in local.replace("\\", "/").split("/")):
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
        _, stdout, stderr = client.exec_command(cmd, timeout=120)
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
