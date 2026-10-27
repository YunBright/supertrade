---
paths: ["dapr/components/**", "deploy/**/*.yaml", "deploy/**/*.yml", "deployments/**/*.yaml"]
---

# nginx map 规则同步

**本仓没有 nginx 配置**(nginx 在 `../deployer/nginx/yun-bright.conf`),
但本仓每加一个 dapr app 都必须同步那边的 map。

## map 长什么样

```nginx
# ../deployer/nginx/yun-bright.conf
map $uri $dapr_appid {
    ~^/api/v1/userd/      "userd";
    ~^/api/v1/cube/       "cube-gateway";
    # 加新条目:一行 = 一个 app,<URL 段> = <dapr app-id>
    ~^/api/v1/<new-app>/  "<new-app>";
    default "";
}
```

- **只有走 dapr 的 app 才在 map 里**;不走 dapr 的(`auth/login`)用 location `^~` 旁路。
- **加新 app 只加这一行 map,不加 location** —— 通用 `location /api/v1/` 块已经会 rewrite + 转发。
  多写一个 location = 多一处能写错的地方,写错的症状是"公网 404 且没有任何日志"。

## 通用 block(不要碰)

```nginx
location /api/v1/ {
    set $appid $dapr_appid;          # 必须 `set` 缓存,否则 rewrite 后 map 失效
    if ($appid = "") { return 404; }
    rewrite ^/api/v1/[^/]+/(.*)$ /$1 break;
    proxy_set_header dapr-app-id    $appid;
    proxy_pass http://dapr_sidecar;
}
```

`set $appid` 必备:nginx map 变量 lazy 求值、不缓存,rewrite 改 `$uri` 后 map 规则全部失效。

## proxy_pass URI 替换(容易踩坑)

`proxy_pass http://backend/<URI>;` 带尾 `/` 时,**用 `<URI>` 替换 location 匹配的前缀**,不是简单剥前缀。

```nginx
# ✅ login 旁路:location 必须含 /auth/,否则替换后会双倍 /auth/
location ^~ /api/v1/login/auth/ {
    proxy_pass http://localhost:8081/auth/ ;
}
# 请求 /api/v1/login/auth/login/password → 替换 /api/v1/login/auth/ 为 /auth/ → /auth/login/password ✓

# ❌ 错误:location 只到 /api/v1/login/,替换后会是 /auth/auth/... → 404
location ^~ /api/v1/login/ {
    proxy_pass http://localhost:8081/auth/ ;
}
```

## 加新 app 的同步清单

1. `deployer/nginx/yun-bright.conf` map 加一行。
2. `deployer/README.md` 路径映射表加一行。
3. `deployer/docs/proxy_pass.md` 路径映射注释加一行。
4. 本仓 `.claude/rules/00-app-catalog.md` 表加一行。
5. authd `JWT_AUDIENCE` 加 **aud 短名**(短名!不是 dapr app-id)。
