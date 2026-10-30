package agentlib

// 镜像管道安全测试：URL 白名单、规范名穿越防护、命令行组装（FakeRunner 断言）。

import (
	"strings"
	"testing"
)

func TestImagePipelineRejectsBadURL(t *testing.T) {
	ip := &ImagePipeline{R: &FakeRunner{}}
	spec := ImagePullSpec{
		Name: "ubuntu-24.04", Format: "qcow2",
		DownloadURL:  "http://127.0.0.1/evil.qcow2", // 非 https
		AllowDomains: []string{"mirrors.example.com"},
	}
	if _, err := ip.Prepare(spec); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected scheme rejection, got %v", err)
	}
	spec.DownloadURL = "https://evil.example.com/x.qcow2" // 主机不在 allowlist
	if _, err := ip.Prepare(spec); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist rejection, got %v", err)
	}
}

func TestImagePipelineRejectsPathTraversal(t *testing.T) {
	ip := &ImagePipeline{R: &FakeRunner{}}
	spec := ImagePullSpec{
		Name: "../etc/shadow", Format: "qcow2",
		DownloadURL:  "https://mirrors.example.com/a.qcow2",
		AllowDomains: []string{"mirrors.example.com"},
	}
	if _, err := ip.Prepare(spec); err == nil || !strings.Contains(err.Error(), "规范名") {
		t.Fatalf("expected name rejection, got %v", err)
	}
}

func TestImagePipelineWhitelistedCommandSequence(t *testing.T) {
	fake := &FakeRunner{Out: ""}
	ip := &ImagePipeline{R: fake, DataDir: "/var/lib/kvmcloud/images"}
	spec := ImagePullSpec{
		Name: "ubuntu-24.04-lts", Version: "1", Format: "qcow2",
		DownloadURL:  "https://mirrors.example.com/ubuntu.qcow2",
		SizeLimit:    1073741824,
		AllowDomains: []string{"mirrors.example.com"},
	}
	if _, err := ip.Prepare(spec); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	joined := strings.Join(fake.Calls, "\n")
	for _, want := range []string{
		"curl --fail --silent --show-error --proto =https --max-filesize 1073741824",
		"qemu-img convert -f qcow2 -O qcow2",
		"qemu-img check",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing command %q in:\n%s", want, joined)
		}
	}
}
