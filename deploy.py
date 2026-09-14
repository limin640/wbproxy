#!/usr/bin/env python3
# wbproxy Zeabur 部署脚本：幂等，可重复执行，只动 wbproxy 服务
# 用法: python3 deploy.py
import json
import subprocess
import sys

import yaml

TOK = yaml.safe_load(open('/Users/limin/.config/zeabur/cli.yaml'))['token']
PROJECT = "69422387c95d806d08bfcdf3"
ENV_ID = "694223874947dd57c4fd039b"
REPO = "limin640/wbproxy"
BRANCH = "main"
GATE_KEY = "sk-wb-work-2026"


def gql(query, variables):
    r = subprocess.run([
        'curl', '-sS', '-m', '30', '-x', 'http://127.0.0.1:7897',
        '-X', 'POST', 'https://api.zeabur.com/graphql',
        '-H', f'Authorization: Bearer {TOK}',
        '-H', 'Content-Type: application/json',
        '-d', json.dumps({'query': query, 'variables': variables}),
    ], capture_output=True, text=True)
    return json.loads(r.stdout)


def main():
    auth = json.load(open('auth.json'))
    auth_flat = json.dumps(auth, ensure_ascii=False, separators=(',', ':'))

    # 1. 服务已用 createServiceFromArbitraryGit 建好（wbproxy-git），幂等复用
    r = gql(
        'query($id: ObjectID!){ project(_id: $id){ _id services { _id name } } }',
        {"id": PROJECT},
    )
    if r.get('errors'):
        sys.exit(f"查服务失败: {r['errors'][0].get('message')}")
    services = {s['name']: s['_id'] for s in r['data']['project']['services']}
    sid = services.get('wbproxy-git')
    if not sid:
        # 不存在则重建（arbitrary git，绑定仓库）
        gh_token = subprocess.run(['gh', 'auth', 'token'], capture_output=True, text=True).stdout.strip()
        r = gql(
            'mutation($p: ObjectID!, $n: String!, $url: String!, $u: String, $pw: String, $b: String){'
            ' createServiceFromArbitraryGit(projectID: $p, name: $n, gitURL: $url, gitUsername: $u, gitPassword: $pw, branch: $b){ _id name } }',
            {"p": PROJECT, "n": "wbproxy-git", "url": f"https://github.com/{REPO}", "u": "limin640", "pw": gh_token, "b": BRANCH},
        )
        if r.get('errors'):
            sys.exit(f"建服务失败: {r['errors'][0].get('message')}")
        sid = r['data']['createServiceFromArbitraryGit']['_id']
    print(f"[1/4] 服务 wbproxy-git id={sid}")


    # 2. 环境变量（不存在才建）
    envs = {"AUTH_JSON": auth_flat, "GATE_KEY": GATE_KEY}
    for k, val in envs.items():
        r = gql(
            'mutation($id: ObjectID!, $env: ObjectID!, $key: String!, $val: String!){'
            ' createEnvironmentVariable(serviceID: $id, environmentID: $env, key: $key, value: $val){ key } }',
            {"id": sid, "env": ENV_ID, "key": k, "val": val},
        )
        msg = 'ok' if not r.get('errors') else r['errors'][0].get('message', '')[:120]
        if 'already' in msg.lower():
            msg = '已存在，跳过'
        print(f"[3/4] 环境变量 {k}: {msg}")

    # 3. 部署
    r = gql(
        'mutation($id: ObjectID!, $env: ObjectID!){ deploy(serviceID: $id, environmentID: $env) }',
        {"id": sid, "env": ENV_ID},
    )
    if r.get('errors'):
        print(f"[4/4] deploy 失败: {r['errors'][0].get('message', '')[:200]}")
    else:
        print(f"[4/4] 部署已触发: {json.dumps(r['data'])[:200]}")


if __name__ == '__main__':
    main()
