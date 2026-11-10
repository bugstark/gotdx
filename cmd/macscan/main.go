package main

// macscan 从一组任意 host 中筛选真正支持 MAC 协议、且 K 线请求较快的节点。
//
// 用法：
//   go run ./cmd/macscan -file hosts.txt -samples 3 -count 30 -concurrency 8
//   go run ./cmd/macscan -file - < hosts.txt
//
// hosts.txt 每行可以是：
//   public-ip-xxxx:7709
//   ('深圳双线主站1', 'public-ip-xxxx', 7709)
//   public-ip-yyyy
// 空的端口默认补 7709。工具会去重，并用真实 MACSymbolBars 请求验证协议，
// 不支持 MAC 的 host 不会计入可用结果。

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bensema/gotdx"
	tdxtypes "github.com/bensema/gotdx/types"
)

type hostResult struct {
	Host        string `json:"host"`
	MAC         bool   `json:"mac"`
	Connected   bool   `json:"connected"`
	ConnectMS   int64  `json:"connect_ms"`
	MedianMS    int64  `json:"median_ms"`
	P95MS       int64  `json:"p95_ms"`
	Samples     int    `json:"samples"`
	LastError   string `json:"last_error,omitempty"`
	lastLatency []time.Duration
}

var publicHostRE = regexp.MustCompile(`public-ip-[a-z0-9]+(?::\d+)?`)
var hostPortRE = regexp.MustCompile(`[a-zA-Z0-9][a-zA-Z0-9_.-]*(?::\d+)`)
var quotedHostRE = regexp.MustCompile(`['"]([a-zA-Z0-9][a-zA-Z0-9_.-]*(?::\d+)?)['"]`)
var bareHostRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*(?::\d+)?$`)

func main() {
	var (
		file        = flag.String("file", "", "host 列表文件；传 - 表示 stdin")
		samples     = flag.Int("samples", 3, "每个 host 的 K 线采样次数")
		count       = flag.Uint("count", 30, "MACSymbolBars 请求根数")
		concurrency = flag.Int("concurrency", 8, "并发探测 host 数")
		timeout     = flag.Duration("timeout", 3*time.Second, "单次连接/请求超时")
		top         = flag.Int("top", 0, "只显示前 N 个可用 host；0 显示全部")
		asJSON      = flag.Bool("json", false, "输出 JSON")
	)
	flag.Parse()

	text, err := readInput(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 host 列表失败: %v\n", err)
		os.Exit(2)
	}
	hosts := parseHosts(text)
	if len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "没有解析到 host；可用 -file hosts.txt 或把 host 放命令行参数")
		os.Exit(2)
	}
	if *samples <= 0 {
		*samples = 1
	}
	if *count == 0 {
		*count = 30
	}
	if *concurrency <= 0 {
		*concurrency = 1
	}

	results := scanHosts(hosts, *samples, uint32(*count), *timeout, *concurrency)
	sortResults(results)
	if *top > 0 && len(results) > *top {
		results = results[:*top]
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return
	}
	fmt.Printf("%-42s %-5s %-7s %-10s %-9s %-8s %s\n", "HOST", "MAC", "CONN_MS", "MEDIAN_MS", "P95_MS", "SAMPLES", "ERROR")
	for _, r := range results {
		fmt.Printf("%-42s %-5s %-7d %-10s %-9s %-8d %s\n",
			r.Host, yesNo(r.MAC), r.ConnectMS, msOrDash(r.MedianMS), msOrDash(r.P95MS), r.Samples, r.LastError)
	}
}

func readInput(file string) (string, error) {
	if file == "-" {
		return readAll(os.Stdin)
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if flag.NArg() > 0 {
		return strings.Join(flag.Args(), "\n"), nil
	}
	return "", nil
}

func readAll(f *os.File) (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	return b.String(), sc.Err()
}

// parseHosts 从混合文本中提取 host[:port]，默认端口 7709，最后去重。
func parseHosts(text string) []string {
	seen := make(map[string]struct{})
	var hosts []string
	add := func(raw string) {
		h := normalizeHost(raw)
		if h == "" {
			return
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}
	for _, line := range strings.Split(text, "\n") {
		matches := publicHostRE.FindAllString(line, -1)
		if len(matches) == 0 {
			matches = hostPortRE.FindAllString(line, -1)
		}
		if len(matches) == 0 {
			for _, sub := range quotedHostRE.FindAllStringSubmatch(line, -1) {
				if len(sub) >= 2 {
					matches = append(matches, sub[1])
				}
			}
		}
		if len(matches) == 0 {
			candidate := strings.TrimSpace(strings.Trim(line, `"'(),[]`))
			if bareHostRE.MatchString(candidate) {
				matches = []string{candidate}
			}
		}
		for _, m := range matches {
			add(m)
		}
	}
	return hosts
}

func normalizeHost(raw string) string {
	h := strings.TrimSpace(strings.Trim(raw, `"'(),[]`))
	if h == "" {
		return ""
	}
	if !strings.Contains(h, ":") {
		return h + ":7709"
	}
	return h
}

func scanHosts(hosts []string, samples int, count uint32, timeout time.Duration, concurrency int) []hostResult {
	results := make([]hostResult, 0, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for _, host := range hosts {
		host := host
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := probeHost(host, samples, count, timeout)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

func probeHost(host string, samples int, count uint32, timeout time.Duration) hostResult {
	r := hostResult{Host: host}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	c := gotdx.NewMAC(
		gotdx.WithMacTCPAddress(host),
		gotdx.WithPoolSize(1),
		gotdx.WithTimeoutSec(int(timeout.Seconds())),
	)
	defer c.Disconnect()

	started := time.Now()
	if err := c.ConnectMAC(); err != nil {
		r.LastError = err.Error()
		return r
	}
	r.Connected = true
	r.ConnectMS = time.Since(started).Milliseconds()

	for i := 0; i < samples; i++ {
		reqStarted := time.Now()
		_, err := c.MACSymbolBars(
			uint8(tdxtypes.MarketSH), "600519", uint16(tdxtypes.KLINE_TYPE_DAILY),
			1, 0, count, tdxtypes.AdjustQFQ,
		)
		if err != nil {
			r.LastError = err.Error()
			return r
		}
		r.lastLatency = append(r.lastLatency, time.Since(reqStarted))
	}
	r.MAC = len(r.lastLatency) > 0
	r.Samples = len(r.lastLatency)
	r.MedianMS = median(r.lastLatency).Milliseconds()
	r.P95MS = percentile(r.lastLatency, 0.95).Milliseconds()
	return r
}

func median(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

func percentile(values []time.Duration, q float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * q)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func sortResults(results []hostResult) {
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.MAC != b.MAC {
			return a.MAC
		}
		if a.Connected != b.Connected {
			return a.Connected
		}
		if a.MedianMS != b.MedianMS {
			if a.MedianMS == 0 {
				return false
			}
			if b.MedianMS == 0 {
				return true
			}
			return a.MedianMS < b.MedianMS
		}
		return a.Host < b.Host
	})
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func msOrDash(v int64) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}
