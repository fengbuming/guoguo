package app

import (
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// configureProcessMemory 在用户没有显式设定 GOMEMLIMIT 时，按机器可用内存给 Go 堆设软上限，
// 并在小内存机器上压紧 GC 目标。
//
// 剧库常驻只有几十 MB，真正的峰值来自整库列表的瞬时分配。默认 GOGC=100 会把堆目标放到
// 存活集的两倍，峰值过后 Go 只在后台慢慢把页还给系统，在 1GB 的 VPS 上表现为
// "RSS 冲到两三百 MB、然后几十分钟不降"，正好撞上 OOM killer 或把机器压进 swap。
//
// 显式设置了 GOMEMLIMIT/GOGC 时完全尊重用户设定，不做任何覆盖。
func configureProcessMemory() {
	total := machineMemoryBytes()
	if total <= 0 {
		return
	}
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		// 45%：给页缓存、ffmpeg 子进程和内核留出余量。存活堆只有几十 MB，
		// 这个软上限不会触发 GC 抖动，只用来兜住异常峰值。
		limit := total * 9 / 20
		if limit < 192<<20 {
			limit = 192 << 20
		}
		if limit > 1536<<20 {
			limit = 1536 << 20
		}
		debug.SetMemoryLimit(limit)
	}
	if strings.TrimSpace(os.Getenv("GOGC")) == "" && total <= 4<<30 {
		debug.SetGCPercent(50)
	}
	// Linux 上 Go 默认用 MADV_FREE 归还内存：内核只是标记可回收，RSS 不会马上下降，
	// 监控和 OOM 判定看到的都是"没降"。改成 MADV_DONTNEED 后归还即生效。
	if runtime.GOOS == "linux" && !strings.Contains(os.Getenv("GODEBUG"), "madvdontneed=") {
		value := strings.TrimSpace(os.Getenv("GODEBUG"))
		if value != "" {
			value += ","
		}
		_ = os.Setenv("GODEBUG", value+"madvdontneed=1")
	}
}

// releaseRuntimeMemory 在重活之后安排两级回收。
//
// Go 的后台 scavenger 是渐进式的，默认要几分钟才把 RSS 降下来；小内存机器上这段时间
// 足够被 OOM killer 盯上。两个定时器都只在"最后一次重活之后"触发一次：
//   - 1 秒后：立刻把整库列表造出的临时对象还给系统；
//   - 35 秒后：如果期间没有新的列表请求，放掉共享条目快照再回收一次，
//     让空闲常驻内存回到只含索引的水平。
//
// 反复调用只会重置/复用这两个定时器，不会叠加回收次数。
func (app *UIApp) releaseRuntimeMemory() {
	app.memoryReleaseMu.Lock()
	defer app.memoryReleaseMu.Unlock()
	if app.memoryReleaseTimer == nil {
		app.memoryReleaseTimer = time.AfterFunc(time.Second, func() {
			app.memoryReleaseMu.Lock()
			app.memoryReleaseTimer = nil
			app.memoryReleaseMu.Unlock()
			debug.FreeOSMemory()
		})
	}
	if app.memoryShrinkTimer == nil {
		app.memoryShrinkTimer = time.AfterFunc(35*time.Second, func() {
			app.memoryReleaseMu.Lock()
			app.memoryShrinkTimer = nil
			app.memoryReleaseMu.Unlock()
			if app.releaseLibrarySnapshot() {
				debug.FreeOSMemory()
			}
		})
	}
}

// releaseLibrarySnapshot 释放闲置的剧库共享条目快照（下次列表请求会重建）。
func (app *UIApp) releaseLibrarySnapshot() bool {
	app.mu.Lock()
	store := app.library
	app.mu.Unlock()
	if store == nil {
		return false
	}
	return store.dropIdlePublished(30 * time.Second)
}

// machineMemoryBytes 返回本进程实际可用的内存上限：取 cgroup 限制与物理内存的较小值。
func machineMemoryBytes() int64 {
	limit := cgroupMemoryBytes("/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes")
	total := meminfoTotalBytes("/proc/meminfo")
	switch {
	case total > 0 && limit > 0:
		if limit < total {
			return limit
		}
		return total
	case total > 0:
		return total
	default:
		return limit
	}
}

// cgroupMemoryBytes 读取 cgroup v2/v1 的内存上限；"max"/超出物理内存的哨兵值视为未设置。
func cgroupMemoryBytes(paths ...string) int64 {
	const unlimited = int64(1) << 60
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(body))
		if value == "" || value == "max" {
			continue
		}
		bytes, err := strconv.ParseInt(value, 10, 64)
		if err != nil || bytes <= 0 || bytes >= unlimited {
			continue
		}
		return bytes
	}
	return 0
}

// meminfoTotalBytes 读取 /proc/meminfo 的 MemTotal（单位 kB）。
func meminfoTotalBytes(path string) int64 {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kilobytes, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kilobytes <= 0 {
			return 0
		}
		return kilobytes * 1024
	}
	return 0
}
