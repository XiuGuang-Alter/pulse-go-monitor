# v1.2 浏览器自动化验证

结构化结果见 [browser-results.json](../../performance/v1.2/browser-results.json)。结果对应其中的产品源码、测试辅助源码和可执行文件 SHA-256；候选基于记录的提交并包含未提交改动。报告中的 14 项是原有完整流程，分别在 Chromium、Firefox 与 Edge 执行，使用合成回环上游及临时数据库。

默认不设置 `PULSE_BROWSER` 时只选择 Chromium。显式支持 `chromium`、`firefox`、`edge`，未知值会立即报错，避免选择错误浏览器。Edge 使用本机已安装的 `msedge` channel。所有项目保持 `retries: 0`；失败不得靠增加重试隐藏。

从仓库根目录在 PowerShell 7 执行：

```powershell
$env:PLAYWRIGHT_BROWSERS_PATH = Join-Path (Get-Location) '.cache/pw-browsers'
node --test e2e/config-options.node.cjs
npm.cmd run build:e2e
npx.cmd playwright install firefox

Remove-Item Env:PULSE_BROWSER -ErrorAction SilentlyContinue
npx.cmd playwright test
$env:PULSE_BROWSER = 'firefox'
npx.cmd playwright test
$env:PULSE_BROWSER = 'edge'
npx.cmd playwright test
Remove-Item Env:PULSE_BROWSER
```

Firefox 安装目标是项目缓存，Edge 无需通过该命令安装。报告的浏览器版本通过相同 engine/channel 的真实无界面启动及 `browser.version()` 读取。运行期间产品源码、测试辅助源码和测试程序均再次核对指纹；原始 Playwright JSON 包含本机绝对路径，因此只保存在忽略的缓存中，公开报告仅保留汇总、相对源码清单与原始报告哈希。

本次 Chromium 与 Edge 在受限沙箱中首次完整执行。Firefox 的首次沙箱启动未完成自动化连接，独立最小启动分别观察到 15 秒与 180 秒超时；首次完整测试尝试在没有已完成用例时中断。随后在正常本机权限下，以相同缓存及默认安全设置做一次 15 秒上限的最小启动，立即获得 Juggler 连接和版本，再在该环境完整执行 14 项。未修改安全首选项。该证据确认执行环境差异，但没有确定沙箱内部的具体限制；报告同时保留失败和后续执行，不能称为 Firefox 首次执行即通过。

覆盖包含管理与表单校验、分页与删除、断连与持久化恢复、业务规则与 UTC 汇总、精确数字与换行保留、旧库升级、新鲜度和浏览器墙钟偏移、脱敏与原值请求、错误分类，以及保留范围和维护指引。320/375/390px 检查属于窄屏视口布局验证；本报告不提供真机证据。Edge [实际界面验收已另行记录](edge-interface.md)，自动化通过不能替代该验收。
