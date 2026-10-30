# project.md — 改动说明

> 记录时间：2026-09-23
> 基线版本：`1.11.2-panel`（上游 commit `5a6b167`）
> 状态：**已实现、测试、部署验证，并已提交到 `dev` 分支**（部署与提交状态见文末变更记录）

本文件记录对 workbuddy2api-panel 的两项改动：

| # | 改动 | 目标 |
|---|---|---|
| 一 | 新增「导入本地部署」 | 把本地 exe 版 `auths/` 的账号凭证迁移到在线版，免重走 OAuth |
| 二 | 系统提示词面板化 + 修热生效 | 在面板里直接编辑提示词内容、看到当前生效内容，并让改动立即生效 |
| 三 | 模型汇聚开关 | 「模型与档位」把两域同名模型合并显示，单域模型保持独立 |

---

# 改动一：导入本地部署（auths 目录）

## 1.1 需求与背景

原版是 Windows exe 本地启动，账号凭证落在本地 `auths/` 目录。换成在线版部署后，已有账号
无法直接复用，只能重新走一遍 OAuth。

面板原本已有导入功能（`POST /panel/api/import/cockpit`），但只支持第三方工具 cockpit tools
导出的**扁平数组**格式，与本项目自身落盘的凭证格式不兼容，因此导不进去。

### 本地版的存储结构（实测结论）

| 位置 | 内容 | 是否值得迁移 |
|---|---|---|
| `auths/workbuddy-<uid>.json` | 每账号一文件，嵌套形 `{account:{uid,nickname,enterpriseId}, auth:{accessToken,refreshToken,expiresAt,domain,realm}}` | **是，核心** |
| `data/state.json` | 运行期池状态（冷却、熔断、积分、用量），按 uid 索引 | 否，可从上游刷新再生 |
| `data/usage.json` | 逐请求用量记录 | 否 |
| `config.json` | 与仓库 `config.example.json` 同构 | 否，服务端有自己的 |

关键发现：`auths/*.json` 的格式与面板内部 `auth.Auth` 的落盘格式**完全一致**，而
`internal/auth/auth.go` 的 `Parse()` 本就同时支持嵌套形与扁平形。因此不需要新写解析器，
直接复用 `auth.Parse`，保证本地版与在线版对"什么是合法凭证"的判定口径只有一份。

## 1.2 改动清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/panel/import_auths.go` | 新增 | 导入端点处理器 + 格式自动识别 |
| `internal/panel/import_auths_test.go` | 新增 | 6 个测试函数 |
| `internal/panel/import.go` | 修改 | 抽出两条导入路径的公共逻辑 |
| `internal/panel/panel.go` | 修改 | 注册新路由 |
| `internal/panel/index.html` | 修改 | 新增第三个标签页 |
| `internal/panel/app.js` | 修改 | 新标签页交互 + 页脚按钮状态同步 |

**新增路由**

```
POST /panel/api/import/auths
Content-Type: multipart/form-data
Body: files=<file>[, files=<file>...]     # file 字段名亦兼容，便于 curl 单文件调用
```

响应：`{ok, files, total, imported, skipped, errors}`

**`import_auths.go` 的职责**

- `importAuths`：解析 multipart、逐文件解析、逐个账号落盘进池，汇总计数
- `parseAuthBlob`：纯函数，自动识别一个上传文件的内容形态
  - 单个对象：嵌套形 `{"auth":{...},"account":{...}}` 或扁平形 `{"accessToken":...}`
  - 对象数组：`[{...},{...}]`，单个坏元素不废掉整批
  - 解析不出账号时返回 error，由调用方按"非凭证文件"静默跳过

**`import.go` 抽出的公共逻辑**（原 cockpit 导入的循环体与新路径完全重合）

- `importAccount(a)` — 落盘 + 热加载 + 附属动作。返回 error 仅表示**落盘失败**；落盘成功后
  的附属动作失败只记日志，不回滚（账号此刻已可用）
- `normalizeStoredRealm(r)` — 归一化凭证文件里的 realm 标识，仅 `cn`/`global` 有效
- `importSideEffects(a, realm)` — global 走注册激活 + trial 领取，cn 走每日签到，随后刷新余额
  并按积分解冻。`Upstream` 未注入时整体跳过（便于测试）

**一个刻意的设计选择**：realm 判定统一走 `auth.ResolveRealm(存储标识, domain)`，而**不用**
`Auth.IsGlobal()`。因为后者受 global 逃生门（`SetGlobalEnabled(false)`）影响，关掉即恒判 cn，
会把 global 账号的 realm 写死成 cn 永久污染凭证。逃生门只该锁路由，不该改写落盘数据。

**前端**：「添加账号」弹窗从两个标签扩为三个，新标签页提供两个入口：多选凭证文件
（`<input multiple>`）或直接选整个 `auths` 文件夹（`webkitdirectory`）。`switchAddTab` 同时
改为让页脚按钮跟随标签与登录阶段。

## 1.3 验证记录

| 项目 | 结果 |
|---|---|
| `go vet ./...` | 无告警 |
| `go test ./...` | 全部通过（含新增 6 个测试） |
| `node --check app.js` | 通过 |
| app.js 引用 ID vs index.html 定义 | 104 个引用 ID 全部存在，差集为空 |
| 真实数据实测 | 本地 5 个真实凭证文件 5/5 解析成功 |
| 线上 HTTP 实测 | 无密钥 401；`state.json` → `skipped=1` 不报错；合法凭证 → `imported=1` |
| 浏览器验收 | 三标签切换、页脚显隐、弹窗重开重置均正确 |

新增测试覆盖：三种内容形态解析、非凭证文件报错、端到端 HTTP 导入、**路径穿越拦截**
（uid 含 `../` 被拒且零落盘）、无密钥 401、缺字段 400、realm 归一化边界。

**实际迁移结果**：5 个账号经该端点导入，`imported=5 skipped=0`，随后全量余额刷新从上游真实
拉回积分，证明凭证有效可用。

---

# 改动二：系统提示词面板化 + 修热生效静默失效

## 2.1 需求与背景

需求原话：「在程序里面配置，可以支持每次对话之前，都能插入提示词，看看如何优化，然后携带过去」。

排查后发现**能力本身早已存在**：`internal/prompt/` 在出站前改写 messages，三种模式
`passthrough` / `custom`（`Rewrite` 替换客户端 system）/ `append`（`Append` 插入网关 system），
面板配置页也已映射 `prompt.mode` / `prompt.file`。

真正的缺口有两个，其中第二个是**静默失效的 bug**：

1. **看不到、也改不了提示词本体**。面板只有 `prompt.file` 一个**服务器文件路径**输入框，
   改内容必须 SSH 上服务器建文件；且没有任何地方能显示"当前生效的提示词是什么"。
2. **改了不生效，还不提示**。`PromptMode` / `PromptText` 是 handler 启动期捕获的静态字段，
   既不在 `livecfg` 快照里、也没被 `saveConfig` 应用，而 `restartRequiredFields` 又**没有列
   prompt** —— 面板报"保存成功并立即生效"，实际出站注入的仍是旧提示词，用户完全无从察觉。

## 2.2 改动清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/prompt/prompt.go` | 修改 | `Load` 支持内联内容并报告来源 |
| `internal/prompt/prompt_test.go` | 修改 | 更新旧签名调用 + 4 个优先级测试 |
| `cmd/server/config.go` | 修改 | 新增 `prompt.text`、`PromptSource`、env 覆盖 |
| `cmd/server/config_test.go` | 修改 | 内联优先 + 来源标注测试 |
| `internal/livecfg/livecfg.go` | 修改 | 快照加入 `PromptMode/PromptText/PromptSource` |
| `internal/server/handler.go` | 修改 | 新增 `promptCfg()`，Live 优先、静态回落 |
| `internal/server/prompt_live_test.go` | 新增 | 热生效回归测试 |
| `internal/panel/prompt.go` | 新增 | `GET /panel/api/prompt` 端点 |
| `internal/panel/prompt_test.go` | 新增 | 端点 4 个测试 |
| `internal/panel/panel.go` | 修改 | 注册路由 + `PromptInfo` 注入点 |
| `cmd/server/main.go` | 修改 | 快照写入提示词字段 + 注入 `PromptInfo` |
| `internal/panel/index.html` | 修改 | 内容 textarea + 载入按钮 + textarea 样式 |
| `internal/panel/app.js` | 修改 | 字段映射 + 载入逻辑 + 来源显示 |

**取值优先级**：`prompt.text`（内联）> `prompt.file`（文件）> 内置 `defaultprompt.md`。

内联优先于文件是刻意的：面板里写提示词是主路径，若文件能盖掉内联内容，用户会以为改了、
实际没改。全空白的内联内容视为未填（走文件/内置），避免"内容框里留几个空格"把提示词变成
空串从而静默关闭注入。`Load` 同时返回来源标识，供面板显示"当前注入的文本从哪来"。

**热生效**：提示词三字段进 `livecfg` 快照，`saveConfig` 写入，handler 的 `promptCfg()` 优先读
快照、快照未携带该组字段时整体回落静态值（避免只装了 api_key 的快照把 mode 误判成空串从而
静默关闭注入）。

**新端点**

```
GET /panel/api/prompt  →  {ok, mode, text, source, injected}
```

`source` ∈ `inline` / `file` / `builtin` / `none`；`injected` 报告是否真的会在出站前注入。
面板用它显示"当前生效：custom 模式，注入「内置默认」，共 820 字符"，并提供
**「载入当前生效」**按钮把生效文本填进内容框，便于在内置默认基础上改写。

## 2.3 验证记录

| 项目 | 结果 |
|---|---|
| `go vet ./...` | 无告警 |
| `go test ./...` | 全部通过（含新增 9 个测试） |
| `node --check app.js` | 通过 |
| 热生效实测 | 保存后立即读到新值，且**容器 PID 未变**（确认未重启） |
| `restart_required` | 不再包含 prompt（此前也未包含，但实际需要重启——已修） |
| 浏览器验收 | 内容框渲染、来源提示、载入按钮填充 820 字符均正确 |

新增测试覆盖：`Load` 三级优先级、空白内联回落、来源标注四态、`promptCfg` 快照优先 /
静态回落 / 无 Live / 显式切回 passthrough、面板端点四态（正常 / passthrough 不注入 / 零值
快照回落 / 未注入 501 / 无密钥 401）。

---

# 改动三：模型汇聚开关

## 3.1 需求与背景

需求原话：「在后台做一个功能，开启/关闭模型汇聚，然后将模型与档位里面的模型，自动从上游读取后，
进行汇聚显示，也就是 cn/global 显示为一个，如果有些不是同一个模型，比如国内有、国外没有的，
再降级为分别显示」。

现状：`/panel/api/models` 分别拉两域，每条 `id` 都带 `cn:`/`global:` 前缀，前端平铺渲染 ——
同名模型显示成两行。

### 动手前实测到的关键约束

汇聚**不能简单取其一**。实测 9 个同名模型中 **7 个两域元数据不同**，且差异有实际影响：

| 模型 | 差异 |
|---|---|
| `deepseek-v4.1-flash` | 积分倍率 CN `x0.03` / global `x0.00`；最大输出 393216 / 128000；思考档位 `[low,high,max]` / `[high]` |
| `glm-5.3-flash` | 最大输出 131072 / 32000 |
| `kimi-k2.6` | 最大输出 262144 / 32000 |
| `hy4-preview-f` | CN 有倍率 `x0.29` 且支持视觉/工具，global 均无 |

若取其一，用户会按**错误的上限与成本预期**使用模型（例如以为 global 的 glm-5.3-flash 能出 13 万
token，实际只有 3.2 万）。故实现为**差异字段两域并排显示**，相同字段只显示一次。

### 为什么只作用于面板、不作用于 `/v1/models`

model 名的 realm 前缀是网关侧**路由信号**（`internal/server.resolveModel`），而无前缀默认判 `cn`。
若把 `/v1/models` 也汇聚成裸名，global 独有模型会被静默路由到 CN 账号，去打上游不存在的模型。
因此该开关严格限定为面板展示，`/v1/models` 的带前缀命名不变（已实测验证）。

## 3.2 改动清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/panel/modelmerge.go` | 新增 | 汇聚纯函数 `mergeModelEntries` + `splitRealmID` |
| `internal/panel/modelmerge_test.go` | 新增 | 6 个测试函数 |
| `internal/panel/panel.go` | 修改 | `models` 端点接入开关 + `modelMergeEnabled()` |
| `internal/panel/app.js` | 修改 | 汇聚行渲染（`mergedRow`/`multiCell`）+ 配置字段映射 |
| `internal/panel/index.html` | 修改 | 「模型汇聚」checkbox + `.ms` 域标注样式 |
| `cmd/server/config.go` | 修改 | 新增 `panel.model_merge` |
| `cmd/server/main.go` | 修改 | 快照写入 `PanelModelMerge` |
| `internal/livecfg/livecfg.go` | 修改 | 快照加入 `PanelModelMerge` |
| `config.example.json` | 修改 | 补 `panel` 段与 `prompt.text` |

**汇聚规则**（`mergeModelEntries`）

- 跨域同名 → 合成一条 `{id: 裸名, merged: true, realms: [...], variants: [各域原条目]}`
- 仅单域存在 → 原样保留（`id` 仍带前缀），前端走普通分支
- 输出顺序稳定：跨域项落在首次出现的位置，单域项原地保留 —— 开关来回切换时行序不变，便于对比

**响应**新增 `merged` 字段报告开关状态；`/panel/api/models` 返回的条目形状在开关开启时变化。

**前端**：汇聚行显示裸名 + `CN`/`Global` 域徽标，逐列对比 `variants` —— 值相同只显示一次，
不同则**每域一行**（上行 CN、下行 Global），行尾标域。用分行而非行内拼接：差异列（尤其思考
档位是多标签）行内拼会连成一长串 `low high maxCNhighGlobal`，域归属无法分辨。行高由「模型」
列（名称+ID+徽标三行）决定，故其余列分行**不额外增高**（实测汇聚行与单域行同为 82px）。
最大输出列保留实测探测标注（逐域取各自 probe，钳制仍标 ⚠），避免汇聚把风险标注吃掉。

## 3.3 验证记录

| 项目 | 结果 |
|---|---|
| `go vet ./...` | 无告警 |
| `go test ./...` | 全部通过（含新增 6 个测试） |
| `node --check app.js` | 通过 |
| `config.example.json` | JSON 合法 |
| 关闭态 | 45 条，全部带前缀（原行为不变） |
| 开启态 | `merged=true`，36 条 = 9 条汇聚 + 27 条单域 |
| 热生效 | `restart_required` 不含 panel，保存后立即生效 |
| **`/v1/models` 未受影响** | 45 条，无前缀条目为 0 ✓ |
| 浏览器验收 | 36 行、9 条汇聚行、差异字段两域并排、单域行仍带前缀 |

新增测试覆盖：`splitRealmID` 六种输入（含未知前缀/空串/边界）、跨域合并且**两域取值不被抹平**、
单域保持独立、输出顺序稳定、空输入返回非 nil 空切片、开关读 Live 快照且可热切换。

---

# 改动四：跨域模型白名单（同一模型名通用两域）

## 4.1 需求与背景

需求原话：「帮我处理 deepseek-v4.1-flash，支持国内、国际版本同一使用」。

现状：模型名的 `cn:` / `global:` 前缀是选号路由信号，**裸名缺省判 cn**
（`resolveModel`）。所以 `deepseek-v4.1-flash` 只用 CN 账号，想用国际版必须显式写
`global:deepseek-v4.1-flash`。

### 实现可行性：池内已有现成语义

`PickExcludingForRealm(tried, model, realm)` 的谓词是
`realmOK := realm == "" || e.a.Realm() == realm` —— **`realm==""` 就表示不做域过滤**，
两域账号同为候选。所以核心改动只是"决定传不传 realm"，不必改选号算法。

同一语义在四条路径上一致生效：首次选号、每次轮换重试、会话粘性绑定校验、冷却兜底。

## 4.2 改动清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/server/resolve_model.go` | 修改 | `resolveModelEx`（报告前缀是否显式）+ `ResolveRoute`（唯一入口） |
| `internal/server/resolve_model_test.go` | 新增 | 5 个测试函数 |
| `internal/server/handler.go` | 修改 | chat 主链路改用 `ResolveRoute` |
| `cmd/server/wiring.go` | 修改 | 粘性可用集改用 `ResolveRoute`（与选号同口径） |
| `cmd/server/config.go` | 修改 | 新增 `routing.cross_realm_models` + `normalizeRouting` |
| `cmd/server/config_test.go` | 修改 | 白名单归一化测试 |
| `cmd/server/main.go` | 修改 | 快照写入白名单；**`live` 前移**到粘性装配之前 |
| `internal/livecfg/livecfg.go` | 修改 | 快照加入 `CrossRealmModels` |
| `internal/panel/app.js` | 修改 | 字段映射 + 字符串列表收集（清空即清空） |
| `internal/panel/index.html` | 修改 | 配置页输入框 |
| `config.example.json` | 修改 | 补 `routing` 段 |

**优先级**（`ResolveRoute`）

1. 显式前缀（`cn:` / `global:`）→ **强制该域，白名单不覆盖**（显式意图不被配置推翻）
2. 裸名且命中白名单 → `realm=""`，两域账号同为候选
3. 其余裸名 → `cn`（现状零回归；白名单为空时行为与仅前缀解析完全一致）

**关键实现细节**：粘性可用集闭包（`wiring.go`）与 chat 选号必须**同一口径**，故两者都走
`ResolveRoute`。若这里漏了，跨域请求会被粘到与选号口径不符的号上（粘性命中即跳过选号，
白名单等于失效）。为此 `live` 变量前移到会话粘性装配之前（原本在其后定义）。

## 4.3 验证记录

实测时先踩了一个坑，值得记录：**首轮测试用了完全相同的请求 body，结果全部粘在同一个号上** ——
`session.ExtractKey` 在无显式会话键时会从「system + 首条 user 消息内容」派生会话键
（`deriveKey`），相同 body → 同一个会话 → 粘性生效。这测的是粘性，不是选号分布。
改为每请求带唯一 `conversation_id` 后得到真实分布：

| 请求 | 12/8/8 次 | 选号分布 | 按域 |
|---|---|---|---|
| 裸名（跨域开启） | 12 | 全部 7 个账号 | **7 global / 5 CN** ✓ |
| 显式 `cn:` | 8 | 2 个 CN 号（4/4） | 8 CN / 0 global ✓ |
| 显式 `global:` | 8 | 5 个 global 号 | 8 global / 0 CN ✓ |

| 项目 | 结果 |
|---|---|
| `go vet ./...` | 无告警 |
| `go test ./...` | 全部通过（含新增 6 个测试） |
| `node --check app.js` | 通过 |
| 热生效 | `restart_required` 不含 routing，保存即生效 |

新增测试覆盖：`resolveModelEx` 七种输入（含未知前缀 / 大小写敏感 / 空串）、白名单命中与
未命中、**显式前缀不被白名单覆盖**、空白名单零回归、白名单归一化（trim / 丢空 / 去重 /
空转 nil）。

---

# 改动五：账号池「隐藏账号信息」开关

需求原话：「账号池这里，在右侧增加一个按钮，可以显示/隐藏账号信息」。

实现为**隐私开关**（截图 / 分享面板时不暴露账号身份），改动仅前端两个文件：

| 文件 | 说明 |
|---|---|
| `internal/panel/app.js` | `accMask` 状态（存 `localStorage: wb2api.accmask`）+ `renderAccounts` 内替换昵称与 uid + 切换按钮处理器 |
| `internal/panel/index.html` | 页头最右侧按钮 `#btnAccMask` + `.acc-mask` 样式 |

**关键设计取舍**

- **纯显示层**：只替换渲染出的文本，操作按钮仍走 `data-u` 上的真实 uid，功能完全不受影响
- **摘掉 uid tooltip**：行的 `title="uid: ..."` 在脱敏时一并移除，否则悬停就泄漏了
- **域徽标保留**：`国际版` 不是敏感信息，且有助于辨认行
- **切换不重新请求接口**：用已缓存的 `overviewData` 就地重绘

**边界（重要）**：脱敏只作用于**渲染出的文本**，uid 仍存在于 DOM 的 `data-u` 属性里（操作按钮需要它）。所以这是**防截图 / 防肉眼**的措施，**不能防**打开开发者工具的人。

**验证**：隐藏 → 掩码 + tooltip 消失 + `localStorage=1`；显示 → 还原；隐藏后刷新仍隐藏；操作按钮 `data-u` 仍为真实 uid、积分列正常渲染。`node --check` 通过。浏览器实测通过。

---

# 部署状态

服务器 `<服务器IP>`（Debian 12, x86_64），代码在 `/root/workbuddy2api-panel`，
Docker Compose 常驻（`restart: unless-stopped`，宿主 docker 守护进程 `enabled`）。

- 面板：`http://<服务器IP>:7863/panel/`
- OpenAI 兼容端点：`http://<服务器IP>:7863/v1`
- 运行版本：`1.12.5-panel`（2026-10-18 重建对齐）
- 仓库形态：**fork 的正常 clone**——`origin` = `4kercc/workbuddy2api-panel`，
  `upstream` = `linguo2625469/workbuddy2api-panel`，当前分支 `dev`（跟踪 `origin/dev`）。
  部署命令：`git pull --ff-only && docker compose up -d --build`。
  历史：2026-10-07 之前服务器上只有未提交的工作树改动，当天已全部固化为 `dev` 提交，
  之后每次部署都走 git。
- 账号池：15 个 —— **12 可用 / 3 已禁用**。这 3 个 CN 号的 refresh token 被上游判定
  失效（`invalid_grant: Session doesn't have required client`），需重新 OAuth 登录；
  详见文末 2026-10-18 变更记录。
- 提示词配置：`passthrough`（保持原状，未改动）
- 模型汇聚：**已开启**（`panel.model_merge: true`，验证后保留；配置页取消勾选即关闭）
- 跨域模型：**已开启**（`routing.cross_realm_models: ["deepseek-v4.1-flash"]`，验证后保留）

---

# 已知事项与后续

## 待办

- ~~**改动尚未提交**。本地与服务器两份仓库都是未提交状态（14 个文件）。服务器那份一旦
  `git pull` 或 `git reset --hard` 会被覆盖，需要建分支提交或导出 patch 固化。~~
  **已解决（2026-10-07）**：fork 成果已全部提交并推到 `4kercc/workbuddy2api-panel` 的
  `dev` 分支；服务器 `/root/workbuddy2api-panel` 已改为该 fork 的正常 clone
  （`dev` 跟踪 `origin/dev`），不再存在「未提交改动被 pull 覆盖」的风险。
- ~~**`project.md` 被 `.gitignore` 排除**。仓库约定 `*.md` 全部忽略（仅 `!README.md` 例外），
  因此本文件不会进版本库。若要纳入，需在 `.gitignore` 补 `!project.md`。~~
  **已解决**：`.gitignore` 已补 `!project.md`，本文件随代码进版本库。
  （服务器那份含真实 IP 的未脱敏原文，在 2026-10-07 首次 git 部署时按设计被脱敏版替换；
  备份在服务器 `/root/workbuddy2api-panel-project.md.bak`。）
- **待人工处理**：3 个 CN 号（`18587313131` / `16608713131` / `꯭刺꯭猬꯭`）已因上游 401
  被自动禁用，需在面板「添加账号」重走 OAuth。**点「解冻」无效**——refresh token 已失效，
  解冻只会让它在 15 分钟内再次被判失效并禁用。

## 运维注意

- **迁移后应停掉本地 exe 版**。OAuth 刷新令牌会轮换：本地与服务端持有同一批账号凭证时，
  任一方触发刷新都会使另一方的 refresh token 失效，表现为账号间歇性 401、需反复重登。
- **面板目前是纯 HTTP**，密钥经 `Authorization` 头明文传输，公网可嗅探。服务器 443 已被
  其他服务占用，收紧方案二选一：SSH 隧道（只监听 `127.0.0.1`，无需域名）或挂 HTTPS 反代。
- **容器健康检查依赖账号数**：`/healthz` 在账号数为 0 时按设计返回 503，Dockerfile 的
  HEALTHCHECK 用 `wget` 判定，因此空池时容器显示 `unhealthy`。加入账号后自动转 `healthy`；
  不影响 `restart: unless-stopped`（该策略只看进程是否退出）。
- **`go test` 别用 `-mod=mod`**：会把 `go-redis/v9` 从 indirect 提升为直接依赖而改动 `go.mod`。
  默认的 `-mod=readonly` 即可。

## 合规边界

本项目为非官方网关，README 已明确仅限本人授权账号在私有环境使用。相关自动化会调用上游
活动接口，账号存在被风控的可能，风险自担。

---

# 变更记录（追加）

## 2026-10-18 · `1.12.5-panel`：401 死号识别

**问题**：3 个 CN 号连续 10 天余额查询返回 `401 Authorization Required`（APISIX 网关 HTML，
无 `12153` 业务码），但账号池仍报 15/15 可用、首页显示「服务正常」，运维完全看不到。

**根因**：`internal/upstream/client.go` 的 `Classify` 只认 `sessionDeadMarkers`（`12153` /
`Offline user session not found`）。裸 401 HTML 命中不了任何 marker，一路落到 4xx 兜底
`ErrClient` → `applyErrorPolicy` 走「只换号不罚」；余额刷新（`RunBalanceRefreshNow`）
更是只打一行日志、完全不回写池状态。

**改动**：
- `Classify`：HTTP 401 一律判 `ErrSessionDead`（判在 marker 层之前）
- `RunBalanceRefreshNow`：`ErrSessionDead` → `NoteSessionDeadReason`（连续 3 次才禁用），
  查询成功 → `ClearSessionDead` 清零。余额刷新每 5 分钟一次，死号 15 分钟内必被识别
- `Pool.NoteSessionDeadReason`：新增，允许指定禁用 reason（`NoteSessionDead` 委托给它，
  12153 行为不变）
- `applyErrorPolicy`：12153 仍立即禁用；裸 401 改走连续计数（防瞬时抖动误杀）
- 禁用 reason 区分：`12153 session dead` / `401 auth failed (re-login required)`

**验证**：日志实证这 3 个号的 refresh token 已失效
（`invalid_grant: Session doesn't have required client`），无法刷新恢复，只能重新登录；
部署后余额刷新 3 轮内应转为「已禁用」。单元测试：`TestClassify` 新增裸 401 用例；新增
`TestRunBalanceRefreshMarksAuthFailure` / `TestRunBalanceRefreshClearsAuthFailureCount`。

**部署形态变更**：服务器 `/root/workbuddy2api-panel` 已改为 fork 的正常 clone
（`origin` = `4kercc/workbuddy2api-panel`，`upstream` = 原 `linguo2625469`，当前分支
`dev`）。此前服务器上的 fork 成果全是未提交工作树改动，现已全部固化进 git；
以后部署直接 `git pull && docker compose up -d --build`。

## 2026-10-27 · `dev@735cfc3`：修复 global 账号「假冻结」+ 面板对话测试

**现象**：面板 14 个账号里 11 个被标「已禁用」，reason 全是
`account banned by upstream (11140 request illegal), re-login required`。但用户发现
点「签到」这些账号仍能走通上游（返回业务错误 `{"code":10001,"msg":"签到活动未开启或已过期"}`
而非连接/鉴权失败）——怀疑是**假冻结**。

**排查结论（实测确证为假冻结）**：
- 11 个「已禁用」账号**全部是 global（国际版）域**；3 个正常账号全部是 cn 域。
- 容器日志实测：上游返回的是 `{"code":11128,"msg":"request illegal",
  "displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。"}}`
  ——**11128 是「通道校验失败」**（账号健康），与账号级限制 11140 的 msg 文案
  **完全相同**（都是 "request illegal"）。
- 旧 `Classify` 只按裸文案 "request illegal" 判定 → 把 11128 通道校验失败误判为
  `ErrAccountFault` → `applyErrorPolicy` **硬禁用健康账号**。容器日志里 11140 出现 0 次、
  11128 出现 3 次，reason 串却写死「11140」——reason 本身也是错的。

**一线根因**：上游国际端点收紧通道校验后，**缺 `X-Conversation-ID` 的请求直接
400/11128**（conversation id missing）。`injectConversationHeaders` 此前只在入站带
conversationId 时才发该头（「透传优先」），global 请求因此稳定被拦——3/3 复现。

**改动**（吸收上游权威修复 thrasher824 PR #233 / commit `7a8b7a7`）：
- `internal/upstream/client.go`：新增 `ErrChannelRefused`；`Classify` 按**业务码**分流
  ——`isChannelRefused`（11128+文案）→ `ErrChannelRefused`；`IsAccountRestricted`
  （11140+文案）→ `ErrAccountFault`；`accountFaultMarkers` 移除裸 "request illegal"。
- `internal/upstream/headers.go`：`injectConversationHeaders` **始终发送**
  `X-Conversation-ID`，入站未带时补 32 hex（一线根因修复）。
- `internal/server/handler.go`：`ErrAccountFault` 改用 `IsAccountRestricted`；新增
  `case ErrChannelRefused`（零动作，不罚账号）；`hint.go` 补排查指引。
- 回归测试：`client_test.go` / `handler_test.go` / `headers_test.go`（会话头 wire-level）。

**新增能力**（本 fork）：面板账号卡片新增「对话」按钮 + `POST /panel/api/accounts/{uid}/chat`
——用指定账号直连上游真实发一次 chat，聚合 SSE 回回复。用于**分辨真封禁/假封禁**：
能出回复 = 账号健康（假封禁，点「解冻」即可）；401/12153 = 凭证失效（真封禁）。
- `internal/panel/chat.go`（新增）：`accountChat` 处理器。
- `internal/panel/{panel.go,app.js,index.html}`：路由注册 + 行按钮 + 结果弹窗。

**验证（线上实测）**：

| 项 | 结果 |
|---|---|
| 修复前 global 账号对话 | 3/3 稳定 11128 |
| 修复后 global 账号对话 | 11/11 HTTP 200 + 真实回复 |
| 全量解冻后 `/healthz` | `healthy:14 total:14`，`realm_servable:{cn:true,global:true}` |
| `/v1/chat/completions` 端到端 | 200，正常返回 |
| `go vet ./...` / `go test ./...` | 全通过 |

**运维动作**：11 个被误禁的 global 账号已通过 `POST /panel/api/accounts/{uid}/revive`
全部解冻（`{ok:true}` ×11），池恢复 14/14 可用。

**回滚**：`git reset --hard e7b561d && docker compose up -d --build`（回退到本次改动前）。

## 2026-10-27 · `dev@8aa0924`：首页「导出配置」（账号凭证 + config.json 单文件备份）

**需求**：首页加一个配置导出功能，方便迁移数据（换服务器 / 重建部署免重新走 OAuth）。

**设计**：
- `GET /panel/api/export` → 单个 JSON 下载（`Content-Disposition: attachment;
  filename="wb2api-backup-<时间>.json"`），结构：
  `{app, version, exported_at, accounts:[嵌套形 auth 文档…], config:{config.json 内容}}`。
- **accounts 直接读 AuthDir 下的 `workbuddy-*.json`**（而非读内存）：盘上文件是
  `SaveAtomic` 的原子一致快照，避免与 `RefreshToken` 并发改写 token 字段的撕裂读——
  `AccessToken / RefreshToken / ExpiresAt` 三个字段无法在一次加锁内一并取出（sync.Mutex
  不可重入，逐个 accessor 取值可能跨一次刷新，出现「新 accessToken + 旧 refreshToken」
  的坏快照）。逐字就是导入侧期望的格式，往返无损。
- **迁移闭环**：`parseAuthBlob` 新增第三种形态识别——备份包 `{"accounts":[…]}`（抽出
  `parseAuthItems` 复用数组分支）。导出文件因此可直接经「添加账号 → 本地部署导入」
  重新导入，无需手工拆包。

**改动清单**：

| 文件 | 类型 | 说明 |
|---|---|---|
| `internal/panel/export.go` | 新增 | `exportConfig` 处理器 + `readAuthDocs` |
| `internal/panel/export_test.go` | 新增 | 3 个测试（备份包解析 / 导出端到端+往返 / 401） |
| `internal/panel/import_auths.go` | 修改 | `parseAuthBlob` 支持备份包；抽出 `parseAuthItems` |
| `internal/panel/panel.go` | 修改 | 注册 `GET /panel/api/export` |
| `internal/panel/index.html` | 修改 | 顶栏「导出配置」按钮 |
| `internal/panel/app.js` | 修改 | 下载交互（原生 fetch + blob，保留服务端文件名） |

**验证**：

| 项 | 结果 |
|---|---|
| `go test ./internal/panel/`（含 3 个新测试） | 通过 |
| `go vet ./...` / `go test ./...` | 全通过 |
| 线上 `GET /panel/api/export` | 200 + `Content-Disposition: attachment; filename="wb2api-backup-20261027-233309.json"` |
| 线上导出内容 | `accounts` 14 条 + `config` 段齐全 |
| 面板 UI | 顶栏「导出配置」按钮已就位 |

**安全提示**：导出文件含 `accessToken` / `refreshToken`（等同账号密码），只应保存在可信位置。

**回滚**：`git reset --hard 5574f59 && docker compose up -d --build`。



## 2026-10-30 · `1.12.6-panel`：新任务自动发现（展示与执行口径拆分）

**问题**：上游活动会自动上下线（开学季「校园日」已下线），但上游新上的任务在面板里
**完全看不见**——`internal/panel/taskcenter.go` 的 `growthPending` 用动作白名单过滤
（`return autoActionFor(t.TaskCode) != nil`），没登记动作的任务连展示都不进。

**线上实测（2026-10-30，抓原始响应）**：上游任务接口返回 **24 个字段**，网关解析器只读
16 个，丢掉的四个里有关键信息：

| 字段 | 实测值示例 | 作用 |
|---|---|---|
| `valid_start` / `valid_end` | `2026-10-19T11:46:00+08:00` ~ `2026-11-15T23:59:00+08:00` | **活动起止**——上游据此自动上下线，这就是「活动会自动更新」的机制 |
| `is_new` | `true` / `false` | 运营侧新任务角标 |
| `template_id_fixed` | `23`、`11` | 模板类任务的目标模板 ID |

当时 6 个未完成任务（合计 **400 积分 + 30 能量**）全被隐藏：
`WorkBuddy_Valorant`(100c) / `expert_use_bowangwang` / `expert_use_douyinyin` /
`expert_use_maidehao` / `expert_use_shangjia_xuanchuan`（各 50c）/
`wb_wechat_oa_subscribe_task`(100c)。`school_season` 已从默认与 mp 两个口径双双消失。

**改动（仅展示层，不碰执行路径）**：
- `internal/upstream/tasks.go`：`Task` 增 `ValidStart` / `ValidEnd` / `IsNew` / `TemplateID`
  四字段并在 `parseGrowthTasks` 解析
- `internal/panel/taskcenter.go`：
  - `growthPending` 去掉动作白名单 → **展示口径**（未领取且未达标全列）
  - 新增 `growthRunnable` = `growthPending && autoActionFor != nil` → **执行口径**，
    与改动前逐字等价（`planQueue` 两处改用它），队列行为不变
  - 新增 `growthTaskView{upstream.Task; Automatable}` + `growthView()`；`scanAccountItem.Growth`
    类型随之改为 `[]growthTaskView`（`automatable` 字段随 JSON 透出）
  - 扫描返回增 `manual_count`，日志区分「其中 N 项待适配」
- `internal/panel/app.js`：待办条目加「待适配」(warn) /「新」(ok) 标记 + 活动截止倒计时
  （`validEndInfo`：<48h 红、<7 天黄）；扫描后 `manual_count>0` 弹 warn toast
- `internal/panel/index.html`：补 `.tst.warn` 样式
- 版本号 `1.12.6-panel`

**为什么不自动执行新任务**：上游任务接口**不返回机器可读的动作描述**（无 event_code /
action_type），只有 `task_code` + 人读文案。要自动完成必须知道「报什么事件」。本次只做
发现层，执行仍需人工适配动作。

**后续可做（已验证可行，未实施）**：5 个新专家任务的 `task_desc` 是同一模板
「在专家页搜索并召唤「X」，完成一次对话。」；专家市场 `POST /portal/operation-platform/
market/expert/list` 支持 `keyword` 参数，实测 6/6 专家可按名命中（需同时匹配
`display_name_zh` 与 `profession_zh`，例「商家宣传页与海报设计师」的 display name 是
「商家宣传设计师」）。即：从 `task_desc` 抽「X」→ keyword 搜市场 → 复用现有
`runExpertBatch` 召唤链，可做到新专家任务零代码改动。**该层会真实消耗对话额度，未开。**

**唯一不可自动化**：`wb_wechat_oa_subscribe_task`（要求关注公众号满 24 小时）。

### 2026-10-30 补充：开学季活动期口径对齐（同一根因）

**问题**：开学季活动已结束（上游 `in_period=false`），但面板仍把 4 项/账号列为待办——
实测 3 个 CN 账号共 12 项假待办，且「执行全部待办」会为每个账号排入一个 scheduler 侧
**静默跳过的空闭环**（`scheduler/school.go` 的 `schoolAccount` 在 `!inPeriod` 时直接
`return`），队列却报"完成"。

**根因**：`internal/panel/taskcenter.go` 的 `tasksScanAll` 与 `planQueue` 都记录了
`inPeriod` 但**没有用它过滤**——只有 scheduler 侧做了门控，面板侧漏了。

**改动**：
- `tasksScanAll`：`inPeriod` 为假时不收集开学季待办（保留 `in_period` 字段供前端展示）
- `planQueue`：同上，不排开学季闭环
- `internal/panel/app.js`：状态卡在 `in_period=false` 时账号行标「活动已结束」，
  汇总行改为「开学季活动已结束（上游 in_period=false），任务条目不计分」

**验证**：部署后扫描 `pending_count` 由 30 降为 18（12 项开学季假待办消失），
`manual_count` 仍为 18（3 账号 × 6 个新任务）。

## 2026-10-30 · `1.12.7-panel`：L2 专家类任务动态适配 + L3 活动窗口动态化

### L2 · 专家类任务动态适配（零代码完成新专家任务）

**背景**：L1 让新任务"看得见"，但 5 个 `expert_use_*` / `WorkBuddy_Valorant` 仍标
「待适配」——上游每上一个「体验某个专家」的任务，此前都要写一个 `runXxx` 并登记进
`autoActions`。实测这些任务的 `task_desc` 高度同构：

```
在专家页搜索并召唤「抖音策略师」，完成一次对话。
即日起至11月15日，在专家页搜索并召唤「无畏契约拼豆专家」，完成一次对话。
```

**实现**：
- `internal/panel/autotask.go`
  - `expertNameFromTask()`：从 `task_desc`/`description` 取「召唤「X」」，`title` 为
    「体验「X」专家」时兜底（要求「」后紧跟「专家」）。**刻意不认裸「X」**——关注公众号
    任务标题是「关注「腾讯 WorkBuddy」官方公众号」，裸模式会把公众号当专家名
  - `resolveAction(t)`：静态表优先，其次动态适配出带闭包的 `autoAction`
  - `runExpertNamed()`：`keyword` 搜市场 → `matchExpertByName`（`display_name_zh` 与
    `profession_zh` 都要看）→ 精确命中才执行；**搜不到直接报错返回，不猜不换人**
  - `summonExpertOnce()`：从 `runExpertBatch` 抽出单专家链路，批量与动态适配共用
    （避免两条链路事件形状漂移——事件形状是上游判据，漂移即不点亮）
  - `growthRunnable` / `growthView` / `runGrowthQueued` 改用 `resolveAction`；
    执行端改为「拿到任务后再解析动作」（动态适配要看 task 文案，只凭 code 解析不出）
- `internal/upstream/desktop.go`：`MarketExpertSearch(a, expertType, keyword)` 与
  `marketExpertList` 公共实现（`keyword` 实测服务端支持，搜「抖音策略师」第 2 条命中）
- 新增 `internal/panel/dynamic_action_test.go`：9 个识别用例（全部取自真实文案）+
  静态优先 + 动态适配 + 不可自动化 + 展示/执行口径分离

**验证（2026-10-30 实测）**：6 个专家名 keyword 搜索 5 个精确命中；「商家宣传页与海报
设计师」的 `display_name_zh` 是「商家宣传设计师」、`profession_zh` 才是任务文案里的名字
——两个字段都匹配后 6/6 命中。

**副作用（已知并接受）**：识别为专家类的任务会**进执行队列**，触发时对每个账号跑一次
真实对话（消耗额度）。3 个 CN 账号 × 5 个专家任务 = 15 次对话/轮。要停就关
`schedule.task_auto_enabled` 或不点「执行全部待办」。

### L3 · 活动窗口动态化（不再硬编码活动日期）

**实现**：
- `internal/upstream/school.go`：新增 `SchoolWindow{InPeriod,StartAt,EndAt,Chances}` 与
  `SchoolConfig()`；`SchoolChances()` 改为委托它（同一次 `/config` 调用）
- `internal/panel/taskcenter.go`：`schoolStatus` 返回顶层 `window`（活动级数据，取首个
  成功账号值），并把抽奖余额改由 `SchoolConfig` 一并取得（不增加上游调用次数）
- `internal/panel/index.html`：`<h3>开学季 <span class="hint" id="schoolWindow">` 去掉写死的
  「09-13 ~ 09-24」
- `internal/panel/app.js`：`renderSchoolWindow()` 按 `start_at`/`end_at` 动态拼
  「09-13 ~ 09-24 · 已结束」；`schoolDay()` 只取 MM-DD
- `internal/scheduler/school.go` / `internal/upstream/school.go`：注释改为「窗口由上游控制」，
  保留历史日期仅作判据逆向时点记录

**实测上游窗口**：`start_at=2026-09-13 00:00:00`、`end_at=2026-09-24 23:59:59`、
`in_period=False`。

### 2026-10-30 修正：按名字匹配会召唤错专家（已修，实测通过）

**踩坑**：L2 首版按名字在专家市场里取**第一个**精确命中，实测召唤了 `ex_loEfkMTwHi8v`
并跑完召唤链+真实对话，但 `progress` 仍 `0/1`——**任务不计数**。

**根因**：任务文案里的专家名**不唯一**。搜「抖音策略师」返回 4 个
`display_name_zh` 或 `profession_zh` 都叫这个名字的专家：

| expert_id | display_name_zh | profession_zh |
|---|---|---|
| `ex_loEfkMTwHi8v` | 抖音策略师 | 抖音短视频与直播电商策略专家 |
| `ex_lKRHiHEbYoHL` | 抖音策略师 | 抖音内容策略顾问 |
| `ex_WmcyDlsTPaiS` | 抖音策略师 | 抖音全域增长策略师 |
| **`ex_FbY5nlWTqDod`** | **斗音音** | **抖音策略师** ← 任务真正指向的 |

而任务的 `jump_url` 里带着**权威 expertId**：

```
https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_FbY5nlWTqDod
```

**修复**：
- 新增 `expertIDFromTask()`：整体 URL 解码后取 `expertId=` 到非标识符字符
- `runExpertTask()`：**expertId 优先**（市场搜索只为补齐 version/categories，搜不到也用
  最小记录继续——ID 是上游自己给的）；无 ID 时按名字匹配且**要求唯一**，命中多个直接
  报错拒绝执行
- `matchExpertsByName()` 返回全部命中（原 `matchExpertByName` 取首个，已删除）

**实测覆盖**：4/5 个目标任务的 `jump_url` 带 expertId（`expert_use_maidehao` /
`douyinyin` / `shangjia_xuanchuan` / `bowangwang`）；`WorkBuddy_Valorant` 与
`Expert_lighthouse` 没有，走名字唯一匹配兜底。

**端到端实测（2026-10-30，账号 2cfe9ae1）**：

```
expert_use_douyinyin  0/1 → 1/1  已召唤「斗音音」(ex_FbY5nlWTqDod)   自动领奖 +50 分 +5 能
WorkBuddy_Valorant    0/1 → 1/1  已召唤「无畏契约拼豆专家」(ex_xQHHD1u5Jkua) 自动领奖 +100 分 +5 能
```

两条路径（权威 ID / 名字唯一匹配）均实测通过，共 +150 积分 +10 能量。

**教训**：上游文案里的「名字」是给人看的，不能当主键；能用 ID 就用 ID，用名字时必须
要求唯一，宁可报错也不猜。
