# project.md — 改动说明

> 记录时间：2026-09-23
> 基线版本：`1.11.2-panel`（上游 commit `5a6b167`）
> 状态：**均已实现、测试、部署验证，但尚未提交**

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
- 运行版本：`1.12.1-panel`（服务器容器已重建对齐）
- 账号池：9 个，全部可用
- 提示词配置：`passthrough`（保持原状，未改动）
- 模型汇聚：**已开启**（`panel.model_merge: true`，验证后保留；配置页取消勾选即关闭）
- 跨域模型：**已开启**（`routing.cross_realm_models: ["deepseek-v4.1-flash"]`，验证后保留）

---

# 已知事项与后续

## 待办

- **改动尚未提交**。本地与服务器两份仓库都是未提交状态（14 个文件）。服务器那份一旦
  `git pull` 或 `git reset --hard` 会被覆盖，需要建分支提交或导出 patch 固化。
- **`project.md` 被 `.gitignore` 排除**。仓库约定 `*.md` 全部忽略（仅 `!README.md` 例外），
  因此本文件不会进版本库。若要纳入，需在 `.gitignore` 补 `!project.md`。

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
