[English](auto-renew.md) | 中文文档

# 自动续签设计

## 设计目标

让活跃用户无需重新登录，同时为会话设定一个无法被活跃绕过的上限。

## 两种超时，职责分离

| 配置 | 含义 | 是否被续签延长 |
|---|---|---|
| `Timeout` | 固定有效期，**绝对上限** | **否**，到点必失效 |
| `ActiveTimeout` | 最低活跃频率，滑动空闲窗口 | 是，每次登录校验都刷新 |

`AutoRenew` 只续 `ActiveTimeout` 的滑动窗口，**不延长 `Timeout`**。
典型配置 `Timeout=604800, ActiveTimeout=1800`：空闲 30 分钟即冻结，
最长 7 天后无论多活跃都强制重新登录。

需要显式延长固定有效期时调用 `RenewTimeout(token, seconds)`。

> 早期实现曾在 `IsLogin()` 里把 `Timeout` 的 TTL 重置回满额，
> 导致偶尔活跃的会话永不过期。该行为已移除。

### 实现代码

```go
// IsLogin 检查是否登录
func (m *Manager) IsLogin(tokenValue string) bool {
    if tokenValue == "" {
        return false
    }
    tokenValue = m.CutTokenPrefix(tokenValue)

    // 1. 读取 TokenInfo；固定 Timeout 由存储 TTL 兜底，键不存在即已过期
    info, err := m.getTokenInfo(tokenValue, false)
    if err != nil || info == nil {
        return false
    }

    // 2. 校验活跃超时并按需续签
    return m.passActiveTimeout(tokenValue, info)
}

// passActiveTimeout 先判冻结、再续活跃时间，顺序不可反
func (m *Manager) passActiveTimeout(tokenValue string, info *TokenInfo) bool {
    if !m.isOpenCheckActiveTimeout() {
        return true
    }
    if m.activeTimeoutOf(tokenValue, info) == AlreadyFrozen {
        return false
    }
    if m.config.AutoRenew {
        // RenewInterval 内只续一次；配了续期池则异步执行
        if m.config.RenewInterval <= 0 || !m.storage.Exists(m.getRenewKey(tokenValue)) {
            m.renewToken(tokenValue) // 只刷新 ActiveTime，不动 TTL
        }
    }
    return true
}
```

### 冻结是软状态

活跃超时触发后**不删除任何键**，仅让校验不通过。
`UpdateLastActiveToNow(token)` 可将其解冻。这与 `Timeout` 到点后键被存储层删除是两件事。

### 边界语义

`GetTokenActiveTimeout` 返回距离冻结的剩余秒数：

| 情形 | 返回 |
|---|---|
| 未开启校验（`ActiveTimeout<=0` 且未开动态开关） | `-1` 永不冻结 |
| 该 token 声明 `ActiveTimeout=-1` | `-1` 永不冻结 |
| `ActiveTime` 缺失或 token 不存在 | `-2` 视为已冻结 |
| 剩余 `< 0` | `-2` 已冻结 |
| 剩余 `== 0` | `0` 尚未冻结 |

注意：全局或单 token 的活跃超时为非正数时一律按「未配置」处理而非「0 秒即冻结」，
避免漏配把所有人瞬间锁在门外。

### 按客户端设定超时

开启 `DynamicActiveTimeout` 后，可按次登录覆盖：

```go
p := manager.NewLoginParameter(cfg)
p.Timeout = 604800              // 本次登录的固定有效期
p.SetActiveTimeout(1800)        // 本次登录的活跃超时
token, err := mgr.LoginWithParameter(loginID, p)
```

覆盖值随 `TokenInfo` 落库。未开 `DynamicActiveTimeout` 时传入的
`ActiveTimeout` 会被丢弃（`IsLog=true` 时打印告警）。

## 工作流程

```
1. Token存在性检查（固定 Timeout 由存储 TTL 兜底）
   ├─ 不存在/已过期 → 返回false
   └─ 存在 → 继续

2. 活跃超时检查（如果开启）
   ├─ 已冻结 → 返回false（不删键，可被续签解冻）
   └─ 未冻结 → 继续

3. 续签活跃时间（AutoRenew=true 时）
   └─ 只刷新 ActiveTime（SetKeepTTL，不动固定 TTL）
      配了续期池则提交到池中异步执行，否则同步执行

4. 返回true
```

## 性能对比

### 同步续签（旧）

```
请求 → IsLogin()
         ↓
      检查Token
         ↓
      [同步续签]
      - Expire()        (100ms)
      - GetTokenInfo()  (50ms)
      - SaveTokenInfo() (100ms)
         ↓
      返回true          (总耗时: 250ms)
         ↓
      响应用户
```

### 异步续签（新）

```
请求 → IsLogin()
         ↓
      检查Token
         ↓
      提交续签任务到池 ──┐
         ↓               │
      立即返回true       │ (总耗时: 10ms)
         ↓               │
      响应用户           │
                        │
                        └→ 后台续签
                           - SetKeepTTL(ActiveTime)
                           (用户已收到响应)
```

### 性能提升

| 指标 | 同步 | 异步 | 提升 |
|------|------|------|------|
| 单次IsLogin耗时 | 150-500ms | 10-50ms | **↑ 80-90%** |
| 10000次调用耗时 | ~2.5秒 | ~0.5秒 | **↑ 400%** |
| QPS（单核） | ~2000 | ~10000 | **↑ 400%** |
| 用户感知延迟 | 明显 | 几乎无 | ⭐⭐⭐⭐⭐ |

## 触发时机

任何调用 `IsLogin()` 的场景都会触发续签：

### 1. 中间件验证

```go
r.Use(plugin.AuthMiddleware())
// ↓ 内部调用 IsLogin()
```

### 2. 装饰器

```go
r.GET("/api", sagin.CheckLogin(), handler)
// ↓ 装饰器调用 IsLogin()
```

### 3. 手动检查

```go
stputil.IsLogin(token)
// ↓ 直接调用
```

### 4. GetLoginID等方法

```go
stputil.GetLoginID(token)
// ↓ 内部先调用 IsLogin()
```

## 配置选项

### 启用自动续签

```go
core.NewBuilder().
    Timeout(86400).      // 必须>0
    AutoRenew(true).     // 开启自动续签
    Build()
```

### 禁用自动续签

```go
core.NewBuilder().
    Timeout(1800).       // 30分钟硬超时
    AutoRenew(false).    // 关闭续签
    Build()
```

### 结合活跃超时

```go
core.NewBuilder().
    Timeout(86400).       // 24小时绝对超时
    ActiveTimeout(1800).  // 30分钟无操作冻结
    AutoRenew(true).      // 自动续签活跃时间
    Build()
```

**效果**：
- 最长 24 小时，到点无论多活跃都强制重新登录
- 30 分钟无操作即冻结
- 每次请求刷新活跃时间，响应快速

### 按客户端区分超时

```go
core.NewBuilder().
    Timeout(604800).
    ActiveTimeout(1800).
    DynamicActiveTimeout(true). // 允许各 token 自带活跃超时
    Build()
```

## 并发安全

### Storage线程安全

```go
// Memory存储使用锁
type Storage struct {
    data map[string]*item
    mu   sync.RWMutex  // ← 读写锁
}

// Redis天然支持并发
```

### Goroutine管理

```go
go func() {
    // 异步续签
    // 自动回收，无需手动管理
}()
```

**优势**：
- ✅ Storage操作都是线程安全的
- ✅ Goroutine自动回收
- ✅ 无内存泄漏
- ✅ 并发性能优异

## 续签失败处理

### 策略

异步续签失败**不影响**当前请求：

1. 用户已收到响应（true）
2. Token仍然有效
3. 下次请求时会重试续签

### 场景

```go
// 续签操作
go func() {
    err := m.storage.Expire(tokenKey, expiration)
    if err != nil {
        // 续签失败，但不影响当前请求
        // 下次IsLogin时会重试
    }
}()

return true  // 当前请求成功
```

## 性能测试

### 测试代码

```go
func BenchmarkIsLogin(b *testing.B) {
    stputil.SetManager(
        core.NewBuilder().
            Storage(memory.NewStorage()).
            Timeout(3600).
            AutoRenew(true).
            Build(),
    )

    token, _ := stputil.Login(1000)

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        stputil.IsLogin(token)
    }
}
```

### 预期结果

```
BenchmarkIsLogin-8   10000000   120 ns/op   0 B/op   0 allocs/op
```

- 每次调用仅需 ~120纳秒
- 零内存分配
- 高并发友好

## 最佳实践

### 生产环境配置

```go
core.NewBuilder().
    Storage(redisStorage).     // Redis存储
    Timeout(86400).            // 24小时
    ActiveTimeout(1800).       // 30分钟活跃超时
    AutoRenew(true).           // 异步续签
    Build()
```

### 开发环境配置

```go
core.NewBuilder().
    Storage(memory.NewStorage()).
    Timeout(7200).             // 2小时
    AutoRenew(true).           // 异步续签
    Build()
```

### 安全优先配置

```go
core.NewBuilder().
    Storage(redisStorage).
    Timeout(1800).             // 30分钟硬超时
    AutoRenew(false).          // 不续签
    Build()
```

## 下一步

- [架构设计](architecture.md)
- [性能优化](performance.md)
- [模块化设计](modular.md)

