package cloud189

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"litepan/internal/domain"
)

type authTestTransport func(*http.Request) (*http.Response, error)

func (f authTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func authTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

// 模拟会话轮换刷新令牌，随后重建驱动并用数据库中的新令牌恢复过期会话。
func TestSessionRefreshTokenSurvivesDriverRecreation(t *testing.T) {
	for _, initialAccess := range []string{"mock-old-access", ""} {
		name := "existing_access"
		if initialAccess == "" {
			name = "refresh_first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			expires := time.Now().Add(24 * time.Hour)
			stored := &domain.AuthState{AccessToken: initialAccess, RefreshToken: "mock-original-refresh", TokenExpires: expires}
			sessions, refreshes := 0, 0
			transport := authTestTransport(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/api/oauth2/refreshToken.do":
					refreshes++
					want := "mock-session-refresh"
					if initialAccess == "" && refreshes == 1 {
						want = "mock-original-refresh"
					}
					if req.Method != http.MethodPost || req.FormValue("refreshToken") != want {
						t.Error("刷新请求未使用已保存的最新令牌")
					}
					return authTestResponse(http.StatusOK, `{"accessToken":"mock-new-access","refreshToken":"mock-exchange-refresh"}`), nil
				case "/getSessionForPC.action":
					sessions++
					if sessions == 2 {
						return authTestResponse(http.StatusBadRequest, `{"res_code":"UserInvalidOpenToken","res_message":"unifyAccountInfo is null"}`), nil
					}
					if sessions == 1 {
						return authTestResponse(http.StatusOK, `{"res_code":0,"sessionKey":"mock-session","sessionSecret":"mock-secret","refreshToken":"mock-session-refresh"}`), nil
					}
					return authTestResponse(http.StatusOK, `{"res_code":0,"sessionKey":"mock-session","sessionSecret":"mock-secret"}`), nil
				default:
					t.Errorf("意外请求路径：%s", req.URL.Path)
					return authTestResponse(http.StatusNotFound, `{}`), nil
				}
			})
			newDriver := func() *Driver {
				d := &Driver{client: &http.Client{Transport: transport}}
				d.SetAuthCredentials(domain.CredentialsFromState(stored))
				d.SetAuthPersister(func(_ context.Context, creds domain.AuthCredentials) error {
					stored = domain.MergeAuthCredentials(stored, creds)
					return nil
				})
				return d
			}
			d := newDriver()
			if err := d.Init(ctx); err != nil {
				t.Fatal(err)
			}
			wantAccess := initialAccess
			if wantAccess == "" {
				wantAccess = "mock-new-access"
			}
			if stored.RefreshToken != "mock-session-refresh" || stored.AccessToken != wantAccess || !stored.TokenExpires.Equal(expires) {
				t.Fatal("会话令牌未保存，或访问令牌/到期时间被意外覆盖")
			}
			if err := d.Drop(ctx); err != nil {
				t.Fatal(err)
			}
			recreated := newDriver()
			defer recreated.Drop(ctx)
			if err := recreated.Init(ctx); err != nil {
				t.Fatal(err)
			}
			wantRefreshes := 1
			if initialAccess == "" {
				wantRefreshes++
			}
			if refreshes != wantRefreshes || sessions != 3 || stored.RefreshToken != "mock-exchange-refresh" || !stored.TokenExpires.Equal(expires) {
				t.Fatal("重建驱动后未正确续期，或无令牌的会话响应覆盖了已保存凭据")
			}
		})
	}
}

func TestSessionRefreshTokenPersistenceFailure(t *testing.T) {
	wantErr := errors.New("mock persistence failure")
	d := &Driver{client: &http.Client{Transport: authTestTransport(func(*http.Request) (*http.Response, error) {
		return authTestResponse(http.StatusOK, `{"res_code":0,"sessionKey":"mock-session","sessionSecret":"mock-secret","refreshToken":"mock-session-refresh"}`), nil
	})}}
	defer d.Drop(context.Background())
	d.SetAuthPersister(func(_ context.Context, creds domain.AuthCredentials) error {
		// 回调可以读取驱动状态，验证持久化没有在 d.mu 锁内执行。
		if !d.mu.TryLock() {
			t.Error("持久化回调不能在驱动锁内执行")
			return wantErr
		}
		defer d.mu.Unlock()
		if creds.RefreshToken != "mock-session-refresh" || d.refreshToken != creds.RefreshToken {
			t.Error("持久化未收到会话返回的新令牌")
		}
		if creds.AccessToken != "" || !creds.TokenExpires.IsZero() {
			t.Error("会话令牌补写不应覆盖其他凭据字段")
		}
		return wantErr
	})
	err := d.refreshSession(context.Background(), "mock-access")
	if !errors.Is(err, wantErr) {
		t.Fatalf("持久化失败未向上传递：%v", err)
	}
	if ae, ok := domain.AsAppError(err); !ok || ae.Code != domain.CodeInternal {
		t.Fatalf("持久化失败应为内部错误：%v", err)
	}
	if d.hasSession() {
		t.Fatal("保存失败后不应接收新会话")
	}
}
