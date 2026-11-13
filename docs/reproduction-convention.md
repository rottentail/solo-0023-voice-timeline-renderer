# 制作复现约定（Voice Timeline Renderer）

本文是**可演练的**交付/复现约定，不是建议清单。它规定：

1. 交付给第三方的**最小输入证据**是什么；
2. 哪些结论可以**比较字节**，哪些只能**比较信号**；
3. 当另一台机器上的成品与历史成品不同时，如何用分层证据**定位偏离发生在哪一层**；
4. 历史回执与重新制作的结果如何被**明确保留为两件东西**；
5. 服务运行行为不受本约定影响（本约定只增加旁路工具，不改变 HTTP/渲染语义）。

> 演练入口：`go run ./cmd/reprocheck -out ./var/repro-drill`
> 第三方校验入口：`go run ./cmd/reproverify -action verify|audit|fingerprint ...`

---

## 0. 为什么"相同 source_id"不够

`source_id` 只是一个名字。跨机器复现同一成品，至少有五类差异会混在一起：

| 差异来源 | 例子 | 只看 source_id 能否发现 |
|---|---|---|
| 源字节 | 同一段录音被重新导出，尾巴静音不同 | 否 |
| 采样网格 | 源 44.1 kHz、工程 48 kHz，重采样实现不同 | 否 |
| 声道布局 | 单声道上混立体声的实现不同 | 否 |
| 处理工具链 | 重采样算法、增益顺序、交叉淡化权重、归一化次序 | 否 |
| 容器/量化 | WAV 头字段、PCM 量化舍入 | 否 |

因此本约定以**内容摘要（SHA-256）+ 分层中间摘要 + 工具链指纹**作为证据主体，
`source_id` 只作为人读标签。

---

## 1. 最小输入证据：Replayable bundle

一个第三方可复算的最小交付物是一个**目录**，自描述、只读、不可覆盖：

```text
<bundle>/
  manifest.json                # 索引 + 每文件摘要 + 内容摘要 + provenance
  timeline.json                # 历史时间线原样字节
  receipt.json                 # 历史回执原样字节（不得被重写）
  output.wav                   # 历史成品原样字节
  job.json                     # 历史 render job（可选但强烈建议）
  trace.json                   # 生产者分层摘要（可选，缺失会限制定位能力）
  sources/<source_id>/source.wav
  sources/<source_id>/metadata.json
```

`manifest.json` 的 `files[]` 列出除 manifest 自身外的每个文件的路径、大小、SHA-256；
`content_digest` 是对"去掉 `content_digest` 字段后的 manifest"规范化序列化的 SHA-256。
`LoadBundle` 会先校验 manifest 自身摘要，再逐文件校验内容，任何一处不符即拒绝使用。

**最小性**：以下四项缺一不可，缺任意一项就不是 replayable：

1. 精确的源 WAV 字节（`source.wav`，且摘要必须等于 `metadata.json` 里记录的 `digest`）；
2. 时间线原样 JSON（含 `spans`、`sample_rate`、`output_channels`、`target_peak_dbfs`）；
3. 历史回执原样字节；
4. 声明过的运行时边界（`max_source_bytes`、`max_spans`、`max_output_frames`）与工具链指纹。
   `max_output_frames` 尤其重要：同一个时间线在更紧的上限下会被直接拒绝，而不是"渲染得不一样"。

**不需要**把原运行目录整体复制。bundle 只包含上述证据；原数据目录里的临时文件、
其它 timeline、其它 render、索引文件都不属于证据。第三方在**自己的**目录里解包和运算，
不读取生产者的路径。

### 1.1 Attested bundle：字节不能共享时的最小替代

当源字节或成品不能离开生产方时，交付 **attested** bundle：

- `manifest.json` + `timeline.json` + `receipt.json`，`evidence_level: "attested"`；
- `sources[]` 只保留 id / 摘要 / 采样参数 / 峰值 / RMS，`media_file` 与 `metadata_file` 为空；
- 不带 `output.wav`。

它的用途只有"核对声明是否内部一致"（见 §3.3）。**任何工具都不得从 attested bundle
声称复现**：`Verify` 对它固定返回 `attested-only-not-reproduced`。

### 1.2 工具链指纹

`repro.CurrentToolchain(...)` 记录：

- `algorithm_version` 与 `algorithm_digest`：对**处理契约**（解码→线性重采样→声道映射→
  增益→等功率交叉淡化→峰值归一化→WAV 编码→量化舍入）的描述文本做 SHA-256。
  任何会改变输出采样的实现改动都必须提升 `algorithm_version`。
- `go_version` / `goos` / `goarch` / `module` / `vcs_revision`：仅供参考与排查，
  **不单独构成字节可比依据**（同一版本可以重新构建出不同二进制）。

`algorithm_digest` 相等是"这条处理链可字节比较"的机器无关前提。

### 1.3 Provenance 与血缘

`provenance` 记录 `producer`、`captured_by`、`source_of_truth`（`live-render` /
`foreign-toolchain` / `faulted-trace` / `attested-claims-only` …）、`lineage`
（父 bundle 的 `content_digest`）和说明。重新制作的产物写入**另一个目录**，
其 provenance 必须带 `lineage: parent-bundle:<digest>`，永不改写历史 bundle。

---

## 2. 什么可以比较字节，什么只能比较信号

证据分两层，**升级不可越级**：

| 层 | 内容 | 结论级别 |
|---|---|---|
| 字节层 | 源 WAV 字节、manifest/回执原样字节、成品 WAV 字节 | 可以比较**字节相等/不等** |
| 信号层 | 解码后的 float32 样本、分层中间样本解码摘要、peak/RMS | 只能比较**信号**，加上明确容差 |

规则：

- **可以**说"成品字节与历史一致"：`output.wav` 的 SHA-256 与历史相同，
  且 bundle 完整性校验通过。结论 `byte-identical`。
- **可以**说"解码样本与历史一致"：几何一致（帧数/声道/采样率），且逐样本最大绝对差为 0、
  容器或量化字节可能不同。结论 `sample-identical`。
- **可以**说"信号在容差内等价"：几何一致，且
  逐样本最大绝对差 ≤ 1 LSB（`1/32768`），RMS 差 ≤ 0.05 dB。结论 `signal-equivalent-within-tolerance`。
- **不可以**把"归一化后峰值相同"当作等价。峰值只是一个标量；容差内的峰值可能掩盖
  波形整体改变。工具会强制输出一条 `peak-equality-is-not-equivalence` 检查。
  演练里把波形做 tanh 压缩并保持峰值，得到 `peak_delta=0.0000 dB` 但
  `rms_delta≈1.34 dB`、`max_sample_delta≈0.096`，判定 `signal_equal=false`。
- **不可以**在 `algorithm_digest` 不同时给出任何字节结论。此时只允许
  `incomparable`，并另行给出解码信号比较（如果几何一致）。
- **不可以**用 `source_id` 或 `requested_peak_dbfs` 相等来推断成品等价。

`trace.json` 里的中间摘要注意两点：

- 源字节与编码输出摘要属于**字节层**；
- 解码、切片、重采样、声道映射、增益、混音、归一化摘要属于**信号层**（float32 位模式）。
  它们相等说明"这台机器上浮点结果逐位相同"，不自动等于"另一台机器也逐位相同"——
  跨架构浮点差异只能靠解码成品比较。

---

## 3. 定位偏离发生在哪一层

### 3.1 层序列（严格按执行顺序）

| 顺序 | 层 id | 隔离的变换 | 凭证 |
|---|---|---|---|
| 1 | `manifest-integrity` | bundle 自身完整性 | 字节 |
| 2 | `replay-inputs` | 本次复算实际使用了哪套输入 | 字节 |
| 3 | `source-bytes` | 源 WAV 字节 | 字节 |
| 4 | `decode` | RIFF/PCM 解析为 float | 信号 |
| 5 | `selection` | 半开区间切片、目标采样率 | 信号 |
| 6 | `resample` | 线性插值重采样 | 信号 |
| 7 | `channel-map` | 单→双 / 双→单 | 信号 |
| 8 | `placement` | 输出起点、帧数、交叉淡化长度 | 信号 |
| 9 | `gain` | 每 span 增益后的样本 | 信号 |
| 10 | `mix` | 等功率淡化 + 叠加 | 信号 |
| 11 | `normalization` | 峰值归一化 | 信号 |
| 12 | `encoding` | WAV 头与 PCM16 量化 | 字节（成品摘要） |
| 13 | `output-bytes` | 历史成品 vs 复算成品 | 字节，或信号 |
| 14 | `receipt` | 历史回执 vs 复算回执 | 字节字段比较 |

`earliest_divergence_layer` = 第一个状态非 `match` 的层。
`root_cause_layer` = 第一个状态严格为 `differ` 的层（早于它的 `inconclusive` 视为
"还没确认"）。当 `algorithm_digest` 不同时，信号层一律 `inconclusive`，
因此根本原因不会被误报。

### 3.2 三种必需的定位演示（由 `reprocheck` 产出）

演练工程本身就是一个混合采样率 + 立体声工程：

- 源 A：44.1 kHz / 单声道 / 26 460 帧；
- 源 B：48 kHz / 立体声 / 24 000 帧；
- 时间线：48 kHz / 双声道输出，`target_peak_dbfs=-1`，span 0 增益 -3 dB，
  span 1 增益 +0.5 dB、交叉淡化 4 800 帧；
- 成品：48 000 帧 / 2 声道 / 峰值约 -1.000 dBFS。

对同一份历史 bundle：

| 演练场景 | 预期定位 | 实测 |
|---|---|---|
| 原样复算 | `earliest=""`，`byte-identical` | ✅ |
| 源字节整体 +3 LSB（几何不变） | `earliest=source-bytes` | ✅ |
| span 0 增益 +0.5 dB（源不变） | `earliest=gain` | ✅ |
| 历史 trace 由"最近邻重采样"实现记录（输入与工具链声明不变） | `earliest=resample` | ✅ |
| 有真实工具链差异且如实声明 | 信号层 `inconclusive`，`incomparable` | ✅ |
| 历史 trace 被 withhold | 成品可见差异，但 `earliest=""` | ✅ |

每个场景的完整层表都在 `var/repro-drill/drill.json` 与对应 report 中。

### 3.3 没有 trace 时能说什么

- 只有 `output.wav` + 回执 + 源字节，没有 `trace.json`：
  可以判定成品是否一致/是否在容差内，**不能**说出是哪一层。
  工具会给出 `signal-divergent` 且 `earliest=""`，并在 `notes` 里明确记录原因。
- 只有 attested bundle：只能说"声明内部一致/不一致"，结论固定
  `attested-only-not-reproduced`。`AuditBundle` 会检查：回执可解析、回执摘要与文件一致、
  回执 span 的 `source_digest` 与 manifest 一致、成品摘要/字节数/帧数/布局/峰值与回执一致、
  归一化目标达成、job 与回执描述同一成品。

### 3.4 输入替换必须显式标注

`ReplayInputs` 允许第三方用自己的源字节/时间线复算，但**必须**设置 `Label`；
`verify` 报告里的 `replay_inputs` 会写 `substituted-*`，`replay-inputs` 层记录实际输入。
替换源字节时，源元数据会按新字节**重新推导**（摘要、帧数、峰值、RMS），
不允许沿用过时元数据。替换的时间线会用**与线上写入相同的** `domain.ValidateTimeline`
校验（含 `max_output_frames`）；不合法时直接判 `incomparable` 并说明原因。

---

## 4. 历史回执与重新制作的差异必须保留

- bundle 内 `receipt.json`、`output.wav` 是**历史原物**，只读。`Verify` 不写 bundle 目录；
  `ProveNoBundleMutation` 在复算后重新加载 bundle 比较 `content_digest`。
- 重新制作只能写入 `-recreated-dir` 指定的**新目录**（目标必须不存在），
  产物是新的 `output.wav` + `receipt.json`。演练验证：
  `bundle_unchanged=true`、`receipt_unchanged=true`。
- 回执比较逐字段列出差异，并标记 `volatile`。只有 `created_at` 属于 volatile：
  新成品的 `render_id`、`output.digest`、`output.byte_count`、峰值/RMS 等
  **一律是语义差异**，即使重制是合法的，也不得被"折算成相同"。
  报告同时给出 `historical_receipt_digest` 与 `recreated_receipt_digest`，两者并存。

一个典型原样重算的回执差异应只有：

```text
created_at  <历史时间>  ->  <复算时间>   volatile=true
```

其余字段必须全部相等；否则 `receipt` 层为 `differ`，`earliest` 之前若有层不同，
按 §3.1 的规则定位。

---

## 5. 服务行为不改变

本约定只新增旁路工具，不修改服务语义：

- `internal/render` 新增 `RenderWithTrace` 与一个可选 `*Trace` 形参；
  `Render(...)` 的签名与实现语义保持不变，内部转调 `RenderWithTrace(..., nil)`；
  trace 为 nil 时不做任何额外哈希或分配。
  **字节中性已演练验证**：同一输入在改动前后产出的 WAV/回执摘要完全相同。
- 未改动 `httpapi`、`service`、`storage`、`domain` 的任何已声明契约；
  `checkflow` 的三条既有 workflow 不变。
- 生产环境不自动挂 trace；需要分层证据时由捕获方显式开启（`cmd/reprocheck` / 未来接入方）。

---

## 6. 复现演练与第三方操作

```bash
# 1) 完整演练（生成历史 bundle + 10 条断言 + drill.json）
go run ./cmd/reprocheck -out ./var/repro-drill

# 2) 只拿到 bundle 的第三方：内部一致性审计（不依赖处理链）
go run ./cmd/reproverify -action audit -bundle ./var/repro-drill/history/render-01

# 3) 第三方复算并给出分层报告
go run ./cmd/reproverify -action verify -bundle ./var/repro-drill/history/render-01 -report report.json

# 4) 用自己的源字节复算（必须显式替换，产出新目录，不动历史）
go run ./cmd/reproverify -action verify \
  -bundle ./var/repro-drill/history/render-01 \
  -source studio_voice_a=./local/studio_voice_a.wav \
  -write-recreated -recreated-dir ./var/local-recreated

# 5) 声明本机处理链指纹
go run ./cmd/reproverify -action fingerprint
```

退出码：`0` = 一致/可通过；`1` = `audit` 不通过或 `verify` 为 `signal-divergent`；
`2` = 用法/IO/解码错误。

---

## 7. 约定自检清单（改动前必须重跑）

- [ ] `go build ./...` 与 `go vet ./...` 通过。
- [ ] `go run ./cmd/reprocheck -out <新目录>` 全部断言 PASS。
- [ ] 任何影响输出采样的改动都提升了 `repro.AlgorithmVersion`；
      否则跨机器复算仍会被判 `byte-identical`，这是**错误**的自信。
- [ ] `Render` 的字节中性检查：用固定输入对比改动前后成品摘要。
- [ ] 历史 bundle 未被任何验证流程写入（`content_digest` 与回执摘要不变）。
- [ ] 回执差异只允许 `created_at` 为 volatile。

---

## 8. 术语

- **byte-comparable**：同一条 `algorithm_digest`，且证据为字节层，允许"字节相等"结论。
- **signal-equivalent**：几何一致且落在 §2 容差内，只允许"信号等价"结论。
- **incomparable**：处理链契约不同或输入被替换但未声明，不给出字节结论。
- **attested**：只有声明，没有可复算字节证据；永不声称复现。
- **earliest_divergence_layer / root_cause_layer**：见 §3.1。
- **volatile receipt field**：唯一 `created_at`；其余差异均视为语义差异。
