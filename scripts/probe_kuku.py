#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
库库AI（百度 GenFlow）账号 / 额度探测工具
=========================================

背景
----
库库AI 是百度 GenFlow 改名的消费级智能体产品，**没有对外开发者 API**。
它的网页端（https://kuku.baidu.com，pc-genflow-pro 1.7.0）走私有接口：

    积分首页   GET  /api/genflowpro/freepoint/homenew
    我的资料   GET  /api/genflowpro/settings/profile
    VIP 余量   GET  /bizapi/gfpro/getgfvipremain
    任务状态   GET  /api/genflowpro/freepoint/taskstatus?task_key=<key>
    领奖励     POST /api/genflowpro/freepoint/rewardClaim
    任务完成   POST /api/genflowpro/freepoint/taskComplete
    邀请码     POST /api/genflowpro/freepoint/inviteCode
    绑定邀请   POST /api/genflowpro/freepoint/inviteBind
    （对话侧另有 /wenchain/genflowpro/sendmsg + /sse/msgexchange，本工具不碰）

鉴权是**百度登录 Cookie**（BDUSS / STOKEN 为 httpOnly），没有 OAuth。
所以本工具的唯一输入就是浏览器里那份 Cookie。

本工具做什么
------------
把上述接口按「先资料、再额度、后动作」跑一遍，原样打印每一层的 JSON。
目的是**把契约钉死**：StarPort 侧写适配器时，字段名/信封/错误码都以本工具
的真实输出为准，而不是靠猜。

用法
----
    # 1) 只读探测（推荐先跑这个，不产生任何副作用）
    python scripts/probe_kuku.py --cookie "BDUSS=xxx; STOKEN=yyy"

    # 2) Cookie 很长时，从文件读（Chrome DevTools 里复制 Cookie 请求头整行）
    python scripts/probe_kuku.py --cookie-file kuku_cookie.txt

    # 3) 带上你的邀请码，顺带验证绑定链路（会产生真实副作用，慎用）
    python scripts/probe_kuku.py --cookie-file kuku_cookie.txt --invite-code X4WFL6FH --do-bind

    # 4) 只看要打哪些接口，不发请求
    python scripts/probe_kuku.py --dry-run

怎么拿 Cookie
-------------
浏览器打开 https://kuku.baidu.com 并登录 → F12 → Network → 过滤 `freepoint`
→ 任选一条请求 → 右键 Copy as cURL → 把 `-H 'cookie: ...'` 里那一整串贴进来。
注意 BDUSS 等同于账号密码，别贴到公开渠道。

输出怎么用
----------
把输出贴回对话即可。重点看三处：
  - `profile` 里的 `uk`（账号数字 ID，适配器要存）
  - `homenew` 里的余额字段名与 `activities` 结构（决定 Balance 怎么映射）
  - 各接口的 `errno` 语义（0 成功 / -6 未登录 / 151004 无活动 / 2 参数错）
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request

BASE = "https://kuku.baidu.com"

# 与网页端一致的最小头集合。Cookie 单独拼。
COMMON_HEADERS = {
    "Accept": "application/json, text/plain, */*",
    "Accept-Language": "zh-CN,zh;q=0.9",
    "Origin": BASE,
    "Referer": BASE + "/",
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
    ),
}

# errno 语义（来自前端 credits-activity-api 与实测）：
#   0        成功
#   -6       未登录
#   151004   该活动未开放（前端按「无活动」处理，不报错）
#   2        参数错误
ERRNO_MEANING = {
    0: "成功",
    -6: "未登录（Cookie 失效或没带全）",
    151004: "该活动未开放",
    2: "参数错误（通常是未登录时服务端拿不到 uk）",
}


def _req(method: str, path: str, cookie: str, body: dict | None = None,
         timeout: int = 20) -> tuple[int, str]:
    """发一个请求，返回 (HTTP 状态码, 响应原文)。不抛异常，错误也当数据返回。"""
    url = BASE + path
    data = None
    headers = dict(COMMON_HEADERS)
    headers["Cookie"] = cookie
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:  # 网络层失败：原样报出，别吞
        return 0, f"<transport error: {e}>"


def _pretty(raw: str, limit: int = 4000) -> str:
    """能解析就缩进，不能就原样截断——目标是看清结构，不是好看。"""
    try:
        obj = json.loads(raw)
    except Exception:
        return raw[:limit]
    return json.dumps(obj, ensure_ascii=False, indent=2)[:limit]


def _errno_of(raw: str) -> int | None:
    try:
        obj = json.loads(raw)
    except Exception:
        return None
    if isinstance(obj, dict) and "errno" in obj:
        v = obj["errno"]
        return v if isinstance(v, int) else None
    return None


def call(label: str, method: str, path: str, cookie: str,
         body: dict | None = None) -> str:
    status, raw = _req(method, path, cookie, body)
    print(f"\n===== {label} =====")
    print(f"{method} {path}" + (f"  body={json.dumps(body, ensure_ascii=False)}" if body else ""))
    print(f"HTTP {status}")
    print(_pretty(raw))
    e = _errno_of(raw)
    if e is not None:
        print(f"→ errno={e} ({ERRNO_MEANING.get(e, '未知')})")
    return raw


def main() -> int:
    ap = argparse.ArgumentParser(
        description="库库AI 账号/额度探测（只读为主，动作需显式开关）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("--cookie", help="浏览器里复制的整串 Cookie")
    ap.add_argument("--cookie-file", help="从文件读 Cookie（一行）")
    ap.add_argument("--invite-code", default="", help="邀请码（配合 --do-bind 使用）")
    ap.add_argument("--task-key", default="", help="要查状态的任务 key（默认不查）")
    ap.add_argument("--do-claim", action="store_true", help="执行 rewardClaim（会产生副作用）")
    ap.add_argument("--do-bind", action="store_true", help="执行 inviteBind（会产生副作用）")
    ap.add_argument("--dry-run", action="store_true", help="只列出将请求的接口")
    args = ap.parse_args()

    if args.dry_run:
        print("将按顺序请求：")
        for m, p, why in [
            ("GET", "/api/genflowpro/settings/profile", "拿 uk / 昵称，确认登录态"),
            ("GET", "/api/genflowpro/freepoint/homenew", "积分余额与活动列表"),
            ("GET", "/bizapi/gfpro/getgfvipremain", "VIP 与剩余额度"),
        ]:
            print(f"  {m:4} {p:45} # {why}")
        if args.task_key:
            print(f"  GET  /api/genflowpro/freepoint/taskstatus?task_key={args.task_key}")
        if args.do_claim:
            print("  POST /api/genflowpro/freepoint/rewardClaim   # 有副作用")
        if args.do_bind:
            print("  POST /api/genflowpro/freepoint/inviteBind    # 有副作用")
        return 0

    cookie = (args.cookie or "").strip()
    if not cookie and args.cookie_file:
        with open(args.cookie_file, encoding="utf-8") as f:
            cookie = f.read().strip()
    if not cookie:
        print("缺少 Cookie：用 --cookie 或 --cookie-file 提供（先跑 --dry-run 看要打哪些接口）",
              file=sys.stderr)
        return 2

    # 1) 资料：确认登录态，拿 uk（后续接口多半靠它）
    profile_raw = call("profile 资料", "GET", "/api/genflowpro/settings/profile", cookie)

    # 2) 积分首页：余额 + 活动
    home_raw = call("freepoint/homenew 积分首页", "GET", "/api/genflowpro/freepoint/homenew", cookie)

    # 3) VIP 余量：另一套信封（status.code），与上面不是同一层
    call("gfpro/getgfvipremain VIP 余量", "GET", "/bizapi/gfpro/getgfvipremain", cookie)

    # 4) 可选：任务状态
    if args.task_key:
        call(f"taskstatus {args.task_key}", "GET",
             f"/api/genflowpro/freepoint/taskstatus?task_key={args.task_key}", cookie)

    # 5) 可选副作用动作（默认不跑）
    if args.do_claim:
        call("rewardClaim 领取奖励", "POST", "/api/genflowpro/freepoint/rewardClaim", cookie, {})
    if args.do_bind:
        if not args.invite_code:
            print("\n--do-bind 需要同时给 --invite-code", file=sys.stderr)
        else:
            call("inviteBind 绑定邀请码", "POST", "/api/genflowpro/freepoint/inviteBind",
                 cookie, {"invite_code": args.invite_code})

    # 6) 一句话结论，方便直接贴回来
    print("\n===== 小结 =====")
    p_ok = _errno_of(profile_raw) == 0
    print(f"登录态：{'有效' if p_ok else '无效/未登录'}")
    if p_ok:
        try:
            d = json.loads(profile_raw).get("data") or {}
            print(f"uk={d.get('uk')} nickname={d.get('nickname')!r} phone={d.get('phone')!r}")
        except Exception:
            pass
    print("把上面的 JSON 贴回对话，我按真实字段写适配器。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
