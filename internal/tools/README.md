# UCloud / CompShare STS 多用户调用模板

服务进程用自己的 IAM AK/SK，按需 AssumeRole 拿到客户的临时凭证，再代客户调 UCloud / CompShare API。多用户共用一个 executor，身份从 ctx 流过来。单用户场景把同一个身份永远塞进 ctx 即可，不需要单独写一套。

## 适用场景

- 内网服务收到外部 HTTP 请求，需要"代多个客户"调云 API
- 不想把客户的长期 AK/SK 落地到本服务
- 想统一在一个进程里管理凭证缓存和签名

不适用：单进程只伺候一个固定身份且不打算长期运行——直接用长期 AK/SK 更简单。

## 整体结构

```
ctx (含 UserContext)
  └─► CredentialProvider.Get(ctx)
        ├─ 缓存命中：直接返回 *Credentials
        └─ 未命中：用服务自身 AK/SK 调 STS.AssumeRole
              └─► (AccessKeyId, AccessKeySecret, SecurityToken, ExpireAt)
  └─► ExternalExecutor.Execute(ctx, action, args)
        └─ 用临时凭证签名 + POST + 解响应
```

三个文件：

| 文件 | 职责 |
|---|---|
| `usercontext.go` | `UserContext` 结构 + `WithUser` / `UserFrom` |
| `sts_provider.go` | `STSProvider`：按 RoleUrn 缓存 + singleflight |
| `external.go` | `ExternalExecutor`：每次 `Execute` 先取凭证再签名 |

## 最小可跑示例

### 身份 + ctx

```go
// usercontext.go
type UserContext struct {
    RoleUrn     string // 必填：ucs:iam::{companyId}:role/...
    SessionName string // 推荐：用 userId / companyId 做审计标识
    ProjectId   string // 可选：覆盖默认 ProjectId
    Region      string // 可选：覆盖默认 Region
}

type userKey struct{}

func WithUser(ctx context.Context, u UserContext) context.Context {
    return context.WithValue(ctx, userKey{}, u)
}

func UserFrom(ctx context.Context) (UserContext, bool) {
    u, ok := ctx.Value(userKey{}).(UserContext)
    return u, ok
}
```

### STS Provider（按 RoleUrn 缓存）

```go
// sts_provider.go
type Credentials struct {
    AccessKeyId, AccessKeySecret, SecurityToken string
    ExpireAt                                    time.Time
}

type CredentialProvider interface {
    Get(ctx context.Context) (*Credentials, error)
}

type STSProvider struct {
    serviceAK, serviceSK, stsURL string
    httpClient                   *http.Client

    mu       sync.Mutex
    cache    map[string]*Credentials       // key = RoleUrn
    inflight map[string]chan struct{}      // 同 key 并发去重
}

func NewSTSProvider(ak, sk, url string) *STSProvider {
    return &STSProvider{
        serviceAK: ak, serviceSK: sk, stsURL: url,
        httpClient: &http.Client{Timeout: 10 * time.Second},
        cache:      make(map[string]*Credentials),
        inflight:   make(map[string]chan struct{}),
    }
}

func (p *STSProvider) Get(ctx context.Context) (*Credentials, error) {
    u, ok := UserFrom(ctx)
    if !ok || u.RoleUrn == "" {
        return nil, fmt.Errorf("no user in context (use tools.WithUser)")
    }

    p.mu.Lock()
    if c, ok := p.cache[u.RoleUrn]; ok && time.Until(c.ExpireAt) > 5*time.Minute {
        p.mu.Unlock()
        return c, nil
    }
    if ch, ok := p.inflight[u.RoleUrn]; ok {
        p.mu.Unlock()
        <-ch
        return p.Get(ctx) // 复查缓存
    }
    ch := make(chan struct{})
    p.inflight[u.RoleUrn] = ch
    p.mu.Unlock()

    defer func() {
        p.mu.Lock()
        delete(p.inflight, u.RoleUrn)
        close(ch)
        p.mu.Unlock()
    }()

    cred, err := p.assumeRole(ctx, u)
    if err != nil {
        return nil, err
    }
    p.mu.Lock()
    p.cache[u.RoleUrn] = cred
    p.mu.Unlock()
    return cred, nil
}

func (p *STSProvider) assumeRole(ctx context.Context, u UserContext) (*Credentials, error) {
    session := u.SessionName
    if session == "" {
        session = "agent-default"
    }
    params := map[string]string{
        "Action":          "AssumeRole",
        "RoleUrn":         u.RoleUrn,
        "RoleSessionName": session,
        "PublicKey":       p.serviceAK,
    }
    params["Signature"] = ucloudSign(params, p.serviceSK)

    body, err := postForm(ctx, p.httpClient, p.stsURL, params)
    if err != nil {
        return nil, err
    }
    var resp struct {
        RetCode     int
        Message     string
        Credentials struct {
            AccessKeyId, AccessKeySecret, SecurityToken, Expiration string
        }
    }
    if err := json.Unmarshal(body, &resp); err != nil {
        return nil, err
    }
    if resp.RetCode != 0 {
        return nil, fmt.Errorf("AssumeRole RetCode=%d: %s", resp.RetCode, resp.Message)
    }
    exp, err := time.Parse(time.RFC3339, resp.Credentials.Expiration)
    if err != nil {
        exp = time.Now().Add(55 * time.Minute) // 兜底，避免每次都打 STS
    }
    return &Credentials{
        AccessKeyId:     resp.Credentials.AccessKeyId,
        AccessKeySecret: resp.Credentials.AccessKeySecret,
        SecurityToken:   resp.Credentials.SecurityToken,
        ExpireAt:        exp,
    }, nil
}
```

### Executor 用 provider 拿凭证

```go
// external.go 关键段
func (e *ExternalExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
    cred, err := e.creds.Get(ctx)
    if err != nil {
        return nil, err
    }
    region, project := e.defaultRegion, e.defaultProject
    if u, ok := UserFrom(ctx); ok {
        if u.Region != ""    { region = u.Region }
        if u.ProjectId != "" { project = u.ProjectId }
    }

    params := map[string]string{
        "Action":    action,
        "Region":    region,
        "PublicKey": cred.AccessKeyId,
    }
    if cred.SecurityToken != "" {
        params["SecurityToken"] = cred.SecurityToken // 必须参与签名
    }
    flattenInto(params, args, "")
    if project != "" {
        if _, ok := params["ProjectId"]; !ok {
            params["ProjectId"] = project
        }
    }
    params["Signature"] = ucloudSign(params, cred.AccessKeySecret)
    // ...HTTP POST + 解响应不变
}
```

### HTTP 入口注入身份

```go
http.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
    var req struct {
        RoleUrn, UserId, ProjectId, Action string
        Args                               map[string]any
    }
    json.NewDecoder(r.Body).Decode(&req)

    ctx := WithUser(r.Context(), UserContext{
        RoleUrn:     req.RoleUrn,
        SessionName: req.UserId,
        ProjectId:   req.ProjectId,
    })
    result, err := executor.Execute(ctx, req.Action, req.Args)
    // ...
})
```

## 单用户场景如何复用

在初始化处构造一个固定的 ctx 并到处带着：

```go
defaultUser := UserContext{RoleUrn: cfg.AssumeRoleUrn, ProjectId: cfg.ProjectId}
ctx := WithUser(context.Background(), defaultUser)
executor.Execute(ctx, "DescribeUHostInstance", args)
```

Provider / Executor 一行不改。

## 六个易错点

1. **`SecurityToken` 必须参与签名**——它是普通参数，和其他键一起 `sort + concat` 后再拼 SK 算 SHA1。漏了就报签名错。
2. **`Expiration` 是 RFC3339 字符串**，不是 Unix 秒。用 `time.Parse(time.RFC3339, ...)`，并加兜底（解析失败按 55min 算），避免触发"永远未命中"。
3. **缓存 key 用 `RoleUrn`，不要把 `SessionName` 也放进 key**——同角色不同 session 共享凭证更省 STS 配额且权限一致。
4. **提前 5 分钟续约**——别等真到期，跨网络往返很容易踩边界。
5. **AssumeRole 调用要用服务自身的 AK/SK 签**，不是任何用户的凭证。AssumeRole 接口本身就是普通 UCloud API，走同一个 BaseUrl（如 `http://channel-124.public-api.service.ucloud.cn/`）。
6. **不要给"全局默认 RoleUrn"做兜底**——ctx 没有 RoleUrn 直接报错。否则一旦上游漏传，本进程会以"上一个用户"或"默认账号"身份继续调用，跨用户串权限非常难排查。

## AssumeRole 参数速查

| 参数 | 必填 | 说明 |
|---|---|---|
| `Action` | 是 | `"AssumeRole"` |
| `PublicKey` | 是 | 调用方（服务自己）的 AK |
| `Signature` | 是 | 用调用方 SK 算的 HMAC-SHA1 |
| `RoleUrn` | 是 | 要扮演的角色，如 `ucs:iam::66406608:role/ucs-service-role/ServiceRoleForCompshare` |
| `RoleSessionName` | 是 | 自定义会话名，建议带 userId 便于审计 |
| `DurationSeconds` | 否 | 凭证有效期秒数，默认 3600；上限看账户策略，通常 ≤ 43200 |
| `Policy` | 否 | 进一步缩小（不能扩大）已授予权限的临时策略 |

## 参考

- UCloud SDK STS：`github.com/ucloud/ucloud-sdk-go/services/sts`
- 老版定时关机实现（无缓存、单用户）：`uhost-extension` 仓库 `internal/logic/sdk/stop_scheduler/`
- 签名算法：`ucloudSign` / `ucloudSignJSON` 在本目录 `external.go`
