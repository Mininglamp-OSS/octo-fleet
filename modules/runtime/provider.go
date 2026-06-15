package runtime

import (
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

const defaultUpgradeTimeoutSec = 600

const (
	providerStatusActive   = "active"
	providerStatusDisabled = "disabled"
)

// providerDef 是注册表里一行 provider 的定义。
type providerDef struct {
	Name              string `db:"name"`
	DisplayName       string `db:"display_name"`
	BinaryName        string `db:"binary_name"`
	UpgradeTimeoutSec int    `db:"upgrade_timeout_sec"`
	Status            string `db:"status"`
}

// providerSnapshot 是不可变快照：一次构造、只读、整体替换。
type providerSnapshot struct {
	byName      map[string]providerDef
	activeNames []string
}

func newProviderSnapshot(defs []providerDef) *providerSnapshot {
	s := &providerSnapshot{byName: make(map[string]providerDef, len(defs))}
	for _, d := range defs {
		s.byName[d.Name] = d
		if d.Status == providerStatusActive {
			s.activeNames = append(s.activeNames, d.Name)
		}
	}
	return s
}

// IsKnownKind 区分"注册表里有(无论 active/disabled)"与"完全未知"。
// 当前各 gate 只用 IsActiveKind;此方法供 PR-C(daemon 消费)及未来 enable/
// disable 流程区分 known-but-disabled 用,且有单测覆盖,保留。
func (s *providerSnapshot) IsKnownKind(name string) bool {
	_, ok := s.byName[name]
	return ok
}

func (s *providerSnapshot) IsActiveKind(name string) bool {
	d, ok := s.byName[name]
	return ok && d.Status == providerStatusActive
}

func (s *providerSnapshot) TimeoutSec(name string) int {
	if d, ok := s.byName[name]; ok && d.UpgradeTimeoutSec > 0 {
		return d.UpgradeTimeoutSec
	}
	return defaultUpgradeTimeoutSec
}

// ActiveNames 返回 active provider 名（拷贝，调用方不可改内部切片）。
func (s *providerSnapshot) ActiveNames() []string {
	out := make([]string, len(s.activeNames))
	copy(out, s.activeNames)
	return out
}

// fallbackProviderSnapshot 是 DB 加载失败时的编译期兜底（与本期决策一致：
// 只 claude/openclaw active）。保证 DB 抖动不影响建 bot / 升级 gate。
func fallbackProviderSnapshot() *providerSnapshot {
	return newProviderSnapshot([]providerDef{
		{Name: "claude", DisplayName: "Claude", BinaryName: "claude", UpgradeTimeoutSec: 600, Status: providerStatusActive},
		{Name: "openclaw", DisplayName: "OpenClaw", BinaryName: "openclaw", UpgradeTimeoutSec: 720, Status: providerStatusActive},
		{Name: "codex", DisplayName: "Codex", BinaryName: "codex", UpgradeTimeoutSec: 600, Status: providerStatusDisabled},
		{Name: "hermes", DisplayName: "Hermes", BinaryName: "hermes", UpgradeTimeoutSec: 1200, Status: providerStatusDisabled},
	})
}

// providerRegistry 持有 atomic 快照，并发读零锁、刷新整体替换。
type providerRegistry struct {
	log.Log
	db   *runtimeDB
	snap atomic.Value // *providerSnapshot
}

func newProviderRegistry(db *runtimeDB) *providerRegistry {
	r := &providerRegistry{Log: log.NewTLog("ProviderRegistry"), db: db}
	r.snap.Store(fallbackProviderSnapshot()) // 先放兜底，load 成功再替换
	if err := r.reload(); err != nil {
		r.Warn("initial provider registry load failed, using fallback", zap.Error(err))
	}
	return r
}

// current 返回当前快照。snap 在 newProviderRegistry 里启动即 Store(fallback),
// 正常不会为 nil；这里仍兜一层 nil（防未来 refactor 漏初始化）返回 fallback 而非 panic。
func (r *providerRegistry) current() *providerSnapshot {
	v := r.snap.Load()
	if v == nil {
		return fallbackProviderSnapshot()
	}
	return v.(*providerSnapshot)
}

func (r *providerRegistry) IsKnownKind(n string) bool  { return r.current().IsKnownKind(n) }
func (r *providerRegistry) IsActiveKind(n string) bool { return r.current().IsActiveKind(n) }
func (r *providerRegistry) TimeoutSec(n string) int    { return r.current().TimeoutSec(n) }
func (r *providerRegistry) ActiveNames() []string      { return r.current().ActiveNames() }

// reload 从 DB 读全表，成功才整体替换快照；失败保留上一份（启动时是 fallback）。
func (r *providerRegistry) reload() error {
	defs, err := r.db.loadProviders()
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		return nil // 空表不覆盖（防误清），保留现有快照
	}
	r.snap.Store(newProviderSnapshot(defs))
	return nil
}

// refreshLoop 每 60s 刷新一次，让人工改库后 ≤60s 生效，无需重启 fleet。
func (r *providerRegistry) refreshLoop() {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for range t.C {
		if err := r.reload(); err != nil {
			r.Warn("provider registry refresh failed, keeping previous snapshot", zap.Error(err))
		}
	}
}
