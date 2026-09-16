// login 工具：Passport OIDC 浏览器登录 → 保存 auths/catpaw-{uid}.json。
//
// 流程（对齐桌面端 CatxPassportLoginProvider）：
//  1. 本地起 127.0.0.1 回调服务
//  2. GET /api/gateway/passport/login-config → loginEntryUrl
//  3. 拼接 state/redirect/sid 打开浏览器
//  4. 回调带 token+state 或轮询 /api/gateway/passport/poll-token?sid=
//  5. GET /api/gateway/passport/current-user → uid → 落盘
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth output dir")
	printOnly := flag.Bool("print-only", false, "server mode: print login link and poll (no local browser/callback)")
	flag.Parse()

	client := upstream.New(30 * time.Second)
	ctx := context.Background()

	// 1. 回调目标：本地模式起真实服务；print-only 用固定端口（浏览器回调打不开也没关系，
	//    靠 poll-token 通道拿 token，与 traework2api login.sh 的「粘贴回调」同理）。
	var (
		ln      net.Listener
		tokenCh chan string
		token   string
		err     error
	)
	callback := "http://127.0.0.1:37890/callback"
	if !*printOnly {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("loopback listen: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		callback = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
		tokenCh = make(chan string, 1)
		defer ln.Close()
	}

	// 2. login-config
	loginEntry, err := client.LoginConfig(ctx)
	if err != nil {
		log.Fatalf("fetch login-config: %v", err)
	}
	log.Printf("login entry: %s", loginEntry)

	state := randHex(16)
	sid := randHex(16)
	if ln != nil {
		go serveCallback(ln, state, tokenCh)
	}
	u, err := url.Parse(loginEntry)
	if err != nil {
		log.Fatalf("parse loginEntryUrl: %v", err)
	}
	q := u.Query()
	q.Set("state", state)
	q.Set("redirect", callback)
	q.Set("sid", sid)
	u.RawQuery = q.Encode()

	// 3. 打开浏览器（失败则打印链接）
	browserOK := false
	if !*printOnly {
		if err := openBrowser(u.String()); err == nil {
			browserOK = true
		}
	}
	if !browserOK {
		fmt.Printf("\n请手动打开以下链接登录：\n%s\n\n", u.String())
	} else {
		log.Printf("浏览器已打开，请在浏览器中完成登录…")
	}

	// 4. 回调 vs 轮询双通道（print-only 只有轮询）
	if !*printOnly {
		token, err = waitToken(ctx, client, sid, tokenCh)
	} else {
		token, err = waitPoll(ctx, client, sid)
	}
	if err != nil {
		log.Fatalf("login timeout: %v", err)
	}

	// 5. 用户信息 + 落盘
	user, err := client.CurrentUser(ctx, token)
	if err != nil {
		log.Fatalf("fetch current-user: %v", err)
	}
	if user == nil || user.UserID == "" {
		log.Fatalf("fetch current-user: empty user id")
	}
	a := auth.New(user.UserID, user.UserName, token)
	if err := auth.SaveNew(*authDir, a); err != nil {
		log.Fatalf("save auth: %v", err)
	}
	fmt.Printf("\n✅ 登录成功：uid=%s name=%s\n", user.UserID, user.UserName)
	fmt.Printf("凭证已保存：%s\n", filepath.Join(*authDir, a.FileName()))
	fmt.Println("提示：个人版 token 有效期约 72h，到期后重新运行本工具登录。")
}

// serveCallback 处理网关回调。expectedState 必须与授权请求生成的 state 完全一致，
// 防止本地回调被其它页面/进程注入不属于本次登录会话的 token。
func serveCallback(ln net.Listener, expectedState string, ch chan<- string) {
	_ = http.Serve(ln, callbackHandler(expectedState, ch))
}

func callbackHandler(expectedState string, ch chan<- string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		var token, state string
		switch r.Method {
		case http.MethodGet:
			token = r.URL.Query().Get("token")
			state = r.URL.Query().Get("state")
		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			if err != nil {
				http.Error(w, "读取回调失败", http.StatusBadRequest)
				return
			}
			ct := r.Header.Get("Content-Type")
			if strings.Contains(ct, "application/json") {
				var p struct {
					Token string `json:"token"`
					State string `json:"state"`
				}
				if err := json.Unmarshal(body, &p); err != nil {
					http.Error(w, "回调 JSON 无效", http.StatusBadRequest)
					return
				}
				token, state = p.Token, p.State
			} else {
				vals, err := url.ParseQuery(string(body))
				if err != nil {
					http.Error(w, "回调表单无效", http.StatusBadRequest)
					return
				}
				token, state = vals.Get("token"), vals.Get("state")
			}
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if expectedState == "" || state == "" || state != expectedState {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<h3>登录失败：state 校验失败，请重新发起登录。</h3>"))
			return
		}
		if token == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<h3>登录失败：缺少 token</h3>"))
			return
		}
		_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面。</h3>"))
		select {
		case ch <- token:
		default:
		}
	})
	return mux
}

func waitToken(ctx context.Context, client *upstream.Client, sid string, ch chan string) (string, error) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", fmt.Errorf("登录超时（5 分钟）")
		case tok := <-ch:
			return tok, nil
		case <-ticker.C:
			tok, err := client.PollToken(ctx, sid)
			if err == nil && tok != "" {
				return tok, nil
			}
		}
	}
}

// waitPoll 仅轮询 poll-token（服务端/无浏览器场景）。
func waitPoll(ctx context.Context, client *upstream.Client, sid string) (string, error) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", fmt.Errorf("登录超时（5 分钟）")
		case <-ticker.C:
			tok, err := client.PollToken(ctx, sid)
			if err == nil && tok != "" {
				return tok, nil
			}
		}
	}
}

func openBrowser(rawurl string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawurl)
	case "darwin":
		cmd = exec.Command("open", rawurl)
	default:
		cmd = exec.Command("xdg-open", rawurl)
	}
	return cmd.Start()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
