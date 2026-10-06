# 架构

本文记录 q-ime 的技术设计：进程模型、词库格式与编译、搜索与排序、
用户词典、上下文续写、emoji、面板与性能。面向维护者；安装和使用见
[`README.md`](../README.md)。

## 一、总览

q-ime 是 macOS 输入法（InputMethodKit / IMK）。IMK 那一层是
`bridge_darwin.{h,m}` 的薄转发，**所有决策都在 Go**：候选是什么、preedit
是什么、某个键要不要吞，全在 Go 里，这样不可测的 ObjC 半边尽量小。

```
main.go          入口：激活策略 → NewApp → 面板 → 引擎 → IMKServer → app.Run()
session.go       状态机：缓冲区、候选、翻页、上屏、标点、中/英模式、Shift 手势
panel.go         qui 覆盖面板：候选条、高亮、页码、屏幕边缘翻转、模式提示
engine.go        引擎接线：定位词库、mmap、定时保存用户词典、嵌入英文/emoji 表
prefs.go         ~/Library/Application Support/QuiIME/prefs.json
punctuation.go   ASCII → 全角标点（含引号配对）
bridge.go        cgo：Go ↔ ObjC 的导出/调用面
bridge_darwin.m  IMK 控制器、菜单、marked text、光标矩形
bridge_darwin.h  C 接口契约

internal/dict/    编译后词库的格式 + mmap 读取（音节 ID trie）
internal/pinyin/  声母表、模糊音、ü 归一、对调/漏键/重复的拼写变换
internal/engine/  格 → trie 搜索 → 打分 → Viterbi 整句 → 用户词典 → 上下文 → emoji
internal/english/ 英文完全命中表（go:embed，只读 blob + 二分索引）
internal/emoji/   词 → emoji 表（go:embed，同样只读 blob + 二分索引）
internal/golden/  排序回归评测集的解析与运行

tools/dictc/      离线：rime-ice YAML（+ -lite）→ lexicon.bin
tools/engc/       离线：rime-ice en_dicts → internal/english/data/english.txt
tools/emojic/     离线：rime-ice emoji-map.txt → internal/emoji/data/emoji.txt
tools/dictdump/   倒出 lexicon.bin 的全部条目，用于对比两次编译
tools/qime-query/ 在 shell 里跑引擎，不用安装输入法
tools/qime-audit/ 量纠错代价、覆盖度、golden 回归
```

**引擎完全不认识 macOS、IMK 或 qui。** `internal/engine` 的输入是 ASCII
字符串，输出是排好序的候选，可以在没有窗口、没有 text client、没有 .app
的情况下测试。

### 搜索流水线

```
raw text ──▶ lattice ──▶ trie walk ──▶ ranked candidates
             (输入可以被      (这些读法里
              读成哪些音节)     哪些真的存在词)
```

外加一次 Viterbi，把多个词拼成整句。`Candidates` 的步骤：

1. 单词：读输入前缀的词库词；
2. 预测（completions）：续一个音节、越过已输入部分的词；
3. 整句：Viterbi 覆盖全部输入，附最多 3 个同音字变体；
4. 英文完全命中；
5. 上下文：用户在上一个词后打过的词；
6. 造词：用户自己攒出来、词库没有的多音节词；
7. 排序后，给最佳词条挂 emoji；
8. 截断到 `maxCandidates`，截断时保住英文完全命中。

## 二、进程与线程模型

- **永不激活**：`main.go` 在 `NewApp` 之前调用
  `qui.SetActivationPolicy(ActivationProhibited)`。输入法一旦成为 active
  app，用户正在打字的输入框就掉焦点，之后每个键都无处可去。macOS 上已经
  激活过的进程无法悄悄取消激活，所以顺序是硬要求。
- **平台**：强制 `QUI_PLATFORM=cocoa`。只有 cocoa 后端能创建非激活的
  overlay 窗口，也只有它尊重激活策略。
- **运行循环**：用 `app.Run()` 而不是 `[NSApp run]`。两者都服务默认
  mode（IMK 的 mach port 需要），但只有 qui 的循环会绘制面板，因此不需要
  第二条线程或 display-link。
- **单例会话**：IMK 会给每个 text client 建一个 `IMKInputController`，但
  缓冲区、候选、面板必须全进程一份，否则在 A 框打一半切到 B 框，A 的
  preedit 会被孤悬。状态放在 `imeSession`，控制器保持无状态。
- **事件路径**：只实现 `IMKInputController` 三种事件途径中的第三种——
  `handleEvent:client:`（原始 NSEvent）。修饰键的按下/抬起只在原始事件里
  可见，而 Shift 切中英依赖它；同时它也能收到 key-down。
  `inputText:key:modifiers:` **故意不实现**：同时实现两种会让 IMK 只选一
  种（实测选了后者），于是 flags-changed 永远到不了 Go。特殊键按 keyCode
  分发，因为退格是 U+007F、方向键是私用区码点，用字符匹配不可靠。
  `commitComposition:` / `deactivateServer:` / `menu` 不属于这三种派发，
  照常收到。
- **client 生命周期**：正在处理的 `client` 只在一次回调内有效。鼠标点候选
  发生在回调之外，所以缓存 client 必须 retain：
  `qimeBridgeRetainClient` / `ReleaseClient`，所有赋值都走
  `imeSession.setClient` 保证配平。ARC 的 `__bridge` 不取引用。
- **marked text**：用 `markForStyle:atRange:` 构造带属性的字符串，而不是
  裸 `NSString`。裸串只有 default marking，不带 `NSMarkedClauseSegment`，
  自维护组字模型的客户端（Java/Swing、部分 IDE）读不到。style 用
  `kTSMHiliteSelectedRawText`（罗马字输入，不是转换结果），整条 preedit
  是一个 clause。`markForStyle:` 是控制器实例方法，而 Go 只拿得到 client，
  所以 ObjC 侧存一个「最近处理事件的 controller」的 weak 引用。
- **菜单**：输入法菜单由系统菜单栏 agent 在另一个进程绘制，点击回传的
  `sender` 是 infoDictionary，菜单项是其中的拷贝，键为
  `kIMKCommandMenuItemName`。**不能设 target**（指针过不了进程边界），也不
  走 responder chain；IMK 通过 `doCommandBySelector:commandDictionary:`
  在 controller 上 perform。**不能用子菜单**：嵌套菜单没有跨进程承诺，改成
  平铺 + `setIndentationLevel:`。`setAutoenablesItems:NO`，因为这个进程没有
  responder chain 可问。候选数量的上下界由 Go 通过
  `qimeBridgeGoPageSizeBounds` 提供，避免在两处各写一份。
- **调试日志**：launchd 启动的输入法没有可读 stderr，NSLog 的 Go 字符串
  在统一日志里会变成 `<decode: missing data>`，所以 Go 直接写
  `~/Library/Logs/QuiIME.log`（`LogPath`）。ObjC 侧也能经 `qimeBridgeGoLog`
  写进同一个文件。
- **C 符号**：前缀是 `qimeBridge`，不是 `quiIME`——后者已被 root qui 作为
  输入法**客户端**的桥占用，Go `//export` 在整个二进制里是全局 C 链接，会
  撞车。

### 注册与安装要点

- **bundle ID 必须含 `.inputmethod.` 作为中缀**（点号在两侧）。少了它，
  `TISRegisterInputSource` 返回 `noErr`，但输入源永远不会进 TIS 数据库，
  而且完全静默。
- **`tsInputMethodIconFileKey` 指向的文件必须真实存在**，否则同样静默失败，
  设置里看不到。
- **mode key 不是原样使用**：TIS 会剥掉 `com.<vendor>.inputmethod.` 前缀再接
  到 bundle ID 后面，所以 mode key 不要再带自己的 bundle ID。
- **首次安装要注销重登一次**：`TISRegisterInputSource` 只给瞬态条目，持久化
  注册发生在登录时；真正管注册的 `imklaunchagent` / `hiservices-xpcservice`
  受 SIP 保护，用户态踢不动。
- **重装前必须 `killall`**：旧进程占着 mach connection name。
- **secure event input 生效期间**，系统只信任装在 `/Library/Input Methods`
  的输入法，装在 `~/Library/Input Methods` 的一律置灰。

## 三、词库

### 来源与编译

主体来自 [rime-ice](https://github.com/iDvel/rime-ice)，外加一份腾讯精炼子集
（随仓库提交在 `third_party/tencent_lite.dict.yaml`）。rime-ice 以 git
submodule 放在 `third_party/rime-ice`（浅克隆），`tools/dictc` 把这些文件合并、
归一权重、编成一个二进制。

| 源 | 词条 | trust | 说明 |
|---|---|---|---|
| `8105` | 8.8k | 1.00 | 通用规范汉字表，有真实字频 |
| `41448` | 46k | 0.55 | 大字表，无权重 |
| `base` | 543k | 1.00 | 基础词库，有真实词频 |
| `ext` | 339k | 0.70 | 扩展词库，权重全 100 |
| `others` | 633 | 0.80 | |
| `tencent` | 981k | 0.42 | 腾讯词向量，无注音，权重全 100 |
| `tencent_lite` | 167k | 0.70 | 上一行的语义精炼子集，已提交 |

英文词表来自同一 checkout 的 `en_dicts/`，`tools/engc` 编成文本表，
`go:embed` 进二进制。emoji 表来自 `others/emoji-map.txt`，`tools/emojic`
反转成词→emoji。

**授权**：rime-ice 是 GPL-3.0-only，编译产物继承。`lexicon.bin` 不进 git，
`build.sh` 在本机现编；已提交的 `tencent_lite.dict.yaml` / `english.txt` /
`emoji.txt` 是 GPL 数据。完整说明见 [`LICENSES.md`](../LICENSES.md)。
完整说明见 [`LICENSES.md`](../LICENSES.md)。

### 权重对齐

各源权重量纲不同，其中两个没有量纲（ext、tencent 全是 100）。直接合并，
98 万条无词频的 tencent 会和最常用的词平起平坐。做法：

- 每个源**线性缩放**到共同区间。线性缩放在打分用的对数域里是常数偏移，
  于是源自身的频率比值原样保留——这些比值是最值钱的东西（我们 是 我么 的
  36 倍，正决定两者谁在前）。
- 完全没有区分度的源（缩放前后 `hi <= lo`）统一放到 `flatSourceLevel`
  （`0.002`），不给它并不具备的顺序。
- 每个源再乘 `trust`，表示发言权。

### tencent 的注音是推出来的

`tencent.dict.yaml` 覆盖广但无注音。和 Rime 一样从字表推：每字取最常见
读音；**任何一个字存在真正的多音歧义（次选读音占比超过 20%）就整词丢弃**
——注错音比缺词更糟，它让词出现在用户没打的读法下，同时在打了的读法下仍
找不到。实现见 `compiler.annotate`。

### tencent_lite 是质量标签，不是覆盖面

`tencent_lite.dict.yaml` 是 `tencent.dict.yaml` 的语义精炼子集，绝大部分与
它重合，带来的新词很少——当新词库合并没有意义。它的价值是指出 98 万条 tencent
里哪部分是真正的词。被它筛掉的多是网页切出来的句子碎片，留下的多为专名。

问题是 tencent 里两类权重完全一样（全是 100），文件本身无法区分。做法是
把 lite 当**独立源**排在 tencent **之后**编入：`Builder.Add` 的「同词取大
权重」把重叠部分**提档**到 `trust 0.70 × flatSourceLevel`（即 ext 的档），
于是真词在格中压得住同读音按字拼出来的句子。**只提不降**：lite 没收录的
约 81 万条仍保持原权重。刻意不给更高档——「大模型认为这是个词」不等于
「这个词常被打」，后者只有 base/8105 有。

### 为什么离线编成二进制并 mmap

输入法是常驻后台进程。188 万词条放 Go heap 上，GC 每轮都要扫，RSS 也不
像话。所以编成偏移寻址的扁平文件，运行时 `mmap`：字节不进 Go heap，区域
里没有指针，GC 完全不看它，操作系统按需换页；一次查询只为真正返回的候选
分配。

### 文件格式

全部小端；偏移从文件头起算。见 `internal/dict/format.go`。

```
Header     48 byte（magic "QIMEDIC1"、version、各段偏移、节点/词条数、参考权重）
Syllables  count u16，然后 count×(len u8, bytes)——升序，下标即 ID
Trie       节点：postOff u32, postCount u16, childCount u16,
           然后 childCount×(sylID u16, nodeOff u32)，按 sylID 升序
Postings   每条 10 byte：weight u32, wordOff u32, wordLen u16
           节点内按 weight 降序
Pool       原始 UTF-8 词字节
```

`dict.Builder` 离线构建，`dict.Reader` 运行期只读。`FormatVersion` 变更时
旧文件会被明确拒绝，而不是误读。

### 为什么是音节 ID 的 trie，不是拼音字符串的 map

map 能处理 `nihao`，但处理不了用户真正依赖的两个：**简拼**（`zhg` → 中国）
和**混拼**（`nhao` → 你好）。两者都是「第 i 个音节是任意以此开头的音节」，
是一个集合查询，精确索引答不了。

边缘是音节 ID 的 trie 能直接答。音节 ID **按字典序分配**，所以「所有 n 开
头的音节」是一段连续区间；节点子节点又按 ID 排序，于是该查询是一次二分加
一段顺序扫描（`PrefixRange` / `ChildrenInRange`）。模糊音不连续，当额外的
几个 ID 单独探。

## 四、搜索与排序

### 格（lattice）

`buildLattice` 不查词库，只按拼写把输入切成「词库可能要的读音」，四类弧：

- **完整音节**（`hao`），可能来自一次模糊替换；
- **声母缩写**（`h` 在 `nih` 里），任意位置可用，简拼由此而来；
- **半打的末尾音节前缀**（`ha` 在 `niha` 里），**只在输入末尾**——中间的
  前缀会把 `hao` 读成 `h`+`ao`，淹没候选；
- **拼错但可修的完整音节**：相邻对调（`pign`）、漏一键（`zhog`）、键重复
  （`guuo`），仅在该段本身不是合法音节时提供。

弧按「长段优先、精确优先于模糊优先于对调」加入。这个顺序是承重的：
`collect` 用 `(position, node)` 做 memo、首个到达的读法胜出，所以最便宜的
解释先被记录。用户打的撇号是强制边界，没有弧可以跨过它（`xi'an` 保持两个
音节）。

`canContinue` 丢掉死路读法：剩下的字母必须能不靠拼错假设开始一个音节。
`ni` 读成声母 `n` 剩 `i`，没有音节以 i 开头，这个读法永远成不了词，不该
作为候选出现。

### 罚分与常数

分数是对数概率：`ln P(词) + ln P(用户打的这串 | 该词的读音)`。下列常数都是
第二项的组成部分，单位自然对数。

| 常数 | 值 | 作用 |
|---|---|---|
| `coverBonus` | 6.5 /字节 | 解释掉用户打的内容 |
| `abbrevPenalty` | 1.6 | 一个音节只给了声母 |
| `partialPenalty` | 3.2 | 延长了用户已打完的音节 |
| `syllablePenalty` | 1.6 | 每多一个音节 |
| `fuzzyPenalty` | 1.1 | 每个模糊替换 |
| `typoPenalty` | 6.0 | 音节内一次相邻字母对调 |
| `completionPenalty` | 5.0 | 词超出已输入部分（预测） |
| `singletonPenalty` | 4.0 | 整句里只解释一个字节的链词 |
| `junctionPenalty` | 3.2 | 整句里每个词边界 |
| `englishBase` | −8.0 | 英文完全命中的分数 |
| `englishAltPenalty` | 0.4 | 同码上的第 2、3… 个英文词 |

几条结构约束：

- **`partialPenalty` 必须比 `abbrevPenalty` 陡。** an/ang、en/eng、in/ing
  里长音节那个通常更常用，但声母是用户说「你来补」，打完的音节是用户已经
  说清楚了，覆盖它属于臆测。
- **`syllablePenalty` 不能并进 `abbrevPenalty`。** 后者的上界是
  `coverBonus`：简拼只换来一个字节的覆盖，代价一旦超过这个字节值的分，
  简拼就永远不划算，`zhg` 再也到不了中国。
- **`coverBonus` 必须够大。** 全部解释掉用户打的内容要能赢过更常用的残缺
  读法（`zhg`→中国 对 `zh`→这）。同时它是唯一能调的旋钮：两个**覆盖相同**
  的候选之间覆盖项精确抵消（英文对中文、纠错读法对正确读法），所以调它
  只影响「该不该解释掉剩下的字母」。
- **`junctionPenalty`：** 你好 是词库词，也能由更常用的 你+好 拼出，真正的
  词必须赢。

### 整句是 Viterbi

`best[i]` = `input[:i]` 的最佳读法，每条转移是一个词典词，按单独出现的分数
减一个 junction 罚分。词库没有 `womenmingtianqubeijing`，它是
我们+明天+去+北京。

关键：分数用 **ln(概率)** 而非 ln(词频计数)。用计数时每个词都是正贡献，
切得越碎分越高；改成概率后每个词都是负贡献，多一个词多一分代价。词库头里
因此存了一个参考权重，读侧和编译侧用同一尺度还原概率。

整句**不查用户词典**：boost 是按词累加的，五字句能收五份，把一切淹没；而且
整句只取每个跨度权重最高的词，boost 也改不了选词，只能改这种切法看起来多
划算。只给最优切分，外加最多 3 个**同音字变体**（每条只换链上一个词位），
换一种切分仍然不给。

### 预测

`completions` 越过已输入部分多走一个音节。每个节点只展开
`maxCompletionExpansions = 12` 个孩子，**按孩子最优词条权重取前 12**，而不是
按音节字母序取前 12——否则一个续写能不能被预测到取决于它下一个音节排第几
（`ni` 会给出 你啊/你把，而 你们/你好 永远进不来）。每个孩子最多取 4 条词条。
扫描每个孩子只读最优词条权重（两次 mmap 读、零分配）。

**预测不得压过用户实际拼出的词**。这不只靠 `completionPenalty`（常数挡不住
语料权重倾斜，如「版权所有」样板文字抬高的 版权），还加了一条结构保证：没有
用户 boost 的预测，分数封顶在「最佳全覆盖已拼候选」之下，仍然给出但没资格抢
空格上屏那一格。用户 boost 的预测不受限（`真`+`b`→不错 是文档化例外）。

### 英文完全命中

中文模式下打 `hello`，若词表里正好有这个词，把英文也作为候选。英文命中是
最干净的解析：每个字母都有交代，无简拼、无延长、无模糊、无词接缝，所以其
分数就是 `coverBonus × 长度` 加一个常数。

当它和**同一串字母**的中文读法比较时，覆盖项两边一样、直接抵消，于是
`englishBase` 是在和「ln P(词) − 硬凑了多少」比较。干净中文读法落在 −2 ~ −6，
硬凑的落在 −13 附近，`englishBase = −8.0` 卡在中间：**中文必须硬凑时英文才
赢**。这也是 `hen` `ban` `song` 等仍是中文优先的原因。

「至少要看得见」是两道保险：`truncateKeepingEnglish`（引擎）在截到 60 条时
不把英文截掉；`reserveEnglishSlot`（会话）若英文没靠分数进第一页，就放到第一
页最后一格。两处都只让出最后一格，绝不碰第 0 格（空格提交的那一格）。

### 纠错

音节段内试每个相邻字母对调、漏一键、键重复，合法音节就加一条罚
`typoPenalty` 的弧。**承重的是守卫：这一段本身已是合法音节时，一条纠错弧都
不加。** 打完的音节是用户已说清楚的，重读属于臆测。

- 漏键：对字母表里每个 ≥3 字母的音节枚举「去掉一个字母」的拼法建
  `delIndex`，格构建时一次 map 查询。指向太多音节的键整个不收
  （`maxDeletionRepairs = 3`）：含糊恰好和昂贵成正比。
- 键重复：只折叠**连按重复**（`guuo`→guo），**故意不做任意多余字母**——后者
  会把尾部简拼当手滑解释掉。
- 一次读法只允许一滑（`maxTypos = 1`），跨种类也只允许一滑。整句的额度按
  `(位置, 已用次数)` 索引，否则每个词各拿一份额度。当格里没有纠错弧时，
  lanes 收成 1，整句开销与没有此功能时相同。
- 纯纠错弧不算活路（`canContinue`），否则「剩下的字母全按打错了算」会复活
  死读法。

默认开，和模糊音不同：模糊音会把打对输入也变宽，纠错弧只在「这几个字母压根
不成音节」时才存在。

### 用户词典的 boost 只在完全覆盖时生效

boost 的记录形式是「对读音 R，这个用户要词 W」，不是「现在要 W」。若一个
候选只解释了输入的一半（`sz` 里的 `s` 读成 shi），却拿「解释全部时积累的
历史」来支撑，就会出现 是 压过 深圳。

**全有或全无**，不按覆盖率打折：全简拼输入里多解释一个字节只值约 1.8 分，
而 boost 随选择次数无上界增长，任何固定折扣都能被某个次数吃穿。代价是
`nihaoo` 里 拟好 只覆盖 5/6 就失去 boost；接受，因为另一头是对用户打得最多
的词悄悄失效。

## 五、用户词典

### 四个命名空间，一个 map、一个文件

| 键 | 问的问题 | 用在哪 |
|---|---|---|
| `读音\0词` | 这串音你要哪个词 | 候选排序 |
| `\x01字母\0词` | 你打这串字母想打出什么 | 英文/品牌/术语 |
| `\x02上词\0读音\0词` | 这个词后面你接什么 | 上下文 |
| `\x03读音\0词` | 你自己攒出来的词 | 产出词库没有的词 |

都记 次数 + 最近时间。频次取对数，最近度是半衰期两周的衰减位。键里带读音，
所以「行 háng」学到的东西不影响「行 xíng」。

**为什么字面词要独立命名空间**：读音键只能给词库已有的词**排序**；品牌、
术语需要能**产出**词库根本没有的词，所以另开一格，中文模式下按完全命中给
（`aftership` 也出 `AfterShip`），并且只给「拼写和打的不一样」的。

**为什么造词要独立命名空间**：读音条目不能信。真实历史里 754 条多音节读音
条目中有 49 条是词库没有的词，全是整句误提交。它们被记下来但只要没人能查就
无害地躺着；若去读读音命名空间来产出候选，这 49 条会变成压倒一切的全覆盖
候选。`\x03` 只由「每一段都是明确选择」的造词写。

### 只有「选中」写词典，「冲刷」不写

能把文字送进文档的路径中，只有一部分是选择：空格/回车/Tab、数字键/点面板、
Shift 轻敲是选择；打大写字母、打标点、任何其它字面键、焦点离开是冲刷。
分界线是「这个键的全部意思就是提交」还是「组合串挡了别的事的路」。

**为什么必须分开：排序会自我训练。** 空格提交的是高亮那个，没动过选择时
就是第 1 个。若首选被被动接受也加分，下次更是首选，错误默认越陷越深。冲刷
仍照常更新上下文（`lastWord` / `recent`），因为那些字确实在屏幕上；不写的
只是「这是你的偏好」这个断言。

### 存储：快照 + 追加日志

`user.json` 是快照，旁边的 `user.json.journal` 是 JSON lines，每行一个键改动
后的**完整状态**（不是增量）。启动时快照 + 重放；只有日志超过 512KB、或发生
淘汰/清空这种大面积改动，才重写快照并删日志。记录是绝对状态，重放两遍无
害，所以「新快照已 rename、日志还没删」时崩了也不会错。文件坏了就当空的
重来——丢掉学习记录远好过输入法起不来。

每 20 秒落盘一次（不是每次上屏——那是键盘路径；也不是只在退出时——输入法
是被 kill 的）。条目上限 20000，超了淘汰**挣分最低**的（对数频次 + 衰减后
的最近度），不是按最后使用时间：后者会先扔掉三个月前选过五十次的词、留下
两个月前误选一次的。

## 六、上下文续写

### 第三个命名空间：用户 bigram

Viterbi 一直在，缺的是转移边上的数。这一格把 `junctionPenalty` 从常数变成
用户自己的搭配记忆，键是 `\x02上一个词\x00读音\x00词`。**为什么键里存读音**：
格里没有那条弧，排序救不了——`真` 再打 `b` 要一个双音节词，搜索根本产不出
它。

**故意不进整句**，理由和用户词典一样（boost 会在链上累加）。匹配对输入是
严格的：每个字母都得由该词读音解释（整音节或音节开头），所以多打一个字母
预测就消失。分数用同一套常数，`bigramBase = 3.0` 是**锚出来的**：一条新记录
的 boost ≈ 5.7，刚好压过 `completionPenalty = 5.0`，即只有真打过的搭配才够格
越过这道罚分。

上下文在句末标点和焦点离开时清空；逗号不清（「我们，明天」是一句话）；
`reset()` 不清（它在每次上屏结束时跑，而上屏结束正是这份记忆开始有用的
时刻）。

### 词库短语的冷启动续写

提交一个中文词时，会话同时保留它的读音；下一次开始输入后，把「上词读音 +
当前字母」作为一条完整读音再查一次，只接受文字和读音都以前一个已提交词开头
的结果，剥掉已上屏前缀，只把后缀放进候选。

已提交读音**带音节边界**去查，且直接**从其在 trie 中的节点开始搜**新输入
（不需要每次从头重搜整条长读音），实测比旧实现快约 29 倍。续写分数是**边际
分数**：

```
marginal = coverBonus·len(新输入) − 后缀新增的惩罚 + lnP(短语) − lnP(已提交词)
```

覆盖项自己抵消，不需要新常数；最后一项是直接来自词库的转移概率。基线
**故意从词库权重算，不从候选列表读**（候选带用户 boost，用它会导致越常单独
选某个词、引擎越不愿意把它续成短语）。用户真正选过一次以后，bigram 学习
接管排序。

`lastWord` 是 bigram 的键，必须严格是一个已提交词；短语查询另有一个
`recent`（最近 3 段已提交文字 + 读音），因为「博物」当一个词提交和分两次提交
在文档里是同样三个字，只有前者能从 `lastWord` 到博物馆。失焦忘掉上文是刻意
的。

## 七、emoji

`internal/emoji` 是词 → emoji 的映射，按**词**挂而不是按拼音查。emoji 不是
任何东西的读音，是已有候选身上的装饰，所以它挂在**排完序的候选列表**上：
简拼、模糊音、纠错、用户词典、上下文全都自动生效。

- 主候选列表里，emoji 紧挨着它修饰的那个词，**不占保留格**——图站在词身上，
  词不在第一页它更没道理在。默认永不占首选，但选过就能（查
  `Boost(词的读音, emoji)` 是否够高）。
- 只在完全覆盖时出现，和用户词典、上下文同一条规矩。
- 一个词多个 emoji：主列表只给第一个，`v` 模式全给。
- `v` 前缀是显式模式：剥掉 v → 跑普通查询 → 只留有 emoji 的。打头的 v 今天
  返回零候选，且 v 只在声母后当 ü，所以安全。开关是 `prefs.json` 的
  `emojiInCandidates`；关掉只是不往主列表掺，`v` 模式照常。

## 八、面板

- 深色卡片而非主题派生：窗口浮在他人的文档上，必须在任何 app、任何壁纸下
  都读作 overlay。
- **尺寸按内容收缩**：窗口接收整个 frame 范围的鼠标事件，透明像素也算，所以
  没盖住的宽度就是从用户文档偷走的点击区。`fitToContent` 量 widget 树后
  `SetSize`（ObjC 侧钉住顶边，避免每多一行就漂走）。
- 面板宽度是**正确性**而不是审美，`panel_measure_test.go` 直接量并设上限。
- 点击候选在 mouse **up** 且 down 也落在同一 chip 时才提交，避免从别处拖过来
  的点击误上屏。面板不抢焦点。
- preedit 显示**打了什么**（`Candidate.Spans` 决定在哪插分隔符），不显示引擎
  怎么理解；纠错时面板额外显示 `pign → ping`，宿主文档里的 marked text 仍然
  只有打了的字母。
- 模式提示复用同一个面板（`EN` / `中 CN` / `英 EN`），900ms 后消失。

## 九、性能

- worst case 约 1.2ms（`zhg` 这类全简拼扇出最宽的，以及 49 字整句），常见
  20–500µs，`nihao` 约 22µs，远在一帧预算内。
- 整句从 15.6ms 降到 1.2ms 的三处：读音按 trie 节点缓存（节点到根的路径唯一，
  读音是节点的纯函数）、状态集合复用、整句里单个词音节数封顶。
- **头几次查询会慢一个数量级**（`zhg` 冷 33ms / 热 1.2ms）——那是 mmap 首次
  触页的缺页中断，每页一次，正是 mmap 想要的形状。
- 搜索有 60000 个 trie 状态的硬预算：按住一个键不能变成宿主 App 卡住。慢的
  输入法就是坏的输入法。
- 英文查询是 22803 条上的二分，每次按键约 4µs。表是 `go:embed` 的只读字符串，
  索引是无指针的 `[]int32`，不会进堆、不被 GC 扫。换成 `map[string][]string`
  会给跑几周的进程添 4.5 万个字符串头。

## 十、测试与评测

```bash
go test ./...                      # 单元测试；需要词库的测试在缺词库时 skip
```

- 引擎测试**不用真词库**，用手搭词典，测搜索的性质而不是某个词今天的语料
  频率。权重特意设计成「错的那个靠词频赢」，规则一调弱测试就红。
- **`testdata/golden.tsv`** 是排序回归评测集：每行钉住一个期望（首选，或带
  `has` 的「面板 60 条里必须有」）。带 `lite` 列的行只在词库带
  `tencent_lite` 时检查，否则自动跳过（`golden.HasTencentLite` 探测），所以
  没有该源的构建仍然全绿。两个入口跑同一文件：

  ```bash
  go run ./tools/qime-audit -mode golden    # 手工，出错退出码非零
  go test . -run TestGoldenRankings         # 有词库的机器上自动跑
  ```

  追加为主；一个案例抓到过一次回归就永久留着。期望在出厂默认下量取，并且
  **与 `third_party/rime-ice` 固定的 revision 绑定**：换 rime-ice 版本可能改变
  排序，需要重新量取并有意地更新这个文件，而不是让它悄悄红。
- **`tools/qime-query`** 在 shell 里跑引擎：

  ```bash
  go build -o /tmp/qime-query ./tools/qime-query
  /tmp/qime-query -dict build/lexicon.bin -n 8 -bench 50 nihao zhg womenmingtianqubeijing
  /tmp/qime-query -dict build/lexicon.bin -user real -context 真 -n 6 b
  ```

- **`tools/qime-audit`** 量纠错代价、覆盖度、golden：

  ```bash
  go build -o /tmp/qime-audit ./tools/qime-audit
  /tmp/qime-audit -dict build/lexicon.bin                 # -mode typo，默认
  /tmp/qime-audit -dict build/lexicon.bin -edit delete
  /tmp/qime-audit -dict build/lexicon.bin -mode coverage
  /tmp/qime-audit -dict build/lexicon.bin -mode golden
  ```

  HARM 必须为 0（打对的输入首选不许变）；RECOVERY 分「音节内」和「跨边界」
  报，后者按构造救不了。

## 十一、已知限制

- 模糊音默认关且无界面，改 `prefs.json` 的 `fuzzy`。
- 没有双拼；方案映射是纯数据，`internal/pinyin` 加一层即可。
- 只有一个设置有界面（候选词数量在输入法菜单）；其余只在 `prefs.json`。
- 符号表、`v` 开头的数字/单位未接；`v` 入口已被 emoji 开出来。
- 候选不换行，超宽的一页被面板宽度上限截断。
- 引号配对状态是进程级的，不是每个文本框一份。
- 整句只给最优切分 + 同音字变体，不给第二种切分。
- 没有语料 bigram；用户侧搭配记忆只对打过的搭配生效，新句子第一次仍是常数。
- 中文模式下英文只做完全命中，不做前缀补全。
- 英文没有词频，同长度比较时覆盖项抵消，`the`/`and` 这类短词只能靠保留格
  出现。
- `tsinghua` 这类词表外英文、拼错的英文、专有名词，仍会被硬拼成中文；区分
  需要词间 n-gram，这里没有，逃生舱是 Shift 切英文。
