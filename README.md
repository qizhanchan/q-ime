# q-ime

macOS 拼音输入法。Go + cgo + [qui](https://github.com/qizhanchan/qui)，候选窗由 qui 绘制。

IMK 那一层是 `bridge_darwin.m` 的薄转发，所有决策在 Go。引擎是 188 万词条的
mmap 词库 + 格搜索 + Viterbi 整句：

```
nihao                    → 你好
zhg                      → 中国
womenmingtianqubeijing   → 我们明天去北京
```

技术设计与实现细节见 [`docs/arch.md`](docs/arch.md)。

## 安装

```bash
git clone --recurse-submodules https://github.com/qizhanchan/q-ime
cd q-ime
./build.sh install
```

已经克隆过的话，初始化词库子模块即可（rime-ice，浅克隆约 50MB）：

```bash
git submodule update --init --depth 1
```

**首次安装之后必须注销重登一次**：`TISRegisterInputSource` 只写入一个瞬态
条目，持久化注册发生在登录时。之后改代码重新 `install` 不需要注销，切走再
切回输入法即可加载新二进制（改了 bundle ID、connection name 或 mode key 才
需要再注销一次）。

安装后手动启用：`系统设置 > 键盘 > 输入源 > 编辑 > + > 简体中文`，选
「qui 拼音」，用 `Ctrl+Space` 切换。

```bash
./build.sh check             # 验收注册（注册失败是完全静默的，别靠肉眼看设置）
./build.sh logs              # 看日志
./build.sh lexicon           # 只重建中文词库
./build.sh english           # 只重建英文词表（会改 internal/english/data/，要提交）
./build.sh uninstall
```

词库在本机从 rime-ice 现编，`build/lexicon.bin` 不进仓库。rime-ice 以 git
submodule 放在 `third_party/rime-ice`，腾讯精简子集
`third_party/tencent_lite.dict.yaml` 已随仓库提交，构建时自动带上。要换一份
rime-ice checkout 时覆盖 `RIME_ICE`：

```bash
RIME_ICE=/path/to/rime-ice ./build.sh install
```

## 按键

| 按键 | 行为 |
|---|---|
| `a`–`z` | 累积拼音 |
| `'` | 强制音节边界（`xi'an` ≠ `xian`） |
| 空格 | 上屏高亮候选 |
| `1`–`9` | 选当前页第 N 个候选 |
| 回车 | **无条件**上屏原始字母（`nihao` + 回车出 `nihao`）。URL、密码、英文单词的逃生舱；`Shift+回车` 同义 |
| `←` `→` | 移动高亮 |
| `↑` `↓` / `PageUp` `PageDown` | 翻页 |
| `-` `,` / `=` `.` | 翻页（组字中） |
| 单独轻敲 Shift | 中/英模式切换（延后 60ms，期间来了按键就取消）；组字中会把已打字母原样上屏（`go` 出 `go` 而不是 公） |
| `Shift` + 字母 | 开始一个英文单词（不切换模式）；组字中先上屏拼音 |
| 英文单词中 | 字母和 `'` 按字面接上（`Don't`），空格上屏首选 |
| Tab | 上屏高亮候选 |
| 标点 | 中文模式下转全角：`,`→`，`、`.`→`。`、`\`→`、`，引号自动配对 |
| 鼠标点候选 | 上屏，且不会把焦点从宿主 App 抢走 |
| Backspace | 退一个字母 |
| `Shift+Backspace` | 忘掉当前高亮候选（的学习记录） |
| Esc | 放弃 |
| 带 Cmd/Ctrl/Opt 的键 | 一律放行，绝不吞宿主的快捷键 |

`@ # $ % ^ & * _ + = / | ~` **不**转全角——它们在地址、代码、算式里出现得
比中文用法还多。

## 设置

菜单栏输入法图标 → **qui 拼音** → **候选词数量**，可选 5–8，勾选即生效，组字
中改也立刻重排当前页。这是输入法**唯一**的设置界面：输入法没有 Dock 图标，
进程被 `ActivationProhibited` 钉死为永不激活，自己的设置窗口没人能切到前台。

其余设置在 `~/Library/Application Support/QuiIME/prefs.json`，首次启动会写
一份带默认值的：

| 键 | 默认 | 说明 |
|---|---|---|
| `pageSize` | 5 | 每页候选数，菜单改的就是它 |
| `fuzzy` | 全关 | 模糊音（zh↔z、n↔l、ang↔an…） |
| `typoCorrection` | true | 相邻字母对调 / 漏键 / 键重复的纠正 |
| `chinesePunctuation` | true | ASCII 标点转全角 |
| `emojiInCandidates` | true | 主候选列表里在词旁给 emoji（`v` 模式不受影响） |

手改文件写了越界值也不会出事，会被夹回范围内并写回。

## 词库与授权

中文词库主体来自 [rime-ice](https://github.com/iDvel/rime-ice)，英文词表和
emoji 表来自同一个 checkout，另有 `third_party/tencent_lite.dict.yaml`
（rime-ice 的腾讯词库 LLM 精简子集，来自一个尚未合并的 PR），已随仓库提交，
构建时自动使用。

**rime-ice 是 GPL-3.0-only，其编译产物也继承该许可。** `lexicon.bin` 不进
git、在本机现编；但 `third_party/tencent_lite.dict.yaml`、
`internal/english/data/english.txt`、`internal/emoji/data/emoji.txt` 是已提交
的 GPL 数据。因此：

- 本仓库的**源代码**是 [MIT](LICENSE)；
- 任何**分发物**只要打包了上述词典数据（包括编译后的 `.app`），就是
  GPL-3.0-only 的组合作品。

完整清单与影响见 [`LICENSES.md`](LICENSES.md)。要发布纯 MIT 产物就得换掉
词库——引擎只认 `tools/dictc`、`tools/engc` 的输出格式，不依赖 rime-ice
本身。

## 安全输入使输入法变灰

输入法在菜单里带着图标出现但名字是灰的、点不动时，先排除是不是有 App 持有
**secure keyboard input**（密码框拿了 `EnableSecureEventInput` 后忘了配平是
Electron/WebView 的常见泄漏）：

```bash
ioreg -l -w 0 | grep -o 'kCGSSessionSecureInputPID"=[0-9]*'
ps -o pid,lstart,command -p <PID>      # 非 0 就是有人持有
```

secure input 生效期间，系统只信任装在 `/Library/Input Methods` 的输入法，
`~/Library/Input Methods` 的一律置灰。退出并重开那个 App 即可；根治是装到
系统域 `./build.sh install-system`（需要 sudo，且换域等于换注册位置，要再注销
一次）。

## 已知限制

- **模糊音默认关**且没有开关界面，改 `prefs.json` 的 `fuzzy`。
- **没有双拼。**
- **只有一个设置有界面**（候选词数量），其余只在 `prefs.json`。
- **符号表、`v` 开头的数字/单位（`v2`→②/二/贰）还没接**；`v` 入口已被
  emoji 开出来了。
- **候选不换行**，超宽的一页会被面板宽度上限截断。
- **引号配对状态是进程级的**，不是每个文本框一份。
- **整句只给一种切分**，但同一读法下会附带最多 3 条同音字变体（他/她/它 各
  占一格，选错一个字不用整句重打）。
- **没有语料 bigram。** 用户自己的搭配记忆只对打过的搭配生效，第一次打一句
  新话转移边上仍是常数。
- **中文模式下英文只做完全命中**，大写字母开的英文模式里才做前缀补全。
- **英文没有词频**，`the`、`and` 这类极高频短词只能靠保留格出现。
- **词表外的英文、拼错的英文、专有名词**仍会被硬拼成中文（`tsinghua` →
  铁丝+能够+话）。区分需要词间 n-gram，这里没有；逃生舱是 Shift 切英文。
- 数字键始终是选候选，所以英文模式下 `Web3`、`COVID19` 打不出来；`-` `,`
  `.` `=` 始终翻页，`well-known`、`e.g.` 同理。

## 开发

```bash
go test ./...                                            # 需要词库的用例缺词库时 skip
go run ./tools/qime-query -dict build/lexicon.bin nihao   # 在 shell 里跑引擎
go run ./tools/qime-audit -mode golden                    # 排序回归
```

`third_party/rime-ice` 固定在某个 revision；换版本可能改变排序。golden 里带
`lite` 的用例在没有 `tencent_lite` 的词库上会自动跳过。评测与工具说明见
[`docs/arch.md`](docs/arch.md#十测试与评测)。

## 许可

- 源代码：[MIT](LICENSE)。
- 词库、英文词表、emoji 表：来自 rime-ice，**GPL-3.0-only**。打包这些数据的
  分发物须按 GPL-3.0-only 发布。

详见 [`LICENSES.md`](LICENSES.md)。

## 参考

- <https://github.com/iDvel/rime-ice> —— 词库来源（GPL-3.0-only）
- <https://github.com/qizhanchan/qui> —— 窗口 / 面板 / 渲染
- <https://github.com/ensan-hcl/macOS_IMKitSample_2021> —— IMK 最小样本
