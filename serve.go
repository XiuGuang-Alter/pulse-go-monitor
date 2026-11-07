package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

func runServe(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(errOut)
	address := flags.String("addr", "127.0.0.1:8080", "本机监听地址")
	path := flags.String("db", "data/monitor.db", "SQLite数据库路径")
	concurrency := flags.Int("concurrency", 4, "全局并发数1–32")
	demo := flags.Bool("demo", false, "同时启动本地演示接口并添加示例目标")
	demoAddress := flags.String("demo-addr", "127.0.0.1:8091", "本机演示接口地址（允许端口0）")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || !loopbackAddress(*address) || !loopbackAddress(*demoAddress) || *concurrency < 1 || *concurrency > 32 {
		fmt.Fprintln(errOut, "参数无效：仅支持回环地址，并发数为1–32；使用 serve -h 查看用法")
		return 2
	}
	store, err := OpenStore(*path)
	if err != nil {
		fmt.Fprintln(errOut, "打开数据库失败：", err)
		return 1
	}
	defer store.Close()
	scheduler, err := NewScheduler(store, *concurrency, nil)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer scheduler.Stop()
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		fmt.Fprintln(errOut, "监听失败：", err)
		return 1
	}
	defer listener.Close()
	if *demo {
		_, cleanup, err := startDemo(store, *demoAddress)
		if err != nil {
			fmt.Fprintln(errOut, "启动演示失败：", err)
			return 1
		}
		defer cleanup()
	}
	server := &http.Server{Handler: NewApp(store, scheduler).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	scheduler.Start()
	completed := make(chan error, 1)
	go func() { completed <- server.Serve(listener) }()
	fmt.Fprintf(out, "监测控制台：http://%s\n数据库：%s\n按 Ctrl+C 停止服务。\n", listener.Addr().String(), *path)
	select {
	case <-ctx.Done():
	case err = <-completed:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(errOut, err)
			return 1
		}
	}
	scheduler.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		server.Close()
		fmt.Fprintln(errOut, "服务已强制关闭：", err)
		return 1
	}
	return 0
}

func demoHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]string{"message": "演示服务正常"})
	})
	mux.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 500, map[string]string{"message": "模拟服务端错误"})
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(3 * time.Second):
			jsonResponse(w, 200, map[string]string{"message": "延迟响应"})
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", 302) })
	return mux
}
func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && (host == "localhost" || (ip != nil && ip.IsLoopback()))
}

func startDemo(store *Store, address string) (string, func(), error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return "", nil, err
	}
	address = listener.Addr().String()
	server := &http.Server{Handler: demoHandler(), ReadHeaderTimeout: 5 * time.Second}
	cleanup := func() { server.Close(); listener.Close() }
	existing, err := store.ListTargets(context.Background())
	if err != nil {
		cleanup()
		return "", nil, err
	}
	for _, item := range []struct{ name, path string }{{"演示 · 正常接口", "/ok"}, {"演示 · 服务端错误", "/error"}, {"演示 · 超时接口", "/slow"}} {
		url := "http://" + address + item.path
		found := false
		for _, target := range existing {
			if target.URL == url {
				found = true
				break
			}
		}
		if !found {
			_, err = store.SaveTarget(context.Background(), 0, TargetInput{Name: item.name, URL: url, IntervalSeconds: 10, TimeoutSeconds: 1, Enabled: true})
			if err != nil {
				cleanup()
				return "", nil, err
			}
		}
	}
	go server.Serve(listener)
	return address, cleanup, nil
}
