package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCgroupMemoryBytesIgnoresUnlimitedMarkers(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"max":        "max\n",
		"sentinel":   "9223372036854771712\n",
		"detail":     "1073741824\n",
		"zero":       "0\n",
		"garbage":    "\n",
		"tinyvalue":  "1048576\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		switch name {
		case "detail":
			want = 1 << 30
		case "tinyvalue":
			want = 1 << 20
		}
		if got := cgroupMemoryBytes(path); got != want {
			t.Fatalf("%s: 解析结果 %d，期望 %d", name, got, want)
		}
	}
	if got := cgroupMemoryBytes(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("缺失文件应返回 0，实际 %d", got)
	}
}

func TestMeminfoTotalBytesParsesMemTotal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	body := "MemTotal:        1947136 kB\nMemFree:          123456 kB\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := meminfoTotalBytes(path), int64(1947136)*1024; got != want {
		t.Fatalf("MemTotal 解析为 %d，期望 %d", got, want)
	}
	if got := meminfoTotalBytes(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("缺失文件应返回 0，实际 %d", got)
	}
}
