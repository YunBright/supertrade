---
paths: ["dapr/components/**", "deploy/**/*.yaml", "deploy/**/*.yml", "deployments/**/*.yaml"]
---

# nginx map 规则同步

**本仓没有 nginx 配置**（nginx 在 `../deployer/nginx/yun-bright.conf`），但本仓每加一个 dapr app 都必须同步那边的 map。

## 加新 app 时同步清单

```nginx
# ../deployer/nginx/yun-bright.conf 第 36 行附近
map $uri $dapr_appid {
    ~^/api/v1/userd/      "userd";
    ~^/api/v1/cube/       "cube-gateway";
    # 加新条目: 一行 = 一个 app, <URL 段> = <dapr app-id>
    ~^/api/v1/<new-app>/  "<new-app>";
    default "";
}
```

**只有走 dapr 的 app 才在 map 里**；不走 dapr 的（`auth/login`）用 location `^~` 旁路。

## proxy_pass URI 替换规则（容易踩坑）

nginx `proxy_pass http://backend/<URI>;` 带尾 `/` 时，**用 `<URI>` 替换 location 匹配的前缀**，不是简单剥前缀。

```nginx
# ✅ login 旁路: location 必须含 /auth/, 否则替换后会双倍 /auth/
location ^~ /api/v1/login/auth/ {
    proxy_pass http://localhost:8081/auth/ ;
}
# 请求 /api/v1/login/auth/login/password → 替换 /api/v1/login/auth/ 为 /auth/ → /auth/login/password ✓

# ❌ 错误: location 只到 /api/v1/login/, 替换后会是 /auth/auth/... → 404
location ^~ /api/v1/login/ {
    proxy_pass http://localhost:8081/auth/ ;
}
```

## dapr 通用 block（不要碰）

```nginx
location /api/v1/ {
    set $appid $dapr_appid;          # 必须 `set` 缓存, 否则 rewrite 后 map 失效
    if ($appid = "") { return 404; }
    rewrite ^/api/v1/[^/]+/(.*)$ /$1 break;
    proxy_set_header dapr-app-id    $appid;
    proxy_pass http://dapr_sidecar;
}
```

`set $appid` 必备：nginx map 变量 lazy 求值、不缓存，rewrite 改 `$uri` 后 map 规则全部失效。详细见 deployer 仓该文件的长注释。

## 加新 app 的 metadata（仅 deployer 端）

1. `deployer/nginx/yun-bright.conf` map 加一行
2. `deployer/README.md` 路径映射表加一行
3. `deployer/docs/proxy_pass.md` 路径映射注释加一行
4. `supertrade/.claude/rules/00-app-catalog.md` 表加一行

## 2026-09 已加: cube-router

- `cmd/cube-router` / AppID `cube-router` / 公网前缀 `/api/v1/cube-router/`
- 业务端点 `POST /v1/load` 经 `X-Branch-ID` 路由到 `branch_cube_sources` 配置的 cube 实例
- nginx map 必须新增:
  ```nginx
  ~^/api/v1/cube-router/  "cube-router";
  ```