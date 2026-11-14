package version

import (
	"regexp"
	"testing"
)

// TestVersionFormat 保证版本号只有一个来源，且格式与 git tag 一致（带 v 前缀）。
func TestVersionFormat(t *testing.T) {
	if Version != "v"+Number {
		t.Fatalf("Version (%q) 与 Number (%q) 不一致", Version, Number)
	}
	// 语义化版本：主.次.修订。
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version %q 不是 vX.Y.Z 形式", Version)
	}
}
