// Command find_env_probes 扫描 docker-monitor 落盘的日志，
// 找出尝试访问指定敏感路径(默认 /api/.env、/api/.git/config)的客户端 IP，
// 按路径分组、按访问次数聚合输出。
//
// 用法示例：
//
//	go run ./scripts/find_env_probes
//	go run ./scripts/find_env_probes -dir data/coach_prod
//	go run ./scripts/find_env_probes -path /api/.env -path /api/.git/config -v
//	go run ./scripts/find_env_probes -path /api/.env,/api/.git/config
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// rawEvent 对应 LogEvent.Raw 中嵌套的业务请求 JSON。
type rawEvent struct {
	Msg           string `json:"msg"`
	URL           string `json:"url"`
	Method        string `json:"method"`
	IP            string `json:"ip"`
	XRealIP       string `json:"x_real_ip"`
	XForwardedFor string `json:"x_forwarded_for"`
	RemoteAddr    string `json:"remote_addr"`
}

// logEvent 是 LogBatch.Events 元素的最小子集。
type logEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Container struct {
		Name string `json:"name"`
	} `json:"container"`
	Message string `json:"message"`
	Raw     string `json:"raw"`
}

// logBatch 对应一行 jsonl 的最小子集。
type logBatch struct {
	Events []logEvent `json:"events"`
}

// ipStat 聚合单个 IP 在某条目标路径下的探测情况。
type ipStat struct {
	IP         string
	Count      int
	FirstSeen  time.Time
	LastSeen   time.Time
	Methods    map[string]struct{}
	URLs       map[string]struct{}
	Containers map[string]struct{}
}

// pathFlag 支持重复 -path 或逗号分隔的多路径输入。
type pathFlag []string

func (p *pathFlag) String() string { return strings.Join(*p, ",") }

func (p *pathFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*p = append(*p, s)
		}
	}
	return nil
}

func main() {
	dir := flag.String("dir", "data/coach_prod", "日志根目录")
	var targets pathFlag
	flag.Var(&targets, "path", "需要匹配的请求路径(已剔除 query)，可重复或逗号分隔")
	verbose := flag.Bool("v", false, "输出每条命中明细")
	flag.Parse()

	if len(targets) == 0 {
		targets = pathFlag{"/api/.env", "/api/.git/config"}
	}
	targetSet := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		targetSet[t] = struct{}{}
	}

	// stats: path -> ip -> 聚合
	stats := make(map[string]map[string]*ipStat, len(targets))
	for _, t := range targets {
		stats[t] = make(map[string]*ipStat)
	}
	totalHits := 0

	walkErr := filepath.WalkDir(*dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("访问 %s: %w", path, err)
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		hits, scanErr := scanFile(path, targetSet, stats, *verbose)
		if scanErr != nil {
			return fmt.Errorf("扫描 %s: %w", path, scanErr)
		}
		totalHits += hits
		return nil
	})
	if walkErr != nil {
		fmt.Fprintf(os.Stderr, "遍历日志目录失败: %v\n", walkErr)
		os.Exit(1)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, target := range targets {
		bucket := stats[target]
		fmt.Printf("== %s ==\n", target)
		if len(bucket) == 0 {
			fmt.Printf("  未发现访问记录\n\n")
			continue
		}

		ranked := make([]*ipStat, 0, len(bucket))
		hits := 0
		for _, v := range bucket {
			ranked = append(ranked, v)
			hits += v.Count
		}
		sort.Slice(ranked, func(i, j int) bool {
			if ranked[i].Count != ranked[j].Count {
				return ranked[i].Count > ranked[j].Count
			}
			return ranked[i].IP < ranked[j].IP
		})

		fmt.Printf("命中请求数: %d，独立 IP 数: %d\n", hits, len(ranked))
		fmt.Fprintln(w, "IP\tHITS\tFIRST_SEEN\tLAST_SEEN\tMETHODS\tURLS\tCONTAINERS")
		for _, s := range ranked {
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				s.IP,
				s.Count,
				s.FirstSeen.Local().Format(time.RFC3339),
				s.LastSeen.Local().Format(time.RFC3339),
				strings.Join(keysOf(s.Methods), ","),
				strings.Join(keysOf(s.URLs), ","),
				strings.Join(keysOf(s.Containers), ","),
			)
		}
		if err := w.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "刷新输出失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println()
	}

	fmt.Printf("总命中: %d\n", totalHits)
}

func scanFile(path string, targets map[string]struct{}, stats map[string]map[string]*ipStat, verbose bool) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("打开文件: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// 单行可能很长(嵌套了多个事件)，放宽到 16MiB
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	hits := 0
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		var batch logBatch
		if err := json.Unmarshal(scanner.Bytes(), &batch); err != nil {
			fmt.Fprintf(os.Stderr, "%s:%d 解析 batch 失败: %v\n", path, lineNo, err)
			continue
		}
		for _, ev := range batch.Events {
			if ev.Message != "request" || ev.Raw == "" {
				continue
			}
			var raw rawEvent
			if err := json.Unmarshal([]byte(ev.Raw), &raw); err != nil {
				continue
			}
			urlPath := stripQuery(raw.URL)
			if _, ok := targets[urlPath]; !ok {
				continue
			}
			ip := pickIP(raw)
			if ip == "" {
				continue
			}
			hits++
			bucket := stats[urlPath]
			s, ok := bucket[ip]
			if !ok {
				s = &ipStat{
					IP:         ip,
					FirstSeen:  ev.Timestamp,
					LastSeen:   ev.Timestamp,
					Methods:    make(map[string]struct{}),
					URLs:       make(map[string]struct{}),
					Containers: make(map[string]struct{}),
				}
				bucket[ip] = s
			}
			s.Count++
			if !ev.Timestamp.IsZero() {
				if ev.Timestamp.Before(s.FirstSeen) || s.FirstSeen.IsZero() {
					s.FirstSeen = ev.Timestamp
				}
				if ev.Timestamp.After(s.LastSeen) {
					s.LastSeen = ev.Timestamp
				}
			}
			if raw.Method != "" {
				s.Methods[raw.Method] = struct{}{}
			}
			if raw.URL != "" {
				s.URLs[raw.URL] = struct{}{}
			}
			if ev.Container.Name != "" {
				s.Containers[ev.Container.Name] = struct{}{}
			}
			if verbose {
				fmt.Printf("[hit] %s %s %s %s container=%s file=%s\n",
					ev.Timestamp.Local().Format(time.RFC3339),
					ip,
					raw.Method,
					raw.URL,
					ev.Container.Name,
					path,
				)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return hits, fmt.Errorf("读取文件: %w", err)
	}
	return hits, nil
}

// stripQuery 返回 url 的 path 部分(剔除 query / fragment)。
func stripQuery(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return u.Path
	}
	return rawURL
}

// pickIP 按可信度从高到低挑选客户端 IP。
func pickIP(r rawEvent) string {
	if v := strings.TrimSpace(r.XRealIP); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.XForwardedFor); v != "" {
		// XFF 形如 "client, proxy1, proxy2"，取首跳
		return strings.TrimSpace(strings.SplitN(v, ",", 2)[0])
	}
	if v := strings.TrimSpace(r.IP); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.RemoteAddr); v != "" {
		// remote_addr 形如 "172.21.0.1:42532"
		if i := strings.LastIndexByte(v, ':'); i > 0 {
			return v[:i]
		}
		return v
	}
	return ""
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
