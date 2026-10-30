package updater

import (
	"strconv"
	"strings"
)

// Version 三段式语义化版本（与 install.sh / tag 口径一致：vMAJOR.MINOR.PATCH）。
type Version struct {
	Major, Minor, Patch uint64
}

// ParseVersion 严格解析 v1.2.3 / 1.2.3；不接受预发布后缀（与 tag 校验同口径）。
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return Version{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var out Version
	for i, p := range parts {
		if p == "" || len(p) > 10 {
			return Version{}, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return Version{}, false
			}
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Version{}, false
		}
		switch i {
		case 0:
			out.Major = n
		case 1:
			out.Minor = n
		default:
			out.Patch = n
		}
	}
	return out, true
}

// Compare 返回 -1/0/1（按 Major → Minor → Patch 字典序）。
func Compare(a, b Version) int {
	if a.Major != b.Major {
		return cmpUint(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpUint(a.Minor, b.Minor)
	}
	return cmpUint(a.Patch, b.Patch)
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
