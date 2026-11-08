# v1.2 同机性能复核复现

这是实验工具说明，不是完成报告。完整实验必须等最终候选的 24 小时长跑结束后运行，同机串行测量，关闭其他压力实验。普通 CI 只运行统计/fixture 单元测试，不设共享环境下的毫秒耗时门槛。

在仓库根目录的 PowerShell 7 中执行以下一条命令；将候选路径替换为实际冻结的最终候选，不能用旧候选代替：

```powershell
node scripts/performance-v12.cjs .cache/pulse-v11-upgrade.exe .cache/final-candidate.exe
```

默认输出一个全新的 `.cache/performance-v12-<时间戳>` 目录。可用 `--out=.cache/新的目录名` 指定；已有目录、项目 `.cache` 外路径及符号链接祖先均拒绝。数据库和源码快照在专用系统临时目录中创建，正常结束、失败和可处理的中断会停止专属子进程并清理；不会读取或覆盖 `data`、用户数据库、旧二进制或已有证据。强制结束 Node、系统崩溃等无法执行清理的中断只能算未完成。

2026-11-08 的受控预检使用两版各 2 个目标、并发 1、各 1 轮，存储各 2×30 条，没有运行完整对照；预检 3.33 秒。相同测试 helper 在干净 v1.1 与当前源快照上分别编译耗时 2.28 / 2.50 秒。单次候选查询计划 fixture 检查 0.85 秒，另仅执行 1 次分类查询用于验证可执行性。由固定 50ms 工作量推算，完整矩阵纯上游等待约 43.8 秒，另加 36 次启动/停止、RSS 采样、存储 fixture、基准与编译，当前机器预计约 1–4 分钟；这只是基于预检的估计，缓存、后台负载和实际退化会延长时间。每轮调度 90 秒上限，API 10 秒上限，Go 测试 180 秒上限。

快速预检命令：

```powershell
node scripts/performance-v12.cjs .cache/pulse-v11-upgrade.exe .cache/final-candidate.exe --preflight --skip-go
```

预检结果明确标为 `preflight_completed`，不能作为完整矩阵或发布性能证据。`--skip-go` 会缺少分配证据，只用于受控预检。

## 调度矩阵口径

v1.1 先运行全部实验，再运行候选；每版为 10/100 目标 × 并发 1/4/16 × 独立 3 轮，各 18 轮，每轮独立数据库。固定回环上游响应 50ms，不复用上一轮记录。先创建暂停目标并启动应用，单个事务同时启用全部目标，以事务前计时至观察到全部 SQLite 提交；包括调度扫描等待、网络、落库、采样开销，排除进程启动和目标创建耗时。这与 v1.1 旧报告的进程内测试起点不同，旧 CSV 不混入新对照。

每 5ms 轮询落库数，每 50ms 通过真实状态接口采样 Go heap/goroutine。采样可能漏掉瞬时峰值，也会增加双方的工作量。每轮末尾逐目标断言恰好一条成功记录，检查上游请求数、同目标重叠和并发上限。处理速率为实际提交数/完整轮次 wall time，是短批次结果，不是稳态 QPS。

Linux 从 `/proc`、Windows 通过 PowerShell 7 读取激活前和完成后的 RSS；这是前后快照，不是峰值 RSS。其他平台标空值。heap、goroutine 与 RSS 含义不同。

## 存储与分配

每版另建 3 个独立 fixture：100 个暂停目标，各 1000 条明细，分布在最近 30 个 UTC 日期，共 100000 条明细及 3000 个由同批明细计算的日桶。按 `n % 5` 等量分配成功、HTTP503、超时、断言失败和请求错误；超时 duration 为 1000ms，其余为 50ms。数据按日循环，某一日可集中为同一种结果，不冒称随机现实流量。

二进制 HTTP 测量每个 fixture 包含列表、第一页/后续历史分页、30日汇总和状态接口；各 1 次预热、10 次顺序观测。另 3 次手动检查测真实 50ms HTTP 加调度及写入完成，不能将它称为独立 SQL 写入耗时。候选历史接口包含错误分类汇总，状态接口包含新增 monitor health；v1.1 对应新增功能标 N/A，不编造前后收益。

`performance_v12_test.go` 是可原样复制到 v1.1 `fbfd0be` 的测试 helper。runner 用 `git archive` 导出该基线，复制当前非测试 Go/web/module 文件形成候选快照，并把同一个 helper 复制到两侧。所有原有测试文件从临时快照排除，避免版本专有测试辅助函数混入。默认各执行 `-benchtime=10x -count=3 -benchmem`，测 Store 列表/50条分页/日汇总/RecordCheck、API 列表/历史/日汇总/状态的耗时、B/op 和 allocs/op，并保存查询计划。

分类 Store 基准通过接口检测能力；v1.1 缺失时明确跳过为 N/A。该 helper 的两侧 fixture 都是相同旧 NULL 分类数据，候选按结构推断分类（request_error 推为 unknown）。二进制 HTTP fixture 则在 schema4 写入显式分类（请求错误为 dns），schema3 没有该列。两种实验的来源与分类语义应在报告中分别列明。

Go 基准是独立编译的源快照实验；外部传入二进制的内部耗时/分配不能从中直接推定。`environment.json` 记录两侧源文件列表与 SHA256、runner/helper SHA256、二进制 SHA256、Go build info、Git HEAD/工作区状态、运行环境和工具链。最终报告仍须核对最终候选的构建出处，不能仅因提供了源 hash 就断言它生成了某个外部二进制。运行期间源文件、脚本或输入二进制变化会使实验失败。

## 原始证据与判定

`matrix.csv`、`matrix-status-samples.csv`、`storage-raw.csv`、`go-raw.csv` 保存原始轮次/样本；JSON 保留对应结构及 fixture 元数据。`baseline-go-benchmark.txt` / `candidate-go-benchmark.txt` 包含基准、分配及查询计划。`progress.json` 留下最后进入的阶段；失败时 `result.json` 为 failed 且退出码非零，不输出成功结论。

`comparisons.json` 分别按版本计算中位数，不混算；`investigations-required.json` 列出中位耗时增加超过 20% 且绝对增加超过 0.1ms 的可比项。它是人工调查参考，不是 CI gate。短预检的调查项不能解释为可复现退化。

完整测量结束也只标 `measurements_completed_review_required` 或 `investigation_required`，`release_acceptance_passed` 保持 false。最终须复测和定位明显退化，评估是否影响采集、新鲜度及 100 目标交互；重要影响必须修复，不能通过写一段解释自动放行。小幅合理新增成本也须明确披露。

统计/fixture 单元测试：`node --test scripts/performance-v12.test.cjs`。它验证 18 轮组合无漏无重复、版本独立中位数、缺失/重复落库拒绝、输出安全和两种 schema 的混合 fixture 计数，不以耗时作为通过条件。
