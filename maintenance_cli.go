package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"modernc.org/sqlite"
)

const maintenanceTimeout = 2 * time.Minute

const maintenanceHelp = `维护命令（先正常停止监测服务；不覆盖已有输出）：
  maintain backup -db source.db -out backup.zip
  maintain verify -in backup.zip
  maintain restore -in backup.zip -db new.db
  maintain export-config -db source.db -out config.json [-full]
  maintain import-config -in config.json -db new.db [-dry-run] [-keep-enabled]
  maintain export-history -db source.db -out history.json -target 1 -from 2026-11-01T00:00:00Z -to 2026-11-02T00:00:00Z [-format json|csv]
备份压缩/解压各最多256MiB，manifest最多64KiB；配置输入最多4MiB、100目标。
配置默认脱敏导出，无法直接导入；-full导出和数据库备份含原始数据，请妥善保管。
导入默认暂停全部目标，先使用-dry-run预检；-keep-enabled显式保留来源启用状态。
恢复保留备份schema版本，启动服务时才执行迁移和保留清理。历史仅包含现存保留明细，UTC范围左闭右开。
维护命令最多运行2分钟，可用Ctrl+C取消。文件系统不支持原子硬链接时安全失败。
`

func runMaintenance(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, maintenanceHelp)
		return 0
	}
	command := args[0]
	flags := flag.NewFlagSet("maintain "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var source, input, dest, fromRaw, toRaw, format string
	var full, keep, dry bool
	var target int64
	switch command {
	case "backup", "export-config", "export-history":
		flags.StringVar(&source, "db", "", "源数据库")
		flags.StringVar(&dest, "out", "", "新输出文件")
	case "verify":
		flags.StringVar(&input, "in", "", "备份归档")
	case "restore", "import-config":
		flags.StringVar(&input, "in", "", "输入文件")
		flags.StringVar(&dest, "db", "", "全新数据库路径")
	default:
		fmt.Fprintln(errOut, "未知维护命令；运行 maintain help 查看用法")
		return 2
	}
	if command == "export-config" {
		flags.BoolVar(&full, "full", false, "导出含原始值的完整配置")
	}
	if command == "import-config" {
		flags.BoolVar(&keep, "keep-enabled", false, "保留来源启用状态")
		flags.BoolVar(&dry, "dry-run", false, "只预检，不写文件")
	}
	if command == "export-history" {
		flags.Int64Var(&target, "target", 0, "目标ID")
		flags.StringVar(&fromRaw, "from", "", "UTC开始时间")
		flags.StringVar(&toRaw, "to", "", "UTC结束时间")
		flags.StringVar(&format, "format", "json", "json或csv")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, maintenanceHelp)
			return 0
		}
		fmt.Fprintln(errOut, "维护命令参数无效；运行 maintain help 查看用法")
		return 2
	}
	if flags.NArg() != 0 || ((command == "backup" || command == "export-config" || command == "export-history") && (source == "" || dest == "")) || ((command == "restore" || command == "import-config") && (input == "" || dest == "")) || (command == "verify" && input == "") {
		fmt.Fprintln(errOut, "缺少或多余参数：请按 maintain help 填写 -db、-in、-out 等必需项")
		return 2
	}
	var from, to time.Time
	if command == "export-history" {
		var err error
		from, err = time.Parse(time.RFC3339Nano, fromRaw)
		if err != nil {
			fmt.Fprintln(errOut, "-from 必须是UTC RFC3339时间")
			return 2
		}
		to, err = time.Parse(time.RFC3339Nano, toRaw)
		if err != nil {
			fmt.Fprintln(errOut, "-to 必须是UTC RFC3339时间")
			return 2
		}
		_, offsetFrom := from.Zone()
		_, offsetTo := to.Zone()
		if offsetFrom != 0 || offsetTo != 0 || !from.Before(to) || target <= 0 || (format != "json" && format != "csv") {
			fmt.Fprintln(errOut, "历史参数无效：需正整数-target、UTC时间-from<-to以及-format json或csv")
			return 2
		}
	}
	ctx, cancel := context.WithTimeout(ctx, maintenanceTimeout)
	defer cancel()
	var report any = map[string]any{"operation": command, "completed": true}
	var err error
	switch command {
	case "backup":
		fmt.Fprintln(errOut, "提示：数据库备份含原始数据；请先正常停止服务。")
		report, err = BackupDatabase(ctx, source, dest)
	case "verify":
		report, err = VerifyBackup(ctx, input)
	case "restore":
		report, err = RestoreBackup(ctx, input, dest)
	case "export-config":
		if full {
			fmt.Fprintln(errOut, "提示：完整配置含原始敏感值，请妥善保管。")
		}
		err = ExportConfiguration(ctx, source, dest, full)
	case "import-config":
		fmt.Fprintf(errOut, "导入策略：仅新数据库；默认暂停，保留启用状态=%t；仅预检=%t\n", keep, dry)
		preview, previewErr := ImportConfiguration(ctx, input, dest, keep, true)
		if previewErr != nil {
			err = previewErr
			break
		}
		fmt.Fprintf(errOut, "预检：目标%d个，来源启用%d个，导入后启用%d个\n", preview.SourceTargets, preview.SourceEnabled, preview.EnabledTargets)
		if dry {
			report = preview
		} else {
			report, err = ImportConfiguration(ctx, input, dest, keep, false)
		}
	case "export-history":
		err = ExportHistory(ctx, source, dest, target, from.UTC(), to.UTC(), format)
	}
	if err != nil {
		fmt.Fprintln(errOut, "维护操作失败："+safeMaintenanceError(err))
		return 1
	}
	if err = json.NewEncoder(out).Encode(report); err != nil {
		fmt.Fprintln(errOut, "维护结果输出失败")
		return 1
	}
	return 0
}

func safeMaintenanceError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "CANCELED：已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT：已超时，请检查文件锁或缩小操作范围"
	case errors.Is(err, os.ErrNotExist):
		return "NOT_FOUND：输入文件或目标目录不存在"
	case errors.Is(err, os.ErrExist):
		return "EXISTS：目标已存在，拒绝覆盖"
	case errors.Is(err, os.ErrPermission):
		return "PERMISSION：文件权限不足"
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return "FILE_IO：" + pathError.Err.Error()
	}
	var linkError *os.LinkError
	if errors.As(err, &linkError) {
		return "FILE_IO：" + linkError.Err.Error()
	}
	var databaseError *sqlite.Error
	if errors.As(err, &databaseError) {
		return fmt.Sprintf("SQLITE_%d：数据库读取、锁或完整性错误", databaseError.Code())
	}
	return err.Error() // Maintenance validation messages never interpolate input data.
}
