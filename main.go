package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"
)

const defaultTimeout = 5 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 把命令行交互与进程退出分开，让测试可以检查输出和退出码。
func run(args []string, out io.Writer, errOut io.Writer) int {
	if len(args) > 0 && args[0] == "serve" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return runServe(ctx, args[1:], out, errOut)
	}
	if len(args) != 1 {
		fmt.Fprintln(errOut, "用法：go-monitor <完整的 http:// 或 https:// 网址>")
		return 2
	}

	result := Check(context.Background(), args[0], defaultTimeout)
	result.Detail = safeDetail(result.Kind, result.StatusCode, result.Detail)
	if result.Kind == "invalid_input" {
		fmt.Fprintln(errOut, "输入无效："+result.Detail)
		return 2
	}

	writer := out
	if result.Kind != "ok" {
		writer = errOut
	}
	fmt.Fprintln(writer, "网址："+redactURL(result.URL))
	if result.StatusCode != 0 {
		fmt.Fprintf(writer, "HTTP 状态码：%d\n", result.StatusCode)
	}
	if result.StatusCode == 0 {
		fmt.Fprintf(writer, "失败前耗时：%.2f ms\n", float64(result.Duration)/float64(time.Millisecond))
	} else {
		fmt.Fprintf(writer, "耗时（到最终响应头）：%.2f ms\n", float64(result.Duration)/float64(time.Millisecond))
	}
	fmt.Fprintln(writer, "结果："+result.Detail)
	if result.ErrorCategory != "" {
		fmt.Fprintln(writer, "错误分类："+safeCategoryLabel(result.ErrorCategory))
	}
	if result.Kind == "ok" {
		return 0
	}
	return 1
}
