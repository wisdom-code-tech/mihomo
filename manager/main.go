package main

import (
	"context"
	"crypto/rand"
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
	mu        sync.Mutex
	cmd       *exec.Cmd
	startedAt time.Time
	lastError string
	binary    string
	configDir string
	config    string
	secret    string
	logFile   *os.File
	client    *http.Client
}

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
	var binary, configDir, socketPath, logPath string
	flag.StringVar(&binary, "mihomo", "./mihomo", "mihomo binary")
	flag.StringVar(&configDir, "config-dir", ".", "writable config directory")
	flag.StringVar(&socketPath, "socket", "./mihomo.sock", "fnOS gateway socket")
	flag.StringVar(&logPath, "log", "./mihomo.log", "combined log file")
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
		secret: filepath.Join(configDir, "secret"), logFile: lf, client: secureHTTPClient(),
	}
	if err := a.ensureConfig(); err != nil {
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

	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("gateway server failed: %v", err)
	}
	a.stopCore()
	_ = os.Remove(socketPath)
	_ = lf.Close()
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
		mux.ServeHTTP(w, r)
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
	secret, _ := os.ReadFile(a.secret)
	tmp := a.config + ".new"
	if err = a.writeSecuredConfig(raw, strings.TrimSpace(string(secret)), tmp); err != nil {
		writeError(w, http.StatusBadRequest, "订阅不是有效的 Mihomo YAML: "+err.Error())
		return
	}
	check := exec.CommandContext(r.Context(), a.binary, "-t", "-d", a.configDir, "-f", tmp)
	if output, checkErr := check.CombinedOutput(); checkErr != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusBadRequest, "配置校验失败: "+strings.TrimSpace(string(output)))
		return
	}
	_ = os.Rename(a.config, a.config+".bak")
	if err = os.Rename(tmp, a.config); err != nil {
		_ = os.Rename(a.config+".bak", a.config)
		writeError(w, http.StatusInternalServerError, "配置替换失败")
		return
	}
	if err = os.WriteFile(filepath.Join(a.configDir, "subscription.url"), []byte(u.String()+"\n"), 0600); err != nil {
		writeError(w, http.StatusInternalServerError, "订阅地址保存失败")
		return
	}
	if err = a.restartCore(); err != nil {
		_ = os.Rename(a.config, a.config+".failed")
		_ = os.Rename(a.config+".bak", a.config)
		_ = a.restartCore()
		writeError(w, http.StatusInternalServerError, "新配置启动失败，已回滚上一份配置: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

const indexHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Mihomo 管理</title><style>
:root{color-scheme:dark;font-family:Inter,"PingFang SC",sans-serif;background:#090b14;color:#edf2ff}*{box-sizing:border-box}body{margin:0;min-height:100vh;background:radial-gradient(circle at 85% 0,#32276d55,transparent 38%),#090b14}.wrap{max-width:760px;margin:auto;padding:64px 24px}.eyebrow{color:#7de7dc;letter-spacing:.14em;font-size:12px}.card{margin-top:20px;padding:28px;border:1px solid #ffffff18;border-radius:24px;background:#121625dd;box-shadow:0 24px 80px #0008}.row{display:flex;align-items:center;gap:14px;flex-wrap:wrap}.dot{width:11px;height:11px;border-radius:50%;background:#788095;box-shadow:0 0 18px currentColor}.dot.on{background:#62e6b1}.status{font-size:28px;font-weight:700}.meta{color:#9da7bd;margin:10px 0 28px}label{display:block;margin-bottom:8px;color:#cbd3e5}input{width:100%;padding:14px 16px;border-radius:12px;border:1px solid #ffffff20;background:#090c17;color:white;font:inherit;outline:none}input:focus{border-color:#7667ef}.actions{display:flex;gap:12px;margin-top:16px;flex-wrap:wrap}button,a.button{border:0;border-radius:12px;padding:12px 17px;background:#7667ef;color:white;font:600 14px inherit;text-decoration:none;cursor:pointer}.secondary{background:#232a3d!important}.msg{min-height:24px;margin-top:14px;color:#7de7dc}.hint{font-size:13px;color:#818da6;line-height:1.6;margin-top:24px}@media(max-width:520px){.wrap{padding-top:32px}.card{padding:21px}.status{font-size:23px}}
</style></head><body><main class="wrap"><div class="eyebrow">FNOS NATIVE SERVICE</div><h1>Mihomo 管理</h1><section class="card"><div class="row"><span id="dot" class="dot"></span><span id="status" class="status">正在检查…</span></div><div id="meta" class="meta"></div><label for="url">订阅配置地址</label><input id="url" type="url" placeholder="https://example.com/config.yaml" autocomplete="off"><div class="actions"><button id="update">下载、校验并应用</button><button id="restart" class="secondary">重启内核</button><a id="dashboard" class="button secondary" href="dashboard/">打开 Zashboard</a></div><div id="msg" class="msg"></div><div class="hint">控制 API 仅监听 127.0.0.1:9090，并通过 fnOS 登录网关访问。更新配置会保留上一份 config.yaml.bak；下载文件在通过 mihomo 校验前不会生效。</div></section></main><script>
const base=location.pathname.replace(/\/?$/,'/'),$=id=>document.getElementById(id);const qp=new URLSearchParams({hostname:location.hostname,secondaryPath:'/app/mihomo/core',disableUpgradeCore:'1'});if(location.port)qp.set('port',location.port);$('dashboard').href=base+'dashboard/#/setup?'+qp;async function status(){try{const r=await fetch(base+'api/status'),d=await r.json();$('dot').classList.toggle('on',d.running);$('status').textContent=d.running?'服务运行中':'服务未运行';$('meta').textContent=[d.version&&'mihomo '+d.version,d.pid&&'PID '+d.pid,d.lastError].filter(Boolean).join(' · ');if(!$('url').value)$('url').value=d.subscription||''}catch(e){$('status').textContent='状态获取失败';$('meta').textContent=e.message}}async function post(path,body){$('msg').textContent='处理中…';const r=await fetch(base+'api/'+path,{method:'POST',headers:{'Content-Type':'application/json'},body:body?JSON.stringify(body):'{}'}),d=await r.json();if(!r.ok)throw Error(d.error||'请求失败');$('msg').textContent='操作成功';await status()}$('update').onclick=()=>post('subscription',{url:$('url').value}).catch(e=>$('msg').textContent=e.message);$('restart').onclick=()=>post('restart').catch(e=>$('msg').textContent=e.message);status();setInterval(status,10000);
</script></body></html>`
