English | [中文文档](auto-renew_zh.md)

# Auto-Renewal Design

## Design Goals

Keep active users logged in without re-authentication, while enforcing a session ceiling that activity cannot bypass.

## Two Timeouts, Separate Roles

| Config | Meaning | Extended by renewal? |
|---|---|---|
| `Timeout` | Fixed validity, **absolute ceiling** | **No** — expires on schedule |
| `ActiveTimeout` | Minimum activity frequency, sliding idle window | Yes — refreshed on every login check |

`AutoRenew` renews only the `ActiveTimeout` sliding window; it does **not** extend `Timeout`.
With `Timeout=604800, ActiveTimeout=1800`: idle for 30 minutes freezes the token,
and after 7 days re-login is forced no matter how active the user is.

Call `RenewTimeout(token, seconds)` to extend the fixed validity explicitly.

> An earlier implementation reset the `Timeout` TTL back to full inside `IsLogin()`,
> which made occasionally-active sessions never expire. That behavior has been removed.

### Implementation Code

```go
// IsLogin checks if user is logged in
func (m *Manager) IsLogin(tokenValue string) bool {
    if tokenValue == "" {
        return false
    }
    tokenValue = m.CutTokenPrefix(tokenValue)

    // 1. Read TokenInfo; the fixed Timeout is enforced by storage TTL,
    //    so a missing key already means expired.
    info, err := m.getTokenInfo(tokenValue, false)
    if err != nil || info == nil {
        return false
    }

    // 2. Check active timeout, renew if needed
    return m.passActiveTimeout(tokenValue, info)
}

// passActiveTimeout checks freeze first, then renews — the order matters
func (m *Manager) passActiveTimeout(tokenValue string, info *TokenInfo) bool {
    if !m.isOpenCheckActiveTimeout() {
        return true
    }
    if m.activeTimeoutOf(tokenValue, info) == AlreadyFrozen {
        return false
    }
    if m.config.AutoRenew {
        // Renew at most once per RenewInterval
        if m.config.RenewInterval <= 0 || !m.storage.Exists(m.getRenewKey(tokenValue)) {
            m.renewToken(tokenValue) // refreshes ActiveTime only, TTL untouched
        }
    }
    return true
}
```

### Freezing Is a Soft State

Hitting the active timeout deletes **no keys** — it only makes validation fail.
`UpdateLastActiveToNow(token)` thaws it. This is distinct from `Timeout` expiry,
where the storage layer removes the key outright.

### Boundary Semantics

`GetTokenActiveTimeout` returns the seconds left before freezing:

| Case | Returns |
|---|---|
| Checking disabled (`ActiveTimeout<=0` and dynamic off) | `-1` never freeze |
| Token declares `ActiveTimeout=-1` | `-1` never freeze |
| `ActiveTime` missing, or token absent | `-2` treated as frozen |
| Remaining `< 0` | `-2` frozen |
| Remaining `== 0` | `0` not yet frozen |

Note: a non-positive active timeout (global or per-token) is treated as *unconfigured*
rather than "freeze after 0 seconds", so a missing config cannot lock everyone out.

### Per-Client Timeouts

With `DynamicActiveTimeout` enabled, each login can override both values:

```go
p := manager.NewLoginParameter(cfg)
p.Timeout = 604800              // fixed validity for this login
p.SetActiveTimeout(1800)        // active timeout for this login
token, err := mgr.LoginWithParameter(loginID, p)
```

The override is persisted in `TokenInfo`. When `DynamicActiveTimeout` is off,
a supplied `ActiveTimeout` is discarded (with a warning when `IsLog=true`).

## Workflow

```
1. Token existence check (fixed Timeout enforced by storage TTL)
   ├─ Missing/expired → Return false
   └─ Exists → Continue

2. Active timeout check (if enabled)
   ├─ Frozen → Return false (no keys deleted; can be thawed by renewal)
   └─ Not frozen → Continue

3. Renew active time (when AutoRenew=true)
   └─ Refreshes ActiveTime only (SetKeepTTL, fixed TTL untouched).
      Submitted to the renew pool when configured, otherwise run inline.

4. Return true
```

## Performance Comparison

### Synchronous Renewal (Old)

```
Request → IsLogin()
         ↓
      Check Token
         ↓
      [Sync Renewal]
      - Expire()        (100ms)
      - GetTokenInfo()  (50ms)
      - SaveTokenInfo() (100ms)
         ↓
      Return true       (Total: 250ms)
         ↓
      Response to User
```

### Asynchronous Renewal (New)

```
Request → IsLogin()
         ↓
      Check Token
         ↓
      Start renewal goroutine ──┐
         ↓                      │
      Return true immediately   │ (Total: 10ms)
         ↓                      │
      Response to User          │
                               │
                               └→ Background renewal
                                  - Expire()
                                  - GetTokenInfo()
                                  - SaveTokenInfo()
                                  (User already received response)
```

### Performance Improvements

| Metric | Sync | Async | Improvement |
|--------|------|-------|-------------|
| Single IsLogin latency | 150-500ms | 10-50ms | **↑ 80-90%** |
| 10000 calls total time | ~2.5s | ~0.5s | **↑ 400%** |
| QPS (single core) | ~2000 | ~10000 | **↑ 400%** |
| User-perceived latency | Noticeable | Nearly none | ⭐⭐⭐⭐⭐ |

## Trigger Timing

Any scenario calling `IsLogin()` will trigger renewal:

### 1. Middleware Verification

```go
r.Use(plugin.AuthMiddleware())
// ↓ Internally calls IsLogin()
```

### 2. Decorators

```go
r.GET("/api", sagin.CheckLogin(), handler)
// ↓ Decorator calls IsLogin()
```

### 3. Manual Check

```go
stputil.IsLogin(token)
// ↓ Direct call
```

### 4. GetLoginID and Other Methods

```go
stputil.GetLoginID(token)
// ↓ Internally calls IsLogin() first
```

## Configuration Options

### Enable Auto-Renewal

```go
core.NewBuilder().
    Timeout(86400).      // Must be > 0
    AutoRenew(true).     // Enable auto-renewal
    Build()
```

### Disable Auto-Renewal

```go
core.NewBuilder().
    Timeout(1800).       // 30-minute hard timeout
    AutoRenew(false).    // Disable renewal
    Build()
```

### Combined with Active Timeout

```go
core.NewBuilder().
    Timeout(86400).       // 24-hour absolute ceiling
    ActiveTimeout(1800).  // freeze after 30 minutes idle
    AutoRenew(true).      // auto-renew the active window
    Build()
```

**Effect**:
- At most 24 hours; re-login is forced then regardless of activity
- Frozen after 30 minutes of inactivity
- Each request refreshes the active time, fast response

### Per-Client Timeouts

```go
core.NewBuilder().
    Timeout(604800).
    ActiveTimeout(1800).
    DynamicActiveTimeout(true). // let each token carry its own active timeout
    Build()
```

## Concurrency Safety

### Thread-Safe Storage

```go
// Memory storage uses locks
type Storage struct {
    data map[string]*item
    mu   sync.RWMutex  // ← Read-write lock
}

// Redis naturally supports concurrency
```

### Goroutine Management

```go
go func() {
    // Async renewal
    // Auto-recycled, no manual management needed
}()
```

**Advantages**:
- ✅ All Storage operations are thread-safe
- ✅ Goroutines auto-recycled
- ✅ No memory leaks
- ✅ Excellent concurrent performance

## Renewal Failure Handling

### Strategy

Async renewal failures **do not affect** the current request:

1. User has already received response (true)
2. Token is still valid
3. Renewal will be retried on next request

### Scenario

```go
// Renewal operation
go func() {
    err := m.storage.Expire(tokenKey, expiration)
    if err != nil {
        // Renewal failed, but doesn't affect current request
        // Will retry on next IsLogin call
    }
}()

return true  // Current request succeeds
```

## Performance Testing

### Test Code

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

### Expected Results

```
BenchmarkIsLogin-8   10000000   120 ns/op   0 B/op   0 allocs/op
```

- Each call takes only ~120 nanoseconds
- Zero memory allocations
- High concurrency friendly

## Best Practices

### Production Environment Configuration

```go
core.NewBuilder().
    Storage(redisStorage).     // Redis storage
    Timeout(86400).            // 24 hours
    ActiveTimeout(1800).       // 30-minute active timeout
    AutoRenew(true).           // Async renewal
    Build()
```

### Development Environment Configuration

```go
core.NewBuilder().
    Storage(memory.NewStorage()).
    Timeout(7200).             // 2 hours
    AutoRenew(true).           // Async renewal
    Build()
```

### Security-First Configuration

```go
core.NewBuilder().
    Storage(redisStorage).
    Timeout(1800).             // 30-minute hard timeout
    AutoRenew(false).          // No renewal
    Build()
```

## Next Steps

- [Architecture Design](architecture.md)
- [Performance Optimization](performance.md)
- [Modular Design](modular.md)
