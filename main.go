// wbproxy: WorkBuddy AI 云端模型反代 —— 把登录态转成 OpenAI 兼容 API
// 用法: wbproxy -auth <auth.info路径> -listen :8080 [-key sk-xxx]
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ── 登录态结构（CodeBuddyExtension auth info 文件）───────────────────────

type authInfo struct {
	Account struct {
		UID string `json:"uid"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Domain       string `json:"domain"`
		ExpiresAt    int64  `json:"expiresAt"` // ms
	} `json:"auth"`
}

// ── 全局状态 ──────────────────────────────────────────────────────────

var (
	authPath   string
	upstream   string
	gateKey    string
	listenAddr string

	mu         sync.Mutex
	token      string // 当前 accessToken
	refreshTok string
	uid        string
	domain     string
	expireAt   int64 // ms 时间戳
	httpClient = &http.Client{Timeout: 10 * time.Minute}
)

func loadAuth() error {
	// 容器/云环境：AUTH_JSON 环境变量直接给登录态 JSON，省去挂文件
	if env := os.Getenv("AUTH_JSON"); env != "" {
		return loadAuthFromBytes([]byte(env))
	}
	b, err := os.ReadFile(authPath)
	if err != nil {
		return err
	}
	return loadAuthFromBytes(b)
}

func loadAuthFromBytes(b []byte) error {
	var a authInfo
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	if a.Auth.AccessToken == "" {
		return fmt.Errorf("auth 无 accessToken")
	}
	mu.Lock()
	token = a.Auth.AccessToken
	refreshTok = a.Auth.RefreshToken
	uid = a.Account.UID
	domain = a.Auth.Domain
	expireAt = a.Auth.ExpiresAt
	mu.Unlock()
	return nil
}

// ensureToken: 距过期不足 10 分钟或已过期则刷新
func ensureToken() error {
	mu.Lock()
	ok := expireAt > time.Now().Add(10*time.Minute).UnixMilli() && token != ""
	mu.Unlock()
	if ok {
		return nil
	}
	mu.Lock()
	rt := refreshTok
	dm := domain
	mu.Unlock()
	if rt == "" {
		return fmt.Errorf("token 已过期且无 refreshToken")
	}
	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequest("POST", upstream+"/v2/auth/token/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Refresh-Token", rt)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	if dm != "" {
		req.Header.Set("X-Domain", dm)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var wrapped struct {
		Data *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			Domain       string `json:"domain"`
			ExpiresAt    int64  `json:"expiresAt"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapped); err != nil {
		return fmt.Errorf("刷新响应解析失败: %w", err)
	}
	if wrapped.Data == nil || wrapped.Data.AccessToken == "" {
		return fmt.Errorf("刷新失败: HTTP %d", resp.StatusCode)
	}
	d := wrapped.Data
	mu.Lock()
	token = d.AccessToken
	if d.RefreshToken != "" {
		refreshTok = d.RefreshToken
	}
	if d.Domain != "" {
		domain = d.Domain
	}
	if d.ExpiresAt > 0 {
		expireAt = d.ExpiresAt
	} else if d.ExpiresIn > 0 {
		expireAt = time.Now().Add(time.Duration(d.ExpiresIn) * time.Second).UnixMilli()
	}
	mu.Unlock()
	log.Printf("token 已刷新，新的过期时间 %s", time.UnixMilli(expireAt).Format(time.RFC3339))
	// 回写 auth 文件，保持磁盘登录态同步
	syncAuthFile(d)
	return nil
}

func syncAuthFile(d *struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expiresAt"`
	ExpiresIn    int64  `json:"expiresIn"`
}) {
	b, err := os.ReadFile(authPath)
	if err != nil {
		return
	}
	var a map[string]any
	if json.Unmarshal(b, &a) != nil {
		return
	}
	auth, _ := a["auth"].(map[string]any)
	if auth == nil {
		return
	}
	auth["accessToken"] = d.AccessToken
	if d.RefreshToken != "" {
		auth["refreshToken"] = d.RefreshToken
	}
	if d.ExpiresAt > 0 {
		auth["expiresAt"] = d.ExpiresAt
	}
	auth["lastRefreshTime"] = time.Now().UnixMilli()
	nb, _ := json.MarshalIndent(a, "", "  ")
	_ = os.WriteFile(authPath, nb, 0o600)
}

// ── 请求门禁 ──────────────────────────────────────────────────────────

func checkGate(r *http.Request) bool {
	if gateKey == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	h = strings.TrimPrefix(h, "Bearer ")
	return h == gateKey
}
func reject(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": "auth_error"}})
}

// ── 转发核心 ──────────────────────────────────────────────────────────

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	if !checkGate(r) {
		reject(w, 401, "invalid api key")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reject(w, 400, "bad json: "+err.Error())
		return
	}
	// 上游仅支持流式：非流式请求先记下来，转发时改成流式，末尾聚合
	wantStream := false
	if v, ok := body["stream"].(bool); ok && v {
		wantStream = true
	}
	body["stream"] = true

	// 上游 tool_choice 只收 string；object/required 形式降级为 auto
	switch tc := body["tool_choice"].(type) {
	case map[string]any:
		body["tool_choice"] = "auto"
	case string:
		if tc == "required" {
			body["tool_choice"] = "auto"
		}
	}

	// 上游要求第一条必须是 system
	if msgs, ok := body["messages"].([]any); ok {
		firstSystem := false
		if len(msgs) > 0 {
			if m, ok := msgs[0].(map[string]any); ok {
				role, _ := m["role"].(string)
				firstSystem = role == "system"
			}
		}
		if !firstSystem {
			sys := map[string]any{"role": "system", "content": "You are a helpful assistant."}
			body["messages"] = append([]any{sys}, msgs...)
		}
	}

	if err := ensureToken(); err != nil {
		// 刷新失败仍尝试用现有 token 打一次
		log.Printf("token 刷新失败（继续用现有 token）: %v", err)
	}
	mu.Lock()
	tk, dm, accountUID := token, domain, uid
	mu.Unlock()
	if tk == "" {
		reject(w, 500, "no token")
		return
	}

	nb, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(r.Context(), "POST", upstream+"/v2/chat/completions", bytes.NewReader(nb))
	if err != nil {
		reject(w, 500, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tk)
	req.Header.Set("User-Agent", "codebuddy-cli/2.137.1")
	if accountUID != "" {
		req.Header.Set("X-User-Id", accountUID)
	}
	if dm == "" {
		dm = "www.workbuddy.ai"
	}
	req.Header.Set("X-Domain", dm)

	resp, err := httpClient.Do(req)
	if err != nil {
		reject(w, 502, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.Printf("upstream %d: %s", resp.StatusCode, b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(b)
		return
	}

	if wantStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)
		br := bufio.NewScanner(resp.Body)
		br.Buffer(make([]byte, 1024*1024), 1024*1024)
		for br.Scan() {
			line := br.Text()
			fmt.Fprintln(w, line)
			if strings.HasPrefix(line, "data:") {
				flusher.Flush()
			}
		}
		return
	}

	// 非流式：聚合 SSE 成一个 completion（含 tool_calls 流式片段拼装）
	var id, model string
	var content strings.Builder
	var reasoning strings.Builder
	var finish string
	var usage map[string]any
	type tcAcc struct {
		id, name, args string
	}
	tcOrder := []string{}
	tcs := map[string]*tcAcc{}
	br := bufio.NewScanner(resp.Body)
	br.Buffer(make([]byte, 1024*1024), 1024*1024)
	for br.Scan() {
		line := br.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			content.WriteString(c.Delta.Content)
			reasoning.WriteString(c.Delta.ReasoningContent)
			for _, t := range c.Delta.ToolCalls {
				key := fmt.Sprintf("%d", t.Index)
				a, ok := tcs[key]
				if !ok {
					a = &tcAcc{}
					tcs[key] = a
					tcOrder = append(tcOrder, key)
				}
				if t.ID != "" {
					a.id = t.ID
				}
				if t.Function.Name != "" {
					a.name = t.Function.Name
				}
				a.args += t.Function.Arguments
			}
			if s, ok := c.FinishReason.(string); ok && s != "" {
				finish = s
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String(), "reasoning_content": reasoning.String()}
	if len(tcOrder) > 0 {
		arr := []any{}
		for i, key := range tcOrder {
			a := tcs[key]
			arr = append(arr, map[string]any{
				"id":   a.id,
				"type": "function",
				"function": map[string]any{
					"name":      a.name,
					"arguments": a.args,
				},
			})
			_ = i
		}
		msg["tool_calls"] = arr
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	if !checkGate(r) {
		reject(w, 401, "invalid api key")
		return
	}
	ids := []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-work", "deepseek-v4-flash", "deepseek-v4-pro", "glm-5.3", "glm-5.2", "kimi-k2.6", "gemini-3.5-flash", "gpt-5.5"}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "workbuddy"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// debugHandler: 从容器内探测上游连通性，部署排障用
func debugHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"hi"},{"role":"user","content":"hi"}],"stream":true}`
	req, err := http.NewRequest("POST", upstream+"/v2/chat/completions", strings.NewReader(body))
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	mu.Lock()
	tk := token
	mu.Unlock()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tk)
	resp, err := httpClient.Do(req)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "elapsed": time.Since(start).String()})
		return
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(resp.Body, buf)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":   resp.Status,
		"elapsed":  time.Since(start).String(),
		"bodyHead": string(buf[:min(n, 200)]),
	})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	exp := expireAt
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"tokenExp": time.UnixMilli(exp).Format(time.RFC3339),
		"now":      time.Now().Format(time.RFC3339),
	})
}

// ── main ─────────────────────────────────────────────────────────────

func main() {
	// zbpack 的 /bin/server 占用了 -key/-listen 等常见 flag 名，这里用带前缀的名字避免冲突
	defaultAuth := os.Getenv("HOME") + "/Library/Application Support/CodeBuddyExtension/Data/Public/auth/workbuddy-desktop-ai.info"
	flag.StringVar(&gateKey, "wb-gate", os.Getenv("GATE_KEY"), "API 门禁密钥（-wb-gate 或 GATE_KEY 环境变量；为空则不鉴权）")
	flag.StringVar(&authPath, "wb-auth", defaultAuth, "登录态 auth info 文件路径")
	flag.StringVar(&upstream, "wb-upstream", "https://www.workbuddy.ai", "上游地址")
	flag.StringVar(&listenAddr, "wb-listen", ":"+cmp.Or(os.Getenv("PORT"), "8080"), "监听地址")
	flag.Parse()

	if err := loadAuth(); err != nil {
		log.Fatalf("读取登录态失败: %v（先在 WorkBuddy AI 桌面端登录一次）", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatHandler)
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/debug/upstream", debugHandler)

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
	}
	log.Printf("wbproxy 已启动: http://0.0.0.0%s  上游 %s  门禁 %s", listenAddr, upstream, map[bool]string{true: "开启", false: "关闭"}[gateKey != ""])
	log.Fatal(srv.ListenAndServe())
}
