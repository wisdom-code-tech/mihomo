package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxConfigSize = 8 << 20
	defaultConfig = `# 这是可启动的最小配置。请在 Mihomo 管理页填写订阅地址更新配置。
mode: rule
log-level: info
ipv6: false
allow-lan: false
external-controller: 127.0.0.1:9090
external-ui: ui
secret: ""
proxies: []
proxy-groups: []
rules:
  - MATCH,DIRECT
`
)

type app struct {
	mu         sync.Mutex
	configMu   sync.Mutex
	cmd        *exec.Cmd
	startedAt  time.Time
	lastError  string
	binary     string
	configDir  string
	config     string
	secret     string
	proxyAuth  string
	proxyAddr  string
	logFile    *os.File
	managerLog string
	client     *http.Client
}

var errConfigConflict = errors.New("配置文件已被其他操作修改，请重新载入后再保存")

type statusResponse struct {
	Running      bool   `json:"running"`
	PID          int    `json:"pid,omitempty"`
	Version      string `json:"version,omitempty"`
	StartedAt    string `json:"startedAt,omitempty"`
	Configured   bool   `json:"configured"`
	Subscription string `json:"subscription,omitempty"`
	LastError    string `json:"lastError,omitempty"`
}

func main() {
	var binary, configDir, socketPath, logPath, proxyListen string
	flag.StringVar(&binary, "mihomo", "./mihomo", "mihomo binary")
	flag.StringVar(&configDir, "config-dir", ".", "writable config directory")
	flag.StringVar(&socketPath, "socket", "./mihomo.sock", "fnOS gateway socket")
	flag.StringVar(&logPath, "log", "./mihomo.log", "combined log file")
	flag.StringVar(&proxyListen, "proxy-listen", "0.0.0.0:19090", "authenticated reverse proxy listen address; empty disables it")
	flag.Parse()

	if err := os.MkdirAll(configDir, 0750); err != nil {
		log.Fatal(err)
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		log.Fatal(err)
	}
	a := &app{
		binary: binary, configDir: configDir, config: filepath.Join(configDir, "config.yaml"),
		secret: filepath.Join(configDir, "secret"), proxyAuth: filepath.Join(configDir, "reverse-proxy-password"),
		proxyAddr: proxyListen, logFile: lf,
		managerLog: filepath.Join(filepath.Dir(logPath), "manager.log"), client: secureHTTPClient(),
	}
	if err := a.ensureConfig(); err != nil {
		log.Fatal(err)
	}
	if err := a.ensureReverseProxyPassword(); err != nil {
		log.Fatal(err)
	}
	if err := a.startCore(); err != nil {
		log.Printf("mihomo initial start failed: %v", err)
	}

	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0660); err != nil {
		log.Fatal(err)
	}

	gatewayServer := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{gatewayServer}
	errCh := make(chan error, 2)
	go func() {
		errCh <- gatewayServer.Serve(ln)
	}()
	if proxyListen != "" {
		proxyLn, listenErr := net.Listen("tcp", proxyListen)
		if listenErr != nil {
			log.Fatal(listenErr)
		}
		proxyServer := &http.Server{Handler: a.reverseProxyRoutes(), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, proxyServer)
		go func() {
			errCh <- proxyServer.Serve(proxyLn)
		}()
		log.Printf("authenticated reverse proxy listening on %s", proxyListen)
	}
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-done:
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("manager server failed: %v", serveErr)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	for _, server := range servers {
		_ = server.Shutdown(ctx)
	}
	cancel()
	a.stopCore()
	_ = os.Remove(socketPath)
	_ = lf.Close()
}

func (a *app) ensureReverseProxyPassword() error {
	if _, err := os.Stat(a.proxyAuth); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	return os.WriteFile(a.proxyAuth, []byte(hex.EncodeToString(buf)), 0600)
}

func (a *app) ensureConfig() error {
	secret, err := os.ReadFile(a.secret)
	if os.IsNotExist(err) {
		buf := make([]byte, 32)
		if _, err = rand.Read(buf); err != nil {
			return err
		}
		secret = []byte(hex.EncodeToString(buf))
		if err = os.WriteFile(a.secret, secret, 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err = os.Stat(a.config); os.IsNotExist(err) {
		return a.writeSecuredConfig([]byte(defaultConfig), strings.TrimSpace(string(secret)), a.config)
	}
	return err
}

func (a *app) startCore() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != nil && a.cmd.Process != nil {
		return nil
	}
	cmd := exec.Command(a.binary, "-d", a.configDir, "-f", a.config)
	cmd.Stdout, cmd.Stderr = a.logFile, a.logFile
	if err := cmd.Start(); err != nil {
		a.lastError = err.Error()
		return err
	}
	a.cmd, a.startedAt, a.lastError = cmd, time.Now(), ""
	go func(current *exec.Cmd) {
		err := current.Wait()
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.cmd == current {
			a.cmd = nil
			if err != nil {
				a.lastError = err.Error()
			}
		}
	}(cmd)
	return nil
}

func (a *app) stopCore() {
	a.mu.Lock()
	cmd := a.cmd
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		a.mu.Lock()
		stopped := a.cmd != cmd
		a.mu.Unlock()
		if stopped {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
}

func (a *app) restartCore() error {
	a.stopCore()
	return a.startCore()
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /api/reverse-proxy", a.reverseProxyInfo)
	mux.HandleFunc("GET /api/config", a.getConfig)
	mux.HandleFunc("PUT /api/config", a.saveConfig)
	mux.HandleFunc("GET /api/logs/stream", a.streamLogs)
	mux.HandleFunc("POST /api/subscription", a.updateSubscription)
	mux.HandleFunc("POST /api/restart", a.restart)
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.Dir(filepath.Join(a.configDir, "ui")))))
	mux.Handle("/core/", a.coreProxy())
	mux.HandleFunc("/", index)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/app/mihomo"
		if r.URL.Path == prefix {
			http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
			return
		} else if strings.HasPrefix(r.URL.Path, prefix+"/") {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *app) reverseProxyRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.Dir(filepath.Join(a.configDir, "ui")))))
	mux.Handle("/core/", a.coreProxy())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		hostname, port := r.Host, ""
		if parsedHost, parsedPort, err := net.SplitHostPort(r.Host); err == nil {
			hostname, port = parsedHost, parsedPort
		}
		query := url.Values{
			"hostname":           []string{hostname},
			"secondaryPath":      []string{"/core"},
			"disableUpgradeCore": []string{"1"},
		}
		if port != "" {
			query.Set("port", port)
		}
		http.Redirect(w, r, "/dashboard/#/setup?"+query.Encode(), http.StatusTemporaryRedirect)
	})
	return a.requireReverseProxyAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	}))
}

func (a *app) requireReverseProxyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		expected, err := os.ReadFile(a.proxyAuth)
		validUser := subtle.ConstantTimeCompare([]byte(username), []byte("mihomo")) == 1
		validPassword := subtle.ConstantTimeCompare([]byte(password), bytes.TrimSpace(expected)) == 1
		if err != nil || !ok || !validUser || !validPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="Mihomo reverse proxy", charset="UTF-8"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "需要反向代理访问凭据", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) reverseProxyInfo(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅 fnOS 管理员可以查看反向代理凭据")
		return
	}
	password, err := os.ReadFile(a.proxyAuth)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取反向代理凭据失败: "+err.Error())
		return
	}
	_, port, splitErr := net.SplitHostPort(a.proxyAddr)
	if splitErr != nil {
		port = "19090"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": a.proxyAddr != "", "port": port, "username": "mihomo",
		"password": strings.TrimSpace(string(password)),
	})
}

func (a *app) status(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	resp := statusResponse{LastError: a.lastError}
	if a.cmd != nil && a.cmd.Process != nil {
		resp.Running, resp.PID, resp.StartedAt = true, a.cmd.Process.Pid, a.startedAt.Format(time.RFC3339)
	}
	a.mu.Unlock()
	if raw, err := os.ReadFile(filepath.Join(a.configDir, "subscription.url")); err == nil {
		resp.Subscription = strings.TrimSpace(string(raw))
		resp.Configured = resp.Subscription != ""
	}
	secret, _ := os.ReadFile(a.secret)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://127.0.0.1:9090/version", nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	if coreResp, err := http.DefaultClient.Do(req); err == nil {
		defer coreResp.Body.Close()
		var body struct {
			Version string `json:"version"`
		}
		_ = json.NewDecoder(io.LimitReader(coreResp.Body, 1<<20)).Decode(&body)
		resp.Version = body.Version
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) updateSubscription(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅 fnOS 管理员可以修改订阅")
		return
	}
	var input struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	u, err := validateRemoteURL(input.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "mihomo-fnos/1.0")
	resp, err := a.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "订阅下载失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("订阅服务器返回 HTTP %d", resp.StatusCode))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigSize+1))
	if err != nil || len(raw) > maxConfigSize {
		writeError(w, http.StatusBadRequest, "订阅文件读取失败或超过 8 MiB")
		return
	}
	changed, err := a.applyConfig(r.Context(), raw, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, "订阅配置应用失败: "+err.Error())
		return
	}
	if err = os.WriteFile(filepath.Join(a.configDir, "subscription.url"), []byte(u.String()+"\n"), 0600); err != nil {
		writeError(w, http.StatusInternalServerError, "订阅地址保存失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed})
}

func (a *app) getConfig(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅 fnOS 管理员可以查看配置")
		return
	}
	raw, err := os.ReadFile(a.config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取配置失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"content": string(raw),
		"sha256":  contentHash(raw),
	})
}

func (a *app) saveConfig(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅 fnOS 管理员可以修改配置")
		return
	}
	var input struct {
		Content string `json:"content"`
		SHA256  string `json:"sha256"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxConfigSize+1)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式无效或超过 8 MiB")
		return
	}
	changed, err := a.applyConfig(r.Context(), []byte(input.Content), input.SHA256)
	if errors.Is(err, errConfigConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, _ := os.ReadFile(a.config)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "changed": changed, "content": string(raw), "sha256": contentHash(raw),
	})
}

func (a *app) applyConfig(ctx context.Context, raw []byte, expectedHash string) (bool, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()

	current, err := os.ReadFile(a.config)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if expectedHash != "" && contentHash(current) != expectedHash {
		return false, errConfigConflict
	}
	secret, err := os.ReadFile(a.secret)
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(a.configDir, ".config-*.yaml")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	if err = a.writeSecuredConfig(raw, strings.TrimSpace(string(secret)), tmpPath); err != nil {
		return false, errors.New("YAML 无效: " + err.Error())
	}
	normalized, err := os.ReadFile(tmpPath)
	if err != nil {
		return false, err
	}
	if bytes.Equal(current, normalized) {
		return false, nil
	}
	check := exec.CommandContext(ctx, a.binary, "-t", "-d", a.configDir, "-f", tmpPath)
	if output, checkErr := check.CombinedOutput(); checkErr != nil {
		return false, errors.New("mihomo 校验失败: " + strings.TrimSpace(string(output)))
	}
	if len(current) > 0 {
		if err = os.WriteFile(a.config+".bak", current, 0600); err != nil {
			return false, errors.New("配置备份失败: " + err.Error())
		}
	}
	if err = os.Rename(tmpPath, a.config); err != nil {
		return false, errors.New("配置替换失败: " + err.Error())
	}
	if err = a.restartCore(); err != nil {
		_ = os.WriteFile(a.config+".failed", normalized, 0600)
		_ = os.WriteFile(a.config, current, 0600)
		_ = a.restartCore()
		return false, errors.New("新配置启动失败，已回滚: " + err.Error())
	}
	return true, nil
}

func contentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (a *app) writeSecuredConfig(raw []byte, secret, path string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("顶层必须是 YAML 对象")
	}
	root := doc.Content[0]
	setYAMLScalar(root, "external-controller", "127.0.0.1:9090")
	setYAMLScalar(root, "external-ui", "ui")
	setYAMLScalar(root, "secret", secret)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0600)
}

func setYAMLScalar(root *yaml.Node, key, value string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func (a *app) restart(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅 fnOS 管理员可以重启服务")
		return
	}
	if err := a.restartCore(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) streamLogs(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅 fnOS 管理员可以查看日志")
		return
	}
	path := a.logFile.Name()
	if r.URL.Query().Get("source") == "manager" {
		path = a.managerLog
	} else if source := r.URL.Query().Get("source"); source != "" && source != "mihomo" {
		writeError(w, http.StatusBadRequest, "日志来源无效")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前网关不支持实时日志")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var offset int64 = -1
	ticker := time.NewTicker(750 * time.Millisecond)
	keepAlive := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer keepAlive.Stop()
	send := func() bool {
		chunk, next, reset, err := readLogChunk(path, offset)
		if err != nil && !os.IsNotExist(err) {
			chunk = "读取日志失败: " + err.Error() + "\n"
		}
		offset = next
		if chunk == "" && !reset {
			return true
		}
		payload, _ := json.Marshal(map[string]any{"chunk": chunk, "reset": reset})
		if _, err = fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func readLogChunk(path string, offset int64) (chunk string, next int64, reset bool, err error) {
	const initialTail = int64(64 << 10)
	const maxChunk = int64(64 << 10)
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, offset > 0, err
	}
	if offset < 0 {
		offset = info.Size() - initialTail
		if offset < 0 {
			offset = 0
		}
		reset = true
	} else if info.Size() < offset {
		offset = 0
		reset = true
	}
	if info.Size() == offset {
		return "", offset, reset, nil
	}
	length := info.Size() - offset
	if length > maxChunk {
		length = maxChunk
	}
	file, err := os.Open(path)
	if err != nil {
		return "", offset, reset, err
	}
	defer file.Close()
	buf := make([]byte, length)
	n, err := file.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", offset, reset, err
	}
	return string(buf[:n]), offset + int64(n), reset, nil
}

func (a *app) coreProxy() http.Handler {
	target, _ := url.Parse("http://127.0.0.1:9090")
	proxy := httputil.NewSingleHostReverseProxy(target)
	original := proxy.Director
	proxy.Director = func(r *http.Request) {
		original(r)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/core")
		secret, _ := os.ReadFile(a.secret)
		r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeError(w, http.StatusBadGateway, "mihomo API 暂不可用: "+err.Error())
	}
	return proxy
}

func secureHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				return nil, errors.New("订阅地址不能指向本机或私有网络")
			}
		}
		_, port, _ := net.SplitHostPort(address)
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}}
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("订阅重定向次数过多")
		}
		_, err := validateRemoteURL(req.URL.String())
		return err
	}
	return client
}

func validateRemoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("订阅地址必须是有效的 HTTPS URL，且不能包含用户名或密码")
	}
	return u, nil
}

func isAdmin(r *http.Request) bool { return strings.EqualFold(r.Header.Get("X-Trim-Isadmin"), "true") }

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

//go:embed web/index.html
var indexHTML string
